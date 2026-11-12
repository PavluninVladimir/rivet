package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Состояние учётных данных хостинга проекта (change fix-credential-state,
// спека scm-integration «Учётные данные хостинга»). Отказ хостинга по
// авторизации делает подключение нерабочим с причиной, проект перестаёт
// получать назначения, а задачи ждут восстановления без расхода попыток.
// Успешная операция или проверка возвращает подключение в работу.

// ErrSCMCredentials — операция отклонена хостингом из-за нерабочих учётных
// данных проекта. Состояние уже отмечено, эскалация поднята: вызывающий не
// расходует попытку и ждёт восстановления подключения.
var ErrSCMCredentials = errors.New("учётные данные хостинга не работают")

// credentialRecheckEvery — как часто перепроверяются нерабочие учётные
// данные: доступ могли вернуть на хостинге без замены токена. Переменная
// для тестов.
var credentialRecheckEvery = 5 * time.Minute

// credentialRecheckBatch — сколько проектов перепроверяется за проход.
const credentialRecheckBatch = 20

// Откуда известно о состоянии учётных данных (поле source события).
const (
	credSourceOperation = "operation" // операция хостинга на control plane
	credSourceRunner    = "runner"    // git на runner'е, подтверждённый проверкой
	credSourceRecheck   = "recheck"   // повторная проверка подключения
)

// credentialProbeMemory — сколько помнится успешная проверка подключения
// после неоднозначного отказа (403, 404): повторяющийся отказ одной
// операции не должен каждый раз добавлять запросы к хостингу. 401
// проверяется всегда. Переменная для тестов.
var credentialProbeMemory = 5 * time.Minute

// fullAdapter — адаптер со всеми операциями конвейера. Все адаптеры
// хостингов реализуют их, поэтому обёртка не теряет возможностей.
type fullAdapter interface {
	scm.Adapter
	scm.PRFileLister
	scm.SHAMerger
	scm.TreeHasher
}

// Каждый адаптер хостинга обязан реализовать все операции конвейера:
// иначе track молча не обернул бы его и состояние учётных данных не
// отмечалось бы.
var (
	_ fullAdapter = (*scm.GitHub)(nil)
	_ fullAdapter = (*scm.GitLab)(nil)
	_ fullAdapter = (*scm.Gitea)(nil)
	_ fullAdapter = (*scm.Fake)(nil)
	_ fullAdapter = (*scm.FakeToken)(nil)
)

// trackedAdapter — адаптер проекта с собственными учётными данными: итог
// каждой операции отмечается в состоянии именно тех учётных данных, с
// которыми он собран (замена токена не портится старой операцией).
type trackedAdapter struct {
	inner  fullAdapter
	e      *Engine
	p      domain.Project
	credID string
	mu     sync.Mutex
	state  string // известное состояние учётных данных
}

// track оборачивает адаптер проекта; без собственных учётных данных
// следить не за чем (токен установки, стенд без токена).
func (e *Engine) track(a scm.Adapter, p domain.Project, cred store.ProjectCredentialState) scm.Adapter {
	full, ok := a.(fullAdapter)
	if cred.Token == "" || !ok {
		return a
	}
	return &trackedAdapter{inner: full, e: e, p: p, credID: cred.CredID, state: cred.State}
}

// observe отмечает итог операции. Отказ по учётным данным возвращается
// обёрнутым в ErrSCMCredentials; прочие ошибки — как есть.
func (t *trackedAdapter) observe(ctx context.Context, err error) error {
	if err == nil {
		t.works(ctx)
		return nil
	}
	if errors.Is(err, scm.ErrRateLimited) || !scm.AccessRefused(err) {
		return err
	}
	mark, ok := t.e.classifyRefusal(ctx, t.inner, t.p, t.credID, err)
	if !ok {
		return err
	}
	t.mu.Lock()
	t.state = store.CredentialInvalid
	t.mu.Unlock()
	t.e.markCredential(ctx, t.p, t.credID, mark)
	return fmt.Errorf("%w: %w", ErrSCMCredentials, err)
}

// works — операция прошла: нерабочие учётные данные снова рабочие.
func (t *trackedAdapter) works(ctx context.Context) {
	t.mu.Lock()
	known := t.state
	t.state = store.CredentialOK
	t.mu.Unlock()
	if known != store.CredentialOK {
		t.e.markCredential(ctx, t.p, t.credID, store.CredentialMark{State: store.CredentialOK, Source: credSourceOperation})
	}
}

// classifyRefusal решает, виноваты ли учётные данные. Ни один код ответа
// не однозначен: 403 и 404 бывают на одну операцию, а GitLab отвечает 401
// и рабочему токену без права merge. Поэтому решает проверка подключения:
// не прошла — доступ отозван, причина из проверки; прошла — отказ касается
// только операции. Недоступный хостинг подтверждает только 401: токен,
// который хостинг не принял, проверить больше нечем.
func (e *Engine) classifyRefusal(ctx context.Context, a scm.Adapter, p domain.Project, credID string, err error) (store.CredentialMark, bool) {
	// Память о свежей проверке гасит только неоднозначные 403 и 404: 401
	// после переноса GitLab-merge в ErrForbidden почти однозначен, и его
	// проверка стоит одного запроса.
	if !errors.Is(err, scm.ErrUnauthorized) && e.probedRecently(credID) {
		return store.CredentialMark{}, false
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res := a.Probe(pctx, p.RepoPath)
	switch {
	case res.OK:
		e.rememberProbe(credID)
		return store.CredentialMark{}, false
	case res.Reason == scm.ReasonUnreachable:
		if !errors.Is(err, scm.ErrUnauthorized) {
			return store.CredentialMark{}, false
		}
		return store.CredentialMark{State: store.CredentialInvalid, Reason: scm.ReasonBadToken,
			Message: "хостинг не принимает токен (401): он отозван, просрочен или неверен",
			Source:  credSourceOperation}, true
	}
	return store.CredentialMark{State: store.CredentialInvalid, Reason: res.Reason,
		Message: res.Message, Source: credSourceOperation}, true
}

// probedRecently — подключение проверялось успешно недавно: неоднозначный
// отказ операции не повод проверять снова.
func (e *Engine) probedRecently(credID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.credProbedOK[credID]
	return ok && e.Now().Sub(at) < credentialProbeMemory
}

func (e *Engine) rememberProbe(credID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.credProbedOK == nil {
		e.credProbedOK = map[string]time.Time{}
	}
	e.credProbedOK[credID] = e.Now()
}

func (e *Engine) forgetProbe(credID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.credProbedOK, credID)
}

// markCredential записывает состояние учётных данных credID; сбой записи
// не роняет операцию — следующая операция или проверка отметит его снова.
func (e *Engine) markCredential(ctx context.Context, p domain.Project, credID string, m store.CredentialMark) {
	if m.State == store.CredentialInvalid {
		e.forgetProbe(credID)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	changed, err := e.St.MarkCredential(wctx, p.ID, credID, m)
	if err != nil {
		slog.Error("состояние учётных данных хостинга", "project", p.ID, "state", m.State, "err", err)
		return
	}
	if changed {
		slog.Warn("состояние учётных данных хостинга изменилось", "project", p.ID, "state", m.State, "reason", m.Reason)
	}
}

func (t *trackedAdapter) CreatePR(ctx context.Context, repo, branch, base, title, body string) (scm.PR, error) {
	pr, err := t.inner.CreatePR(ctx, repo, branch, base, title, body)
	return pr, t.observe(ctx, err)
}

// OpenPR сохраняет поиск уже созданного PR через обёртку учётных данных.
func (t *trackedAdapter) OpenPR(ctx context.Context, repo, branch, base string) (scm.PR, bool, error) {
	finder, ok := t.inner.(scm.PRFinder)
	if !ok {
		return scm.PR{}, false, nil
	}
	pr, found, err := finder.OpenPR(ctx, repo, branch, base)
	return pr, found, t.observe(ctx, err)
}

func (t *trackedAdapter) Diff(ctx context.Context, repo string, number int) (string, error) {
	d, err := t.inner.Diff(ctx, repo, number)
	if errors.Is(err, scm.ErrDiffTruncated) {
		t.works(ctx)
		return d, err
	}
	return d, t.observe(ctx, err)
}

func (t *trackedAdapter) Merge(ctx context.Context, repo string, number int) (string, error) {
	sha, err := t.inner.Merge(ctx, repo, number)
	return sha, t.observe(ctx, err)
}

func (t *trackedAdapter) MergeAt(ctx context.Context, repo string, number int, headSHA string) (string, error) {
	sha, err := t.inner.MergeAt(ctx, repo, number, headSHA)
	return sha, t.observe(ctx, err)
}

func (t *trackedAdapter) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	sha, err := t.inner.HeadSHA(ctx, repo, branch)
	return sha, t.observe(ctx, err)
}

// Probe — проверка подключения сама по себе ответ о состоянии учётных
// данных: недоступный хостинг его не меняет.
func (t *trackedAdapter) Probe(ctx context.Context, repo string) scm.ProbeResult {
	res := t.inner.Probe(ctx, repo)
	switch {
	case res.OK:
		t.works(ctx)
	case res.Reason != scm.ReasonUnreachable:
		t.mu.Lock()
		t.state = store.CredentialInvalid
		t.mu.Unlock()
		t.e.markCredential(ctx, t.p, t.credID, store.CredentialMark{State: store.CredentialInvalid,
			Reason: res.Reason, Message: res.Message, Source: credSourceOperation})
	}
	return res
}

func (t *trackedAdapter) CreateRepo(ctx context.Context, in scm.NewRepo) (scm.RepoInfo, error) {
	info, err := t.inner.CreateRepo(ctx, in)
	return info, t.observe(ctx, err)
}

// RegisterWebhook — отказ в правах на хуки штатен (false без ошибки) и о
// состоянии учётных данных ничего не говорит.
func (t *trackedAdapter) RegisterWebhook(ctx context.Context, repo, url, secret string) (bool, error) {
	return t.inner.RegisterWebhook(ctx, repo, url, secret)
}

func (t *trackedAdapter) TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (scm.PipelineRun, error) {
	run, err := t.inner.TriggerPipeline(ctx, repo, pipeline, ref, vars)
	return run, t.observe(ctx, err)
}

func (t *trackedAdapter) PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, since time.Time) (scm.PipelineRun, error) {
	run, err := t.inner.PipelineRun(ctx, repo, pipeline, ref, runID, since)
	return run, t.observe(ctx, err)
}

