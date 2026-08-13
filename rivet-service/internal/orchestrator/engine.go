// Package orchestrator — политика конвейера: детерминированные реакции на
// результаты этапов и цикл диспетчеризации. Модель решает задачу,
// оркестратор управляет процессом (спеки backend/orchestration, task-pipeline).
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Sender доставляет сообщение подключённому runner'у (реализация — stream.Registry).
type Sender interface {
	Send(runnerID string, msg *pb.PlaneMsg) bool
}

type Engine struct {
	St   *store.Store
	SCM  scm.Adapter
	Blob *blob.Store
	Out  Sender

	HeartbeatTimeout time.Duration
	BaseBranch       string

	mu sync.Mutex
	// контекст для следующего назначения стадии (вывод тестов, вердикт review)
	stageContext map[string]string
	// транскрипт текущей стадии задачи
	transcripts map[string][]byte
	// открытая сессия стадии задачи
	sessions map[string]string
}

func New(st *store.Store, adapter scm.Adapter, bl *blob.Store, send Sender, heartbeat time.Duration) *Engine {
	return &Engine{
		St: st, SCM: adapter, Blob: bl, Out: send,
		HeartbeatTimeout: heartbeat, BaseBranch: "main",
		stageContext: map[string]string{},
		transcripts:  map[string][]byte{},
		sessions:     map[string]string{},
	}
}

// Run — цикл диспетчеризации.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil {
				slog.Error("orchestrator tick", "err", err)
			}
		}
	}
}

// Tick — один проход планировщика: протухшие runner'ы, пересчёт DAG, назначения.
func (e *Engine) Tick(ctx context.Context) error {
	lost, err := e.St.MarkStaleRunnersOffline(ctx, int(e.HeartbeatTimeout.Seconds()))
	if err != nil {
		return fmt.Errorf("stale runners: %w", err)
	}
	// Сессии задач потерянных runner'ов закрыты в БД — кеш обязан забыть их,
	// иначе replay стадии пройдёт SessionMatches по памяти.
	for _, taskID := range lost {
		e.DropSession(taskID)
	}
	epics, err := e.St.RunningEpics(ctx)
	if err != nil {
		return err
	}
	for _, id := range epics {
		if err := e.St.RecomputeEpic(ctx, id); err != nil {
			return fmt.Errorf("recompute %s: %w", id, err)
		}
	}
	// Назначения: кодирование, исправления, review — до исчерпания кандидатов.
	for {
		a, ok, err := e.St.AssignNext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_CODING, "")
	}
	for {
		a, ok, err := e.St.AssignFixing(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_FIXING, e.takeStageContext(a.Task.ID))
	}
	for {
		a, ok, err := e.St.AssignTesting(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_TESTING, "")
	}
	for {
		a, ok, err := e.St.AssignReview(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		// Контекст ревьюера: diff PR + отчёт автопроверок.
		extra := e.takeStageContext(a.Task.ID)
		if a.Task.PRURL != "" {
			if d, err := e.diffForTask(ctx, a.Task); err != nil {
				slog.Error("diff for review", "task", a.Task.ID, "err", err)
			} else if extra != "" {
				extra = d + "\n\n" + extra
			} else {
				extra = d
			}
		}
		e.dispatch(ctx, a, pb.StageResult_REVIEW, extra)
	}
	return nil
}

func (e *Engine) takeStageContext(taskID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.stageContext[taskID]
	delete(e.stageContext, taskID)
	return c
}

func (e *Engine) projectOf(ctx context.Context, task domain.Task) (domain.Project, domain.Epic, error) {
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return domain.Project{}, epic, err
	}
	p, err := e.St.GetProject(ctx, epic.ProjectID)
	return p, epic, err
}

