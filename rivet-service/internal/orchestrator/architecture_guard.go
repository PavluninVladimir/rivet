package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Граница задачи описания архитектуры на control plane (ревью Opus,
// круги 2 и 3): гейт -only-docs работает в рабочей копии runner'а, и агент
// может обмануть его состоянием .git. Поэтому перед merge, и в Rivet, и
// после внешнего merge на хостинге, список файлов PR берётся у хостинга:
// допустим только docs/architecture. Merge идёт по той вершине ветки PR,
// для которой проверены файлы.

// ErrArchitecturePRRejected — PR задачи описания меняет файлы вне
// docs/architecture или хостинг отдал неполный список: merge не
// выполняется, задача переведена в failed с причиной.
var ErrArchitecturePRRejected = errors.New("PR задачи описания архитектуры отклонён")

// ErrArchitectureFilesUnavailable — список файлов PR не получен (сбой
// хостинга) и после повторов: задача остаётся в review, человек видит
// эскалацию и повторяет merge позже.
var ErrArchitectureFilesUnavailable = errors.New("список файлов PR задачи описания не получен с хостинга")

// ErrArchitectureHeadMoved — ветка PR сдвинулась после проверки файлов:
// merge не выполнен, задача вернулась на проверку.
var ErrArchitectureHeadMoved = errors.New("ветка PR задачи описания сдвинулась после проверки")

// guardRetryDelays — паузы между повторами чтения файлов PR при сбое
// хостинга. Переменная, чтобы тесты не ждали.
var guardRetryDelays = []time.Duration{time.Second, 3 * time.Second}

// eventFilesLimit — сколько путей кладётся в событие (полный список вне
// каталога бывает в тысячи путей, событие идёт в поток и реплей журнала).
const eventFilesLimit = 50

// architecturePRFiles — файлы PR: метод адаптера, иначе разбор diff.
func architecturePRFiles(ctx context.Context, adapter scm.Adapter, repo string, num int) (scm.PRFileList, error) {
	if l, ok := adapter.(scm.PRFileLister); ok {
		return l.PRFiles(ctx, repo, num)
	}
	diff, err := adapter.Diff(ctx, repo, num)
	if err != nil {
		return scm.PRFileList{}, err
	}
	files := policy.PathsFromDiff(diff)
	if len(files) == 0 && strings.TrimSpace(diff) != "" {
		return scm.PRFileList{}, fmt.Errorf("diff PR не разобран: %w", scm.ErrPRFilesTruncated)
	}
	return scm.PRFileList{Files: files}, nil
}

// outsideArchitecture — пути вне каталога описания.
func outsideArchitecture(files []string) []string {
	prefix := architecture.DefaultDir + "/"
	var out []string
	for _, f := range files {
		if !strings.HasPrefix(strings.TrimPrefix(f, "/"), prefix) {
			out = append(out, f)
		}
	}
	return out
}

// guardArchitecturePR проверяет PR задачи описания и возвращает вершину
// ветки PR, для которой получены файлы. merged — PR уже смержен на
// хостинге (внешний merge): отменить его нельзя, при нарушении задача
// уходит в failed, человек разбирается с основной веткой.
func (e *Engine) guardArchitecturePR(ctx context.Context, task domain.Task, p domain.Project, adapter scm.Adapter, num int, merged bool) (string, error) {
	var list scm.PRFileList
	var err error
	for attempt := 0; ; attempt++ {
		list, err = architecturePRFiles(ctx, adapter, p.RepoPath, num)
		if err == nil || errors.Is(err, scm.ErrPRFilesTruncated) || attempt >= len(guardRetryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(guardRetryDelays[attempt]):
		}
	}
	suffix := mergeSuffix(merged)
	switch {
	case errors.Is(err, scm.ErrPRFilesTruncated):
		return "", e.rejectArchitecturePR(ctx, task, nil, true, merged)
	case err != nil:
		// Временный сбой хостинга: задача не падает, остаётся в review с
		// одной эскалацией. Если PR уже смержен на хостинге, повтора доставки
		// не будет (ни GitHub, ни GitLab сами не повторяют): сверка файлов
		// повторяется в фоне и сама доводит задачу до итога.
		msg := "файлы PR задачи описания архитектуры не получены с хостинга после повторов: " + err.Error()
		advice := ": повторите merge, когда хостинг ответит"
		if merged {
			advice = ": PR уже смержен, сверка файлов повторится в фоне; если хостинг так и не ответит, " +
				"повторите доставку webhook'а на хостинге (Redeliver у GitHub, Resend Request у GitLab)"
		}
		if _, aerr := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "task.merge_failed",
			ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID, Text: msg + suffix,
			Payload: map[string]any{"reason": "architecture_files_unavailable", "merged": merged},
		}); aerr != nil {
			return "", aerr
		}
		if eerr := e.St.EscalateOnce(ctx, p.ID, task.ID, domain.AttTestFailed, escFilesUnavailable, msg+advice); eerr != nil {
			return "", eerr
		}
		if merged {
			e.reverifyMergedArchitecture(task.ID)
		}
		return "", fmt.Errorf("%w: %v", ErrArchitectureFilesUnavailable, err)
	}
	outside := outsideArchitecture(list.Files)
	if len(outside) == 0 {
		return list.HeadSHA, nil
	}
	return "", e.rejectArchitecturePR(ctx, task, outside, false, merged)
}

