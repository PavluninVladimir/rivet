package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Архитектура проекта (add-architecture-c4, спека backend/project-architecture):
// снимок описания с основной ветки и задача описания в системном Epic.

// ArchitectureRow — сохранённый снимок проекта.
type ArchitectureRow struct {
	Status      string
	SHA         string
	ReadAt      *time.Time
	Error       string
	Defects     json.RawMessage
	Data        json.RawMessage
	ValidSHA    string
	ValidReadAt *time.Time
	ValidData   json.RawMessage
	UpdatedAt   time.Time
	ErrorKind   string
	TreeSHA     string
}

// Виды ошибки снимка (project_architecture.error_kind).
const (
	ArchErrHost          = "host"           // сбой хостинга: тот же SHA перечитывается
	ArchErrLimit         = "limit"          // превышение лимитов чтения
	ArchErrData          = "data"           // описание не разобрано или не записано
	ArchErrNoCredentials = "no_credentials" // у проекта нет своих учётных данных
)

// ArchitectureSave — результат чтения описания.
type ArchitectureSave struct {
	Status    string // absent | ok | invalid | error
	SHA       string
	Error     string
	ErrorKind string // при error, см. ArchErr*
	TreeSHA   string // SHA дерева docs/architecture, если хостинг его отдал
	Defects   json.RawMessage
	Data      json.RawMessage // только при ok
}

// GetProjectArchitecture — снимок проекта; ok=false, если он ещё не строился.
func (s *Store) GetProjectArchitecture(ctx context.Context, projectID string) (ArchitectureRow, bool, error) {
	var r ArchitectureRow
	var defects, data, valid []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT status, sha, read_at, error, defects, data, valid_sha, valid_read_at, valid_data, updated_at,
			error_kind, tree_sha
		FROM project_architecture WHERE project_id=$1`, projectID).
		Scan(&r.Status, &r.SHA, &r.ReadAt, &r.Error, &defects, &data, &r.ValidSHA, &r.ValidReadAt, &valid, &r.UpdatedAt,
			&r.ErrorKind, &r.TreeSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Defects, r.Data, r.ValidData = defects, data, valid
	if len(r.Defects) == 0 {
		r.Defects = json.RawMessage("[]")
	}
	// При ok последняя корректная версия и есть текущая.
	if r.Status == "ok" {
		r.ValidData = r.Data
	}
	return r, true, nil
}

// ArchitectureStatus — статус снимка проекта; пусто, если снимка нет.
func (s *Store) ArchitectureStatus(ctx context.Context, projectID string) (string, error) {
	var st string
	err := s.Pool.QueryRow(ctx, `SELECT status FROM project_architecture WHERE project_id=$1`, projectID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// SaveProjectArchitecture записывает снимок. При ok данные становятся и
// текущими, и последней корректной версией (одной копией в data). При
// invalid и error последняя корректная версия сохраняется в valid_data.
// При absent описания больше нет, и
// последняя корректная версия тоже снимается. Возвращает прежний SHA и
// признак, что снимок существовал до записи.
func (s *Store) SaveProjectArchitecture(ctx context.Context, projectID string, in ArchitectureSave) (prevSHA string, existed bool, err error) {
	switch in.Status {
	case "absent", "ok", "invalid", "error":
	default:
		return "", false, fmt.Errorf("неизвестный статус снимка архитектуры %q", in.Status)
	}
	defects := in.Defects
	if len(defects) == 0 {
		defects = json.RawMessage("[]")
	}
	var data []byte
	if in.Status == "ok" {
		data = in.Data
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var prev string
		qerr := tx.QueryRow(ctx, `SELECT sha FROM project_architecture WHERE project_id=$1 FOR UPDATE`, projectID).Scan(&prev)
		switch {
		case errors.Is(qerr, pgx.ErrNoRows):
		case qerr != nil:
			return qerr
		default:
			existed, prevSHA = true, prev
		}
		kind := ""
		if in.Status == "error" {
			kind = in.ErrorKind
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO project_architecture (project_id, status, sha, read_at, error, defects, data,
				valid_sha, valid_read_at, valid_data, updated_at, error_kind, tree_sha)
			VALUES ($1, $2, $3, now(), $4, $5, $6,
				CASE WHEN $2 = 'ok' THEN $3 ELSE '' END,
				CASE WHEN $2 = 'ok' THEN now() END,
				NULL, now(), $7, $8)
			ON CONFLICT (project_id) DO UPDATE SET
				status = EXCLUDED.status, sha = EXCLUDED.sha, read_at = EXCLUDED.read_at,
				error = EXCLUDED.error, defects = EXCLUDED.defects, data = EXCLUDED.data,
				error_kind = EXCLUDED.error_kind, tree_sha = EXCLUDED.tree_sha,
				valid_sha = CASE
					WHEN EXCLUDED.status = 'ok' THEN EXCLUDED.sha
					WHEN EXCLUDED.status = 'absent' THEN ''
					ELSE project_architecture.valid_sha END,
				valid_read_at = CASE
					WHEN EXCLUDED.status = 'ok' THEN EXCLUDED.read_at
					WHEN EXCLUDED.status = 'absent' THEN NULL
					ELSE project_architecture.valid_read_at END,
				-- При ok корректная версия лежит в data: копия не хранится.
				-- Она переезжает в valid_data, когда текущая версия
				-- перестаёт быть корректной.
				valid_data = CASE
					WHEN EXCLUDED.status IN ('ok', 'absent') THEN NULL
					WHEN project_architecture.status = 'ok' THEN project_architecture.data
					ELSE project_architecture.valid_data END,
				updated_at = now()`,
			projectID, in.Status, in.SHA, in.Error, []byte(defects), data, kind, in.TreeSHA)
		return err
	})
	return prevSHA, existed, err
}

