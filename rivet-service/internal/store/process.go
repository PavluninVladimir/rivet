package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Процесс задачи как данные (change add-process-model, спека backend/process):
// задача стоит на шаге снимка процесса, участники шага исполняются
// запусками (task_step_runs), планировщик назначает запуски runner'ам по
// типу агента, модели и capabilities.

// StepRun — запуск участника шага. Без runner'а и вердикта — ожидает
// назначения; с runner'ом без вердикта — идёт; вердикт закрывает запуск.
type StepRun struct {
	ID           int64
	TaskID       string
	StepID       string
	StepKind     string
	Pass         int
	Participant  string
	AgentKind    string
	Model        string
	Capabilities []string
	// UserLogin и UserRole — участник-человек (add-process-humans); у
	// агента пусты. VerdictBy — логин человека, давшего вердикт.
	UserLogin  string
	UserRole   string
	RunnerID   string
	SessionID  string
	Verdict    string
	VerdictBy  string
	Detail     string
	CreatedAt  time.Time
	FinishedAt *time.Time
	// Role — роль участника (add-agent-roles); PrefModel — предпочтительная
	// модель роли при выборе runner'а (не требование); AgentID и AgentName —
	// агент команды, который ведёт запуск.
	Role      string
	PrefModel string
	AgentID   string
	AgentName string
	// RoleAgent — профиль шаблона (обязателен без своего профиля агента),
	// PrefAgent — профиль из переопределения проекта (предпочтение),
	// AgentVerdict — вердикт, который вернул сам агент (тестировщик).
	RoleAgent    string
	PrefAgent    string
	AgentVerdict string
	// AssignedModel — модель, с которой стадия ушла runner'у; Model — только
	// явная модель участника (требование к runner'у).
	AssignedModel string
}

// DisplayModel — модель запуска для людей: назначенная, иначе явная.
func (r StepRun) DisplayModel() string {
	if r.AssignedModel != "" {
		return r.AssignedModel
	}
	return r.Model
}

// IsUser — запуск участника-человека.
func (r StepRun) IsUser() bool { return r.UserLogin != "" || r.UserRole != "" }

// Open — запуск ещё не завершён (ожидает или идёт).
func (r StepRun) Open() bool { return r.Verdict == "" }

const runCols = `r.id, r.task_id::text, r.step_id, r.step_kind, r.pass, r.participant, r.agent_kind, r.model,
	r.capabilities, r.user_login, r.user_role, COALESCE(r.runner_id,''), COALESCE(r.session_id::text,''), COALESCE(r.verdict,''),
	r.verdict_by, r.detail, r.created_at, r.finished_at, r.role, r.pref_model, COALESCE(r.agent_id, ''),
	COALESCE((SELECT ta.name FROM team_agents ta WHERE ta.id = r.agent_id), ''), r.role_agent, r.pref_agent, r.agent_verdict, r.assigned_model`

func scanRun(row pgx.Row) (StepRun, error) {
	var r StepRun
	err := row.Scan(&r.ID, &r.TaskID, &r.StepID, &r.StepKind, &r.Pass, &r.Participant, &r.AgentKind, &r.Model,
		&r.Capabilities, &r.UserLogin, &r.UserRole, &r.RunnerID, &r.SessionID, &r.Verdict, &r.VerdictBy,
		&r.Detail, &r.CreatedAt, &r.FinishedAt, &r.Role, &r.PrefModel, &r.AgentID, &r.AgentName,
		&r.RoleAgent, &r.PrefAgent, &r.AgentVerdict, &r.AssignedModel)
	return r, err
}

// agentRunsOnly — условие запросов планировщика: запуски людей runner'ов не
// получают.
const agentRunsOnly = `r.user_login = '' AND r.user_role = ''`

// StepStatus — статус задачи как проекция типа шага и входа на него
// (спека process «Версия процесса на задаче»): code с начала — running
// (coding), code по changes — fixing, test — testing, review и merge — review.
func StepStatus(kind, entry string) domain.TaskStatus {
	switch kind {
	case policy.StepCode:
		if entry == policy.OutcomeChanges {
			return domain.TaskFixing
		}
		return domain.TaskRunning
	case policy.StepTest:
		return domain.TaskTesting
	default:
		return domain.TaskReview
	}
}

// runnerStatus — статус runner'а на шаге (как у прежних очередей назначений).
func runnerStatus(kind string) string {
	switch kind {
	case policy.StepTest:
		return "testing"
	case policy.StepReview:
		return "review"
	}
	return "running"
}

// EnterStep — вход задачи на шаг: текущий шаг и вход, снимок процесса (если
// его ещё нет), запуски участников (все при parallel, первый при
// sequential), статус-проекция с событием. ReuseRunner — runner, уже
// занятый задачей, которому единственный участник назначается сразу (тот же
// worktree: code → test, провал проверок → исправление). ReleaseRunners —
// освободить runner'ы задачи (вход на review). Задача в ready статус не
// меняет: его выставит назначение первого запуска.
type EnterStep struct {
	TaskID         string
	Step           policy.ResolvedStep
	Entry          string
	Process        *policy.Resolved
	ProcessHash    string
	ReuseRunner    string
	ReleaseRunners bool
	// Roles — роли участников шага по идентификатору участника, разрешённые
	// оркестратором по каталогу и политике проекта (add-agent-roles).
	Roles   map[string]RoleBinding
	Text    string
	Payload map[string]any
	Actor   EventInput
	// Silent — без события task.status (вызывающий пишет своё).
	Silent bool
}

