package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Правка плана Epic человеком (change add-plan-editing, спека
// epic-decomposition «Правка плана человеком»).

// PlanEdit — правка задачи: применяются только ненулевые поля; Criteria и
// Deps заменяются целиком. AttemptLimit применим к любой нетерминальной
// задаче, остальные поля — к не начатой (queued/ready).
type PlanEdit struct {
	Title        *string
	Description  *string
	Criteria     *[]string
	Deps         *[]string
	AttemptLimit *int
}

// Fields — имена переданных полей (валидация и payload события).
func (e PlanEdit) Fields() []string {
	var out []string
	if e.Title != nil {
		out = append(out, "title")
	}
	if e.Description != nil {
		out = append(out, "description")
	}
	if e.Criteria != nil {
		out = append(out, "criteria")
	}
	if e.Deps != nil {
		out = append(out, "deps")
	}
	return out
}

// planFields — есть ли поля, требующие статуса queued/ready.
func (e PlanEdit) planFields() bool { return len(e.Fields()) > 0 }

// UpdateTaskPlan правит поля и зависимости не начатой задачи (queued/ready)
// одной транзакцией: карта DAG Epic блокируется FOR UPDATE, ацикличность
// проверяется по полному графу с заменёнными рёбрами (гонка параллельной
// правки закрыта блокировкой). Criteria заменяются с ok=false: отметки
// относились к старым формулировкам. После правки зависимостей при
// работающем Epic вызывающий пересчитывает готовность (RecomputeEpic).
// Всё применяется одной транзакцией: частично применённая правка (лимит
// прошёл, план отклонён) недопустима. Строка Epic блокируется первой —
// правки плана одного Epic сериализуются (двум транзакциям, лочащим
// «своя задача → все задачи Epic», иначе грозил бы deadlock), а гонка с
// запуском/архивацией Epic исключается.
func (s *Store) UpdateTaskPlan(ctx context.Context, taskID string, edit PlanEdit, login string) (epicID string, err error) {
	if len(edit.Fields()) == 0 && edit.AttemptLimit == nil {
		return "", fmt.Errorf("%w: нет полей для правки", ErrInvalid)
	}
	if edit.Title != nil && *edit.Title == "" {
		return "", fmt.Errorf("%w: название задачи не может быть пустым", ErrInvalid)
	}
	if edit.AttemptLimit != nil && *edit.AttemptLimit < 1 {
		return "", fmt.Errorf("%w: лимит попыток должен быть не меньше 1", ErrInvalid)
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicStatus domain.EpicStatus
		var projectID string
		var used int
		// Порядок локов фиксированный: сначала строка Epic, затем задача,
		// затем (при правке рёбер) остальные задачи. Один порядок у всех
		// писателей плана — deadlock невозможен; epic_id задачи неизменен,
		// его чтение без лока безопасно.
		if err := tx.QueryRow(ctx,
			`SELECT epic_id FROM tasks WHERE id=$1`, taskID).Scan(&epicID); err != nil {
			return nf(err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id FROM epics WHERE id=$1 FOR UPDATE`, epicID).
			Scan(&epicStatus, &projectID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT status, attempt_used FROM tasks WHERE id=$1 FOR UPDATE`, taskID).
			Scan(&from, &used); err != nil {
			return nf(err)
		}
		if edit.AttemptLimit != nil && *edit.AttemptLimit < used {
			return fmt.Errorf("%w: лимит %d меньше израсходованных попыток (%d)", ErrInvalid, *edit.AttemptLimit, used)
		}
		if edit.planFields() {
			// Правка плана — только у не начатой задачи живого Epic:
			// done/archived неизменяемы.
			if epicStatus == domain.EpicDone || epicStatus == domain.EpicArchived {
				return fmt.Errorf("%w: Epic в статусе %s не редактируется", ErrConflict, epicStatus)
			}
			if from != domain.TaskQueued && from != domain.TaskReady {
				return domain.ErrBadTransition{Entity: "task", From: string(from), To: "plan-edit"}
			}
		}
		if edit.Deps != nil {
			// Блокировка задач Epic (FOR UPDATE несовместим с GROUP BY —
			// сначала лочим id, затем читаем рёбра): параллельная правка
			// рёбер не создаст цикл между двумя валидациями.
			rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE epic_id=$1 FOR UPDATE`, epicID)
			if err != nil {
				return err
			}
			ids, err := collectIDs(rows)
			if err != nil {
				return err
			}
			graph := make(map[string][]string, len(ids))
			for _, id := range ids {
				graph[id] = nil
			}
			rows, err = tx.Query(ctx, `
				SELECT d.task_id::text, d.dep_id::text FROM task_deps d
				JOIN tasks t ON t.id = d.task_id WHERE t.epic_id = $1`, epicID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var from, to string
				if err := rows.Scan(&from, &to); err != nil {
					rows.Close()
					return err
				}
				graph[from] = append(graph[from], to)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, dep := range *edit.Deps {
				if dep == taskID {
					return fmt.Errorf("%w: задача не может зависеть от себя", ErrInvalid)
				}
				if _, ok := graph[dep]; !ok {
					return fmt.Errorf("%w: зависимость %s вне Epic", ErrInvalid, dep)
				}
			}
			graph[taskID] = *edit.Deps
			if err := ValidateDAG(graph); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM task_deps WHERE task_id=$1`, taskID); err != nil {
				return err
			}
			for _, dep := range *edit.Deps {
				if _, err := tx.Exec(ctx,
					`INSERT INTO task_deps (task_id, dep_id) VALUES ($1,$2)`, taskID, dep); err != nil {
					return err
				}
			}
			// ready → queued здесь же: RecomputeEpic понижение не делает
			// (спека: задача с новой невыполненной зависимостью не назначается).
			if from == domain.TaskReady {
				var unmet int
				if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM task_deps d JOIN tasks dt ON dt.id = d.dep_id
					WHERE d.task_id = $1 AND dt.status NOT IN ('done','cancelled')`, taskID).Scan(&unmet); err != nil {
					return err
				}
				if unmet > 0 {
					if _, err := tx.Exec(ctx,
						`UPDATE tasks SET status='queued', updated_at=now() WHERE id=$1`, taskID); err != nil {
						return err
					}
					// Смена статуса видна timeline и SSE как обычно.
					if _, err := appendEvent(ctx, tx, EventInput{
						ActorKind: domain.ActorUser, ActorID: login, Type: "task.status",
						ProjectID: projectID, EpicID: epicID, TaskID: taskID,
						Text:    "новая зависимость не выполнена — задача возвращена в планирование",
						Payload: map[string]any{"status": "queued"},
					}); err != nil {
						return err
					}
				}
			}
		}
		if edit.Title != nil {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET title=$2, updated_at=now() WHERE id=$1`, taskID, *edit.Title); err != nil {
				return err
			}
		}
		if edit.Description != nil {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET description=$2, updated_at=now() WHERE id=$1`, taskID, *edit.Description); err != nil {
				return err
			}
		}
		if edit.AttemptLimit != nil {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET attempt_limit=$2, updated_at=now() WHERE id=$1`, taskID, *edit.AttemptLimit); err != nil {
				return err
			}
		}
		if edit.Criteria != nil {
			crit := make([]domain.Criterion, 0, len(*edit.Criteria))
			for _, c := range *edit.Criteria {
				crit = append(crit, domain.Criterion{Text: c})
			}
			raw, err := json.Marshal(crit)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET criteria=$2, updated_at=now() WHERE id=$1`, taskID, raw); err != nil {
				return err
			}
		}
		if edit.planFields() {
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: login, Type: "task.plan_edited",
				ProjectID: projectID, EpicID: epicID, TaskID: taskID,
				Text:    "план задачи изменён участником " + login,
				Payload: map[string]any{"fields": edit.Fields()},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return epicID, err
}

// DeletePlannedTask физически удаляет чистую задачу чернового плана: Epic в
// planned, задача queued, истории нет (FK событий/сессий — страж: их
// наличие превращается в ErrConflict с подсказкой отменить задачу).
// Рёбра зависимостей снимаются каскадно (task_deps ON DELETE CASCADE).
func (s *Store) DeletePlannedTask(ctx context.Context, taskID, login string) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status domain.TaskStatus
		var epicStatus domain.EpicStatus
		var epicID, projectID string
		var num int64
		// Порядок локов как в UpdateTaskPlan: Epic первым — параллельный
		// запуск Epic не проскочит между проверкой planned и удалением.
		if err := tx.QueryRow(ctx,
			`SELECT epic_id FROM tasks WHERE id=$1`, taskID).Scan(&epicID); err != nil {
			return nf(err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id FROM epics WHERE id=$1 FOR UPDATE`, epicID).
			Scan(&epicStatus, &projectID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT status, num FROM tasks WHERE id=$1 FOR UPDATE`, taskID).
			Scan(&status, &num); err != nil {
			return nf(err)
		}
		if epicStatus != domain.EpicPlanned || status != domain.TaskQueued {
			return fmt.Errorf("%w: удаление доступно для queued-задачи Epic в статусе planned; иначе отмените задачу", ErrConflict)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, taskID); err != nil {
			return err
		}
		// Задачи больше нет — событие пишется на уровень Epic.
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: login, Type: "epic.plan_edited",
			ProjectID: projectID, EpicID: epicID,
			Text:    fmt.Sprintf("задача task-%d удалена из плана участником %s", num, login),
			Payload: map[string]any{"task_num": num, "action": "deleted"},
		})
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return fmt.Errorf("%w: у задачи уже есть история (события или сессии) — отмените её вместо удаления", ErrConflict)
	}
	return err
}
