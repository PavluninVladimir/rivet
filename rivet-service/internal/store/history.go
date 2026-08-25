package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/history"
)

// Импорт истории проекта (спека domain-model «Импорт истории проекта»):
// единственное место, где даты задаются снаружи. Epic'и и задачи создаются
// сразу в терминальных статусах, поэтому планировщик их не видит, а
// события пишутся с исходными датами, чтобы лента читалась как история.

// ImportResult — что сделал импорт.
type ImportResult struct {
	EpicsCreated int `json:"epics_created"`
	EpicsUpdated int `json:"epics_updated"`
	TasksCreated int `json:"tasks_created"`
	TasksUpdated int `json:"tasks_updated"`
}

// ImportHistory применяет манифест к проекту одной транзакцией: Epic'и
// узнаются по ключу источника, задачи — по порядковому номеру в Epic'е;
// повторный импорт обновляет, а не дублирует.
func (s *Store) ImportHistory(ctx context.Context, projectID string, m history.Manifest, actor string) (ImportResult, error) {
	m = m.Normalize()
	if err := m.Validate(); err != nil {
		return ImportResult{}, err
	}
	var res ImportResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, e := range m.Epics {
			epicID, created, err := upsertHistoryEpic(ctx, tx, projectID, e)
			if err != nil {
				return err
			}
			if created {
				res.EpicsCreated++
			} else {
				res.EpicsUpdated++
			}
			for ord, t := range e.Tasks {
				taskCreated, err := upsertHistoryTask(ctx, tx, epicID, ord+1, e, t)
				if err != nil {
					return err
				}
				if taskCreated {
					res.TasksCreated++
				} else {
					res.TasksUpdated++
				}
			}
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actor, Type: "history.imported", ProjectID: projectID,
			Text: fmt.Sprintf("импортирована история: %d Epic'ов, %d задач (%s)",
				res.EpicsCreated+res.EpicsUpdated, res.TasksCreated+res.TasksUpdated, m.Source),
			Payload: map[string]any{"source": m.Source, "epics": res.EpicsCreated + res.EpicsUpdated,
				"tasks": res.TasksCreated + res.TasksUpdated, "created": res.EpicsCreated, "updated": res.EpicsUpdated},
		})
		return err
	})
	return res, err
}

// upsertHistoryEpic создаёт или обновляет Epic по ключу источника.
// Событие завершения пишется только при создании: повторный импорт не
// должен плодить события в ленте.
func upsertHistoryEpic(ctx context.Context, tx pgx.Tx, projectID string, e history.Epic) (id string, created bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO epics (project_id, title, goal, status, source_key, created_at)
		VALUES ($1, $2, $3, 'done', $4, $5)
		ON CONFLICT (project_id, source_key) WHERE source_key IS NOT NULL
		DO UPDATE SET title=EXCLUDED.title, goal=EXCLUDED.goal, created_at=EXCLUDED.created_at
		RETURNING id, (xmax = 0)`, projectID, e.Title, e.Goal, e.Key, e.CreatedAt).Scan(&id, &created)
	if err != nil {
		return "", false, err
	}
	if created {
		if err := appendHistoryEvent(ctx, tx, e.DoneAt, EventInput{
			ActorKind: domain.ActorSystem, Type: "epic.status", ProjectID: projectID, EpicID: id,
			Text:    "Epic выполнен (история)",
			Payload: map[string]any{"status": "done", "imported": true, "source_key": e.Key},
		}); err != nil {
			return "", false, err
		}
	}
	return id, created, nil
}

// upsertHistoryTask создаёт или обновляет задачу по номеру в Epic'е.
// Выполненная — done, невыполненная на момент архивации — cancelled с
// пометкой; PR — ссылкой репозитория секции.
func upsertHistoryTask(ctx context.Context, tx pgx.Tx, epicID string, ord int, e history.Epic, t history.Task) (bool, error) {
	status, desc := domain.TaskDone, ""
	if !t.Done {
		status, desc = domain.TaskCancelled, "не выполнено на момент архивации"
	}
	criteria, _ := json.Marshal([]domain.Criterion{})
	var id string
	var created bool
	err := tx.QueryRow(ctx, `
		INSERT INTO tasks (epic_id, source_ord, title, description, status, criteria, pr_url,
		                   attempt_used, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7,''), 0, $8, $9)
		ON CONFLICT (epic_id, source_ord) WHERE source_ord IS NOT NULL
		DO UPDATE SET title=EXCLUDED.title, description=EXCLUDED.description, status=EXCLUDED.status,
			pr_url=EXCLUDED.pr_url, created_at=EXCLUDED.created_at, updated_at=EXCLUDED.updated_at
		RETURNING id, (xmax = 0)`,
		epicID, ord, t.Title, desc, string(status), criteria, t.PRURL, e.CreatedAt, e.DoneAt).Scan(&id, &created)
	if err != nil {
		return false, err
	}
	if !created {
		return false, nil
	}
	var projectID string
	if err := tx.QueryRow(ctx, `SELECT project_id FROM epics WHERE id=$1`, epicID).Scan(&projectID); err != nil {
		return false, err
	}
	text := "задача выполнена (история)"
	if !t.Done {
		text = "задача не выполнена на момент архивации (история)"
	}
	payload := map[string]any{"status": string(status), "imported": true}
	if t.PRURL != "" {
		payload["pr"] = t.PRURL
	}
	return true, appendHistoryEvent(ctx, tx, e.DoneAt, EventInput{
		ActorKind: domain.ActorSystem, Type: "task.status", ProjectID: projectID, EpicID: epicID, TaskID: id,
		Text: text, Payload: payload,
	})
}

// appendHistoryEvent — событие с датой из истории, а не now(): лента и
// timeline должны читаться как прошлое, а не как вспышка в момент импорта.
func appendHistoryEvent(ctx context.Context, tx pgx.Tx, ts time.Time, e EventInput) error {
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO events (ts, actor_kind, actor_id, type, project_id, epic_id, task_id, text, payload)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6,'')::uuid, NULLIF($7,'')::uuid, $8, $9)`,
		ts, string(e.ActorKind), e.ActorID, e.Type, e.ProjectID, e.EpicID, e.TaskID, e.Text, raw)
	return err
}
