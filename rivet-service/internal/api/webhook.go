package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Приём событий хостингов (спека backend/scm-integration «Аутентичность
// webhook»): ручной merge PR/MR человеком корректно завершает задачу.
//
// Порядок проверки: из тела достаётся репозиторий и находится
// проект-кандидат — это ещё не доверие, а только выбор ключа; затем тело
// проверяется секретом этого проекта. Проект без своего секрета проверяется
// секретом установки. Нет ни того, ни другого — приём выключен (fail-closed).

// Виды событий хостинга, на которые реагирует конвейер (спека
// scm-integration «События хостинга в конвейере»).
const (
	hookMerge    = "merge"     // PR/MR смержен человеком
	hookPRClosed = "pr_closed" // PR/MR закрыт без merge
	hookChecks   = "checks"    // внешние проверки завершились
	hookReview   = "review"    // review человека на хостинге
	hookPush     = "push"      // push в ветку (снимок архитектуры, add-architecture-c4)
)

// hostingEvent — то общее, что конвейеру нужно от события любого хостинга.
// Пустой Kind — событие нас не интересует.
type hostingEvent struct {
	Kind     string
	RepoPath string // owner/name
	Branch   string // ветка-источник PR/MR или проверок
	URL      string
	MergeSHA string
	Actor    string // кто смержил, закрыл или оставил review
	// PRURL — адрес PR/MR события: у review в URL лежит сам комментарий,
	// а с задачей сверяется именно PR.
	PRURL     string
	Name      string // имя набора проверок
	Body      string // текст review
	State     string // review: approved | changes_requested | commented
	OK        bool   // checks: проверки прошли
	Malformed bool
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWebhookBody(w, r)
	if !ok {
		return
	}
	ev := parseGitHubEvent(r, body)
	if ev.Malformed {
		unprocessable(w, "невалидный payload")
		return
	}
	s.handleHostingEvent(w, r, string(scm.ProviderGitHub), body, ev, verifyGitHubSignature)
}

// gitlabWebhook — тот же конвейер для GitLab: вместо подписи HMAC общий
// токен в X-Gitlab-Token (api-contract add-repo-onboarding).
func (s *Server) gitlabWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWebhookBody(w, r)
	if !ok {
		return
	}
	ev := parseGitLabEvent(r, body)
	if ev.Malformed {
		unprocessable(w, "невалидный payload")
		return
	}
	s.handleHostingEvent(w, r, string(scm.ProviderGitLab), body, ev, verifyGitLabToken)
}

// Лимиты тела webhook'а: подпись проверяется по всему телу, поэтому оно
// читается целиком до проверки. Push несёт списки файлов коммитов и
// бывает больше мегабайта, ему лимит выше.
const (
	webhookBodyLimit = 1 << 20
	pushBodyLimit    = 8 << 20
)

// Тело webhook'а читается целиком до проверки подписи, а заголовок события
// ставит кто угодно. Поэтому одновременных чтений немного, лишнее ждёт
// места, память берётся по фактически пришедшим байтам, а чтение тела
// ограничено по времени. Место отпускается сразу после чтения: обработка
// (проверка подписи, merge, снимок) идёт вне ограничителя (ревью Opus,
// круги 2 и 3, финальная проверка). Потолка на адрес клиента нет: перед
// rivetd стоит TLS-прокси, и адрес у всех доставок один. Таймаут ставится
// на запрос, а не на сервер: общий ReadTimeout отменил бы контекст долгих
// потоков SSE.
//
// Потерянную доставку ни GitHub, ни GitLab сами не повторяют: её повторяют
// вручную (Redeliver у GitHub, Resend Request у GitLab).
// Параметры подобраны под таймаут доставки хостингов (10 с у GitHub и
// GitLab, ревью Opus, ограничитель): место стоит одну горутину, поэтому их
// много, память держит бюджет по пришедшим байтам. Простой чтения
// ограничен отдельно от общего времени: хостинги шлют тело сразу, а
// соединение, которое молчит, место не держит.
const (
	webhookReaders      = 128              // одновременных чтений тела
	webhookMemoryBudget = 64 << 20         // байт тел в чтении одновременно
	webhookSlotWait     = 3 * time.Second  // ожидание места, дальше 503
	webhookIdleTimeout  = 5 * time.Second  // простой чтения без новых байтов
	webhookReadTimeout  = 30 * time.Second // потолок чтения тела целиком
)