// ReadFile — отсутствие файла или версии тоже ответ хостинга, но GitHub
// так же отвечает и на репозиторий без доступа: в рабочее состояние оно не
// возвращает.
func (t *trackedAdapter) ReadFile(ctx context.Context, repo, ref, path string) (scm.FileContent, error) {
	f, err := t.inner.ReadFile(ctx, repo, ref, path)
	return f, t.observe(ctx, err)
}

func (t *trackedAdapter) WriteFile(ctx context.Context, repo, ref, path, prevID, content, message string) (scm.Commit, error) {
	c, err := t.inner.WriteFile(ctx, repo, ref, path, prevID, content, message)
	return c, t.observe(ctx, err)
}

// BranchProtected — «не защищена» хостинги отвечают и без прав на чтение
// защиты (403, 404): такой ответ не доказывает, что учётные данные снова
// работают.
func (t *trackedAdapter) BranchProtected(ctx context.Context, repo, branch string) (bool, error) {
	ok, err := t.inner.BranchProtected(ctx, repo, branch)
	if err == nil && !ok {
		return false, nil
	}
	return ok, t.observe(ctx, err)
}

func (t *trackedAdapter) PRFiles(ctx context.Context, repo string, number int) (scm.PRFileList, error) {
	l, err := t.inner.PRFiles(ctx, repo, number)
	return l, t.observe(ctx, err)
}