// EnterStep выполняет вход на шаг одной транзакцией и возвращает созданные
// запуски (первый — с runner'ом, если он переиспользован).
func (s *Store) EnterStep(ctx context.Context, in EnterStep) ([]StepRun, error) {
	var runs []StepRun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicID, projectID string
		var hasProcess bool
		var gen int
		if err := tx.QueryRow(ctx, `
			SELECT t.status, t.epic_id, e.project_id, t.process IS NOT NULL, t.step_gen
			FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, in.TaskID).
			Scan(&from, &epicID, &projectID, &hasProcess, &gen); err != nil {
			return nf(err)
		}
		// Поколение входа: запуски прежних входов на тот же шаг (blocked,
		// cancelled) в оценку нового входа не попадают.
		gen++
		// Переиспользуемый runner должен быть всё ещё за этой задачей: его
		// могли освободить и отдать другой (решение о переиспользовании
		// принималось раньше).
		reuse := in.ReuseRunner
		if reuse != "" {
			var cur *string
			if err := tx.QueryRow(ctx, `SELECT task_id::text FROM runners WHERE id=$1 FOR UPDATE`, reuse).Scan(&cur); err != nil || (cur != nil && *cur != in.TaskID) {
				reuse = ""
			}
		}
		if !hasProcess && in.Process != nil {
			raw, err := json.Marshal(in.Process)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET process=$2, process_hash=$3 WHERE id=$1`,
				in.TaskID, raw, in.ProcessHash); err != nil {
				return err
			}
		}
		// Остатки прежнего шага (не должно быть, но пусть будет безопасно).
		if err := cancelOpenRuns(ctx, tx, in.TaskID); err != nil {
			return err
		}
		if in.ReleaseRunners {
			if err := freeRunner(ctx, tx, in.TaskID); err != nil {
				return err
			}
		}
		to := from
		// Из ready статус выставит назначение первого запуска агента; шаги
		// без агентов (merge, deploy, только люди) runner'а не ждут —
		// проекция сразу.
		if from != domain.TaskReady || reuse != "" || !hasAgentParticipants(in.Step) {
			to = StepStatus(in.Step.Kind, in.Entry)
		}
		if to != from && !from.CanTransition(to) {
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(to)}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET step_id=$2, step_entry=$3, status=$4, step_gen=$5, wait_reason='', wait_kind='', updated_at=now() WHERE id=$1`,
			in.TaskID, in.Step.ID, in.Entry, to, gen); err != nil {
			return err
		}
		// Ветка задачи появляется на первом шаге code: человеку-исполнителю
		// она нужна сразу, агентам следующих шагов — в Assignment.
		if in.Step.Kind == policy.StepCode {
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET branch = 'agent/task-' || num WHERE id=$1 AND COALESCE(branch,'') = ''`, in.TaskID); err != nil {
				return err
			}
		}
		// Новый вход на review — новые ревьюеры: прежний reviewer_id не
		// должен пережить шаг.
		if in.Step.Kind == policy.StepReview {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET reviewer_id=NULL WHERE id=$1`, in.TaskID); err != nil {
				return err
			}
		}
		pass := gen
		parts := in.Step.Participants
		if in.Step.Mode == policy.ModeSequential && len(parts) > 1 {
			parts = parts[:1]
		}
		for i, p := range parts {
			runner := ""
			if i == 0 && reuse != "" {
				runner = reuse
			}
			r, err := insertRun(ctx, tx, in.TaskID, in.Step, pass, p, runner, in.Roles[p.ID])
			if err != nil {
				return err
			}
			runs = append(runs, r)
		}
		if reuse != "" {
			if err := bindRunner(ctx, tx, in.TaskID, in.Step.Kind, reuse); err != nil {
				return err
			}
		}
		if in.Silent || (in.Text == "" && to == from && reuse == "") {
			return nil
		}
		ev := in.Actor
		if ev.ActorKind == "" {
			ev.ActorKind = domain.ActorScheduler
		}
		ev.Type, ev.ProjectID, ev.EpicID, ev.TaskID = "task.status", projectID, epicID, in.TaskID
		ev.Text = in.Text
		if ev.Text == "" {
			ev.Text = "шаг " + in.Step.Title
		}
		payload := map[string]any{"status": string(to), "step": in.Step.ID}
		for k, v := range in.Payload {
			payload[k] = v
		}
		// Тот же runner продолжает следующий шаг: исполнитель с ролью виден
		// и здесь, как в событии назначения (add-agent-roles).
		if reuse != "" && len(runs) > 0 && runs[0].Role != "" {
			var title, rname string
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE((SELECT title FROM agent_roles WHERE id=$1), ''),
				       COALESCE((SELECT COALESCE(NULLIF(display_name, ''), id) FROM runners WHERE id=$2), '')`,
				runs[0].Role, reuse).Scan(&title, &rname); err != nil {
				return err
			}
			if title != "" {
				payload["role"] = map[string]any{"id": runs[0].Role, "title": title}
			}
			payload["runner"], payload["runner_name"] = reuse, rname
		}
		ev.Payload = payload
		_, err := appendEvent(ctx, tx, ev)
		return err
	})
	return runs, err
}

// hasAgentParticipants — есть ли на шаге участники-агенты (им нужен runner).
func hasAgentParticipants(step policy.ResolvedStep) bool {
	for _, p := range step.Participants {
		if !p.IsUser() {
			return true
		}
	}
	return false
}

func insertRun(ctx context.Context, tx pgx.Tx, taskID string, step policy.ResolvedStep, pass int, p policy.ResolvedParticipant, runner string, rb RoleBinding) (StepRun, error) {
	caps := append([]string{}, step.Capabilities...)
	// Роль участника (add-agent-roles): capability шаблона добавляется к
	// capability шага. Профиль шаблона хранится отдельно от явного типа
	// участника: у агента команды со своим профилем он не действует.
	if !p.IsUser() && rb.Role != "" {
		if rb.Capability != "" && !containsStr(caps, rb.Capability) {
			caps = append(caps, rb.Capability)
		}
	} else {
		rb = RoleBinding{}
	}
	var runnerArg *string
	if runner != "" {
		runnerArg = &runner
	}
	return scanRun(tx.QueryRow(ctx, `
		INSERT INTO task_step_runs AS r (task_id, step_id, step_kind, pass, participant, agent_kind, model, capabilities, runner_id, user_login, user_role,
			role, pref_model, role_agent, pref_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING `+runCols, taskID, step.ID, step.Kind, pass, p.ID, p.Agent.Kind, p.Agent.Model, caps, runnerArg, p.User.Login, p.User.Role,
		rb.Role, rb.PrefModel, rb.Agent, rb.PrefAgent))
}

// ErrRunnerBusy — runner уже ведёт стадию другой задачи.
var ErrRunnerBusy = errors.New("runner занят другой задачей")

