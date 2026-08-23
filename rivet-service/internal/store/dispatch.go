package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Назначения стадий за пределами ready→running (AssignNext):
// fixing без runner'а (после review) и review отдельным runner'ом.

// AssignFixing назначает исполнителя задаче в fixing, оставшейся без runner'а
// (исполнитель был освобождён на время review). ok=false — нечего назначать.
func (s *Store) AssignFixing(ctx context.Context, excludedProjects []string) (Assignment, bool, error) {
	return s.assignStage(ctx, excludedProjects, `
		SELECT t.id, t.num, t.epic_id, e.project_id, r.id
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
			AND e.project_id <> ALL($1::uuid[])
		JOIN LATERAL (
			SELECT r.id FROM runners r
			WHERE r.status = 'idle' AND NOT r.draining AND r.capabilities @> t.capabilities
			-- беречь специалистов: из подходящих берём наименее «богатого» по capabilities
			ORDER BY cardinality(r.capabilities), r.last_seen DESC
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		) r ON true
		WHERE t.status = 'fixing' AND t.runner_id IS NULL
		ORDER BY t.num
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1`,
		`UPDATE tasks SET runner_id=$2, updated_at=now() WHERE id=$1`,
		`UPDATE runners SET status='running', task_id=$2, ctx_pct=NULL WHERE id=$1`,
		"назначен исполнитель для исправлений: ")
}

// AssignTesting назначает исполнителя задаче в testing, оставшейся без
// runner'а (пауза Epic остановила конвейер на границе coding → testing).
// Ветка задачи содержит всю работу, поэтому проверки может гнать и другой runner.
func (s *Store) AssignTesting(ctx context.Context, excludedProjects []string) (Assignment, bool, error) {
	return s.assignStage(ctx, excludedProjects, `
		SELECT t.id, t.num, t.epic_id, e.project_id, r.id
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
			AND e.project_id <> ALL($1::uuid[])
		JOIN LATERAL (
			SELECT r.id FROM runners r
			WHERE r.status = 'idle' AND NOT r.draining AND r.capabilities @> t.capabilities
			-- беречь специалистов: из подходящих берём наименее «богатого» по capabilities
			ORDER BY cardinality(r.capabilities), r.last_seen DESC
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		) r ON true
		WHERE t.status = 'testing' AND t.runner_id IS NULL
		ORDER BY t.num
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1`,
		`UPDATE tasks SET runner_id=$2, updated_at=now() WHERE id=$1`,
		`UPDATE runners SET status='testing', task_id=$2, ctx_pct=NULL WHERE id=$1`,
		"назначен исполнитель для проверок: ")
}

// AssignReview назначает ревьюера (runner с capability review, отличный от
// исполнителя) задаче в review без ревьюера.
func (s *Store) AssignReview(ctx context.Context, excludedProjects []string) (Assignment, bool, error) {
	return s.assignStage(ctx, excludedProjects, `
		SELECT t.id, t.num, t.epic_id, e.project_id, r.id
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
			AND e.project_id <> ALL($1::uuid[])
		JOIN LATERAL (
			SELECT r.id FROM runners r
			WHERE r.status = 'idle' AND NOT r.draining
			  AND r.capabilities @> ARRAY['review']
			  AND r.id IS DISTINCT FROM t.runner_id
			ORDER BY r.last_seen DESC
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		) r ON true
		WHERE t.status = 'review' AND t.reviewer_id IS NULL
		ORDER BY t.num
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1`,
		`UPDATE tasks SET reviewer_id=$2, updated_at=now() WHERE id=$1`,
		`UPDATE runners SET status='review', task_id=$2, ctx_pct=NULL WHERE id=$1`,
		"назначен ревьюер: ")
}

// assignStage — общий примитив назначений; excludedProjects — проекты на
// паузе по бюджету (пустой список — без исключений).
func (s *Store) assignStage(ctx context.Context, excludedProjects []string, selectSQL, taskSQL, runnerSQL, eventText string) (Assignment, bool, error) {
	var a Assignment
	assigned := false
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var taskID, epicID, projectID, runnerID string
		var num int64
		err := tx.QueryRow(ctx, selectSQL, excludedProjects).Scan(&taskID, &num, &epicID, &projectID, &runnerID)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		if _, err := tx.Exec(ctx, taskSQL, taskID, runnerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, runnerSQL, runnerID, taskID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.assign",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text: eventText + runnerID, Payload: map[string]any{"runner": runnerID},
		}); err != nil {
			return err
		}
		a.Task.ID, a.Task.Num, a.Task.EpicID = taskID, num, epicID
		a.Runner.ID = runnerID
		assigned = true
		return nil
	})
	if err != nil || !assigned {
		return a, false, err
	}
	t, err := s.GetTask(ctx, a.Task.ID)
	if err != nil {
		return a, true, err
	}
	a.Task = t
	return a, true, nil
}

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