// TouchArchitectureSHA — основная ветка ушла вперёд, а дерево описания
// то же: снимок не перечитывается, запоминается только новый SHA.
func (s *Store) TouchArchitectureSHA(ctx context.Context, projectID, sha string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE project_architecture SET sha = $2,
			valid_sha = CASE WHEN status = 'ok' THEN $2 ELSE valid_sha END, updated_at = now()
		WHERE project_id = $1`, projectID, sha)
	return err
}

// ErrArchitectureTaskActive — у проекта уже есть незавершённая задача
// описания архитектуры.
type ErrArchitectureTaskActive struct {
	EpicID string
	TaskID string
}

func (e *ErrArchitectureTaskActive) Error() string {
	return "задача описания архитектуры уже идёт"
}

// ArchitectureTaskInput — задача описания: процесс строится вызывающим
// (внутренний процесс describe → check → review → merge).
type ArchitectureTaskInput struct {
	ProjectID   string
	Title       string
	Goal        string
	TaskTitle   string
	Description string
	Criteria    []domain.Criterion
	Process     *policy.Resolved
	ProcessHash string
	Trigger     string // connect | epic_start | manual
	UserLogin   string
	// MarkAuto — автоописание: отметка projects.architecture_auto_at
	// ставится в той же транзакции, что задача. Отметка уже стоит — задача
	// не создаётся, ошибка ErrArchitectureAutoDone.
	MarkAuto bool
	// ReplaceFailed — ручная постановка: упавшая задача описания (failed)
	// отменяется, новая ставится в тот же системный Epic. Незавершённая и
	// не упавшая задача по-прежнему даёт ErrArchitectureTaskActive.
	ReplaceFailed bool
}

// ErrArchitectureAutoDone — автоописание в проекте уже срабатывало.
var ErrArchitectureAutoDone = errors.New("автоописание архитектуры уже срабатывало")

// CreateArchitectureTask создаёт системный Epic вида architecture сразу в
// running и единственную задачу описания со снимком внутреннего процесса.
// Вторая незавершённая задача на проект отклоняется ErrArchitectureTaskActive
// (частичный уникальный индекс по Epic'ам вида architecture).
func (s *Store) CreateArchitectureTask(ctx context.Context, in ArchitectureTaskInput) (epicID, taskID string, err error) {
	if in.Process == nil {
		return "", "", errors.New("нет процесса задачи описания")
	}
	proc, err := json.Marshal(in.Process)
	if err != nil {
		return "", "", err
	}
	crit, _ := json.Marshal(in.Criteria)
	eff, err := s.EffectivePolicy(ctx, in.ProjectID)
	if err != nil {
		return "", "", err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if in.MarkAuto {
			tag, err := tx.Exec(ctx,
				`UPDATE projects SET architecture_auto_at = now() WHERE id = $1 AND architecture_auto_at IS NULL`, in.ProjectID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrArchitectureAutoDone
			}
		}
		reused := false
		if in.ReplaceFailed {
			var err error
			if epicID, reused, err = replaceFailedArchitectureTasks(ctx, tx, in); err != nil {
				return err
			}
		}
		if !reused {
			// Epic создаётся сразу running: триггер 0030 ставит started_at
			// только при смене статуса, поэтому начало пишется здесь.
			if err := tx.QueryRow(ctx, `
				INSERT INTO epics (project_id, title, goal, kind, status, started_at)
				VALUES ($1, $2, $3, 'architecture', 'running', now()) RETURNING id`,
				in.ProjectID, in.Title, in.Goal).Scan(&epicID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO tasks (epic_id, title, description, estimate, capabilities, criteria, attempt_limit,
				kind, process, process_hash)
			VALUES ($1, $2, $3, 2, $4, $5, $6, 'architecture', $7, $8)
			RETURNING id`,
			epicID, in.TaskTitle, in.Description, []string{"architecture"}, crit, eff.Presets.AttemptLimit,
			proc, in.ProcessHash).Scan(&taskID); err != nil {
			return err
		}
		var user any
		if in.UserLogin != "" {
			user = in.UserLogin
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "architecture.describe_started",
			ProjectID: in.ProjectID, EpicID: epicID, TaskID: taskID,
			Text: "поставлена задача описания архитектуры (" + triggerText(in.Trigger) + ")",
			Payload: map[string]any{"project_id": in.ProjectID, "epic_id": epicID, "task_id": taskID,
				"trigger": in.Trigger, "user": user},
		})
		return err
	})
	if err != nil && isUnique(err) {
		active := &ErrArchitectureTaskActive{}
		_ = s.Pool.QueryRow(ctx, `
			SELECT e.id, COALESCE((SELECT t.id::text FROM tasks t WHERE t.epic_id = e.id
				AND t.status NOT IN ('done', 'cancelled') ORDER BY t.created_at DESC LIMIT 1), '')
			FROM epics e WHERE e.project_id = $1 AND e.kind = 'architecture' AND e.status IN ('planned', 'running', 'paused')`,
			in.ProjectID).Scan(&active.EpicID, &active.TaskID)
		return "", "", active
	}
	var active *ErrArchitectureTaskActive
	if errors.As(err, &active) {
		return "", "", active
	}
	return epicID, taskID, err
}