// bindRunner привязывает runner к задаче на шаге: статус runner'а по типу
// шага, исполнитель (code/test) или ревьюер (review) на задаче. Шаг test
// исполнителя задачи не перезаписывает, если он уже есть: тестировщик
// может работать на другом runner'е (add-agent-roles), а ревьюер не должен
// совпадать именно с исполнителем шага code.
func bindRunner(ctx context.Context, tx pgx.Tx, taskID, kind, runnerID string) error {
	// Runner ведёт одну стадию: занятый другой задачей не привязывается
	// (защита от двойного назначения при переиспользовании).
	tag, err := tx.Exec(ctx,
		`UPDATE runners SET status=$2, task_id=$3, ctx_pct=NULL WHERE id=$1 AND (task_id IS NULL OR task_id=$3)`,
		runnerID, runnerStatus(kind), taskID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRunnerBusy
	}
	col := "runner_id"
	if kind == policy.StepReview {
		col = "reviewer_id"
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET `+col+`=$2, updated_at=now() WHERE id=$1 AND (`+col+` IS NULL OR $3)`,
		taskID, runnerID, kind != policy.StepReview && kind != policy.StepTest)
	return err
}

// cancelOpenRuns закрывает незавершённые запуски задачи (отмена, потеря
// runner'а, решение человека, any/blocked) и освобождает их runner'ы,
// если те ещё привязаны к задаче.
func cancelOpenRuns(ctx context.Context, tx pgx.Tx, taskID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE runners SET status='idle', task_id=NULL
		WHERE task_id=$1 AND id IN (SELECT runner_id FROM task_step_runs WHERE task_id=$1 AND verdict IS NULL AND runner_id IS NOT NULL)`, taskID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE task_step_runs SET verdict='cancelled', finished_at=now()
		WHERE task_id=$1 AND verdict IS NULL`, taskID)
	return err
}

// CancelOpenRuns — то же вне транзакции; возвращает runner'ы идущих
// запусков, которым нужно послать отмену, и освобождает их.
func (s *Store) CancelOpenRuns(ctx context.Context, taskID string, except int64) ([]StepRun, error) {
	var open []StepRun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM task_step_runs r
			WHERE r.task_id=$1 AND r.verdict IS NULL AND r.id <> $2 FOR UPDATE`, taskID, except)
		if err != nil {
			return err
		}
		open, err = collectRuns(rows)
		if err != nil {
			return err
		}
		for _, r := range open {
			if _, err := tx.Exec(ctx, `
				UPDATE task_step_runs SET verdict='cancelled', finished_at=now() WHERE id=$1`, r.ID); err != nil {
				return err
			}
			if r.RunnerID != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE runners SET status='idle', task_id=NULL WHERE id=$1 AND task_id=$2`, r.RunnerID, taskID); err != nil {
					return err
				}
			}
			if r.SessionID != "" {
				if _, err := tx.Exec(ctx, `
					UPDATE sessions SET ended_at=now(), outcome='прервана: шаг завершён другими участниками'
					WHERE id=$1 AND ended_at IS NULL`, r.SessionID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return open, err
}

func collectRuns(rows pgx.Rows) ([]StepRun, error) {
	defer rows.Close()
	var out []StepRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StepRuns — запуски задачи на шаге и проходе, в порядке участников.
func (s *Store) StepRuns(ctx context.Context, taskID, stepID string, pass int) ([]StepRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM task_step_runs r
		WHERE r.task_id=$1 AND r.step_id=$2 AND r.pass=$3 ORDER BY r.id`, taskID, stepID, pass)
	if err != nil {
		return nil, err
	}
	return collectRuns(rows)
}

// RunBySession — запуск по сессии стадии (StageResult, Blocked).
func (s *Store) RunBySession(ctx context.Context, sessionID string) (StepRun, error) {
	r, err := scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_step_runs r WHERE r.session_id=$1::uuid`, sessionID))
	return r, nf(err)
}

// SetRunSession привязывает сессию к запуску после её создания. false —
// запуск уже закрыт (отменён другим участником шага между назначением и
// отправкой Assignment): сессию открывать и стадию запускать нельзя.
func (s *Store) SetRunSession(ctx context.Context, runID int64, sessionID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE task_step_runs SET session_id=$2::uuid WHERE id=$1 AND verdict IS NULL`, runID, sessionID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UnbindRun возвращает запуск в очередь ожидания: без runner'а и сессии
// (Assignment не доставлен — планировщик назначит заново).
func (s *Store) UnbindRun(ctx context.Context, runID int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE task_step_runs SET runner_id=NULL, session_id=NULL, agent_id=NULL, assigned_model='' WHERE id=$1 AND verdict IS NULL`, runID)
	return err
}

// AddSequentialRun создаёт запуск следующего участника (sequential) на
// том же проходе; runner назначит планировщик.
func (s *Store) AddSequentialRun(ctx context.Context, taskID string, step policy.ResolvedStep, pass int, p policy.ResolvedParticipant, rb RoleBinding) (StepRun, error) {
	var r StepRun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		r, err = insertRun(ctx, tx, taskID, step, pass, p, "", rb)
		return err
	})
	return r, err
}

// RecordVerdict закрывает запуск вердиктом и освобождает его runner
// (кроме keepRunner: тот же runner продолжит следующий шаг). Возвращает
// false, если запуск уже закрыт (поздний результат).
func (s *Store) RecordVerdict(ctx context.Context, runID int64, verdict, detail string, keepRunner bool) (bool, error) {
	return s.recordVerdict(ctx, runID, verdict, detail, "", keepRunner)
}

// RecordUserVerdict — вердикт человека с его логином; повторный вердикт
// (второй владелец при участнике по роли) не проходит: false.
func (s *Store) RecordUserVerdict(ctx context.Context, runID int64, login, verdict, detail string) (bool, error) {
	return s.recordVerdict(ctx, runID, verdict, detail, login, false)
}

func (s *Store) recordVerdict(ctx context.Context, runID int64, verdict, detail, by string, keepRunner bool) (bool, error) {
	var claimed bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var runnerID, taskID string
		// Адресат проверяется в том же UPDATE: между проверкой снаружи и
		// вердиктом роль или членство могли измениться.
		err := tx.QueryRow(ctx, `
			UPDATE task_step_runs r SET verdict=$2, detail=$3, verdict_by=$4, finished_at=now()
			WHERE r.id=$1 AND r.verdict IS NULL
			  AND ($4 = '' OR r.user_login = $4 OR (r.user_role <> '' AND EXISTS (
					SELECT 1 FROM tasks t JOIN epics e ON e.id = t.epic_id
					JOIN project_members m ON m.project_id = e.project_id
					JOIN users u ON u.id = m.user_id
					WHERE t.id = r.task_id AND u.login = $4 AND m.role = r.user_role)))
			RETURNING COALESCE(r.runner_id,''), r.task_id::text`, runID, verdict, detail, by).
			Scan(&runnerID, &taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		claimed = true
		if runnerID != "" && !keepRunner {
			_, err = tx.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL WHERE id=$1 AND task_id=$2`, runnerID, taskID)
		}
		return err
	})
	return claimed, err
}

