package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// PR задачи (fix-pr-without-test-step): создаётся перед первым шагом,
// которому он нужен (review читает diff, merge мержит PR), при любом
// составе процесса. Временный сбой хостинга повторяется с паузой, PR,
// созданный прошлым запросом, находится по ветке. Задача без PR на review не
// входит и по шагу merge в done не переходит.

// prRetryPauses — паузы повторов создания PR при временном сбое хостинга
// (5xx, 429, обрыв, таймаут). Переменная для тестов.
var prRetryPauses = []time.Duration{2 * time.Second, 5 * time.Second}

// prResumePause — пауза между попытками создать PR у задачи, которая
// вернулась на review без PR и ждёт хостинг. Переменная для тестов.
var prResumePause = 30 * time.Second

// Пауза повтора после отказа по лимиту запросов хостинга (вторичный лимит
// GitHub на создание контента): не меньше prRateLimitPause, по заголовку
// Retry-After, но не больше prRateLimitMaxPause. Создание PR идёт в
// обработке результата стадии, долгая пауза задержала бы heartbeat'ы
// runner'а. Переменные для тестов.
var (
	prRateLimitPause    = 10 * time.Second
	prRateLimitMaxPause = 20 * time.Second
)

// retryPause — пауза перед повтором создания PR после ошибки err.
func retryPause(base time.Duration, err error) time.Duration {
	if !errors.Is(err, scm.ErrRateLimited) {
		return base
	}
	return min(max(base, prRateLimitPause, scm.RetryAfter(err)), max(base, prRateLimitMaxPause))
}

// prSeriesTimeout — общий предел серии попыток создать PR: запросы к
// хостингу, паузы и поиск открытого PR. Серия идёт в обработке результата
// стадии или в запросе человека, дольше держать их нельзя. Переменная для
// тестов.
var prSeriesTimeout = 45 * time.Second

// tickKey — признак прохода планировщика в контексте: там PR создаётся
// одной попыткой без пауз, а повтор делает следующий проход. Пауза в
// проходе держала бы назначения всех задач.
type tickKey struct{}

func tickContext(ctx context.Context) context.Context { return context.WithValue(ctx, tickKey{}, true) }

func inTick(ctx context.Context) bool {
	v, _ := ctx.Value(tickKey{}).(bool)
	return v
}

// ensurePR — PR ветки задачи, если его ещё нет; задача перечитывается. Уже
// созданный PR не пересоздаётся. В проходе планировщика одна попытка.
func (e *Engine) ensurePR(ctx context.Context, task domain.Task) (domain.Task, error) {
	pauses := prRetryPauses
	if inTick(ctx) {
		pauses = nil
	}
	return e.ensurePRWith(ctx, task, pauses)
}

// ensurePRWith — ensurePR с паузами повторов при временном сбое хостинга.
func (e *Engine) ensurePRWith(ctx context.Context, task domain.Task, pauses []time.Duration) (domain.Task, error) {
	if task.PRURL != "" {
		return task, nil
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return task, err
	}
	ctx, cancel := context.WithTimeout(ctx, prSeriesTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		err = e.createPR(ctx, task, p)
		if err == nil {
			// PR есть: эскалация «PR не создан» больше не правда.
			if rerr := e.St.ResolveTaskEscalation(ctx, task.ID, domain.AttPRNotCreated); rerr != nil {
				slog.Error("create PR: закрытие эскалации", "task", task.ID, "err", rerr)
			}
			return e.St.GetTask(ctx, task.ID)
		}
		if !scm.Transient(err) || attempt >= len(pauses) {
			return task, err
		}
		slog.Warn("create PR: временный сбой хостинга, повтор", "task", task.ID, "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return task, err
		case <-time.After(retryPause(pauses[attempt], err)):
		}
	}
}

