package store

import (
	"context"
	"slices"
	"time"
)

// StepTransition: переход задачи по шагу из события task.step, то есть шаг,
// исход, заявленная цель и время (граф процесса в деталке,
// fix-web-console-tails).
type StepTransition struct {
	Step    string    `json:"step"`
	Outcome string    `json:"outcome"`
	Next    string    `json:"next"`
	TS      time.Time `json:"ts"`
}

// TaskStepHistory отдаёт последние limit переходов задачи по шагам в порядке
// времени. Повтор задачи сбрасывает счётчики попыток, поэтому общее число
// переходов не ограничено, и при переполнении важнее хвост: текущий проход.
// Условие type = 'task.step' написано литералом и совпадает с предикатом
// частичного индекса events_task_step (миграция 0044): session.step задачи
// запрос не читает.
func (s *Store) TaskStepHistory(ctx context.Context, taskID string, limit int) ([]StepTransition, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ts, COALESCE(payload->>'step', ''), COALESCE(payload->>'outcome', ''), COALESCE(payload->>'next', '')
		FROM events
		WHERE task_id = $1::uuid AND type = 'task.step'
		ORDER BY id DESC
		LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StepTransition{}
	for rows.Next() {
		var t StepTransition
		if err := rows.Scan(&t.TS, &t.Step, &t.Outcome, &t.Next); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}
