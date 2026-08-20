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
	pool, err := pgxpool.New(ctx, databaseURL)
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
	// ViewerID — обязательный для API фильтр слоя видимости: только события
	// проектов, где пользователь участник. Пустой ViewerID — системный вызов
	// (SSE-реплей после проверки членства, внутренние потребители).
	ViewerID string
	// Installation — лента аудита установки: только события без проекта
	// (спека observability «Лента аудита установки»). Фильтры проекта, Epic и
	// задачи при этом не применяются; право администратора проверяет API.
	Installation bool
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
	rows, err := s.Pool.Query(ctx, `
		SELECT id, ts, actor_kind, actor_id, type, COALESCE(project_id::text,''),
		       COALESCE(epic_id::text,''), COALESCE(task_id::text,''), text, payload
		FROM events
		WHERE ($1 = '' OR project_id = $1::uuid)
		  AND ($2 = '' OR epic_id = $2::uuid)
		  AND ($3 = '' OR task_id = $3::uuid)
		  AND ($4 = '' OR type = $4)
		  AND id > $5
		  AND (CASE WHEN $8 THEN project_id IS NULL
		       ELSE ($7 = '' OR project_id IN (SELECT project_id FROM project_members WHERE user_id = $7::uuid)) END)
		ORDER BY id
		LIMIT $6`,
		f.ProjectID, f.EpicID, f.TaskID, f.Type, f.AfterID, f.Limit, f.ViewerID, f.Installation)
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
		ev.ProjectID, ev.EpicID, ev.TaskID = projectID, epicID, taskID

		if !from.CanTransition(to) {
			bad := domain.ErrBadTransition{Entity: "task", From: string(from), To: string(to)}
			// Событие об ошибке нельзя писать здесь: возврат ошибки откатит
			// транзакцию вместе с ним. Пишем после отката, отдельной записью.
			d := ev
			d.Type = "task.transition_denied"
			d.Text = bad.Error()
			denied = &d
			return bad
		}

		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status = $2, updated_at = now() WHERE id = $1`, taskID, to); err != nil {
			return err
		}
		if mutate != nil {
			if err := mutate(tx); err != nil {
				return err
			}
		}
		_, err = appendEvent(ctx, tx, ev)
		return err
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
		var from domain.EpicStatus
		var projectID string
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id FROM epics WHERE id = $1 FOR UPDATE`, epicID).
			Scan(&from, &projectID); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE epics SET status=$2 WHERE id=$1`, epicID, to); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, ev)
		return err
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