// PathSHA — пустой ответ хостинги дают и на 404 (каталога нет или
// репозиторий не виден): он подключение в работу не возвращает.
func (t *trackedAdapter) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	sha, err := t.inner.PathSHA(ctx, repo, ref, dir)
	if err == nil && sha == "" {
		return "", nil
	}
	return sha, t.observe(ctx, err)
}

// credentialExclusions — проекты с нерабочими учётными данными: им не
// назначается ничего, пока подключение не восстановится (как при
// исчерпанном бюджете, граница — стадия).
func (e *Engine) credentialExclusions(ctx context.Context) ([]string, error) {
	return e.St.InvalidCredentialProjects(ctx)
}

// recheckCredentials перепроверяет нерабочие учётные данные: список берётся
// на тике (дешёвый запрос, не чаще раза в минуту), а хостинг опрашивается
// в фоне — тик его не ждёт, одновременно идёт один проход.
func (e *Engine) recheckCredentials(ctx context.Context) {
	e.mu.Lock()
	if e.credRecheckRunning || e.Now().Sub(e.credRecheckAt) < time.Minute {
		e.mu.Unlock()
		return
	}
	e.credRecheckAt = e.Now()
	e.mu.Unlock()
	due, err := e.St.CredentialsToRecheck(ctx, credentialRecheckEvery, credentialRecheckBatch)
	if err != nil {
		slog.Error("повторная проверка учётных данных", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	e.mu.Lock()
	e.credRecheckRunning = true
	e.mu.Unlock()
	go func() {
		defer func() {
			e.mu.Lock()
			e.credRecheckRunning = false
			e.mu.Unlock()
		}()
		e.recheck(context.WithoutCancel(ctx), due)
	}()
}

// RecheckCredentials — один проход повторной проверки нерабочих учётных
// данных, которые не проверялись дольше credentialRecheckEvery.
func (e *Engine) RecheckCredentials(ctx context.Context) {
	due, err := e.St.CredentialsToRecheck(ctx, credentialRecheckEvery, credentialRecheckBatch)
	if err != nil {
		slog.Error("повторная проверка учётных данных", "err", err)
		return
	}
	e.recheck(ctx, due)
}

// recheck проверяет подключение проектов: прошла — учётные данные снова
// рабочие, эскалация снимается, задачи проекта продолжают конвейер; нет —
// обновляются причина и время проверки. Недоступный хостинг состояние не
// меняет.
func (e *Engine) recheck(ctx context.Context, projects []domain.Project) {
	for _, p := range projects {
		// Попытка отмечается до всего остального и при любом исходе:
		// недоступный инстанс, лимит запросов или нерасшифровываемый токен не
		// держат очередь проверок.
		if err := e.St.TouchCredentialRecheck(ctx, p.CredentialID); err != nil {
			slog.Error("повторная проверка учётных данных", "project", p.ID, "err", err)
			continue
		}
		cred, err := e.St.ProjectTokenState(ctx, p.ID, e.Box)
		if err != nil || cred.Token == "" {
			if err != nil {
				slog.Error("повторная проверка учётных данных: токен проекта", "project", p.ID, "err", err)
			}
			continue
		}
		a, err := e.Adapters.For(scm.Provider(p.Provider), p.BaseURL, cred.Token)
		if err != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res := a.Probe(pctx, p.RepoPath)
		cancel()
		switch {
		case res.OK:
			e.markCredential(ctx, p, cred.CredID, store.CredentialMark{State: store.CredentialOK, Source: credSourceRecheck})
		case res.Reason == scm.ReasonUnreachable:
		default:
			e.markCredential(ctx, p, cred.CredID, store.CredentialMark{State: store.CredentialInvalid,
				Reason: res.Reason, Message: res.Message, Source: credSourceRecheck})
		}
	}
}

// verdictSCMAuth — вердикт runner'а: git-операция (clone, push) отклонена
// хостингом по авторизации. Попытка не расходуется, если проверка
// подключения подтверждает, что виноваты учётные данные.
const verdictSCMAuth = "scm_auth"

// runnerAuthFailure — runner сообщил об отказе git по авторизации. Если
// проверка подключения подтверждает нерабочие учётные данные, запуск
// откладывается без вердикта (попытка не расходуется), проект ждёт
// восстановления. true — результат стадии обработан.
func (e *Engine) runnerAuthFailure(ctx context.Context, task domain.Task, run store.StepRun, runnerID, detail string) (bool, error) {
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return false, err
	}
	cred, err := e.St.ProjectTokenState(ctx, p.ID, e.Box)
	if err != nil || cred.Token == "" {
		// Токен установки или стенд без токена: учётных данных проекта нет,
		// стадия разбирается как обычно.
		return false, err
	}
	a, err := e.Adapters.For(scm.Provider(p.Provider), p.BaseURL, cred.Token)
	if err != nil {
		return false, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	res := a.Probe(pctx, p.RepoPath)
	cancel()
	if res.OK || res.Reason == scm.ReasonUnreachable {
		// Проверка не подтверждает отзыв: причина не в учётных данных,
		// стадия разбирается как обычно.
		return false, nil
	}
	e.markCredential(ctx, p, cred.CredID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: res.Reason, Message: res.Message, Source: credSourceRunner})
	e.deferRun(ctx, task, run, runnerID, "учётные данные хостинга не работают: "+res.Message)
	slog.Warn("git на runner'е отклонён хостингом: запуск отложен до восстановления подключения",
		"task", task.ID, "runner", runnerID, "detail", detail)
	return true, nil
}

// credentialResumeLease — сколько держится захват продолжения задачи после
// восстановления подключения. Продолжение не удалось (сбой хостинга или
// базы, рестарт) — следующая попытка после этой паузы: постоянный отказ не
// бьёт хостинг на каждом тике. Переменная для тестов.
var credentialResumeLease = time.Minute

// awaitCredentials — переход на шаг target не выполнен из-за нерабочих
// учётных данных (PR не открылся). Исход шага остаётся закреплённым, у
// задачи отметка ожидания с целью перехода: после восстановления
// подключения её продолжает resumeAwaitingCredentials тем же переходом, без
// повторной оценки и повторного применения исхода шага (прерванные
// участники require: any и отказы шага не мешают). Отметка ставится до
// освобождения runner'а, попытка не расходуется.
func (e *Engine) awaitCredentials(ctx context.Context, task domain.Task, target, entry, what string, cause error) error {
	reason := "учётные данные хостинга не работают: " + what + " после восстановления подключения"
	if err := e.St.SetTaskCredentialsWait(ctx, task.ID, reason, target, entry); err != nil {
		return err
	}
	if err := e.St.FreeTaskRunner(ctx, task.ID); err != nil {
		return err
	}
	projectID, epicID, err := e.St.TaskRefs(ctx, task.ID)
	if err != nil {
		return err
	}
	_, err = e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "task.wait",
		ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Text:    "ожидание: " + reason + " (шаг " + task.StepID + ")",
		Payload: map[string]any{"step": task.StepID, "next": target, "reason": reason, "credentials": true, "error": cause.Error()},
	})
	return err
}