func mergeSuffix(merged bool) string {
	if merged {
		return ". PR уже смержен на хостинге, проверьте основную ветку"
	}
	return ". Merge не выполнен"
}

// rejectArchitecturePR переводит задачу описания в failed: файлы вне
// docs/architecture или неполный список. Возвращает ErrArchitecturePRRejected.
func (e *Engine) rejectArchitecturePR(ctx context.Context, task domain.Task, outside []string, truncated, merged bool) error {
	if truncated {
		msg := "список файлов PR задачи описания архитектуры неполный, границу docs/architecture проверить нельзя" + mergeSuffix(merged)
		if ferr := e.failTaskBy(ctx, task, domain.ActorSystem, "architecture", msg,
			map[string]any{"reason": "architecture_files_truncated", "files": []string{}, "files_total": 0, "merged": merged}); ferr != nil {
			return ferr
		}
		return ErrArchitecturePRRejected
	}
	shown := outside
	if len(shown) > eventFilesLimit {
		shown = shown[:eventFilesLimit]
	}
	textShown := shown
	if len(textShown) > 10 {
		textShown = textShown[:10]
	}
	msg := "PR задачи описания архитектуры меняет файлы вне " + architecture.DefaultDir + ": " + strings.Join(textShown, ", ")
	if len(outside) > len(textShown) {
		msg += " и ещё " + strconv.Itoa(len(outside)-len(textShown))
	}
	if ferr := e.failTaskBy(ctx, task, domain.ActorSystem, "architecture", msg+mergeSuffix(merged),
		map[string]any{"reason": "architecture_outside_docs", "files": shown, "files_total": len(outside), "merged": merged}); ferr != nil {
		return ferr
	}
	return ErrArchitecturePRRejected
}

// recordCheckedSHA запоминает вершину ветки задачи описания, на которой
// идёт шаг проверки: merge привязан к ней (ревью Opus, финальная проверка).
// Хостинг не отдал вершину — ошибка, шаг откладывается и назначается
// повторно: с пустой вершиной merge не был бы привязан к проверенному
// коммиту (ревью ограничителя, P3-3).
func (e *Engine) recordCheckedSHA(ctx context.Context, task domain.Task, p domain.Project) error {
	if task.Branch == "" {
		return nil
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return fmt.Errorf("хостинг проекта: %w", err)
	}
	sha, err := adapter.HeadSHA(ctx, p.RepoPath, task.Branch)
	if err == nil && sha == "" {
		err = errors.New("хостинг вернул пустую вершину")
	}
	if err != nil {
		return fmt.Errorf("вершина ветки %s не получена с хостинга: %w", task.Branch, err)
	}
	return e.St.SetTaskCheckedSHA(ctx, task.ID, sha)
}

// headMovedLimit — сколько раз подряд задача описания возвращается на
// проверку из-за сдвига ветки PR, дальше эскалация.
const headMovedLimit = 3

// mergeArchitecturePR — merge PR задачи описания: проверка файлов, затем
// merge той вершины, что прошла шаг проверки. Вершина сдвинулась после
// проверки — задача возвращается на шаг проверки (новый push проверят
// заново), а не падает и не мержит непроверенное.
func (e *Engine) mergeArchitecturePR(ctx context.Context, task domain.Task, p domain.Project, adapter scm.Adapter, num int) (string, error) {
	head, err := e.guardArchitecturePR(ctx, task, p, adapter, num, false)
	if err != nil {
		return "", err
	}
	checked, err := e.St.TaskCheckedSHA(ctx, task.ID)
	if err != nil {
		return "", err
	}
	if checked != "" && head != "" && head != checked {
		return "", e.headMoved(ctx, task, p)
	}
	if checked != "" {
		head = checked
	}
	m, ok := adapter.(scm.SHAMerger)
	if !ok || head == "" {
		return adapter.Merge(ctx, p.RepoPath, num)
	}
	sha, err := m.MergeAt(ctx, p.RepoPath, num, head)
	if !errors.Is(err, scm.ErrHeadMoved) {
		return sha, err
	}
	return "", e.headMoved(ctx, task, p)
}