// webhookLimiter — места для чтения тел, бюджет памяти, ожидание места
// (дальше 503) и дедлайны чтения. Один на процесс, тесты подставляют свой
// в Server.
type webhookLimiter struct {
	slots chan struct{}
	mem   *semaphore.Weighted
	wait  time.Duration
	idle  time.Duration
	total time.Duration
}

func newWebhookLimiter(readers int, memory int64, wait time.Duration) *webhookLimiter {
	return &webhookLimiter{slots: make(chan struct{}, readers), mem: semaphore.NewWeighted(memory), wait: wait,
		idle: webhookIdleTimeout, total: webhookReadTimeout}
}

var defaultWebhookLimiter = newWebhookLimiter(webhookReaders, webhookMemoryBudget, webhookSlotWait)

func (s *Server) webhookLimits() *webhookLimiter {
	if s.webhookLim != nil {
		return s.webhookLim
	}
	return defaultWebhookLimiter
}

// errWebhookBudget — бюджет памяти занят дольше ожидания места.
var errWebhookBudget = errors.New("бюджет памяти webhook'ов занят")

// budgetReader берёт память из бюджета по мере прихода байтов: соединение
// без тела почти ничего не держит.
type budgetReader struct {
	r     io.Reader
	ctx   context.Context
	sem   *semaphore.Weighted // бюджет, из которого взята память
	wait  time.Duration
	taken int64
	// rc, idle, until — дедлайн простоя переставляется перед каждым
	// чтением, но не дальше общего потолка until.
	rc    *http.ResponseController
	idle  time.Duration
	until time.Time
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.rc != nil {
		d := time.Now().Add(b.idle)
		if d.After(b.until) {
			d = b.until
		}
		_ = b.rc.SetReadDeadline(d)
	}
	n, err := b.r.Read(p)
	if n > 0 {
		wctx, cancel := context.WithTimeout(b.ctx, b.wait)
		aerr := b.sem.Acquire(wctx, int64(n))
		cancel()
		if aerr != nil {
			return 0, errWebhookBudget
		}
		b.taken += int64(n)
	}
	return n, err
}

func (b *budgetReader) release() {
	if b.taken > 0 {
		b.sem.Release(b.taken)
		b.taken = 0
	}
}

func writePayloadTooLarge(w http.ResponseWriter) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]apiError{"error": {
		Code: "payload_too_large", Message: "payload webhook больше допустимого"}})
}

func writeWebhookBusy(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
		Code: "webhook_busy", Message: "приём webhook'ов перегружен, повторите доставку"}})
}