// resumeAwaitingCredentials продолжает задачи, ждавшие восстановления
// подключения: PR открывается, задача идёт на шаг, куда шла. Снова не
// вышло из-за учётных данных — опять ждёт; иная ошибка — повтор после
// аренды захвата с настоящей причиной в ожидании, а после нескольких
// неудач подряд — эскалация.
func (e *Engine) resumeAwaitingCredentials(ctx context.Context) {
	tasks, err := e.St.AwaitingCredentials(ctx, credentialResumeLease)
	if err != nil {
		slog.Error("продолжение задач после восстановления подключения", "err", err)
		return
	}
	for _, task := range tasks {
		if err := e.resumeAwaiting(ctx, task); err != nil {
			slog.Error("продолжение задачи после восстановления подключения: повтор после паузы",
				"task", task.ID, "retry_in", credentialResumeLease, "err", err)
		}
	}
}

// credentialResumeAttempts — сколько неудачных продолжений подряд (сбой
// хостинга не про учётные данные: PR уже есть, нет права на PR, ветку
// удалили, долгий 5xx) задача выдерживает, прежде чем уйти человеку.
const credentialResumeAttempts = 5

func (e *Engine) resumeAwaiting(ctx context.Context, task domain.Task) error {
	claim, claimed, err := e.St.ClaimCredentialsWait(ctx, task.ID, credentialResumeLease)
	if err != nil || !claimed {
		return err
	}
	err = e.continueAwaiting(ctx, task, claim)
	if err == nil || errors.Is(err, ErrSCMCredentials) {
		return nil
	}
	return e.resumeFailed(ctx, task, err)
}