// ReadyToEnter — задачи в ready работающих Epic'ов без открытых запусков:
// движок вводит их на первый шаг (или возобновляет прежний).
func (s *Store) ReadyToEnter(ctx context.Context, excludedProjects, excludedEpics []string) ([]domain.Task, error) {
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	if excludedEpics == nil {
		excludedEpics = []string{}
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+taskCols+` FROM `+taskFrom+`
		JOIN epics e ON e.id=t.epic_id AND e.status='running'
			AND e.project_id <> ALL($1::uuid[]) AND e.id <> ALL($2::uuid[])
		WHERE t.status='ready'
		  AND NOT EXISTS (SELECT 1 FROM task_step_runs r WHERE r.task_id=t.id AND r.verdict IS NULL)
		ORDER BY t.num`, excludedProjects, excludedEpics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RunAssignment — назначенный запуск с задачей и runner'ом.
type RunAssignment struct {
	Run    StepRun
	Task   domain.Task
	Runner domain.Runner
	// Pending — событие назначения и переход статуса задачи: событие
	// пишется после доставки стадии (RunDelivered), при недоставке переход
	// откатывается (UndoAssign).
	Pending *PendingAssign
}

// PendingAssign — итог назначения, который подтверждает доставка. Статус
// задачи меняется ещё в транзакции назначения: результат стадии может
// прийти сразу после отправки, раньше подтверждения. Недоставка возвращает
// задачу в ready. Событие «назначена» или «назначен исполнитель» пишется
// только после доставки, иначе устойчивая ошибка старта писала бы его на
// каждый повтор.
type PendingAssign struct {
	Event EventInput
	// To — статус, в который назначение перевело задачу из ready (пусто —
	// задача уже была в работе).
	To domain.TaskStatus
	// FirstStart — назначение первым поставило задаче started_at: при
	// недоставке время начала снимается.
	FirstStart bool
}

// AssignRun атомарно назначает один ожидающий запуск подходящему свободному
// runner'у: capabilities шага (для code/test — плюс capabilities задачи),
// тип агента и модель участника, для review — не исполнитель задачи.
// Задача в ready получает статус-проекцию шага и ветку. ok=false —
// назначать нечего.
func (s *Store) AssignRun(ctx context.Context, excludedProjects, excludedEpics []string) (RunAssignment, bool, error) {
	return s.AssignRunExcept(ctx, excludedProjects, excludedEpics, nil, nil)
}

// AssignRunExcept — AssignRun без указанных runner'ов и запусков: тик
// планировщика исключает те, чья стадия в этом тике не стартовала, и
// продолжает назначать остальные (add-agent-roles).
func (s *Store) AssignRunExcept(ctx context.Context, excludedProjects, excludedEpics, excludedRunners []string, excludedRuns []int64) (RunAssignment, bool, error) {
	var a RunAssignment
	assigned := false
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	if excludedEpics == nil {
		excludedEpics = []string{}
	}
	if excludedRunners == nil {
		excludedRunners = []string{}
	}
	if excludedRuns == nil {
		excludedRuns = []int64{}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var runID int64
		var taskID, epicID, projectID, runnerID, kind, entry string
		var from domain.TaskStatus
		var num int64
		var branch, role, roleTitle, runnerName, agentID, agentName string
		var rolesFeature bool
		var seen time.Time
		err := tx.QueryRow(ctx, `
			SELECT r.id, t.id, t.epic_id, e.project_id, x.id, r.step_kind, t.step_entry, t.status, t.num, COALESCE(t.branch,''),
				r.role, COALESCE((SELECT ar.title FROM agent_roles ar WHERE ar.id = r.role), ''),
				COALESCE(NULLIF(x.display_name, ''), x.id), 'roles' = ANY(x.features),
				COALESCE(x.agent_id, ''), COALESCE(x.agent_name, ''), x.last_seen
			FROM task_step_runs r
			JOIN tasks t ON t.id = r.task_id
			JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
				AND e.project_id <> ALL($1::uuid[])
				AND e.id <> ALL($2::uuid[])
			JOIN LATERAL (
				SELECT x.id, x.display_name, x.features, x.last_seen, ag.id AS agent_id, ag.name AS agent_name FROM runners x
				-- агент команды роли (add-agent-roles): включённый, без открытого
				-- запуска, runner подходит ему по профилю и модели
				LEFT JOIN team_agents ag ON r.role <> '' AND ag.role_id = r.role AND ag.enabled
					AND NOT EXISTS (SELECT 1 FROM task_step_runs o WHERE o.agent_id = ag.id AND o.verdict IS NULL)
					AND (ag.model = '' OR ag.model = ANY(x.models) OR x.model = ag.model)
				WHERE x.status = 'idle' AND NOT x.draining AND x.task_id IS NULL AND x.id <> ALL($3::text[])
				  AND x.capabilities @> (r.capabilities || CASE WHEN r.step_kind IN ('code','test','prompt') THEN t.capabilities ELSE '{}'::text[] END)
				  AND (r.agent_kind = '' OR x.agent = r.agent_kind)
				  -- профиль: агента команды, иначе шаблона (если задан); профиль
				  -- переопределения проекта — только предпочтение (ниже)
				  AND x.agent = COALESCE(NULLIF(ag.agent, ''), NULLIF(r.role_agent, ''), x.agent)
				  AND (r.model = '' OR r.model = ANY(x.models))
				  AND (r.step_kind <> 'prompt' OR 'PROMPT' = ANY(x.stages))
				  AND (r.step_kind <> 'review' OR x.id IS DISTINCT FROM t.runner_id)
				  -- у роли есть команда — нужен свободный подходящий агент; без
				  -- агентов роли участник работает по шаблону через capability
				  AND (ag.id IS NOT NULL OR r.role = ''
				       OR NOT EXISTS (SELECT 1 FROM team_agents ta WHERE ta.role_id = r.role AND ta.enabled))
				-- агент команды: исправление — тому же агенту роли, что вёл задачу,
				-- и по возможности на том же runner'е (тёплая рабочая копия);
				-- роль (add-agent-roles): сначала runner с моделью роли, если она
				-- задана; тестировщику — runner, не писавший код задачи (оба
				-- правила — предпочтение: без такого runner'а подойдёт любой);
				-- дальше беречь специалистов: из подходящих берём наименее
				-- «богатого» по capabilities и моделям, чтобы runner с редкой
				-- моделью достался участнику, которому без него не обойтись
				ORDER BY (ag.id IS NOT NULL AND ag.id = (
						SELECT o.agent_id FROM task_step_runs o
						WHERE o.task_id = t.id AND o.role = r.role AND o.agent_id IS NOT NULL
						ORDER BY o.id DESC LIMIT 1)) DESC,
					(ag.id IS NOT NULL AND r.step_kind = 'code' AND x.id IS NOT DISTINCT FROM (
						SELECT o.runner_id FROM task_step_runs o
						WHERE o.task_id = t.id AND o.step_kind = 'code' AND o.runner_id IS NOT NULL
						ORDER BY o.id DESC LIMIT 1)) DESC,
					(r.pref_agent <> '' AND x.agent = r.pref_agent) DESC,
					(r.pref_model <> '' AND (r.pref_model = ANY(x.models) OR x.model = r.pref_model)) DESC,
					(r.step_kind = 'test' AND r.role <> '' AND x.id IS NOT DISTINCT FROM t.runner_id) ASC,
					cardinality(x.capabilities), cardinality(x.models), x.last_seen DESC, ag.position
				FOR UPDATE OF x SKIP LOCKED
				LIMIT 1
			) x ON true
			WHERE r.runner_id IS NULL AND r.verdict IS NULL AND `+agentRunsOnly+`
			  AND t.status IN ('ready','running','fixing','testing','review')
			  AND r.id <> ALL($4::bigint[])
			  AND (r.not_before IS NULL OR r.not_before <= now())
			ORDER BY t.num, r.id
			FOR UPDATE OF t, r SKIP LOCKED
			LIMIT 1`, excludedProjects, excludedEpics, excludedRunners, excludedRuns).
			Scan(&runID, &taskID, &epicID, &projectID, &runnerID, &kind, &entry, &from, &num, &branch, &role, &roleTitle, &runnerName, &rolesFeature,
				&agentID, &agentName, &seen)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		// Исполнитель для людей: роль и имя runner'а (спека agent-roles
		// «Роль исполнителя в событиях и API»).
		who := "runner " + runnerID
		payload := map[string]any{"runner": runnerID, "runner_name": runnerName}
		if role != "" && roleTitle != "" {
			who = roleTitle + " (runner " + runnerID + ")"
			if agentName != "" {
				who = roleTitle + " " + agentName + " (runner " + runnerID + ")"
				payload["agent"] = map[string]any{"id": agentID, "name": agentName}
			}
			payload["role"] = map[string]any{"id": role, "title": roleTitle}
			// Runner без поддержки ролей выполнит стадию по прежнему
			// заданию: инструкция роли до агента не дойдёт.
			if !rolesFeature {
				payload["role_delivered"] = false
				who += ", роль не передана: runner без поддержки ролей"
			}
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_step_runs SET runner_id=$2, agent_id=NULLIF($3, '') WHERE id=$1 AND verdict IS NULL`,
			runID, runnerID, agentID); err != nil {
			return err
		}
		if err := bindRunner(ctx, tx, taskID, kind, runnerID); err != nil {
			return err
		}
		text := "назначен исполнитель: "
		switch {
		case kind == policy.StepReview:
			text = "назначен ревьюер: "
		case kind == policy.StepTest:
			text = "назначен исполнитель для проверок: "
		case entry == policy.OutcomeChanges:
			text = "назначен исполнитель для исправлений: "
		}
		if from == domain.TaskReady {
			to := StepStatus(kind, entry)
			if branch == "" {
				branch = fmt.Sprintf("agent/task-%d", num)
			}
			// Первое назначение ставит started_at (триггер 0030). Если стадия
			// не будет доставлена, UndoAssign его снимет.
			var firstStart bool
			if err := tx.QueryRow(ctx, `SELECT started_at IS NULL FROM tasks WHERE id=$1`, taskID).Scan(&firstStart); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status=$2, branch=$3, wait_reason='', wait_kind='', updated_at=now() WHERE id=$1 AND status='ready'`,
				taskID, to, branch); err != nil {
				return err
			}
			a.Pending = &PendingAssign{To: to, FirstStart: firstStart, Event: EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.status",
				ProjectID: projectID, EpicID: epicID, TaskID: taskID,
				Text:    "назначена: " + who,
				Payload: withPayload(payload, "status", string(to)),
			}}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET wait_reason='', wait_kind='', updated_at=now() WHERE id=$1`, taskID); err != nil {
				return err
			}
			a.Pending = &PendingAssign{Event: EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.assign",
				ProjectID: projectID, EpicID: epicID, TaskID: taskID,
				Text: text + who, Payload: payload,
			}}
		}
		a.Run.ID, a.Task.ID, a.Runner.ID, a.Runner.LastSeen = runID, taskID, runnerID, seen
		assigned = true
		return nil
	})
	// Два назначения выбрали одного свободного агента одновременно:
	// уникальный индекс открытого запуска агента отклонил второе, запуск
	// назначится на следующем тике.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "task_step_runs_agent_open" {
		return a, false, nil
	}
	if err != nil || !assigned {
		return a, false, err
	}
	// Полные сущности — вне «горячей» транзакции.
	if a.Task, err = s.GetTask(ctx, a.Task.ID); err != nil {
		return a, true, err
	}
	if a.Run, err = scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_step_runs r WHERE r.id=$1`, a.Run.ID)); err != nil {
		return a, true, err
	}
	seen := a.Runner.LastSeen
	if a.Runner, err = s.GetRunner(ctx, a.Runner.ID); err != nil {
		return a, true, err
	}
	// LastSeen — снимок из транзакции назначения: по нему недоставка
	// отличает мёртвый runner от переподключившегося (MarkRunnerOffline).
	a.Runner.LastSeen = seen
	return a, true, nil
}