// readWebhookBody читает тело под ограничителем и сразу отпускает место.
// Content-Length больше лимита — 413 сразу, без чтения.
func (s *Server) readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	lim := s.webhookLimits()
	// Лимит по заголовку события: push без Content-Length (chunked через
	// прокси) тоже бывает больше мегабайта, а память и так считается по
	// пришедшим байтам.
	limit := int64(webhookBodyLimit)
	if r.Header.Get("X-GitHub-Event") == "push" || r.Header.Get("X-Gitlab-Event") == "Push Hook" {
		limit = pushBodyLimit
	}
	// Отказ без чтения тела: соединение закрывается, иначе сервер ждал бы
	// непришедшее тело, чтобы переиспользовать соединение, и ответ не ушёл
	// бы клиенту.
	reject := func() { w.Header().Set("Connection", "close") }
	if r.ContentLength > limit {
		reject()
		writePayloadTooLarge(w)
		return nil, false
	}
	select {
	case lim.slots <- struct{}{}:
	case <-time.After(lim.wait):
		reject()
		writeWebhookBusy(w)
		return nil, false
	case <-r.Context().Done():
		return nil, false
	}
	defer func() { <-lim.slots }()
	br := &budgetReader{r: http.MaxBytesReader(w, r.Body, limit), ctx: r.Context(), sem: lim.mem, wait: lim.wait,
		rc: http.NewResponseController(w), idle: lim.idle, until: time.Now().Add(lim.total)}
	defer br.release()
	body, err := io.ReadAll(br)
	br.release()
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writePayloadTooLarge(w)
		case errors.Is(err, errWebhookBudget):
			reject()
			writeWebhookBusy(w)
		default:
			unprocessable(w, "невалидный payload")
		}
		return nil, false
	}
	// Обработка дольше чтения: дедлайн чтения снимается, ответ пишется
	// без него (WriteTimeout сервера не задан).
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	return body, true
}

func verifyGitHubSignature(r *http.Request, body []byte, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want))
}