// createPR открывает PR ветки задачи. Если хостинг ответил, что PR ветки
// уже есть (прошлый запрос его создал, ответ потерялся), записывается
// найденный открытый PR из ветки самого репозитория в базовую ветку
// проекта: дубля и ложной эскалации нет. PR из форка и в другую ветку не
// подходят, несколько подходящих — отказ, выбирать среди них нельзя.
func (e *Engine) createPR(ctx context.Context, task domain.Task, p domain.Project) error {
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return err
	}
	pr, err := adapter.CreatePR(ctx, p.RepoPath, task.Branch, p.DefaultBranch,
		fmt.Sprintf("task-%d: %s", task.Num, task.Title), task.Description)
	if err == nil {
		return e.St.SetTaskPR(ctx, task.ID, pr.URL)
	}
	if f, ok := adapter.(scm.PRFinder); ok && task.Branch != "" && errors.Is(err, scm.ErrPRExists) {
		open, found, ferr := f.OpenPR(ctx, p.RepoPath, task.Branch, p.DefaultBranch)
		switch {
		case ferr != nil:
			return fmt.Errorf("create PR: %w (поиск открытого PR ветки: %w)", err, ferr)
		case found:
			slog.Info("create PR: открытый PR ветки уже есть", "task", task.ID, "pr", open.URL)
			return e.St.SetTaskPR(ctx, task.ID, open.URL)
		}
	}
	return fmt.Errorf("create PR: %w", err)
}

// prFailed — PR перед шагом review создать не удалось (хостинг отказал, в
// ветке нет изменений). Исход шага уже применён, на review без PR задача не
// идёт: она эскалируется с ответом хостинга кодом PR_NOT_CREATED, а её шаг
// переставляется на review. «Повторить» создаёт PR и продолжает с review,
// агент пройденного шага заново не запускается.
func (e *Engine) prFailed(ctx context.Context, task domain.Task, next policy.ResolvedStep, cause error, evPayload map[string]any) error {
	return e.prFail(ctx, task, next, cause, evPayload, nil)
}