// GetRunner — runner по идентификатору.
func (s *Store) GetRunner(ctx context.Context, id string) (domain.Runner, error) {
	r, err := scanRunner(s.Pool.QueryRow(ctx, `
		SELECT `+runnerCols+` FROM `+runnerFrom+` WHERE r.id=$1`, id))
	return r, nf(err)
}

// WaitingRuns — ожидающие запуски, которым не подходит ни один
// зарегистрированный runner (любого статуса): причина ожидания для задачи
// (спека orchestration «Нет runner'а с нужным агентом и моделью»).
func (s *Store) WaitingRuns(ctx context.Context) ([]StepRun, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+runCols+` FROM task_step_runs r
		JOIN tasks t ON t.id=r.task_id
		JOIN epics e ON e.id=t.epic_id AND e.status='running'
		WHERE r.runner_id IS NULL AND r.verdict IS NULL AND t.wait_reason='' AND `+agentRunsOnly+`
		  -- роли с командой разбирает WaitingTeamRuns (агенты и их профили)
		  AND (r.role = '' OR NOT EXISTS (SELECT 1 FROM team_agents ta WHERE ta.role_id = r.role AND ta.enabled))
		  AND NOT EXISTS (
			SELECT 1 FROM runners x
			WHERE x.status <> 'offline' AND NOT x.draining
			  AND x.capabilities @> (r.capabilities || CASE WHEN r.step_kind IN ('code','test','prompt') THEN t.capabilities ELSE '{}'::text[] END)
			  AND (r.agent_kind = '' OR x.agent = r.agent_kind)
			  AND (r.role_agent = '' OR x.agent = r.role_agent)
			  AND (r.model = '' OR r.model = ANY(x.models))
			  AND (r.step_kind <> 'prompt' OR 'PROMPT' = ANY(x.stages)))
		ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	return collectRuns(rows)
}

// SetTaskWaitReason фиксирует причину ожидания (пусто — снять).
func (s *Store) SetTaskWaitReason(ctx context.Context, taskID, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET wait_reason=$2, wait_kind='' WHERE id=$1`, taskID, reason)
	return err
}