// replaceFailedArchitectureTasks — активный системный Epic проекта для
// новой задачи описания: упавшие задачи в нём отменяются (событие на
// каждую), незавершённая и не упавшая задача даёт ErrArchitectureTaskActive.
// reused=false — активного Epic нет, его создаст вызывающий.
func replaceFailedArchitectureTasks(ctx context.Context, tx pgx.Tx, in ArchitectureTaskInput) (epicID string, reused bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT id FROM epics WHERE project_id = $1 AND kind = 'architecture'
			AND status IN ('planned', 'running', 'paused') FOR UPDATE`, in.ProjectID).Scan(&epicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, status FROM tasks WHERE epic_id = $1 AND status NOT IN ('done', 'cancelled')
		ORDER BY created_at FOR UPDATE`, epicID)
	if err != nil {
		return "", false, err
	}
	var failed []string
	var running string
	for rows.Next() {
		var id string
		var st domain.TaskStatus
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return "", false, err
		}
		if st == domain.TaskFailed {
			failed = append(failed, id)
		} else {
			running = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if running != "" {
		return "", false, &ErrArchitectureTaskActive{EpicID: epicID, TaskID: running}
	}
	for _, id := range failed {
		if err := endOpenSessions(ctx, tx, id); err != nil {
			return "", false, err
		}
		if err := freeRunner(ctx, tx, id); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status = 'cancelled', runner_id = NULL, reviewer_id = NULL, wait_reason = '', updated_at = now()
			WHERE id = $1`, id); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE attention SET status = 'resolved', resolved_at = now() WHERE task_id = $1 AND status <> 'resolved'`, id); err != nil {
			return "", false, err
		}
		actor, actorID := domain.ActorSystem, "architecture"
		if in.UserLogin != "" {
			actor, actorID = domain.ActorUser, in.UserLogin
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: actor, ActorID: actorID, Type: "task.status",
			ProjectID: in.ProjectID, EpicID: epicID, TaskID: id,
			Text:    "упавшая задача описания архитектуры отменена: поставлена новая",
			Payload: map[string]any{"status": string(domain.TaskCancelled), "reason": "architecture_replaced"},
		}); err != nil {
			return "", false, err
		}
	}
	return epicID, true, nil
}

func triggerText(t string) string {
	switch t {
	case "connect":
		return "подключение репозитория"
	case "epic_start":
		return "запуск Epic"
	default:
		return "вручную"
	}
}