// prFail — prFailed одной транзакцией: шаг задачи, переход в failed и
// эскалация. via — событие промежуточного перехода в review для задачи в
// ready (перехода ready → failed в матрице нет).
func (e *Engine) prFail(ctx context.Context, task domain.Task, next policy.ResolvedStep, cause error, evPayload map[string]any,
	via *store.EventInput) error {
	slog.Error("create PR", "task", task.ID, "step", next.ID, "err", cause)
	payload := map[string]any{"pr_error": cause.Error(), "next": next.ID, "reason": "pr_not_created"}
	for k, v := range evPayload {
		payload[k] = v
	}
	ev, fail := e.failParts(ctx, task, domain.AttPRNotCreated, domain.ActorSystem, "",
		fmt.Sprintf("не удалось создать PR ветки %s перед шагом %s: %v. «Повторить» создаст PR и продолжит с шага %s, работа агента не повторяется. Если в ветке нет изменений, задачу можно доработать в сессии из консоли или отменить",
			task.Branch, next.Title, cause, next.Title),
		payload)
	mutate := func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET step_id=$2, step_entry=$3, updated_at=now() WHERE id=$1`,
			task.ID, next.ID, policy.OutcomeOk); err != nil {
			return err
		}
		return fail(tx)
	}
	if via != nil {
		return e.St.TransitionTaskVia(ctx, task.ID, domain.TaskReview, domain.TaskFailed, *via, ev, mutate)
	}
	return e.St.TransitionTask(ctx, task.ID, domain.TaskFailed, ev, mutate)
}

// prWaitAtReview — временный сбой хостинга при переходе на review в
// проходе планировщика. Пауз там нет, поэтому задача без эскалации ждёт в
// ready на шаге review с причиной. Следующую попытку делает prBeforeResume
// не раньше prResumePause, после prResumeAttempts неудач задача
// эскалируется, как отказ перед review.
func (e *Engine) prWaitAtReview(ctx context.Context, task domain.Task, next policy.ResolvedStep, cause error) error {
	slog.Warn("create PR перед review: временный сбой хостинга в проходе планировщика", "task", task.ID, "err", cause)
	e.mu.Lock()
	if e.prWait == nil {
		e.prWait = map[string]prWaitState{}
	}
	e.prWait[task.ID] = prWaitState{next: time.Now().Add(prResumePause), fails: 1}
	e.mu.Unlock()
	return e.St.TransitionTask(ctx, task.ID, domain.TaskReady, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "task.status",
		Text:    "шаг " + next.Title + " ждёт PR: хостинг не ответил, повтор позже",
		Payload: map[string]any{"status": string(domain.TaskReady), "next": next.ID, "reason": "pr_not_created", "pr_error": cause.Error()},
	}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL WHERE task_id=$1`, task.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tasks SET step_id=$2, step_entry=$3, wait_reason=$4, wait_kind='' WHERE id=$1`,
			task.ID, next.ID, policy.OutcomeOk, "PR ветки не создан: "+cause.Error())
		return err
	})
}

// retryMergePRs — задачи, которые ждут на шаге merge без PR (хостинг не
// ответил при входе в merge), пробуют создать PR раз в prResumePause одной
// попыткой без пауз. С PR дальше решает авто-merge по политике. Неудача
// ничего не пишет: событие о неудачном merge и эскалация уже есть. Первый
// проход после старта пропускается: эти задачи берёт ResumePendingPRs.
func (e *Engine) retryMergePRs(ctx context.Context) {
	e.mu.Lock()
	now := time.Now()
	due := !e.prMergeRetryAt.IsZero() && !now.Before(e.prMergeRetryAt)
	if e.prMergeRetryAt.IsZero() || due {
		e.prMergeRetryAt = now.Add(prResumePause)
	}
	e.mu.Unlock()
	if !due {
		return
	}
	tasks, err := e.St.TasksAwaitingPR(ctx)
	if err != nil {
		slog.Error("PR задач на merge", "err", err)
		return
	}
	for _, task := range tasks {
		projectID, _, err := e.St.TaskRefs(ctx, task.ID)
		if err != nil || e.credentialsInvalid(ctx, projectID) {
			continue
		}
		proc := store.TaskProcess(task)
		if proc == nil {
			continue
		}
		if step, ok := proc.Step(task.StepID); !ok || (step.Kind != policy.StepMerge && step.Kind != policy.StepReview) {
			continue
		}
		p, _, err := e.projectOf(ctx, task)
		if err == nil && e.credentialsInvalid(ctx, p.ID) {
			continue
		}
		if err != nil {
			continue
		}
		withPR, err := e.ensurePR(ctx, task)
		if err != nil {
			slog.Warn("PR задачи на merge: хостинг снова не ответил", "task", task.ID, "err", err)
			continue
		}
		if step, ok := proc.Step(task.StepID); ok && step.Kind == policy.StepReview {
			continue
		}
		eff, err := e.St.EffectivePolicy(ctx, p.ID)
		if err != nil {
			continue
		}
		if err := e.autoMerge(ctx, withPR, p, eff); err != nil {
			slog.Error("авто-merge после создания PR", "task", task.ID, "err", err)
		}
	}
}

// prMissingAtMerge — у задачи на шаге merge нет PR, и хостинг его не
// создал: merge не выполняется, задача ждёт на merge. Событие о неудачном
// merge и одна эскалация PR_NOT_CREATED: человек подтвердит merge, когда
// хостинг ответит (подтверждение создаёт PR), или отменит задачу.
func (e *Engine) prMissingAtMerge(ctx context.Context, task domain.Task, p domain.Project, cause error) error {
	if errors.Is(cause, ErrSCMCredentials) {
		return e.St.SetTaskWaitReason(ctx, task.ID, "учётные данные хостинга не работают: PR откроется после восстановления подключения")
	}
	slog.Error("create PR на шаге merge", "task", task.ID, "err", cause)
	msg := fmt.Sprintf("merge не выполнен: PR ветки %s не создан: %v. Подтвердите merge, когда хостинг ответит. Если в ветке нет изменений, задачу можно доработать в сессии из консоли или отменить", task.Branch, cause)
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "task.merge_failed",
		ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
		Text:    msg,
		Payload: map[string]any{"reason": "pr_not_created", "error": cause.Error()},
	}); err != nil {
		return err
	}
	return e.St.EscalateOnce(ctx, p.ID, task.ID, domain.AttPRNotCreated, "pr_not_created", msg)
}

// prResumeAttempts — сколько попыток создать PR делает задача, которая
// вернулась на review без PR, прежде чем эскалироваться.
const prResumeAttempts = 3

// prWaitState — попытки создать PR у задачи, вернувшейся на review без PR.
type prWaitState struct {
	next  time.Time
	fails int
}

// prBeforeResume — PR задачи, которая возвращается на review без PR: одна
// попытка без пауз, следующая не раньше prResumePause. После
// prResumeAttempts неудач задача эскалируется PR_NOT_CREATED, как отказ
// перед review: повтор создаст PR и продолжит с review. false — PR нет.
func (e *Engine) prBeforeResume(ctx context.Context, task domain.Task, step policy.ResolvedStep) (domain.Task, bool) {
	e.mu.Lock()
	if e.prWait == nil {
		e.prWait = map[string]prWaitState{}
	}
	st := e.prWait[task.ID]
	e.mu.Unlock()
	if time.Now().Before(st.next) {
		return task, false
	}
	withPR, err := e.ensurePRWith(ctx, task, nil)
	if err == nil {
		e.mu.Lock()
		delete(e.prWait, task.ID)
		e.mu.Unlock()
		return withPR, true
	}
	if errors.Is(err, ErrSCMCredentials) {
		if werr := e.St.SetTaskWaitReason(ctx, task.ID, "учётные данные хостинга не работают: PR откроется после восстановления подключения"); werr != nil {
			slog.Error("ожидание PR", "err", werr)
		}
		return task, false
	}
	slog.Error("create PR перед review", "task", task.ID, "err", err)
	st.fails++
	st.next = time.Now().Add(prResumePause)
	e.mu.Lock()
	if st.fails >= prResumeAttempts {
		delete(e.prWait, task.ID)
	} else {
		e.prWait[task.ID] = st
	}
	e.mu.Unlock()
	if st.fails < prResumeAttempts {
		if werr := e.St.SetTaskWaitReason(ctx, task.ID, "PR ветки не создан: "+err.Error()); werr != nil {
			slog.Error("create PR перед review: причина ожидания", "task", task.ID, "err", werr)
		}
		return task, false
	}
	// Задача в ready, из ready в failed перехода нет: она переходит через
	// review и эскалируется одной транзакцией. Сбой оставит её в ready на
	// шаге review, следующий проход попробует снова.
	via := store.EventInput{ActorKind: domain.ActorSystem, Type: "task.status",
		Text: "возврат на шаг " + step.Title + ": PR ветки не создан", Payload: map[string]any{"status": string(domain.TaskReview)}}
	if ferr := e.prFail(ctx, task, step, err, map[string]any{}, &via); ferr != nil {
		slog.Error("create PR перед review: эскалация", "task", task.ID, "err", ferr)
	}
	return task, false
}

// ErrPRNotCreated — у задачи нет PR, и создать его не удалось: merge или
// повтор не выполнены, задача осталась как была.
var ErrPRNotCreated = errors.New("PR задачи не создан")

// PrepareRetry — перед повтором задачи, которая упала без PR на входе в
// review, PR создаётся сразу: повтор продолжает с review, агент шага не
// перезапускается. ErrPRNotCreated — хостинг снова отказал, задача не
// меняется. Остальным задачам повтор ничего не готовит.
func (e *Engine) PrepareRetry(ctx context.Context, taskID string) error {
	task, err := e.St.GetTask(ctx, taskID)
	if err != nil || task.Status != domain.TaskFailed || task.PRURL != "" {
		return nil
	}
	proc := store.TaskProcess(task)
	if proc == nil {
		return nil
	}
	if step, ok := proc.Step(task.StepID); !ok || step.Kind != policy.StepReview {
		return nil
	}
	if _, err := e.ensurePR(ctx, task); err != nil {
		return fmt.Errorf("%w: %w", ErrPRNotCreated, err)
	}
	return nil
}

// ResumePendingPRs — после старта rivetd задачи, которые ждут на review или
// merge без PR (дошли туда до этого исправления или хостинг не ответил),
// получают PR сами. На merge дальше решает авто-merge по политике, отказ
// хостинга даёт событие о неудачном merge и эскалацию.
func (e *Engine) ResumePendingPRs(ctx context.Context) {
	tasks, err := e.St.TasksAwaitingPR(ctx)
	if err != nil {
		slog.Error("PR ждущих задач при старте", "err", err)
		return
	}
	for _, task := range tasks {
		projectID, _, err := e.St.TaskRefs(ctx, task.ID)
		if err != nil || e.credentialsInvalid(ctx, projectID) {
			continue
		}
		proc := store.TaskProcess(task)
		if proc == nil {
			continue
		}
		step, ok := proc.Step(task.StepID)
		if !ok {
			continue
		}
		switch step.Kind {
		case policy.StepMerge:
			p, _, err := e.projectOf(ctx, task)
			if err != nil {
				continue
			}
			eff, err := e.St.EffectivePolicy(ctx, p.ID)
			if err != nil {
				continue
			}
			if err := e.autoMerge(ctx, task, p, eff); err != nil {
				slog.Error("PR ждущей merge задачи при старте", "task", task.ID, "err", err)
			}
		case policy.StepReview:
			if _, err := e.ensurePR(ctx, task); err != nil {
				slog.Error("PR задачи на review при старте", "task", task.ID, "err", err)
			}
		}
	}
}

// reviewEntryText — текст события входа на review: «проверки прошли»
// только после шага test, иначе по шагу, с которого пришла задача, и по
// исходу (переход fail на review — после ошибки шага).
func reviewEntryText(proc *policy.Resolved, fromStep string, viaFail bool) string {
	from, ok := proc.Step(fromStep)
	switch {
	case ok && viaFail:
		return "шаг " + from.Title + " завершился с ошибкой, по процессу — review: PR создан, ожидание review"
	case ok && from.Kind == policy.StepTest:
		return "проверки прошли, PR создан — ожидание review"
	case ok && from.Kind == policy.StepCode:
		return "реализация готова, PR создан — ожидание review"
	case ok:
		return "шаг " + from.Title + " пройден, PR создан — ожидание review"
	}
	return "PR создан — ожидание review"
}

// mergeEntryText — текст входа в merge по шагу, с которого пришла задача,
// исходу (переход fail) и наличию PR; reviewed — задача пришла с review или
// это повторный вход (тогда шаг, с которого пришли, неизвестен).
func mergeEntryText(proc *policy.Resolved, fromStep string, hasPR, viaFail bool) (string, bool) {
	pr := "PR создан"
	if !hasPR {
		pr = "PR не создан"
	}
	from, ok := proc.Step(fromStep)
	switch {
	case ok && viaFail && from.Kind != policy.StepMerge:
		return "шаг " + from.Title + " завершился с ошибкой, по процессу — merge: " + pr + ", ожидание подтверждения merge", false
	case ok && from.Kind == policy.StepReview:
		if hasPR {
			return "review пройден — ожидание подтверждения merge", true
		}
		return "review пройден, PR не создан — ожидание подтверждения merge", true
	case ok && from.Kind == policy.StepMerge:
		if hasPR {
			return "ожидание подтверждения merge", true
		}
		return "PR не создан — ожидание подтверждения merge", true
	case ok && from.Kind == policy.StepCode:
		return "реализация готова, " + pr + " — ожидание подтверждения merge", false
	case ok:
		return "шаг " + from.Title + " пройден, " + pr + " — ожидание подтверждения merge", false
	}
	return pr + " — ожидание подтверждения merge", false
}

// skippedText — пояснение к переходу через отключённые шаги.
func skippedText(ids []string) string {
	if len(ids) == 1 {
		return "шаг " + ids[0] + " пропущен как отключённый"
	}
	return "шаги " + strings.Join(ids, ", ") + " пропущены как отключённые"
}
