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

// ─── прозрачность затрат (change add-cost-transparency) ──────────────────

// CostEstimate — оценка стоимости плана Epic: диапазон p25–p75 удельного
// расхода истории, взвешенный суммой оценок задач (спека monetization
// «Прозрачность затрат до запуска»).
type CostEstimate struct {
	Available   bool     `json:"available"`
	Reason      string   `json:"reason,omitempty"`
	TokensMin   int64    `json:"tokens_min,omitempty"`
	TokensMax   int64    `json:"tokens_max,omitempty"`
	CostMin     *float64 `json:"cost_min,omitempty"`
	CostMax     *float64 `json:"cost_max,omitempty"`
	BasedOn     string   `json:"based_on,omitempty"` // project | installation
	SampleTasks int      `json:"sample_tasks,omitempty"`
}

// estimateSource — перцентили удельного расхода (на единицу оценки) по
// done-задачам источника; scope пустой — вся установка.
func (s *Store) estimateSource(ctx context.Context, projectID string) (p25, p75 float64, c25, c75 *float64, n, nCost int, err error) {
	err = s.Pool.QueryRow(ctx, `
		WITH per_task AS (
			SELECT t.id, t.estimate,
			       SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0)) AS tokens,
			       SUM(u.cost_usd)::float8 AS cost
			FROM tasks t
			JOIN epics e ON e.id = t.epic_id
			JOIN usage_records u ON u.task_id = t.id
			WHERE t.status = 'done'
			  AND ($1 = '' OR e.project_id = NULLIF($1,'')::uuid)
			  AND (u.tokens_in IS NOT NULL OR u.tokens_out IS NOT NULL)
			GROUP BY t.id, t.estimate
			HAVING SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0)) > 0
		)
		SELECT COALESCE(percentile_cont(0.25) WITHIN GROUP (ORDER BY tokens::float8/estimate), 0),
		       COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY tokens::float8/estimate), 0),
		       percentile_cont(0.25) WITHIN GROUP (ORDER BY cost/estimate) FILTER (WHERE cost IS NOT NULL),
		       percentile_cont(0.75) WITHIN GROUP (ORDER BY cost/estimate) FILTER (WHERE cost IS NOT NULL),
		       count(*),
		       count(*) FILTER (WHERE cost IS NOT NULL)
		FROM per_task`, projectID).Scan(&p25, &p75, &c25, &c75, &n, &nCost)
	return
}

// minEstimateSample — минимум завершённых задач в источнике оценки: меньше —
// диапазон случаен, источник не используется.
const minEstimateSample = 3

// EpicCostEstimate — оценка плана Epic: история проекта, при недостатке —
// установки; при пустой истории Available=false с причиной, не нули.
func (s *Store) EpicCostEstimate(ctx context.Context, epicID string) (CostEstimate, error) {
	var projectID string
	var totalEstimate int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT e.project_id::text,
		       COALESCE((SELECT SUM(estimate) FROM tasks t
		                 WHERE t.epic_id = e.id AND t.status <> 'cancelled'), 0)
		FROM epics e WHERE e.id = $1`, epicID).Scan(&projectID, &totalEstimate); err != nil {
		return CostEstimate{}, nf(err)
	}
	if totalEstimate == 0 {
		return CostEstimate{Available: false, Reason: "в плане нет задач"}, nil
	}
	basedOn := "project"
	p25, p75, c25, c75, n, nCost, err := s.estimateSource(ctx, projectID)
	if err != nil {
		return CostEstimate{}, err
	}
	if n < minEstimateSample {
		basedOn = "installation"
		if p25, p75, c25, c75, n, nCost, err = s.estimateSource(ctx, ""); err != nil {
			return CostEstimate{}, err
		}
	}
	if n < minEstimateSample {
		return CostEstimate{Available: false,
			Reason: "нет истории: оценка появится после первых завершённых задач с учтёнными токенами"}, nil
	}
	est := CostEstimate{
		Available: true, BasedOn: basedOn, SampleTasks: n,
		TokensMin: int64(p25 * float64(totalEstimate)),
		TokensMax: int64(p75 * float64(totalEstimate)),
	}
	// Деньги — только при достаточной истории со стоимостью в ТОМ ЖЕ
	// источнике, что и токены: смешение источников дало бы несогласованные
	// диапазоны. Частичные данные не показываются.
	if nCost >= minEstimateSample && c25 != nil && c75 != nil {
		lo, hi := *c25*float64(totalEstimate), *c75*float64(totalEstimate)
		est.CostMin, est.CostMax = &lo, &hi
	}
	return est, nil
}

// SetEpicBudget — бюджет Epic в токенах (nil снимает); меняет владелец
// проекта (проверяет API), возобновление назначений — следующим проходом
// планировщика.
func (s *Store) SetEpicBudget(ctx context.Context, epicID string, budget *int64) (domain.Epic, error) {
	if budget != nil && *budget < 1 {
		return domain.Epic{}, fmt.Errorf("%w: бюджет должен быть не меньше 1 токена", ErrInvalid)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE epics SET token_budget=$2 WHERE id=$1`, epicID, budget)
	if err != nil {
		return domain.Epic{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.Epic{}, ErrNotFound
	}
	return s.GetEpic(ctx, epicID)
}

// EpicTokensUsed — учтённые токены задач Epic (NULL не считается нулём).
func (s *Store) EpicTokensUsed(ctx context.Context, epicID string) (int64, error) {
	var used int64
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(COALESCE(tokens_in,0)+COALESCE(tokens_out,0)), 0)
		FROM usage_records
		WHERE epic_id = $1 AND (tokens_in IS NOT NULL OR tokens_out IS NOT NULL)`, epicID).Scan(&used)
	return used, err
}

// ExceededEpicBudget — превышение бюджета работающего Epic (для Tick).
type ExceededEpicBudget struct {
	EpicID    string
	ProjectID string
	Budget    int64
	Used      int64
}

// ExceededEpicBudgets — работающие Epic с бюджетом, чей учтённый расход
// достиг бюджета: планировщик исключает их из назначений (граница стадии).
func (s *Store) ExceededEpicBudgets(ctx context.Context) ([]ExceededEpicBudget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id::text, e.project_id::text, e.token_budget,
		       COALESCE(SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0))
		                FILTER (WHERE u.tokens_in IS NOT NULL OR u.tokens_out IS NOT NULL), 0) AS used
		FROM epics e
		LEFT JOIN usage_records u ON u.epic_id = e.id
		WHERE e.status = 'running' AND e.token_budget IS NOT NULL
		GROUP BY e.id
		HAVING COALESCE(SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0))
		                FILTER (WHERE u.tokens_in IS NOT NULL OR u.tokens_out IS NOT NULL), 0) >= e.token_budget`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExceededEpicBudget
	for rows.Next() {
		var e ExceededEpicBudget
		if err := rows.Scan(&e.EpicID, &e.ProjectID, &e.Budget, &e.Used); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