// ArchitectureTaskInfo — незавершённая или последняя неудачная задача
// описания для снимка (api-contract ArchitectureTask).
type ArchitectureTaskInfo struct {
	EpicID  string `json:"epic_id"`
	TaskID  string `json:"task_id"`
	TaskNum int64  `json:"task_num"`
	Status  string `json:"status"`
	StepID  string `json:"step_id"`
	// Role и Runner — текущий исполнитель; null, если шаг никто не ведёт
	// (api-contract ArchitectureTask).
	Role    *string `json:"role"`
	Runner  *string `json:"runner"`
	Trigger string  `json:"trigger"`
	// Failure — причина провала failed-задачи, иначе null.
	Failure *domain.TaskFailure `json:"failure"`
}

// ArchitectureTask — задача описания проекта для показа: незавершённая, а
// без неё последняя неудачная. ok=false — показывать нечего.
func (s *Store) ArchitectureTask(ctx context.Context, projectID string) (ArchitectureTaskInfo, bool, error) {
	var t ArchitectureTaskInfo
	err := s.Pool.QueryRow(ctx, `
		SELECT e.id, t.id, t.num, t.status, t.step_id,
			NULLIF(cur.role, ''), NULLIF(cur.runner, ''),
			COALESCE((SELECT ev.payload->>'trigger' FROM events ev
				WHERE ev.task_id = t.id AND ev.type = 'architecture.describe_started' ORDER BY ev.id LIMIT 1), 'manual')
		FROM epics e JOIN tasks t ON t.epic_id = e.id
		LEFT JOIN LATERAL (
			SELECT sr.role, COALESCE(NULLIF(x.display_name, ''), x.id) AS runner
			FROM task_step_runs sr LEFT JOIN runners x ON x.id = sr.runner_id
			WHERE sr.task_id = t.id AND sr.verdict IS NULL ORDER BY sr.id LIMIT 1) cur ON true
		WHERE e.project_id = $1 AND e.kind = 'architecture' AND e.status <> 'archived'
		  AND t.status NOT IN ('done', 'cancelled')
		ORDER BY (t.status = 'failed'), t.created_at DESC LIMIT 1`, projectID).
		Scan(&t.EpicID, &t.TaskID, &t.TaskNum, &t.Status, &t.StepID, &t.Role, &t.Runner, &t.Trigger)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	if t.Status == string(domain.TaskFailed) {
		if t.Failure, err = s.TaskFailure(ctx, t.TaskID); err != nil {
			return t, false, err
		}
	}
	return t, true, nil
}

// ArchitectureAutoDone — автоописание у проекта уже срабатывало.
func (s *Store) ArchitectureAutoDone(ctx context.Context, projectID string) (bool, error) {
	var at *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT architecture_auto_at FROM projects WHERE id = $1`, projectID).Scan(&at)
	return at != nil, nf(err)
}

// SetWebhookEvents записывает версию набора событий подписки: у проекта и
// у других проектов того же репозитория с зарегистрированным хуком (хук
// один на URL, переподписан он для всех).
func (s *Store) SetWebhookEvents(ctx context.Context, projectID string, version int) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE projects q SET webhook_events = $2
		FROM projects p
		WHERE p.id = $1 AND (q.id = p.id OR (q.webhook_registered
			AND q.provider = p.provider AND q.base_url = p.base_url AND q.repo_path = p.repo_path))`, projectID, version)
	return err
}

// ProjectsToResubscribe — проекты с хуком, который Rivet завёл сам на
// собственных учётных данных проекта, но со старым набором событий. Хук
// у репозитория один на URL, и секрет у него последнего
// зарегистрировавшегося проекта: переподписка от имени другого проекта
// молча отобрала бы события у живого. Поэтому проект берётся, только если
// он единственный на репозитории или его регистрация точно последняя.
// Общие хуки с неизвестным порядком регистраций остаются как есть (их
// переподписывает замена учётных данных).
func (s *Store) ProjectsToResubscribe(ctx context.Context, version int) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+projectCols+` FROM projects p
		WHERE p.webhook_registered AND p.credential_id IS NOT NULL AND p.webhook_events < $1
		  AND NOT EXISTS (
			SELECT 1 FROM projects q
			WHERE q.id <> p.id AND q.webhook_registered
			  AND q.provider = p.provider AND q.base_url = p.base_url AND q.repo_path = p.repo_path
			  AND (q.webhook_registered_at IS NULL OR p.webhook_registered_at IS NULL
			       OR q.webhook_registered_at > p.webhook_registered_at))
		ORDER BY p.created_at`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetTaskCheckedSHA запоминает вершину ветки, на которой пошёл шаг проверки.