// WaitKindTeam — причина ожидания ролей с командой (markTeamWaiting).
const WaitKindTeam = "team"

// SetTaskTeamWait фиксирует причину ожидания агента команды: её снимает
// ClearTeamWaits, когда задача стоит из-за бюджета или зависимостей Epic'а.
func (s *Store) SetTaskTeamWait(ctx context.Context, taskID, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET wait_reason=$2, wait_kind=$3 WHERE id=$1`, taskID, reason, WaitKindTeam)
	return err
}

// ClearTeamWaits снимает причину ожидания агента команды у задач проектов
// и Epic'ов, исключённых из назначений (бюджет, зависимости Epic'а): они
// стоят не из-за агентов. Прочие причины не трогаются. Возвращает задачи,
// у которых причина снята.
func (s *Store) ClearTeamWaits(ctx context.Context, excludedProjects, excludedEpics []string) ([]string, error) {
	if len(excludedProjects) == 0 && len(excludedEpics) == 0 {
		return nil, nil
	}
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	if excludedEpics == nil {
		excludedEpics = []string{}
	}
	rows, err := s.Pool.Query(ctx, `
		UPDATE tasks t SET wait_reason='', wait_kind=''
		FROM epics e
		WHERE e.id = t.epic_id AND t.wait_kind = $3
		  AND (e.project_id = ANY($1::uuid[]) OR e.id = ANY($2::uuid[]))
		RETURNING t.id::text`, excludedProjects, excludedEpics, WaitKindTeam)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RejectStep фиксирует отказ шага (замечания review, провал проверок):
// расходует попытку задачи и проход шага; при исчерпании любого из лимитов
// задача проваливается с эскалацией reason. Runner'ы задачи при провале
// освобождаются. Возвращает failed и число отказов шага.
func (s *Store) RejectStep(ctx context.Context, taskID string, step policy.ResolvedStep, reason domain.AttentionReason, detail, policyHash string) (failed bool, rejections int, err error) {
	payload := func(m map[string]any) map[string]any {
		if policyHash != "" {
			m["policy_hash"] = policyHash
		}
		m["step"] = step.ID
		return m
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var used, limit, reviewRej int
		var from domain.TaskStatus
		var epicID, projectID string
		var rejRaw []byte
		if err := tx.QueryRow(ctx, `
			SELECT t.attempt_used, t.attempt_limit, t.review_rejections, t.status, t.epic_id, e.project_id, t.step_rejections
			FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, taskID).
			Scan(&used, &limit, &reviewRej, &from, &epicID, &projectID, &rejRaw); err != nil {
			return err
		}
		rej := map[string]int{}
		_ = json.Unmarshal(rejRaw, &rej)
		rej[step.ID]++
		rejections = rej[step.ID]
		if step.Kind == policy.StepReview {
			reviewRej++
		}
		used++
		rejJSON, _ := json.Marshal(rej)
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET attempt_used=$2, review_rejections=$3, step_rejections=$4, updated_at=now() WHERE id=$1`,
			taskID, used, reviewRej, rejJSON); err != nil {
			return err
		}
		stepExhausted := rejections >= step.Attempts
		if used < limit && !stepExhausted {
			return nil
		}
		if !from.CanTransition(domain.TaskFailed) {
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(domain.TaskFailed)}
		}
		failed = true
		var msg string
		switch step.Kind {
		case policy.StepReview:
			msg = fmt.Sprintf("Review отклонил результат %d раз — лимит отказов review исчерпан.", rejections)
			if used >= limit && !stepExhausted {
				msg = fmt.Sprintf("Review отклонил результат %d раз — лимит попыток исчерпан.", used)
			}
		case policy.StepTest:
			msg = fmt.Sprintf("Автопроверки провалились %d раз — лимит попыток исчерпан.", rejections)
		default:
			msg = fmt.Sprintf("Цикл исправления неуспешен %d раз — лимит попыток исчерпан.", used)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status='failed', runner_id=NULL, updated_at=now() WHERE id=$1`, taskID); err != nil {
			return err
		}
		if err := freeRunner(ctx, tx, taskID); err != nil {
			return err
		}
		if err := cancelOpenRuns(ctx, tx, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO attention (project_id, task_id, reason, message)
			VALUES ($1,$2,$3,$4)`, projectID, taskID, string(reason), msg); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.status",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text: msg, Payload: payload(map[string]any{"status": "failed", "attempt": used, "detail": detail}),
		})
		return err
	})
	return failed, rejections, err
}

// MergeConflictKey — ключ счётчика возвратов на разрешение конфликта в
// tasks.step_rejections. Двоеточие не встречается в id шагов, поэтому
// счётчик не смешивается с отказами самого шага merge (замечания человека
// на хостинге, провал внешних проверок).
func MergeConflictKey(mergeStepID string) string { return mergeStepID + ":conflicts" }

// MergeConflictInput — конфликт при merge задачи. StepID и Gen — снимок
// задачи, с которого начался merge (обычно шаг merge, но человек может
// подтвердить merge и на шаге review); MergeStepID — шаг merge процесса,
// по нему ведётся счётчик.
type MergeConflictInput struct {
	TaskID, ProjectID, StepID, MergeStepID string
	Gen, Limit                             int
	// Reset — вмешательство человека (подтверждение merge): счётчик
	// возвратов обнуляется, открытая эскалация MERGE_CONFLICT закрывается.
	Reset bool
	// Escalation — текст эскалации на случай исчерпания лимита.
	Escalation string
}

// MergeConflictResult — исход учёта конфликта.
type MergeConflictResult struct {
	// Returns — номер этого возврата агенту; при исчерпании — сколько их было.
	Returns int
	// Return — задача должна уйти агенту: счётчик увеличен.
	Return bool
	// Exhausted — лимит исчерпан, задача остаётся на merge.
	Exhausted bool
	// Escalated — эскалация MERGE_CONFLICT создана этим вызовом.
	Escalated bool
	// Stale — задача уже не на этом входе в merge (её увёл другой
	// обработчик): ничего не изменено.
	Stale bool
}

// RecordMergeConflict учитывает конфликт при merge одной транзакцией под
// блокировкой строки задачи (спека task-pipeline «Конфликт с базовой веткой
// при merge»): попытки задачи (attempt_used, review_rejections) не
// тратятся. Операцию merge целиком сериализует движок (блокировка по
// задаче); здесь снимок ещё раз сверяется с задачей: если она ушла с этого
// входа в merge, ответ Stale и ничего не меняется.
func (s *Store) RecordMergeConflict(ctx context.Context, in MergeConflictInput) (MergeConflictResult, error) {
	var res MergeConflictResult
	key := MergeConflictKey(in.MergeStepID)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status domain.TaskStatus
		var stepID string
		var gen, count int
		err := tx.QueryRow(ctx, `
			SELECT status, step_id, step_gen, COALESCE((step_rejections->>$2)::int, 0)
			FROM tasks WHERE id=$1 FOR UPDATE`, in.TaskID, key).Scan(&status, &stepID, &gen, &count)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.TaskReview || stepID != in.StepID || gen != in.Gen {
			res.Stale = true
			return nil
		}
		if in.Reset {
			count = 0
			if _, err := tx.Exec(ctx, `
				UPDATE attention SET status='resolved', resolved_at=now()
				WHERE task_id=$1 AND reason=$2 AND status <> 'resolved'`, in.TaskID, string(domain.AttMergeConflict)); err != nil {
				return err
			}
		}
		if count >= in.Limit {
			res.Returns, res.Exhausted = count, true
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET step_rejections = jsonb_set(step_rejections, ARRAY[$2::text], to_jsonb($3::int)), updated_at=now()
				WHERE id=$1`, in.TaskID, key, count); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO attention (project_id, task_id, reason, message)
				SELECT $1, $2, $3, $4
				WHERE NOT EXISTS (SELECT 1 FROM attention WHERE task_id=$2 AND reason=$3 AND status <> 'resolved')`,
				in.ProjectID, in.TaskID, string(domain.AttMergeConflict), in.Escalation)
			if err != nil {
				return err
			}
			res.Escalated = tag.RowsAffected() > 0
			return nil
		}
		res.Returns, res.Return = count+1, true
		_, err = tx.Exec(ctx, `
			UPDATE tasks SET step_rejections = jsonb_set(step_rejections, ARRAY[$2::text], to_jsonb($3::int)), updated_at=now()
			WHERE id=$1`, in.TaskID, key, res.Returns)
		return err
	})
	return res, err
}

// ClaimStepOutcome закрепляет применение исхода за текущим входом на шаг:
// true — вызывающий применяет исход, false — его уже применил другой
// обработчик (одновременные вердикты участников, повтор доставки).
func (s *Store) ClaimStepOutcome(ctx context.Context, taskID string, gen int) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE tasks SET step_closed_gen=$2 WHERE id=$1 AND step_gen=$2 AND step_closed_gen < $2`, taskID, gen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// IsSessionOpen — открыта ли сессия задачи (StageResult после рестарта
// plane: карта сессий пуста, истина в БД).
func (s *Store) IsSessionOpen(ctx context.Context, taskID, sessionID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM sessions WHERE id=$1::uuid AND task_id=$2 AND ended_at IS NULL)`,
		sessionID, taskID).Scan(&ok)
	return ok, err
}

// TaskProcess — снимок процесса задачи; nil — задача его ещё не получила.
func TaskProcess(t domain.Task) *policy.Resolved {
	if len(t.Process) == 0 {
		return nil
	}
	var r policy.Resolved
	if err := json.Unmarshal(t.Process, &r); err != nil {
		return nil
	}
	return &r
}

// ─── участники-люди (add-process-humans) ─────────────────────────────────

// StepItem — элемент очереди «мои шаги» (api-contract).
type StepItem struct {
	Run       StepRun
	Task      domain.Task
	ProjectID string
	Project   string
	EpicID    string
	Epic      string
	Addressed string
	Context   string
}

// MySteps — открытые запуски людей, адресованные пользователю: по его
// логину или роли в проекте задачи; только работающие Epic'и.
func (s *Store) MySteps(ctx context.Context, userID, login string) ([]StepItem, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+runCols+`, `+taskCols+`, p.id::text, p.name, e.id::text, e.title
		FROM task_step_runs r
		JOIN (`+taskFrom+`) ON t.id = r.task_id
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
		JOIN projects p ON p.id = e.project_id
		JOIN project_members m ON m.project_id = p.id AND m.user_id = $1::uuid
		WHERE r.verdict IS NULL AND (r.user_login = $2 OR (r.user_role <> '' AND r.user_role = m.role))
		ORDER BY r.created_at, r.id`, userID, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StepItem{}
	for rows.Next() {
		var it StepItem
		var r StepRun
		var t domain.Task
		var crit, proc, rej []byte
		if err := rows.Scan(&r.ID, &r.TaskID, &r.StepID, &r.StepKind, &r.Pass, &r.Participant, &r.AgentKind, &r.Model,
			&r.Capabilities, &r.UserLogin, &r.UserRole, &r.RunnerID, &r.SessionID, &r.Verdict, &r.VerdictBy,
			&r.Detail, &r.CreatedAt, &r.FinishedAt, &r.Role, &r.PrefModel, &r.AgentID, &r.AgentName,
			&r.RoleAgent, &r.PrefAgent, &r.AgentVerdict, &r.AssignedModel,
			&t.ID, &t.EpicID, &t.Num, &t.Title, &t.Description, &t.Status, &t.Estimate,
			&t.Capabilities, &crit, &t.AttemptUsed, &t.AttemptLimit, &t.ReviewRejections,
			&t.RunnerID, &t.Branch, &t.PRURL, &t.BlockReason, &t.BlockedBy,
			&t.StepID, &t.StepEntry, &proc, &t.ProcessHash, &rej, &t.WaitReason, &t.StepGen, &t.Created, &t.Updated,
			&t.RoleID, &t.RunnerName, &t.AgentID, &t.AgentName, &t.StartedAt, &t.DoneAt,
			&it.ProjectID, &it.Project, &it.EpicID, &it.Epic); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(crit, &t.Criteria)
		it.Run, it.Task = r, t
		it.Addressed = r.UserLogin
		if r.UserRole != "" {
			it.Addressed = "role:" + r.UserRole
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Контекст входа: вердикты уже закрытых участников того же входа.
	for i := range out {
		out[i].Context, _ = s.stepContext(ctx, out[i].Run)
	}
	return out, nil
}

// stepContext — замечания и отчёты участников текущего входа на шаг.
func (s *Store) stepContext(ctx context.Context, run StepRun) (string, error) {
	runs, err := s.StepRuns(ctx, run.TaskID, run.StepID, run.Pass)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, r := range runs {
		if r.ID == run.ID || r.Verdict == "" || r.Detail == "" {
			continue
		}
		who := r.Participant
		if r.VerdictBy != "" {
			who += " (" + r.VerdictBy + ")"
		} else if r.AgentKind != "" || r.DisplayModel() != "" {
			who += " (" + strings.TrimSuffix(r.AgentKind+"/"+r.DisplayModel(), "/") + ")"
		}
		parts = append(parts, who+": "+r.Verdict+"\n"+r.Detail)
	}
	return strings.Join(parts, "\n\n"), nil
}

// ErrNotAddressed — вердикт не от адресата запуска.
var ErrNotAddressed = errors.New("запуск адресован другому участнику")

// RunForVerdict — открытый запуск человека по задаче, если он адресован
// пользователю (логином или его ролью в проекте задачи).
func (s *Store) RunForVerdict(ctx context.Context, taskID string, runID int64, userID, login string) (StepRun, error) {
	r, err := scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_step_runs r WHERE r.id=$1 AND r.task_id=$2`, runID, taskID))
	if err != nil {
		return StepRun{}, nf(err)
	}
	if !r.IsUser() {
		return r, ErrNotAddressed
	}
	if r.UserLogin != "" {
		if r.UserLogin != login {
			return r, ErrNotAddressed
		}
		return r, nil
	}
	var role string
	if err := s.Pool.QueryRow(ctx, `
		SELECT m.role FROM project_members m JOIN epics e ON e.project_id = m.project_id
		JOIN tasks t ON t.epic_id = e.id WHERE t.id=$1 AND m.user_id=$2::uuid`, taskID, userID).Scan(&role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrNotAddressed
		}
		return r, err
	}
	if role != r.UserRole {
		return r, ErrNotAddressed
	}
	return r, nil
}