// continueAwaiting — PR и переход ждавшей задачи. ErrSCMCredentials —
// подключение снова не работает, задача опять ждёт его (уже отмечено).
func (e *Engine) continueAwaiting(ctx context.Context, task domain.Task, claim store.CredentialsClaim) error {
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	if task.PRURL == "" {
		withPR, err := e.ensurePR(ctx, task)
		if err != nil {
			if errors.Is(err, ErrSCMCredentials) {
				if werr := e.awaitCredentials(ctx, task, claim.Target, claim.Entry, "PR откроется", err); werr != nil {
					return werr
				}
			}
			return err
		}
		task = withPR
	}
	// Отчёт проверок для ревьюера живёт в памяти: после рестарта, пока
	// задача ждала, он собирается заново из запусков шага.
	e.restoreReviewContext(ctx, task, proc, claim.Entry)
	// Захват мог истечь и достаться другому проходу: переход делает только
	// владелец свежего захвата. Отметку ожидания снимает вход на шаг.
	if _, ok, err := e.St.RenewCredentialsClaim(ctx, task.ID, claim); err != nil || !ok {
		return err
	}
	return e.gotoTarget(ctx, task, proc, hash, claim.Target, claim.Entry, "", false, map[string]any{}, task.PRURL)
}

// resumeFailed — продолжение упало не из-за учётных данных: причина
// ожидания показывает ошибку хостинга, повтор после аренды захвата, а
// после credentialResumeAttempts неудач подряд задача уходит человеку
// (failed с эскалацией, как невосстановимый отказ стадии): ждать дальше
// бессмысленно, а из ожидания человек задачу не достанет.
func (e *Engine) resumeFailed(ctx context.Context, task domain.Task, cause error) error {
	reason := "PR не открылся после восстановления подключения: " + cause.Error()
	n, err := e.St.NoteCredentialsResumeFailure(ctx, task.ID, reason+" — повтор через минуту")
	if err != nil || n == 0 {
		return errors.Join(cause, err)
	}
	limit := credentialResumeAttempts
	// Короткий сбой хостинга не должен массово эскалировать задачи через пять минут.
	if scm.Transient(cause) {
		limit = 30
	}
	if n < limit {
		return fmt.Errorf("попытка %d из %d: %w", n, limit, cause)
	}
	proc, _, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	var target string
	if err := e.St.Pool.QueryRow(ctx, `SELECT wait_target FROM tasks WHERE id=$1`, task.ID).Scan(&target); err != nil {
		return err
	}
	next, ok := proc.Step(target)
	if !ok {
		return fmt.Errorf("шаг продолжения %q не найден", target)
	}
	var via *store.EventInput
	if task.Status == domain.TaskReady {
		via = &store.EventInput{ActorKind: domain.ActorSystem, Type: "task.status", Text: "PR ветки не создан: переход к решению", Payload: map[string]any{"status": string(domain.TaskReview)}}
	}
	return e.prFail(ctx, task, next, cause, map[string]any{"credentials_resume": n}, via)
}