// Коды эскалаций задачи описания: открытая с тем же кодом одна, новое
// сообщение заменяет старое (store.EscalateOnce).
const (
	escFilesUnavailable = "architecture_files_unavailable"
	escHeadMoved        = "architecture_head_moved"
)

// ErrArchitectureHeadMovedLimit — ветка PR сдвигалась после проверки
// headMovedLimit раз за время задачи: задача осталась в review с
// эскалацией, merge делает человек.
// Оборачивает ErrArchitectureHeadMoved: merge не выполнен в обоих случаях.
var ErrArchitectureHeadMovedLimit = fmt.Errorf("%w: лимит возвратов на проверку исчерпан", ErrArchitectureHeadMoved)

// headMoved — ветка PR ушла дальше проверенной вершины: возврат на
// проверку, после headMovedLimit возвратов за время задачи эскалация
// (задача остаётся в review, merge делает человек).
func (e *Engine) headMoved(ctx context.Context, task domain.Task, p domain.Project) error {
	n, err := e.St.CountTaskEventsByReason(ctx, task.ID, "architecture_head_moved")
	if err != nil {
		return err
	}
	if n >= headMovedLimit {
		msg := fmt.Sprintf("ветка PR задачи описания сдвигалась после проверки уже %d раз за время задачи: проверьте, кто пушит в ветку, и смержите вручную", n)
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "task.merge_failed",
			ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID, Text: msg,
			Payload: map[string]any{"reason": "architecture_head_moved_limit", "returns": n},
		}); err != nil {
			return err
		}
		if err := e.St.EscalateOnce(ctx, p.ID, task.ID, domain.AttTestFailed, escHeadMoved, msg); err != nil {
			return err
		}
		return ErrArchitectureHeadMovedLimit
	}
	if err := e.recheckArchitecture(ctx, task); err != nil {
		return err
	}
	return ErrArchitectureHeadMoved
}

// recheckArchitecture возвращает задачу описания на шаг проверки.
func (e *Engine) recheckArchitecture(ctx context.Context, task domain.Task) error {
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	if _, ok := proc.Step(archStepCheck); !ok {
		return fmt.Errorf("шаг %s не найден в процессе задачи описания %s", archStepCheck, task.ID)
	}
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return err
	}
	e.cancelOpenRuns(ctx, task, 0)
	return e.gotoTarget(ctx, task, proc, hash, archStepCheck, policy.OutcomeOk, "", epic.Status != domain.EpicRunning, nil, "",
		&store.EventInput{
			Text:    "ветка PR задачи описания сдвинулась после проверки файлов — проверка заново, merge не выполнен",
			Payload: map[string]any{"reason": "architecture_head_moved"},
		})
}

// externalVerifyDelays — паузы фоновой сверки файлов уже смерженного PR,
// когда хостинг не отдал список при приёме webhook'а. Переменная для тестов.
var externalVerifyDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// reverifyMergedArchitecture — фоновая сверка файлов PR, смерженного на
// хостинге: при нарушении задача в failed (merged=true), иначе done. Если
// хостинг так и не ответил, задача остаётся в review, эскалация получает
// итоговый текст. Сверка одна на задачу: повтор доставки во время сверки
// новую не заводит. Проект и адаптер берутся на каждой попытке заново,
// поэтому сверку можно поднять и после рестарта rivetd (resumeExternalReverify).
func (e *Engine) reverifyMergedArchitecture(taskID string) {
	a := e.arch()
	a.mu.Lock()
	if a.reverify[taskID] {
		a.mu.Unlock()
		return
	}
	a.reverify[taskID] = true
	archInflight.Add(1)
	a.mu.Unlock()
	go func() {
		defer archInflight.Done()
		defer func() {
			a.mu.Lock()
			delete(a.reverify, taskID)
			a.mu.Unlock()
		}()
		ctx := context.Background()
		for _, d := range externalVerifyDelays {
			time.Sleep(d)
			finished, err := e.reverifyOnce(ctx, taskID)
			if finished {
				return
			}
			slog.Warn("сверка файлов смерженного PR задачи описания", "task", taskID, "err", err)
		}
		if err := e.reverifyGaveUp(ctx, taskID); err != nil {
			slog.Error("итог сверки файлов смерженного PR", "task", taskID, "err", err)
		}
	}()
}