// dispatch отправляет Assignment runner'у и открывает сессию стадии.
func (e *Engine) dispatch(ctx context.Context, a store.Assignment, stage pb.StageResult_Stage, extra string) {
	p, _, err := e.projectOf(ctx, a.Task)
	if err != nil {
		slog.Error("dispatch: project", "task", a.Task.ID, "err", err)
		return
	}
	criteria := make([]string, 0, len(a.Task.Criteria))
	for _, c := range a.Task.Criteria {
		criteria = append(criteria, c.Text)
	}
	checks := make([]*pb.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, &pb.Check{Name: c.Name, Cmd: c.Cmd})
	}
	runnerID := a.Runner.ID
	// Сессия создаётся до отправки Assignment: runner повторяет session_id
	// во всех сообщениях стадии, сообщения без него отбрасываются (design,
	// решение 4). Без сессии стадию не запускаем — задачу вернёт
	// heartbeat-таймаут, как при недоступном runner'е.
	sessionID, err := e.St.CreateSession(ctx, domain.Session{
		TaskID: a.Task.ID, Attempt: a.Task.AttemptUsed + 1,
		DriverKind: "scheduler", Agent: a.Runner.Agent, Model: a.Runner.Model,
		Depth: domain.DepthMinimal, Scope: stage.String(),
	})
	if err != nil {
		slog.Error("dispatch: session", "task", a.Task.ID, "err", err)
		return
	}
	e.mu.Lock()
	e.sessions[a.Task.ID] = sessionID
	delete(e.transcripts, a.Task.ID)
	e.mu.Unlock()
	msg := &pb.PlaneMsg{
		MsgId: fmt.Sprintf("assign-%s-%s-%d", a.Task.ID, stage, time.Now().UnixNano()),
		Kind: &pb.PlaneMsg_Assign{Assign: &pb.Assignment{
			TaskId: a.Task.ID, TaskNum: a.Task.Num, Stage: stage,
			Title: a.Task.Title, Description: a.Task.Description,
			Criteria: criteria, Repo: p.Repo, Branch: a.Task.Branch,
			Checks: checks, ExtraContext: extra, SessionId: sessionID,
		}},
	}
	if !e.Out.Send(runnerID, msg) {
		slog.Warn("dispatch: runner недоступен, задачу вернёт heartbeat-таймаут",
			"runner", runnerID, "task", a.Task.ID)
	}
}

// transcriptCap — жёсткий предел буфера транскрипта стадии: после append
// буфер не превышает кап, лишнее отбрасывается с маркером обрезки
// (api-contract: транскрипт отдаётся целиком одним ответом).
const transcriptCap = 4 << 20

var truncMarker = []byte("\n…[транскрипт обрезан: превышен лимит 4 МБ]\n")

