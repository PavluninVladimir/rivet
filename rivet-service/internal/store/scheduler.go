package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Транзакционные примитивы планировщика. Политика (кто когда вызывается) —
// в internal/orchestrator; здесь только атомарные операции по спекам
// backend/orchestration и backend/human-escalation.

// ValidateDAG проверяет ацикличность (алгоритм Кана). deps: id → зависимости.
func ValidateDAG(deps map[string][]string) error {
	indeg := map[string]int{}
	adj := map[string][]string{}
	for id := range deps {
		indeg[id] += 0
	}
	for id, ds := range deps {
		seen := map[string]bool{}
		for _, d := range ds {
			if _, ok := deps[d]; !ok {
				return fmt.Errorf("зависимость %s задачи %s вне плана", d, id)
			}
			// Дубль ребра упал бы на первичном ключе task_deps при создании задачи.
			if seen[d] {
				return fmt.Errorf("дубль зависимости %s у задачи %s", d, id)
			}
			seen[d] = true
			adj[d] = append(adj[d], id)
			indeg[id]++
		}
	}
	queue := []string{}
	for id, n := range indeg {
		if n == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, next := range adj[id] {
			if indeg[next]--; indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if seen != len(deps) {
		return fmt.Errorf("план содержит цикл")
	}
	return nil
}

// RecomputeEpic пересчитывает готовность задач Epic:
//   - blocked каскадом → queued, когда первопричина больше не failed;
//   - queued → ready, когда все зависимости выполнены (done или cancelled:
//     отменённая задача исключена из DAG);
//   - queued/ready → blocked каскадом (транзитивно), когда в предках failed;
//     эскалации на потомков не создаются — эскалирована первопричина;
//   - все задачи терминальны → Epic done.
func (s *Store) RecomputeEpic(ctx context.Context, epicID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID string
		var epicStatus domain.EpicStatus
		if err := tx.QueryRow(ctx,
			`SELECT project_id, status FROM epics WHERE id=$1 FOR UPDATE`, epicID).
			Scan(&projectID, &epicStatus); err != nil {
			return err
		}
		if epicStatus != domain.EpicRunning {
			return nil // пауза/план: ничего не двигаем (спека: пауза останавливает планирование)
		}

		// Актуальное множество каскада: транзитивные потомки failed-задач.
		// Отменённые задачи исключены из DAG, поэтому обход через них не идёт.
		// Первопричина при нескольких — с меньшим номером (детерминированно).
		rows, err := tx.Query(ctx, `
			WITH RECURSIVE affected AS (
				SELECT d.task_id AS id, d.dep_id AS root_id
				FROM task_deps d
				JOIN tasks f ON f.id = d.dep_id AND f.epic_id=$1 AND f.status='failed'
				UNION
				SELECT d.task_id, a.root_id
				FROM task_deps d
				JOIN affected a ON d.dep_id = a.id
				JOIN tasks mid ON mid.id = a.id AND mid.status <> 'cancelled'
			), picked AS (
				SELECT a.id, min(r.num) AS root_num
				FROM affected a JOIN tasks r ON r.id = a.root_id
				GROUP BY a.id
			)
			SELECT p.id, root.id, root.num
			FROM picked p JOIN tasks root ON root.num = p.root_num`, epicID)
		if err != nil {
			return err
		}
		type cascade struct {
			id, rootID string
			rootNum    int64
		}
		var affected []cascade
		affectedIDs := []string{}
		for rows.Next() {
			var c cascade
			if err := rows.Scan(&c.id, &c.rootID, &c.rootNum); err != nil {
				rows.Close()
				return err
			}
			affected = append(affected, c)
			affectedIDs = append(affectedIDs, c.id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// каскад снят: задача больше не потомок failed-первопричины
		// (решение человека по первопричине или отмена промежуточного звена).
		rows, err = tx.Query(ctx, `
			SELECT t.id FROM tasks t
			WHERE t.epic_id=$1 AND t.status='blocked' AND t.blocked_by IS NOT NULL
			  AND NOT (t.id = ANY($2))
			FOR UPDATE OF t`, epicID, affectedIDs)
		if err != nil {
			return err
		}
		unblockedIDs, err := collectIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range unblockedIDs {
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status='queued', blocked_by=NULL, block_reason=NULL,
					updated_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.status",
				ProjectID: projectID, EpicID: epicID, TaskID: id,
				Text:    "первопричина блокировки решена — задача возвращена в планирование",
				Payload: map[string]any{"status": "queued"},
			}); err != nil {
				return err
			}
		}

		// queued → ready: нет незавершённых зависимостей (cancelled исключена из DAG).
		rows, err = tx.Query(ctx, `
			SELECT t.id FROM tasks t
			WHERE t.epic_id=$1 AND t.status='queued'
			  AND NOT EXISTS (SELECT 1 FROM task_deps d JOIN tasks dt ON dt.id=d.dep_id
			                  WHERE d.task_id=t.id AND dt.status NOT IN ('done','cancelled'))
			FOR UPDATE OF t`, epicID)
		if err != nil {
			return err
		}
		readyIDs, err := collectIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range readyIDs {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET status='ready', updated_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.status",
				ProjectID: projectID, EpicID: epicID, TaskID: id,
				Text:    "зависимости выполнены — задача готова к назначению",
				Payload: map[string]any{"status": "ready"},
			}); err != nil {
				return err
			}
		}

		// каскад blocked: ещё не начатые потомки failed-задач; blocked_by
		// указывает на первопричину. Эскалации не создаются — первопричина
		// уже эскалирована своим путём.
		for _, c := range affected {
			reason := fmt.Sprintf("зависимость task-%d завершилась неуспешно", c.rootNum)
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET status='blocked', blocked_by=$2, block_reason=$3,
					updated_at=now() WHERE id=$1 AND status IN ('queued','ready')`,
				c.id, c.rootID, reason)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue // уже blocked, выполняется или терминальна
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.status",
				ProjectID: projectID, EpicID: epicID, TaskID: c.id,
				Text:    "заблокирована: " + reason,
				Payload: map[string]any{"status": "blocked", "blocked_by": c.rootID},
			}); err != nil {
				return err
			}
		}

		// Epic done: не осталось нетерминальных задач.
		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM tasks WHERE epic_id=$1 AND status NOT IN ('done','cancelled')`,
			epicID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			if _, err := tx.Exec(ctx, `UPDATE epics SET status='done' WHERE id=$1`, epicID); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "epic.status",
				ProjectID: projectID, EpicID: epicID,
				Text: "все задачи завершены — Epic выполнен", Payload: map[string]any{"status": "done"},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func collectIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Assignment — результат назначения задачи runner'у.
type Assignment struct {
	Task   domain.Task
	Runner domain.Runner
}

// AssignNext атомарно назначает одну ready-задачу подходящему свободному
// runner'у: FOR UPDATE SKIP LOCKED с обеих сторон, один runner — одна задача.
// ok=false — назначать нечего (нет ready-задач или подходящих runner'ов).
func (s *Store) AssignNext(ctx context.Context) (Assignment, bool, error) {
	var a Assignment
	assigned := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Кандидат: ready-задача работающего Epic + свободный runner со всеми capabilities.
		row := tx.QueryRow(ctx, `
			SELECT t.id, t.num, t.epic_id, e.project_id, r.id
			FROM tasks t
			JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
			JOIN LATERAL (
				SELECT r.id FROM runners r
				WHERE r.status = 'idle' AND NOT r.draining AND r.capabilities @> t.capabilities
				-- беречь специалистов: из подходящих берём наименее «богатого» по capabilities
				ORDER BY cardinality(r.capabilities), r.last_seen DESC
				FOR UPDATE OF r SKIP LOCKED
				LIMIT 1
			) r ON true
			WHERE t.status = 'ready'
			ORDER BY t.num
			FOR UPDATE OF t SKIP LOCKED
			LIMIT 1`)
		var taskID, epicID, projectID, runnerID string
		var num int64
		if err := row.Scan(&taskID, &num, &epicID, &projectID, &runnerID); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}

		branch := fmt.Sprintf("agent/task-%d", num)
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status='running', runner_id=$2, branch=$3, updated_at=now() WHERE id=$1`,
			taskID, runnerID, branch); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runners SET status='running', task_id=$2 WHERE id=$1`, runnerID, taskID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.status",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text:    "назначена на runner " + runnerID,
			Payload: map[string]any{"status": "running", "runner": runnerID},
		}); err != nil {
			return err
		}
		a.Task.ID, a.Task.Num, a.Task.EpicID, a.Task.Branch = taskID, num, epicID, branch
		a.Runner.ID = runnerID
		assigned = true
		return nil
	})
	if err != nil || !assigned {
		return a, false, err
	}
	// Дочитываем полные сущности вне «горячей» транзакции.
	t, err := s.GetTask(ctx, a.Task.ID)
	if err != nil {
		return a, true, err
	}
	a.Task = t
	return a, true, nil
}

