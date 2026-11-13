// Package store — доступ к PostgreSQL: единая система записи Rivet.
// Правило: смена статуса и событие фиксируются одной транзакцией;
// прямых UPDATE статусов в обход Transition* быть не должно.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

type Store struct {
	Pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	return NewNamed(ctx, databaseURL, "")
}

// NewNamed: то же с application_name у соединений пула (пусто: как в
// DSN). По нему в pg_stat_activity видно, кто держит блокировки.
func NewNamed(ctx context.Context, databaseURL, appName string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if appName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = appName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// ─── event log ───────────────────────────────────────────────────────────

type EventInput struct {
	ActorKind domain.ActorKind
	ActorID   string
	Type      string
	ProjectID string
	EpicID    string
	TaskID    string
	Text      string
	Payload   any
}

func appendEvent(ctx context.Context, tx pgx.Tx, e EventInput) (int64, error) {
	// Смена фазы цикла разработки — в самом событии (add-sdlc-cycle).
	if err := withPhase(ctx, tx, &e); err != nil {
		return 0, err
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO events (actor_kind, actor_id, type, project_id, epic_id, task_id, text, payload)
		VALUES ($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,$7,$8) RETURNING id`,
		e.ActorKind, e.ActorID, e.Type, e.ProjectID, e.EpicID, e.TaskID, e.Text, raw).Scan(&id)
	return id, err
}

// AppendEvents — несколько событий одной транзакцией (обе стороны
// пересечения работ: либо оба события, либо ни одного).
func (s *Store) AppendEvents(ctx context.Context, evs []EventInput) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, e := range evs {
			if _, err := appendEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// AppendEvent — событие вне перехода статуса (одиночная запись).
func (s *Store) AppendEvent(ctx context.Context, e EventInput) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		id, err = appendEvent(ctx, tx, e)
		return err
	})
	return id, err
}

type EventFilter struct {
	ProjectID string
	EpicID    string
	TaskID    string
	Type      string
	AfterID   int64
	Limit     int
	// Latest — без курсора отдать последние Limit событий (лента консоли),
	// а не первые по id. Порядок в ответе всё равно по id по возрастанию,
	// чтобы контракт курсора не менялся.
	Latest bool
	// ViewerID — обязательный для API фильтр слоя видимости: только события
	// проектов, где пользователь участник. Пустой ViewerID — системный вызов
	// (SSE-реплей после проверки членства, внутренние потребители).
	ViewerID string
	// Installation — лента аудита установки: только события без проекта
	// (спека observability «Лента аудита установки»). Фильтры проекта, Epic и
	// задачи при этом не применяются; право администратора проверяет API.
	Installation bool
	// GlobalTypes — типы событий установки (без проекта), которые
	// добавляются к ленте проекта: правки каталога ролей и имён runner'ов
	// нужны консоли в потоке проекта (add-agent-roles).
	GlobalTypes []string
	// GlobalAfterID — события GlobalTypes только с id больше этого: поток
	// проекта отдаёт события установки вживую, без реплея истории.
	GlobalAfterID int64
}

// Занятость агента команды (add-agent-roles): событие уровня установки, по
// которому консоль любого проекта перечитывает команду. Запуски шагов
// пишет триггер rivet_team_agent_work (миграция 0033), декомпозицию
// Тимлида пишет API. В payload только {"id", "busy"}: чужая задача, проект
// и Epic в событие не попадают.
const (
	EventAgentBusy = "team.agent_busy"
	EventAgentFree = "team.agent_free"
)

// AgentWorkEvent — событие занятости агента. Текст совпадает с текстом
// триггера.
func AgentWorkEvent(id, name string, busy bool) EventInput {
	typ, text := EventAgentFree, "агент команды «"+name+"» свободен"
	if busy {
		typ, text = EventAgentBusy, "агент команды «"+name+"» занят"
	}
	return EventInput{ActorKind: domain.ActorScheduler, Type: typ, Text: text,
		Payload: map[string]any{"id": id, "busy": busy}}
}

// RoleStreamTypes — события установки, которые идут и в потоки проектов.
// Из потока берутся только события без проекта, поэтому policy.activated
// здесь означает активацию политики установки (fix-web-console-tails): она
// действует во всех проектах, и консоль любого проекта перечитывает по ней
// действующую политику. Активация политики проекта идёт событием проекта.
var RoleStreamTypes = []string{"role.created", "role.updated", "role.deleted", "runner.renamed",
	"team.agent_created", "team.agent_updated", "team.agent_deleted", EventAgentBusy, EventAgentFree,
	"policy.activated"}

// auditFeedSQL — лента аудита без фильтра по типу. Занятость агентов
// служебная: пишется на каждую стадию и нужна потокам консоли, а не аудиту,
// с явным фильтром по типу она видна. Условие написано литералом и совпадает
// с предикатом частичного индекса events_audit (миграция 0033), иначе
// планировщик не возьмёт индекс и будет листать скрытые строки.
const auditFeedSQL = ` AND project_id IS NULL AND type NOT IN ('` + EventAgentBusy + `', '` + EventAgentFree + `')`

// LastEventID — id последнего события (0 — событий нет): с него поток
// проекта начинает события установки.
func (s *Store) LastEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM events`).Scan(&id)
	return id, err
}

func (s *Store) Events(ctx context.Context, f EventFilter) ([]domain.Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	if f.Installation {
		f.ProjectID, f.EpicID, f.TaskID = "", "", ""
	}
	// Обычная лента: project_id IN (...) ложен для NULL, поэтому события
	// установки в неё не попадают — это и нужно участникам.
	order := "ORDER BY id"
	if f.Latest && f.AfterID == 0 {
		order = "ORDER BY id DESC"
	}
	audit := ""
	if f.Installation && f.Type == "" {
		audit = auditFeedSQL
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT * FROM (
		SELECT id, ts, actor_kind, actor_id, type, COALESCE(project_id::text,''),
		       COALESCE(epic_id::text,''), COALESCE(task_id::text,''), text, payload
		FROM events
		WHERE ($1 = '' OR project_id = $1::uuid OR (project_id IS NULL AND type = ANY($9::text[]) AND id > $10))
		  AND ($2 = '' OR epic_id = $2::uuid)
		  AND ($3 = '' OR task_id = $3::uuid)
		  AND ($4 = '' OR type = $4)
		  AND id > $5
		  AND (CASE WHEN $8 THEN project_id IS NULL
		       ELSE ($7 = '' OR project_id IN (SELECT project_id FROM project_members WHERE user_id = $7::uuid)) END)
		`+audit+`
		`+order+`
		LIMIT $6) page
		ORDER BY id`,
		f.ProjectID, f.EpicID, f.TaskID, f.Type, f.AfterID, f.Limit, f.ViewerID, f.Installation, globalTypes(f.GlobalTypes),
		f.GlobalAfterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.ActorKind, &e.ActorID, &e.Type,
			&e.ProjectID, &e.EpicID, &e.TaskID, &e.Text, &payload); err != nil {
			return nil, err
		}
		// Payload аддитивен: пустой объект не тащим в JSON ответа.
		if len(payload) > 0 && string(payload) != "{}" {
			if err := json.Unmarshal(payload, &e.Payload); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── переходы статусов ───────────────────────────────────────────────────

// TransitionTask атомарно переводит задачу в новый статус и пишет событие.
// Недопустимый переход не выполняется, но фиксируется событием об ошибке.
// mutate (опционально) правит остальные поля задачи в той же транзакции.
func (s *Store) TransitionTask(ctx context.Context, taskID string, to domain.TaskStatus,
	ev EventInput, mutate func(tx pgx.Tx) error) error {
	return s.transitionTask(ctx, taskID, []domain.TaskStatus{to}, []EventInput{ev}, mutate)
}

// TransitionTaskVia — переход задачи через промежуточный статус одной
// транзакцией: оба шага проверяются по матрице статусов, событие пишется на
// каждый. Нужен, где прямого перехода нет (ready → failed идёт через
// review): сбой между шагами не оставит задачу в промежуточном статусе.
func (s *Store) TransitionTaskVia(ctx context.Context, taskID string, via, to domain.TaskStatus,
	viaEv, ev EventInput, mutate func(tx pgx.Tx) error) error {
	return s.transitionTask(ctx, taskID, []domain.TaskStatus{via, to}, []EventInput{viaEv, ev}, mutate)
}

func (s *Store) transitionTask(ctx context.Context, taskID string, path []domain.TaskStatus,
	evs []EventInput, mutate func(tx pgx.Tx) error) error {

	var denied *EventInput
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicID, projectID string
		err := tx.QueryRow(ctx, `
			SELECT t.status, t.epic_id, e.project_id
			FROM tasks t JOIN epics e ON e.id = t.epic_id
			WHERE t.id = $1 FOR UPDATE OF t`, taskID).Scan(&from, &epicID, &projectID)
		if err != nil {
			return fmt.Errorf("task %s: %w", taskID, err)
		}
		for i, to := range path {
			ev := &evs[i]
			ev.ProjectID, ev.EpicID, ev.TaskID = projectID, epicID, taskID
			if !from.CanTransition(to) {
				bad := domain.ErrBadTransition{Entity: "task", From: string(from), To: string(to)}
				// Событие об ошибке нельзя писать здесь: возврат ошибки откатит
				// транзакцию вместе с ним. Пишем после отката, отдельной записью.
				d := *ev
				d.Type = "task.transition_denied"
				d.Text = bad.Error()
				denied = &d
				return bad
			}
			from = to
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status = $2, updated_at = now() WHERE id = $1`, taskID, path[len(path)-1]); err != nil {
			return err
		}
		if mutate != nil {
			if err := mutate(tx); err != nil {
				return err
			}
		}
		for _, ev := range evs {
			if _, err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	if denied != nil {
		if _, evErr := s.AppendEvent(ctx, *denied); evErr != nil {
			return evErr
		}
	}
	return err
}

// TransitionEpic — то же для Epic.
func (s *Store) TransitionEpic(ctx context.Context, epicID string, to domain.EpicStatus, ev EventInput) error {
	var denied *EventInput
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if to == domain.EpicRunning {
			done, err := assistantMutationBeginTx(ctx, tx, "epic.resume", epicID)
			if err != nil || done {
				return err
			}
		}
		if to == domain.EpicArchived {
			if err := maintenanceArchiveTx(ctx, tx, epicID); err != nil {
				return err
			}
		}
		var from domain.EpicStatus
		var projectID, kind string
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id, kind FROM epics WHERE id = $1 FOR UPDATE`, epicID).
			Scan(&from, &projectID, &kind); err != nil {
			return err
		}
		ev.ProjectID, ev.EpicID = projectID, epicID
		if !from.CanTransition(to) {
			bad := domain.ErrBadTransition{Entity: "epic", From: string(from), To: string(to)}
			d := ev
			d.Type = "epic.transition_denied"
			d.Text = bad.Error()
			denied = &d
			return bad
		}
		if to == domain.EpicRunning {
			if err := requireArchitectureTx(ctx, tx, epicID, 0); err != nil {
				return err
			}
		}
		if to == domain.EpicRunning && kind == domain.KindWork {
			var hasTasks bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE epic_id=$1)`, epicID).Scan(&hasTasks); err != nil {
				return err
			}
			if !hasTasks {
				err := fmt.Errorf("%w: перед запуском Epic составьте план и добавьте хотя бы одну задачу", ErrConflict)
				d := ev
				d.Type, d.Text = "epic.transition_denied", err.Error()
				denied = &d
				return err
			}
		}
		if to == domain.EpicRunning && kind == domain.KindWork {
			if err := prepareLocalStandTx(ctx, tx, projectID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE epics SET status=$2 WHERE id=$1`, epicID, to); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, ev)
		if err != nil {
			return err
		}
		return assistantMutationDoneTx(ctx, tx, string(to))
	})
	if denied != nil {
		if _, evErr := s.AppendEvent(ctx, *denied); evErr != nil {
			return evErr
		}
	}
	return err
}

var ErrNotFound = errors.New("not found")

func nf(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// globalTypes — пустой список вместо nil для параметра запроса.
func globalTypes(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}