// SessionMatches — принадлежит ли сообщение текущей сессии задачи.
// После рестарта rivetd карта сессий пуста: открытая сессия задачи
// поднимается из БД, чтобы не терять результаты стадий, назначенных
// до рестарта (доставка at-least-once переживает рестарт plane).
func (e *Engine) SessionMatches(ctx context.Context, taskID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	e.mu.Lock()
	cur, known := e.sessions[taskID]
	e.mu.Unlock()
	if known {
		return cur == sessionID
	}
	open, err := e.St.OpenSession(ctx, taskID)
	if err != nil || open == "" || open != sessionID {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, known := e.sessions[taskID]; known {
		return cur == sessionID
	}
	e.sessions[taskID] = sessionID
	return true
}

// DropSession инвалидирует память о сессии задачи. Вызывается там, где
// сессия закрывается в БД мимо StageResult/Blocked (отмена, потеря
// runner'а): иначе SessionMatches продолжил бы принимать поздние сообщения
// закрытой сессии из кеша (например, CreatePR после отмены).
func (e *Engine) DropSession(taskID string) {
	e.mu.Lock()
	delete(e.sessions, taskID)
	delete(e.transcripts, taskID)
	e.mu.Unlock()
}

// OnTranscript накапливает чанки транскрипта текущей стадии. Чанки чужой
// сессии отбрасываются; принадлежность проверяет вызывающий через
// SessionMatches (он же поднимает сессию из БД после рестарта), здесь —
// только сверка с картой под общим замком.
func (e *Engine) OnTranscript(taskID, sessionID string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sessionID == "" || e.sessions[taskID] != sessionID {
		return
	}
	buf := e.transcripts[taskID]
	if len(buf) >= transcriptCap {
		return
	}
	if len(buf)+len(data) > transcriptCap {
		// Маркер обрезки входит в кап: итоговый буфер ровно transcriptCap.
		keep := transcriptCap - len(truncMarker)
		if len(buf) > keep {
			buf = buf[:keep]
		} else {
			buf = append(buf, data[:keep-len(buf)]...)
		}
		e.transcripts[taskID] = append(buf, truncMarker...)
		return
	}
	e.transcripts[taskID] = append(buf, data...)
}

// flushTranscript закрывает сессию стадии и уводит транскрипт в blob.
// Сохраняемый буфер маскируется целиком: страховка от секретов,
// разрезанных между чанками (спека team-visibility «Секрет в транскрипте»).
//
// Возвращает, удалось ли атомарно закрыть сессию: false — её уже закрыл
// другой путь (отмена, потеря runner'а) в окне до инвалидации кеша, и
// вызывающий обязан отбросить сообщение стадии без реакций конвейера.
// Объект, записанный в blob до неудавшегося захвата, остаётся без ссылки —
// это безвредный мусор.
func (e *Engine) flushTranscript(ctx context.Context, task domain.Task, stage, sessionID string) bool {
	e.mu.Lock()
	if sessionID == "" || e.sessions[task.ID] != sessionID {
		e.mu.Unlock()
		return false
	}
	buf := e.transcripts[task.ID]
	delete(e.transcripts, task.ID)
	delete(e.sessions, task.ID)
	e.mu.Unlock()
	ref := ""
	if len(buf) > 0 && e.Blob != nil {
		key := fmt.Sprintf("tasks/%d/attempt-%d-%s.log", task.Num, task.AttemptUsed+1, stage)
		var err error
		if ref, err = e.Blob.Put(ctx, key, redact.Bytes(buf)); err != nil {
			slog.Error("transcript flush", "task", task.ID, "err", err)
			ref = ""
		}
	}
	claimed, err := e.St.EndSession(ctx, sessionID, ref)
	if err != nil {
		slog.Error("end session", "session", sessionID, "err", err)
		return false
	}
	return claimed
}

// OnStageResult — детерминированные реакции конвейера (спека task-pipeline).
// Результат чужой сессии (replay после reconnect) отбрасывается; Detail —
// runner-controlled текст, идущий в event log и контекст следующей стадии,
// поэтому маскируется на входе.
func (e *Engine) OnStageResult(ctx context.Context, runnerID string, sr *pb.StageResult) error {
	if !e.SessionMatches(ctx, sr.TaskId, sr.SessionId) {
		slog.Warn("stage result чужой сессии отброшен",
			"task", sr.TaskId, "session", sr.SessionId, "stage", sr.Stage)
		return nil
	}
	sr.Detail = redact.String(sr.Detail)
	task, err := e.St.GetTask(ctx, sr.TaskId)
	if err != nil {
		return err
	}
	if !e.flushTranscript(ctx, task, sr.Stage.String(), sr.SessionId) {
		// Сессию уже закрыл другой путь (отмена, потеря runner'а):
		// результат стадии опоздал, реакции конвейера не выполняются.
		slog.Warn("stage result закрытой сессии отброшен", "task", sr.TaskId, "session", sr.SessionId)
		return nil
	}
	ev := store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.status"}

	switch sr.Stage {
	case pb.StageResult_CODING, pb.StageResult_FIXING:
		if !sr.Ok {
			return e.failTask(ctx, task, "Невосстановимая ошибка этапа: "+sr.Detail, runnerID)
		}
		ev.Text = "реализация готова — запуск проверок"
		ev.Payload = map[string]any{"status": "testing"}
		if err := e.St.TransitionTask(ctx, task.ID, domain.TaskTesting, ev, nil); err != nil {
			return err
		}
		if e.epicPaused(ctx, task) {
			// Пауза Epic: безопасная точка — граница стадии. Проверки не
			// запускаем, runner освобождаем; resume подхватит AssignTesting.
			return e.St.ReleaseTaskRunner(ctx, task.ID)
		}
		a := store.Assignment{Task: task, Runner: domain.Runner{ID: runnerID}}
		e.dispatch(ctx, a, pb.StageResult_TESTING, "")
		return nil

	case pb.StageResult_TESTING:
		if !sr.Ok {
			// Провал проверок расходует попытку (спека orchestration).
			// Вне паузы исправление идёт тем же runner'ом в том же worktree;
			// на паузе runner освобождается, fixing подхватит AssignFixing.
			// Вывод проверок кладётся до перехода в fixing: сразу после
			// коммита resume может назначить FIXING, контекст уже должен лежать.
			e.mu.Lock()
			e.stageContext[task.ID] = "Вывод проверок:\n" + sr.Detail
			e.mu.Unlock()
			paused := e.epicPaused(ctx, task)
			failed, err := e.St.ConsumeAttempt(ctx, task.ID, domain.AttTestFailed, sr.Detail, !paused)
			if err != nil || failed {
				e.takeStageContext(task.ID)
				return err
			}
			if paused {
				return nil
			}
			a := store.Assignment{Task: task, Runner: domain.Runner{ID: runnerID}}
			e.dispatch(ctx, a, pb.StageResult_FIXING, e.takeStageContext(task.ID))
			return nil
		}
		// Отчёт автопроверок сохраняется для ревьюера (спека task-pipeline:
		// ревьюер получает diff, критерии и результаты проверок).
		if sr.Detail != "" {
			e.mu.Lock()
			e.stageContext[task.ID] = "Результаты автопроверок:\n" + sr.Detail
			e.mu.Unlock()
		}
		// Тесты прошли: PR (если ещё нет) → review, исполнитель освобождается.
		if task.PRURL == "" {
			p, _, err := e.projectOf(ctx, task)
			if err != nil {
				return err
			}
			pr, err := e.SCM.CreatePR(ctx, p.Repo, task.Branch, e.BaseBranch,
				fmt.Sprintf("task-%d: %s", task.Num, task.Title), task.Description)
			if err != nil {
				return fmt.Errorf("create PR: %w", err)
			}
			if err := e.St.SetTaskPR(ctx, task.ID, pr.URL); err != nil {
				return err
			}
			if sr.PrUrl == "" {
				sr.PrUrl = pr.URL
			}
		}
		ev.Text = "проверки прошли, PR создан — ожидание review"
		ev.Payload = map[string]any{"status": "review", "pr": sr.PrUrl}
		return e.St.TransitionWithRunnerRelease(ctx, task.ID, domain.TaskReview, ev)

	case pb.StageResult_REVIEW:
		if sr.Ok {
			// Освобождаем runner-ревьюера, но reviewer_id на задаче сохраняем:
			// он же признак «review выполнен» — иначе планировщик назначит review заново.
			if err := e.St.FreeReviewerRunner(ctx, task.ID); err != nil {
				return err
			}
			_, err := e.St.AppendEvent(ctx, store.EventInput{
				ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.review_passed",
				ProjectID: mustProject(ctx, e, task), EpicID: task.EpicID, TaskID: task.ID,
				Text: "review пройден — ожидание подтверждения merge",
			})
			return err
		}
		// Ревьюера освобождает ConsumeAttempt в той же транзакции, что и
		// переход в fixing: без окна, где Tick назначил бы review повторно.
		e.mu.Lock()
		e.stageContext[task.ID] = "Замечания review:\n" + sr.Detail
		e.mu.Unlock()
		_, err := e.St.ConsumeAttempt(ctx, task.ID, domain.AttReviewLimit, sr.Detail, false)
		return err

	default:
		return fmt.Errorf("неизвестная стадия %v", sr.Stage)
	}
}

// epicPaused — Epic задачи не выполняется (пауза и т.п.): конвейер задачи
// останавливается на границе стадии.
func (e *Engine) epicPaused(ctx context.Context, task domain.Task) bool {
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		slog.Error("epicPaused: epic", "task", task.ID, "err", err)
		return false
	}
	return epic.Status != domain.EpicRunning
}