// freeRunner освобождает runner задачи (внутри транзакции перехода).
func freeRunner(ctx context.Context, tx pgx.Tx, taskID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE runners SET status='idle', task_id=NULL WHERE task_id=$1`, taskID)
	return err
}

// ConsumeAttempt фиксирует неуспешный цикл (замечания review или провал
// автопроверок): расходует попытку и либо возвращает задачу в fixing, либо
// (лимит исчерпан) — failed + эскалация с причиной reason.
// keepRunner оставляет задачу за текущим runner'ом (исправление после провала
// тестов в том же worktree); иначе исполнитель освобождается и задачу в fixing
// назначит AssignFixing.
func (s *Store) ConsumeAttempt(ctx context.Context, taskID string,
	reason domain.AttentionReason, detail string, keepRunner bool) (failed bool, err error) {

	limitMsg, fixMsg := "Цикл исправления неуспешен %d раз — лимит попыток исчерпан.", "исправление (попытка %d/%d)"
	switch reason {
	case domain.AttReviewLimit:
		limitMsg, fixMsg = "Review отклонил результат %d раз — лимит попыток исчерпан.",
			"review вернул замечания — исправление (попытка %d/%d)"
	case domain.AttTestFailed:
		limitMsg, fixMsg = "Автопроверки провалились %d раз — лимит попыток исчерпан.",
			"проверки упали — исправление (попытка %d/%d)"
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var used, limit int
		var from domain.TaskStatus
		var epicID, projectID string
		if err := tx.QueryRow(ctx, `
			SELECT t.attempt_used, t.attempt_limit, t.status, t.epic_id, e.project_id
			FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, taskID).
			Scan(&used, &limit, &from, &epicID, &projectID); err != nil {
			return err
		}
		// Ревьюер (если был) освобождается здесь же: отдельная транзакция
		// оставляла окно, в котором Tick назначал review повторно.
		if _, err := tx.Exec(ctx, `
			UPDATE runners SET status='idle', task_id=NULL
			WHERE id = (SELECT reviewer_id FROM tasks WHERE id=$1)`, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET reviewer_id=NULL WHERE id=$1`, taskID); err != nil {
			return err
		}
		used++
		if used >= limit {
			if !from.CanTransition(domain.TaskFailed) {
				return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(domain.TaskFailed)}
			}
			failed = true
			msg := fmt.Sprintf(limitMsg, used)
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status='failed', attempt_used=$2, runner_id=NULL,
					updated_at=now() WHERE id=$1`,
				taskID, used); err != nil {
				return err
			}
			if err := freeRunner(ctx, tx, taskID); err != nil {
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
				Text: msg, Payload: map[string]any{"status": "failed", "attempt": used, "detail": detail},
			})
			return err
		}
		if !from.CanTransition(domain.TaskFixing) {
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(domain.TaskFixing)}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status='fixing', attempt_used=$2, updated_at=now() WHERE id=$1`,
			taskID, used); err != nil {
			return err
		}
		if !keepRunner {
			if err := freeRunner(ctx, tx, taskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tasks SET runner_id=NULL WHERE id=$1`, taskID); err != nil {
				return err
			}
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.status",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text:    fmt.Sprintf(fixMsg, used, limit),
			Payload: map[string]any{"status": "fixing", "attempt": used, "detail": detail},
		})
		return err
	})
	return failed, err
}