func (s *Store) SetTaskCheckedSHA(ctx context.Context, taskID, sha string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET checked_sha = $2 WHERE id = $1`, taskID, sha)
	return err
}

// TaskCheckedSHA — вершина, прошедшая шаг проверки; пусто, если не записана.
func (s *Store) TaskCheckedSHA(ctx context.Context, taskID string) (string, error) {
	var sha string
	err := s.Pool.QueryRow(ctx, `SELECT checked_sha FROM tasks WHERE id = $1`, taskID).Scan(&sha)
	return sha, nf(err)
}

// CountTaskEventsByReason — сколько событий задачи несут payload.reason.
func (s *Store) CountTaskEventsByReason(ctx context.Context, taskID, reason string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id = $1 AND payload->>'reason' = $2`, taskID, reason).Scan(&n)
	return n, err
}

// EscalateOnce — эскалация задачи с точным кодом ситуации: открытая с тем
// же кодом одна (частичный уникальный индекс), повторный вызов заменяет
// сообщение более свежим. Повторные отказы хостинга не плодят дубли, а
// точный совет не глушится старым.
func (s *Store) EscalateOnce(ctx context.Context, projectID, taskID string, reason domain.AttentionReason, code, msg string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO attention (project_id, task_id, reason, message, code)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (task_id, code) WHERE status <> 'resolved' AND code <> ''
		DO UPDATE SET message = EXCLUDED.message`,
		projectID, taskID, string(reason), msg, code)
	return err
}

// PendingExternalReverify — задачи описания в review, чей PR смержен на
// хостинге, а файлы не сверены: последнее событие сверки — «список не
// получен» с merged=true, а не итог «сверка не удалась». При старте rivetd
// их сверка поднимается заново (ревью Opus, ограничитель).
func (s *Store) PendingExternalReverify(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id::text FROM tasks t
		WHERE t.kind = 'architecture' AND t.status = 'review'
		  AND (SELECT e.payload->>'reason' FROM events e
		       WHERE e.task_id = t.id AND e.type = 'task.merge_failed'
		         AND (e.payload->>'reason' = 'architecture_files_unverified'
		              OR (e.payload->>'reason' = 'architecture_files_unavailable' AND e.payload->>'merged' = 'true'))
		       ORDER BY e.id DESC LIMIT 1) = 'architecture_files_unavailable'
		ORDER BY t.updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// taskFailureFiles — потолок путей в причине провала.
const taskFailureFiles = 50

// TaskFailure — причина провала задачи из последнего события task.status
// со статусом failed; nil, если такого события нет.
func (s *Store) TaskFailure(ctx context.Context, taskID string) (*domain.TaskFailure, error) {
	var text string
	var raw []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT text, payload FROM events
		WHERE task_id = $1 AND type = 'task.status' AND payload->>'status' = 'failed'
		ORDER BY id DESC LIMIT 1`, taskID).Scan(&text, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p struct {
		Reason     string   `json:"reason"`
		Files      []string `json:"files"`
		FilesTotal *int     `json:"files_total"`
		Merged     bool     `json:"merged"`
	}
	_ = json.Unmarshal(raw, &p)
	f := &domain.TaskFailure{Reason: p.Reason, Message: text, Files: p.Files, Merged: p.Merged}
	if f.Reason == "" {
		f.Reason = "failed"
	}
	if f.Files == nil {
		f.Files = []string{}
	}
	f.FilesTotal = len(f.Files)
	if p.FilesTotal != nil {
		f.FilesTotal = *p.FilesTotal
	}
	if len(f.Files) > taskFailureFiles {
		f.Files = f.Files[:taskFailureFiles]
	}
	return f, nil
}

// AnyRunnerWithCapability — есть ли зарегистрированный runner с capability.
func (s *Store) AnyRunnerWithCapability(ctx context.Context, capability string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runners WHERE $1 = ANY(capabilities))`, capability).Scan(&ok)
	return ok, err
}

// ProjectsWithArchitecture — проекты, у которых снимок уже строился: при
// старте rivetd они сверяют SHA основной ветки, чтобы не потерять
// обновление, отложенное до рестарта. Проекты без снимка не читаются.
func (s *Store) ProjectsWithArchitecture(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT project_id::text FROM project_architecture ORDER BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