func mustProject(ctx context.Context, e *Engine, task domain.Task) string {
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return ""
	}
	return epic.ProjectID
}

// failTask — невосстановимая ошибка: failed + эскалация TEST_FAILED.
func (e *Engine) failTask(ctx context.Context, task domain.Task, msg, runnerID string) error {
	return e.St.TransitionTask(ctx, task.ID, domain.TaskFailed,
		store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.status",
			Text: msg, Payload: map[string]any{"status": "failed"}},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`UPDATE runners SET status='idle', task_id=NULL WHERE task_id=$1`, task.ID); err != nil {
				return err
			}
			epic, err := e.St.GetEpic(ctx, task.EpicID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO attention (project_id, task_id, reason, message)
				VALUES ($1,$2,'TEST_FAILED',$3)`, epic.ProjectID, task.ID, msg)
			return err
		})
}

// OnBlocked — вопрос агента: blocked + эскалация. Вопрос — runner-controlled
// текст (event log, эскалация), маскируется; чужая сессия отбрасывается.
func (e *Engine) OnBlocked(ctx context.Context, runnerID string, b *pb.BlockedQuestion) error {
	if !e.SessionMatches(ctx, b.TaskId, b.SessionId) {
		slog.Warn("blocked чужой сессии отброшен", "task", b.TaskId, "session", b.SessionId)
		return nil
	}
	b.Question = redact.String(b.Question)
	task, err := e.St.GetTask(ctx, b.TaskId)
	if err != nil {
		return err
	}
	if !e.flushTranscript(ctx, task, "blocked", b.SessionId) {
		slog.Warn("blocked закрытой сессии отброшен", "task", b.TaskId, "session", b.SessionId)
		return nil
	}
	return e.St.BlockTask(ctx, b.TaskId, b.Question,
		store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID})
}

// MergeTask — подтверждение человека: merge PR → done → пересчёт DAG.
func (e *Engine) MergeTask(ctx context.Context, taskID, userID string) error {
	task, err := e.St.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status != domain.TaskReview {
		return domain.ErrBadTransition{Entity: "task", From: string(task.Status), To: string(domain.TaskDone)}
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	if task.PRURL != "" {
		num, err := prNumber(task.PRURL)
		if err != nil {
			return err
		}
		if err := e.SCM.Merge(ctx, p.Repo, num); err != nil {
			return fmt.Errorf("merge PR: %w", err)
		}
	}
	if err := e.St.TransitionWithRunnerRelease(ctx, taskID, domain.TaskDone,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: userID, Type: "task.status",
			Text: "PR смержен — задача выполнена", Payload: map[string]any{"status": "done"}}); err != nil {
		return err
	}
	return e.St.RecomputeEpic(ctx, task.EpicID)
}

func (e *Engine) diffForTask(ctx context.Context, task domain.Task) (string, error) {
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return "", err
	}
	num, err := prNumber(task.PRURL)
	if err != nil {
		return "", err
	}
	return e.SCM.Diff(ctx, p.Repo, num)
}

// prNumber извлекает номер PR из https://github.com/owner/repo/pull/N.
// Разбор строгий: хвост после последнего слэша — ровно каноническая запись
// положительного числа (без ведущих нулей, знака и мусора).
func prNumber(url string) (int, error) {
	seg := url[lastSlash(url)+1:]
	n, err := strconv.Atoi(seg)
	if err != nil || n <= 0 || strconv.Itoa(n) != seg {
		return 0, fmt.Errorf("не разобрать номер PR из %q", url)
	}
	return n, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