// OpenUserRun — открытый запуск человека на текущем входе задачи,
// адресованный автору review с хостинга: по его логину либо по роли,
// которую он имеет в проекте задачи. Автор не из проекта запуск не
// получает: чужой ревьюер на хостинге не должен закрывать шаг человека.
func (s *Store) OpenUserRun(ctx context.Context, task domain.Task, login string) (StepRun, bool, error) {
	if login == "" {
		return StepRun{}, false, nil
	}
	runs, err := s.StepRuns(ctx, task.ID, task.StepID, task.StepGen)
	if err != nil {
		return StepRun{}, false, err
	}
	var role string
	if err := s.Pool.QueryRow(ctx, `
		SELECT m.role FROM tasks t JOIN epics e ON e.id = t.epic_id
		JOIN project_members m ON m.project_id = e.project_id
		JOIN users u ON u.id = m.user_id
		WHERE t.id=$1 AND u.login=$2`, task.ID, login).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StepRun{}, false, err
	}
	var byRole *StepRun
	for i := range runs {
		r := runs[i]
		if !r.IsUser() || r.Verdict != "" {
			continue
		}
		if r.UserLogin != "" && r.UserLogin == login {
			return r, true, nil
		}
		if r.UserRole != "" && role != "" && r.UserRole == role && byRole == nil {
			byRole = &runs[i]
		}
	}
	if byRole != nil {
		return *byRole, true, nil
	}
	return StepRun{}, false, nil
}