// reverifyOnce — одна попытка фоновой сверки. finished — задача доведена
// до итога (сейчас или раньше: повтор доставки, человек), повторять не нужно.
func (e *Engine) reverifyOnce(ctx context.Context, taskID string) (finished bool, err error) {
	task, err := e.St.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	if task.Status != domain.TaskReview || task.PRURL == "" {
		return true, nil
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return false, err
	}
	num, err := prNumber(task.PRURL)
	if err != nil {
		// Адрес PR не разобрать: повтор ничего не изменит.
		slog.Error("сверка файлов смерженного PR: номер PR", "task", taskID, "err", err)
		return true, nil
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return false, err
	}
	list, err := architecturePRFiles(ctx, adapter, p.RepoPath, num)
	if err != nil && !errors.Is(err, scm.ErrPRFilesTruncated) {
		return false, err
	}
	if outside := outsideArchitecture(list.Files); err != nil || len(outside) > 0 {
		// Нарушение границы: тот же итог, что при приёме webhook'а.
		if rerr := e.rejectArchitecturePR(ctx, task, outside, err != nil, true); !errors.Is(rerr, ErrArchitecturePRRejected) {
			slog.Error("отказ задачи описания после сверки", "task", taskID, "err", rerr)
		}
		return true, nil
	}
	if err := e.completeMergedArchitecture(ctx, task, p); err != nil {
		slog.Error("завершение задачи описания после сверки", "task", taskID, "err", err)
	}
	return true, nil
}

// reverifyGaveUp — все попытки сверки исчерпаны: событие итога (сверку
// после рестарта больше не поднимать) и понятный текст в той же эскалации.
func (e *Engine) reverifyGaveUp(ctx context.Context, taskID string) error {
	task, err := e.St.GetTask(ctx, taskID)
	if err != nil || task.Status != domain.TaskReview {
		return err
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	msg := "сверка файлов смерженного PR задачи описания не удалась: хостинг так и не отдал список файлов. " +
		"Повторите доставку webhook'а о merge на хостинге (Redeliver у GitHub, Resend Request у GitLab), " +
		"сверка пройдёт заново. До этого задача остаётся в review"
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "task.merge_failed",
		ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID, Text: msg,
		Payload: map[string]any{"reason": "architecture_files_unverified", "merged": true},
	}); err != nil {
		return err
	}
	return e.St.EscalateOnce(ctx, p.ID, task.ID, domain.AttTestFailed, escFilesUnavailable, msg)
}

// resumeExternalReverify поднимает фоновые сверки, прерванные рестартом
// rivetd: очередь сверок живёт в памяти, а хостинг сам доставку не повторит.
func (e *Engine) resumeExternalReverify(ctx context.Context) {
	ids, err := e.St.PendingExternalReverify(ctx)
	if err != nil {
		slog.Error("сверка смерженных PR задач описания при старте", "err", err)
		return
	}
	for _, id := range ids {
		e.reverifyMergedArchitecture(id)
	}
}

// completeMergedArchitecture — PR смержен на хостинге, файлы в порядке:
// задача done, снимок перечитывается (публикации у задачи описания нет).
func (e *Engine) completeMergedArchitecture(ctx context.Context, task domain.Task, p domain.Project) error {
	if err := e.St.CompleteTask(ctx, task.ID, store.EventInput{
		ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "task.status",
		Text:    "PR задачи описания смержен на хостинге, файлы сверены — задача выполнена",
		Payload: map[string]any{"status": "done", "pr": task.PRURL},
	}); err != nil {
		return err
	}
	if err := e.St.RecomputeEpic(ctx, task.EpicID); err != nil {
		return err
	}
	e.RefreshArchitecture(p.ID, false)
	return nil
}

// GuardExternalArchitectureMerge — внешний merge задачи описания по
// webhook: проверка файлов PR. ErrArchitecturePRRejected — задача уже
// переведена в failed, done ставить нельзя. ErrArchitectureFilesUnavailable —
// список не получен: задача пока в review, сверка повторяется в фоне
// (хостинги потерянную доставку сами не повторяют).
func (e *Engine) GuardExternalArchitectureMerge(ctx context.Context, task domain.Task, p domain.Project) error {
	if task.Kind != domain.KindArchitecture || task.PRURL == "" {
		return nil
	}
	num, err := prNumber(task.PRURL)
	if err != nil {
		return err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return err
	}
	_, err = e.guardArchitecturePR(ctx, task, p, adapter, num, true)
	return err
}