// BlockTask — агент не может продолжить: blocked + эскалация BLOCKED + освобождение runner'а.
func (s *Store) BlockTask(ctx context.Context, taskID, question string, actor EventInput) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicID, projectID string
		if err := tx.QueryRow(ctx, `
			SELECT t.status, t.epic_id, e.project_id FROM tasks t
			JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, taskID).
			Scan(&from, &epicID, &projectID); err != nil {
			return err
		}
		if !from.CanTransition(domain.TaskBlocked) {
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(domain.TaskBlocked)}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status='blocked', block_reason=$2, updated_at=now() WHERE id=$1`,
			taskID, question); err != nil {
			return err
		}
		if err := freeRunner(ctx, tx, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO attention (project_id, task_id, reason, message)
			VALUES ($1,$2,'BLOCKED',$3)`, projectID, taskID, question); err != nil {
			return err
		}
		actor.Type, actor.ProjectID, actor.EpicID, actor.TaskID = "task.status", projectID, epicID, taskID
		actor.Text = "заблокирована: " + question
		actor.Payload = map[string]any{"status": "blocked"}
		_, err := appendEvent(ctx, tx, actor)
		return err
	})
}

// ResolveTask — решение человека по blocked/failed задаче: ответ дополняет
// описание, счётчик попыток полностью сбрасывается (спека human-escalation),
// задача возвращается в планирование, эскалация закрывается.
func (s *Store) ResolveTask(ctx context.Context, taskID, answer, userID string, cancel bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicID, projectID string
		if err := tx.QueryRow(ctx, `
			SELECT t.status, t.epic_id, e.project_id FROM tasks t
			JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, taskID).
			Scan(&from, &epicID, &projectID); err != nil {
			return err
		}
		to := domain.TaskQueued
		if cancel {
			to = domain.TaskCancelled
		}
		if !from.CanTransition(to) {
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(to)}
		}
		if cancel {
			// Отмена может прилететь и в выполняющуюся задачу: освобождаем
			// её runner'ов (исполнителя и ревьюера), иначе они зависнут занятыми.
			if err := freeRunner(ctx, tx, taskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status='cancelled', runner_id=NULL, reviewer_id=NULL,
					updated_at=now() WHERE id=$1`, taskID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status='queued', attempt_used=0, block_reason=NULL, blocked_by=NULL,
					runner_id=NULL, reviewer_id=NULL,
					description = description || CASE WHEN $2 <> '' THEN E'\n\nУточнение человека: ' || $2 ELSE '' END,
					updated_at=now()
				WHERE id=$1`, taskID, answer); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE attention SET status='resolved', resolved_at=now()
			WHERE task_id=$1 AND status <> 'resolved'`, taskID); err != nil {
			return err
		}
		text := "решение человека: задача возвращена в планирование, счётчик попыток сброшен"
		if cancel {
			text = "решение человека: задача отменена"
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: userID, Type: "task.status",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text: text, Payload: map[string]any{"status": string(to)},
		})
		return err
	})
}

// TransitionWithRunnerRelease — переход, освобождающий runner (done и т.п.).
func (s *Store) TransitionWithRunnerRelease(ctx context.Context, taskID string,
	to domain.TaskStatus, ev EventInput) error {
	return s.TransitionTask(ctx, taskID, to, ev, func(tx pgx.Tx) error {
		return freeRunner(ctx, tx, taskID)
	})
}

// MarkStaleRunnersOffline: тишина дольше таймаута → offline. Потеря runner'а
// расходует попытку задачи (спека orchestration: бесконечный перезапуск
// недопустим): попытки остались → ready, перезапуск через планирование
// (частичная работа остаётся в ветке); лимит исчерпан → failed + эскалация
// RUNNER_LOST.
func (s *Store) MarkStaleRunnersOffline(ctx context.Context, timeoutSeconds int) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.id, COALESCE(r.task_id::text,'') FROM runners r
			WHERE r.status <> 'offline' AND r.last_seen < now() - make_interval(secs => $1)
			FOR UPDATE OF r SKIP LOCKED`, timeoutSeconds)
		if err != nil {
			return err
		}
		type stale struct{ runnerID, taskID string }
		var stales []stale
		for rows.Next() {
			var s stale
			if err := rows.Scan(&s.runnerID, &s.taskID); err != nil {
				rows.Close()
				return err
			}
			stales = append(stales, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, st := range stales {
			if _, err := tx.Exec(ctx,
				`UPDATE runners SET status='offline', task_id=NULL WHERE id=$1`, st.runnerID); err != nil {
				return err
			}
			if st.taskID == "" {
				continue
			}
			var from domain.TaskStatus
			var epicID, projectID string
			var used, limit int
			if err := tx.QueryRow(ctx, `
				SELECT t.status, t.epic_id, e.project_id, t.attempt_used, t.attempt_limit
				FROM tasks t
				JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, st.taskID).
				Scan(&from, &epicID, &projectID, &used, &limit); err != nil {
				return err
			}
			if !from.CanTransition(domain.TaskReady) {
				continue // терминальные, blocked и queued не трогаем
			}
			used++
			if used >= limit {
				msg := fmt.Sprintf("Runner %s потерян, лимит попыток исчерпан (%d/%d).", st.runnerID, used, limit)
				if _, err := tx.Exec(ctx, `
					UPDATE tasks SET status='failed', attempt_used=$2, runner_id=NULL, reviewer_id=NULL,
						updated_at=now() WHERE id=$1`, st.taskID, used); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO attention (project_id, task_id, reason, message)
					VALUES ($1,$2,$3,$4)`, projectID, st.taskID, string(domain.AttRunnerLost), msg); err != nil {
					return err
				}
				if _, err := appendEvent(ctx, tx, EventInput{
					ActorKind: domain.ActorScheduler, Type: "task.status",
					ProjectID: projectID, EpicID: epicID, TaskID: st.taskID,
					Text:    msg,
					Payload: map[string]any{"status": "failed", "attempt": used, "runner_offline": st.runnerID},
				}); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status='ready', attempt_used=$2, runner_id=NULL, reviewer_id=NULL,
					updated_at=now() WHERE id=$1`, st.taskID, used); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.status",
				ProjectID: projectID, EpicID: epicID, TaskID: st.taskID,
				Text: fmt.Sprintf("runner %s offline — попытка израсходована (%d/%d), задача будет перезапущена",
					st.runnerID, used, limit),
				Payload: map[string]any{"status": "ready", "attempt": used, "runner_offline": st.runnerID},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