// restoreReviewContext — отчёт проверок шага test для ревьюера, если
// контекст стадии потерян (рестарт rivetd во время ожидания).
func (e *Engine) restoreReviewContext(ctx context.Context, task domain.Task, proc *policy.Resolved, entry string) {
	if e.peekStageContext(task.ID) != "" || entry != policy.OutcomeOk {
		return
	}
	step, ok := proc.Step(task.StepID)
	if !ok || step.Kind != policy.StepTest {
		return
	}
	runs, err := e.St.StepRuns(ctx, task.ID, step.ID, task.StepGen)
	if err != nil {
		return
	}
	verdicts, detail := e.stepVerdicts(ctx, step, runs, policy.OutcomeOk)
	if detail == "" {
		return
	}
	label := "Результаты автопроверок:\n"
	if testerRan(verdicts) {
		label = "Отчёт тестировщика и автопроверок:\n"
	}
	e.setStageContext(task.ID, label+detail)
}

// credentialsInvalid — учётные данные проекта задачи нерабочие: новые
// стадии проекта не запускаются, в том числе на том же runner'е.
func (e *Engine) credentialsInvalid(ctx context.Context, projectID string) bool {
	invalid, err := e.St.ProjectCredentialsInvalid(ctx, projectID)
	if err != nil {
		slog.Error("состояние учётных данных проекта", "project", projectID, "err", err)
		return false
	}
	return invalid
}