// CurrentStepRuns — запуски текущего входа задачи, а если у него нет
// участников (merge, deploy) — последнего входа с запусками: деталка
// показывает вердикты review, пока задача ждёт merge.
func (s *Store) CurrentStepRuns(ctx context.Context, task domain.Task) ([]StepRun, error) {
	if task.StepID == "" {
		return nil, nil
	}
	runs, err := s.StepRuns(ctx, task.ID, task.StepID, task.StepGen)
	if err != nil || len(runs) > 0 {
		return runs, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM task_step_runs r
		WHERE r.task_id=$1 AND r.pass = (SELECT max(pass) FROM task_step_runs WHERE task_id=$1)
		ORDER BY r.id`, task.ID)
	if err != nil {
		return nil, err
	}
	return collectRuns(rows)
}

// ValidateProcessMembers проверяет, что участники-люди по логину состоят
// в проекте (спека process «Логин не в проекте»).
func (s *Store) ValidateProcessMembers(ctx context.Context, projectID string, o policy.Overrides) error {
	if o.Process == nil {
		return nil
	}
	for login, step := range o.Process.UserLogins() {
		var n int
		if err := s.Pool.QueryRow(ctx, `
			SELECT count(*) FROM project_members m JOIN users u ON u.id = m.user_id
			WHERE m.project_id=$1 AND u.login=$2`, projectID, login).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &policy.ProcessError{Step: step, Field: "participants",
				Msg: fmt.Sprintf("участник %q не состоит в проекте", login)}
		}
	}
	return nil
}

// StepsToReconcile — задачи, у которых все запуски текущего входа закрыты,
// а исход шага ещё не применён (сбой между вердиктом и продвижением):
// движок дожимает их на тике.
func (s *Store) StepsToReconcile(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+taskCols+` FROM `+taskFrom+`
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
		WHERE t.status IN ('ready','running','fixing','testing','review')
		  AND t.step_gen > t.step_closed_gen
		  AND EXISTS (SELECT 1 FROM task_step_runs r WHERE r.task_id = t.id AND r.pass = t.step_gen)
		  AND NOT EXISTS (SELECT 1 FROM task_step_runs r WHERE r.task_id = t.id AND r.pass = t.step_gen AND r.verdict IS NULL)
		ORDER BY t.num`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// withPayload — копия payload с добавленным ключом.
func withPayload(p map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(p)+1)
	for key, val := range p {
		out[key] = val
	}
	out[k] = v
	return out
}
