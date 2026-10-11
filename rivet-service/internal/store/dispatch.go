package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Служебные операции над runner'ами и задачами вокруг назначений
// (сами назначения — AssignRun в process.go).

// ReleaseTaskRunner освобождает runner задачи и снимает привязку задачи к нему
// (пауза Epic на границе стадии; задачу без runner'а подхватят Assign*-циклы).
func (s *Store) ReleaseTaskRunner(ctx context.Context, taskID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := freeRunner(ctx, tx, taskID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tasks SET runner_id=NULL, updated_at=now() WHERE id=$1`, taskID)
		return err
	})
}

// FreeReviewerRunner освобождает runner-ревьюера, сохраняя reviewer_id на
// задаче как признак пройденного review (защита от повторного назначения).
func (s *Store) FreeReviewerRunner(ctx context.Context, taskID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE runners SET status='idle', task_id=NULL
		WHERE id = (SELECT reviewer_id FROM tasks WHERE id=$1)`, taskID)
	return err
}

// SetTaskPR фиксирует созданный PR.
func (s *Store) SetTaskPR(ctx context.Context, taskID, prURL string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET pr_url=$2, updated_at=now() WHERE id=$1`, taskID, prURL)
	return err
}

// TaskByBranch находит задачу по ветке (webhook'и SCM).
func (s *Store) TaskByBranch(ctx context.Context, branch string) (domain.Task, error) {
	t, err := scanTask(s.Pool.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks t WHERE t.branch=$1`, branch))
	if err != nil {
		return t, nf(err)
	}
	t.Deps, err = s.taskDeps(ctx, t.ID)
	return t, err
}

// TaskRefs — projectID и epicID задачи (для атрибуции событий).
func (s *Store) TaskRefs(ctx context.Context, taskID string) (projectID, epicID string, err error) {
	err = nf(s.Pool.QueryRow(ctx, `
		SELECT e.project_id, t.epic_id FROM tasks t
		JOIN epics e ON e.id=t.epic_id WHERE t.id=$1`, taskID).Scan(&projectID, &epicID))
	return
}

// RunningEpics — id всех выполняющихся Epic'ов (для цикла пересчёта).
func (s *Store) RunningEpics(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM epics WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}