func verifyGitLabToken(r *http.Request, _ []byte, secret string) bool {
	got := r.Header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// handleHostingEvent — общая часть: выбор проекта, проверка подлинности,
// связывание с задачей и реакция конвейера по виду события.
func (s *Server) handleHostingEvent(w http.ResponseWriter, r *http.Request, provider string,
	body []byte, ev hostingEvent, verify func(*http.Request, []byte, string) bool) {

	if ev.RepoPath == "" {
		// Событие без репозитория: не с чем сопоставить ключ проверки.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	candidates, err := s.St.ProjectsByRepo(r.Context(), provider, ev.RepoPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(candidates) == 0 {
		// Репозиторий не наш: событие не подтверждено, существование
		// проектов не раскрываем.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Один и тот же путь может быть у проектов на разных инстансах, а
	// событие инстанс не называет: кандидата выбирает подпись. Секрет
	// проекта, иначе секрет установки; нет ни одного — приём выключен.
	// Push касается всех проектов репозитория с подходящей подписью
	// (снимок архитектуры у каждого свой), остальные события — первого.
	var project domain.Project
	var verified []domain.Project
	matched, anySecret := false, false
	for _, c := range candidates {
		secret := c.WebhookSecret
		if secret == "" {
			secret = s.WebhookSecret
		}
		if secret == "" {
			continue
		}
		anySecret = true
		if verify(r, body, secret) {
			if !matched {
				project, matched = c, true
			}
			verified = append(verified, c)
		}
	}
	if !anySecret {
		writeJSON(w, http.StatusForbidden,
			map[string]apiError{"error": {Code: "webhook_disabled", Message: "секрет webhook не настроен"}})
		return
	}
	if !matched {
		writeJSON(w, http.StatusUnauthorized,
			map[string]apiError{"error": {Code: "bad_signature", Message: "невалидная подпись webhook"}})
		return
	}
	// Подлинность проверена до всего остального; дальше решаем, интересует
	// ли нас это событие (спека: проверяется каждое входящее событие).
	if ev.Kind == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Push в основную ветку: снимок архитектуры перечитывается в фоне
	// (add-architecture-c4); при неизменном SHA чтения нет.
	if ev.Kind == hookPush {
		status := "ignored"
		for _, p := range verified {
			base := p.DefaultBranch
			if base == "" {
				base = "main"
			}
			if ev.Branch == base {
				s.refreshArchitecture(p.ID, false)
				status = "noted"
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
		return
	}

	task, err := s.St.TaskByBranch(r.Context(), ev.Branch)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Подпись подтверждает подлинность, но не принадлежность: задача обязана
	// быть из этого же проекта и (если PR создавали мы) про тот же PR.
	epic, err := s.St.GetEpic(r.Context(), task.EpicID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if epic.ProjectID != project.ID {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Событие сверяется с задачей по адресу PR, когда хостинг его дал:
	// отложенная повторная доставка со старого PR не должна попасть в
	// новую задачу на той же ветке. У прогонов проверок такой привязки
	// нет — там остаётся ветка (известное ограничение).
	if task.PRURL != "" && ev.PRURL != "" && ev.PRURL != task.PRURL {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	switch ev.Kind {
	case hookMerge:
		s.hostingMerge(w, r, project, task, ev)
	case hookPRClosed:
		if task.Status == domain.TaskDone {
			// Хостинг закрывает PR и после merge: задача уже завершена.
			writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
			return
		}
		if err := s.Engine.OnPRClosed(r.Context(), task, ev.Actor, ev.URL); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "noted"})
	case hookChecks:
		reacted, err := s.Engine.OnExternalChecks(r.Context(), task, ev.OK, ev.Name, ev.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": statusOf(reacted)})
	case hookReview:
		reacted, err := s.Engine.OnExternalReview(r.Context(), task, ev.State, ev.Actor, ev.Body, ev.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": statusOf(reacted)})
	}
}

// statusOf — ответ webhook'у: изменили конвейер или только записали событие.
func statusOf(reacted orchestrator.ExternalReacted) string {
	if reacted {
		return "done"
	}
	return "noted"
}

// hostingMerge — PR смержен человеком на хостинге: задача завершается и
// запускаются автопубликации, как при merge кнопкой.
func (s *Server) hostingMerge(w http.ResponseWriter, r *http.Request,
	project domain.Project, task domain.Task, ev hostingEvent) {

	if task.Status == domain.TaskDone {
		// Задача завершилась процессом без шага merge, а PR смержили позже:
		// точка merge ставится сейчас, иначе изменение не войдёт ни в один
		// релиз. Задача описания архитектуры в релиз не входит.
		if task.Kind != domain.KindArchitecture {
			if err := s.St.MarkMerged(r.Context(), task.ID); err != nil {
				writeErr(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
		return
	}
	// Хостинг ждёт ответа недолго и рвёт соединение: обрыв не должен
	// оборвать уже начатую обработку merge (публикацию, снимок).
	ctx := context.WithoutCancel(r.Context())
	// PR задачи описания с файлами вне docs/architecture: merge на хостинге
	// уже случился, задача уходит в failed с причиной, а не в done
	// (add-architecture-c4). Основная ветка изменилась, снимок перечитывается.
	if task.Kind == domain.KindArchitecture && task.Status != domain.TaskFailed {
		if err := s.Engine.GuardExternalArchitectureMerge(ctx, task, project); err != nil {
			switch {
			case errors.Is(err, orchestrator.ErrArchitecturePRRejected):
				s.refreshArchitecture(project.ID, false)
				writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
			case errors.Is(err, orchestrator.ErrArchitectureFilesUnavailable):
				// Список файлов не получен: доставку хостинг сам не повторит,
				// поэтому сверка идёт в фоне и доводит задачу до done или
				// failed; пока она идёт, задача в review с одной эскалацией.
				writeJSON(w, http.StatusAccepted, map[string]string{"status": "verifying"})
			default:
				writeErr(w, err)
			}
			return
		}
	}
	// Точка merge задаёт состав релиза (add-sdlc-cycle): у задачи описания
	// архитектуры её нет, документация в релиз не входит и не публикуется.
	complete := s.St.CompleteMergedTask
	if task.Kind == domain.KindArchitecture {
		complete = s.St.CompleteTask
	}
	if err := complete(ctx, task.ID,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: ev.Actor,
			Type: "task.status", Text: "PR смержен вручную на хостинге",
			Payload: map[string]any{"status": "done", "pr": ev.URL}}); err != nil {
		writeErr(w, err)
		return
	}
	// PR смержили, пока шли стадии задачи (review, проверки): они
	// прерываются, как при merge кнопкой.
	s.Engine.CancelRunsOfMerged(ctx, task)
	if err := s.St.RecomputeEpic(ctx, task.EpicID); err != nil {
		writeErr(w, err)
		return
	}
	s.refreshArchitecture(project.ID, false)
	// Внешний merge запускает автопубликации так же, как merge кнопкой
	// (спека deployment «Режимы запуска»), с теми же исключениями: задача
	// описания архитектуры и процесс без шага deploy. Ошибка постановки не
	// валит ответ: задача уже done, публикацию догонит следующий merge, а
	// проблема видна в логе.
	s.Engine.DeployAfterMerge(ctx, project.ID, task, ev.MergeSHA)
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}

func parseGitHubEvent(r *http.Request, body []byte) hostingEvent {
	var payload struct {
		Action string `json:"action"`
		// push: ветка и новая вершина.
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Merged         bool   `json:"merged"`
			HTMLURL        string `json:"html_url"`
			MergeCommitSHA string `json:"merge_commit_sha"`
			Head           struct {
				Ref string `json:"ref"`
			} `json:"head"`
			MergedBy struct {
				Login string `json:"login"`
			} `json:"merged_by"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Review struct {
			State   string `json:"state"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"review"`
		CheckSuite struct {
			HeadBranch string `json:"head_branch"`
			Conclusion string `json:"conclusion"`
			Status     string `json:"status"`
			App        struct {
				Name string `json:"name"`
			} `json:"app"`
		} `json:"check_suite"`
		WorkflowRun struct {
			Name       string `json:"name"`
			HeadBranch string `json:"head_branch"`
			Conclusion string `json:"conclusion"`
			Status     string `json:"status"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return hostingEvent{Malformed: true}
	}
	ev := hostingEvent{RepoPath: payload.Repository.FullName}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		if branch, ok := strings.CutPrefix(payload.Ref, "refs/heads/"); ok {
			ev.Kind, ev.Branch, ev.MergeSHA = hookPush, branch, payload.After
		}
	case "pull_request":
		if payload.Action != "closed" {
			return ev
		}
		ev.Branch, ev.URL = payload.PullRequest.Head.Ref, payload.PullRequest.HTMLURL
		ev.PRURL, ev.MergeSHA = payload.PullRequest.HTMLURL, payload.PullRequest.MergeCommitSHA
		if payload.PullRequest.Merged {
			ev.Kind, ev.Actor = hookMerge, payload.PullRequest.MergedBy.Login
			return ev
		}
		ev.Kind, ev.Actor = hookPRClosed, payload.Sender.Login
	case "pull_request_review":
		if payload.Action != "submitted" {
			return ev
		}
		ev.Kind = hookReview
		ev.Branch, ev.URL = payload.PullRequest.Head.Ref, payload.Review.HTMLURL
		ev.PRURL = payload.PullRequest.HTMLURL
		ev.Actor, ev.Body = payload.Review.User.Login, payload.Review.Body
		ev.State = reviewState(payload.Review.State)
	case "workflow_run":
		if payload.WorkflowRun.Status != "completed" {
			return ev
		}
		ok, decisive := checksOutcome(payload.WorkflowRun.Conclusion)
		if !decisive {
			return ev
		}
		ev.Kind, ev.Branch, ev.OK = hookChecks, payload.WorkflowRun.HeadBranch, ok
		ev.Name, ev.URL = payload.WorkflowRun.Name, payload.WorkflowRun.HTMLURL
	case "check_suite":
		if payload.CheckSuite.Status != "completed" {
			return ev
		}
		ok, decisive := checksOutcome(payload.CheckSuite.Conclusion)
		if !decisive {
			return ev
		}
		ev.Kind, ev.Branch, ev.OK = hookChecks, payload.CheckSuite.HeadBranch, ok
		ev.Name = payload.CheckSuite.App.Name
	}
	return ev
}

// firstNonEmpty — первое непустое значение.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// checksOutcome — итог набора проверок GitHub. Отменённый или пропущенный
// прогон вердикта не даёт: возвращать из-за него задачу в работу нельзя,
// и писать «проверки прошли» тоже неправда.
func checksOutcome(conclusion string) (ok, decisive bool) {
	switch strings.ToLower(conclusion) {
	case "success", "neutral":
		return true, true
	case "failure", "timed_out", "action_required", "startup_failure":
		return false, true
	}
	return false, false
}

// reviewState нормализует состояние review GitHub (APPROVED,
// CHANGES_REQUESTED, COMMENTED) в значения контракта.
func reviewState(s string) string {
	switch strings.ToLower(s) {
	case "approved":
		return "approved"
	case "changes_requested":
		return "changes_requested"
	}
	return "commented"
}

func parseGitLabEvent(r *http.Request, body []byte) hostingEvent {
	var payload struct {
		ObjectKind string `json:"object_kind"`
		// Push Hook: ветка и новая вершина.
		Ref   string `json:"ref"`
		After string `json:"after"`
		User  struct {
			Username string `json:"username"`
		} `json:"user"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ObjectAttributes struct {
			Action       string `json:"action"`
			State        string `json:"state"`
			SourceBranch string `json:"source_branch"`
			URL          string `json:"url"`
			MergeCommit  string `json:"merge_commit_sha"`
			SquashCommit string `json:"squash_commit_sha"`
			SHA          string `json:"sha"`
			// Pipeline Hook: ветка и статус пайплайна.
			Ref    string `json:"ref"`
			Status string `json:"status"`
			// Note Hook: текст комментария и к чему он относится.
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
		} `json:"object_attributes"`
		MergeRequest struct {
			SourceBranch string `json:"source_branch"`
			URL          string `json:"url"`
		} `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return hostingEvent{Malformed: true}
	}
	a := payload.ObjectAttributes
	ev := hostingEvent{RepoPath: payload.Project.PathWithNamespace, Actor: payload.User.Username}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Push Hook":
		if branch, ok := strings.CutPrefix(payload.Ref, "refs/heads/"); ok {
			ev.Kind, ev.Branch, ev.MergeSHA = hookPush, branch, payload.After
		}
	case "Merge Request Hook":
		ev.Branch, ev.URL, ev.PRURL = a.SourceBranch, a.URL, a.URL
		// При squash и fast-forward sha приезжает не в merge_commit_sha:
		// тот же порядок, что у собственного merge-адаптера GitLab.
		ev.MergeSHA = firstNonEmpty(a.MergeCommit, a.SquashCommit, a.SHA)
		switch {
		case a.Action == "merge" || a.State == "merged":
			ev.Kind = hookMerge
		case a.Action == "close" || a.State == "closed":
			ev.Kind = hookPRClosed
		case a.Action == "approved":
			ev.Kind, ev.State = hookReview, "approved"
		case a.Action == "unapproved":
			// В GitLab запрос изменений выражается снятием одобрения:
			// отдельного changes_requested в модели MR нет.
			ev.Kind, ev.State = hookReview, "changes_requested"
		}
	case "Pipeline Hook":
		switch a.Status {
		case "success", "failed":
			ev.Kind, ev.Branch, ev.URL = hookChecks, a.Ref, a.URL
			ev.OK = a.Status == "success"
			ev.Name = "pipeline"
		}
	case "Note Hook":
		if a.NoteableType != "MergeRequest" {
			return ev
		}
		ev.Kind, ev.State = hookReview, "commented"
		ev.Branch, ev.Body = payload.MergeRequest.SourceBranch, a.Note
		// URL — сам комментарий, PRURL — MR: сверка задачи идёт по второму.
		ev.URL, ev.PRURL = a.URL, payload.MergeRequest.URL
	}
	return ev
}
