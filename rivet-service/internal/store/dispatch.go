package store

import (
	"context"
	"errors"
	"time"

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

// FreeTaskRunner освобождает runner задачи, сохраняя исполнителя на задаче
// (add-agent-roles: тестировщик на другом runner'е, ревьюер не совпадает
// с исполнителем кода).
func (s *Store) FreeTaskRunner(ctx context.Context, taskID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return freeRunner(ctx, tx, taskID)
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
	t, err := scanTask(s.Pool.QueryRow(ctx, `SELECT `+taskCols+` FROM `+taskFrom+` WHERE t.branch=$1`, branch))
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

// FreeRunnerFor освобождает runner, если он всё ещё за задачей.
func (s *Store) FreeRunnerFor(ctx context.Context, runnerID, taskID string) error {
	if runnerID == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL WHERE id=$1 AND task_id=$2`, runnerID, taskID)
	return err
}

// SetRunAgentVerdict — вердикт, который вернул сам агент запуска.
func (s *Store) SetRunAgentVerdict(ctx context.Context, runID int64, verdict string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET agent_verdict=$2 WHERE id=$1 AND verdict IS NULL`, runID, verdict)
	return err
}

// InfraRetryKey — счётчик повторов шага после сбоя инфраструктуры в
// step_rejections задачи.
func InfraRetryKey(stepID string) string { return stepID + ":infra" }

// RecordInfraRetry — повторить ли шаг после сбоя инфраструктуры: счётчик
// задачи по шагу меньше лимита — увеличивается и retry=true.
func (s *Store) RecordInfraRetry(ctx context.Context, taskID, stepID string, limit int) (retry bool, n int, err error) {
	key := InfraRetryKey(stepID)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((step_rejections->>$2)::int, 0) FROM tasks WHERE id=$1 FOR UPDATE`, taskID, key).Scan(&n); err != nil {
			return nf(err)
		}
		if n >= limit {
			return nil
		}
		n++
		retry = true
		_, err := tx.Exec(ctx, `UPDATE tasks SET step_rejections = step_rejections || jsonb_build_object($2::text, $3::int), updated_at=now() WHERE id=$1`, taskID, key, n)
		return err
	})
	return retry, n, err
}

// RunnerBootID — идентификатор процесса runner'а из прошлой регистрации.
func (s *Store) RunnerBootID(ctx context.Context, runnerID string) (string, error) {
	var b string
	err := s.Pool.QueryRow(ctx, `SELECT boot_id FROM runners WHERE id=$1`, runnerID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return b, err
}

// ReleaseRunnerRuns снимает открытые запуски runner'а, процесс которого
// перезапустился: стадии потеряны, сессии закрываются, запуски отвязываются
// от runner'а и агента команды и ждут нового назначения в том же проходе
// без расхода попытки. Возвращает снятые запуски.
func (s *Store) ReleaseRunnerRuns(ctx context.Context, runnerID string, keepSessions []string) ([]StepRun, error) {
	var out []StepRun
	if keepSessions == nil {
		keepSessions = []string{}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Сессии, чьи результаты лежат в журнале runner'а, не снимаются:
		// перезапущенный runner проиграет их после регистрации.
		rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM task_step_runs r
			WHERE r.runner_id=$1 AND r.verdict IS NULL AND COALESCE(r.session_id::text, '') <> ALL($2::text[])
			FOR UPDATE OF r`, runnerID, keepSessions)
		if err != nil {
			return err
		}
		runs, err := collectRuns(rows)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if r.SessionID != "" {
				if _, err := tx.Exec(ctx, `UPDATE sessions SET ended_at=now(), outcome='прервана: runner перезапущен или переподключился' WHERE id=$1::uuid AND ended_at IS NULL`, r.SessionID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE task_step_runs SET runner_id=NULL, session_id=NULL, agent_id=NULL, assigned_model='' WHERE id=$1`, r.ID); err != nil {
				return err
			}
			var projectID, epicID string
			if err := tx.QueryRow(ctx, `SELECT e.project_id, t.epic_id FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE t.id=$1`, r.TaskID).Scan(&projectID, &epicID); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorScheduler, Type: "task.assign",
				ProjectID: projectID, EpicID: epicID, TaskID: r.TaskID,
				Text:    "runner " + runnerID + " перезапущен или переподключился: прерванная стадия будет назначена заново",
				Payload: map[string]any{"runner": runnerID, "step": r.StepID, "restarted": true},
			}); err != nil {
				return err
			}
		}
		out = runs
		return nil
	})
	return out, err
}

// DeferRun — стадия не смогла стартовать по причине до отправки (нет
// проекта, сессии, учётных данных, шага в снимке): запуск отвязывается от
// runner'а и агента и не назначается раньше паузы, задача показывает
// причину ожидания. Пауза растёт с каждым откладыванием подряд: base,
// 2·base, 4·base и так до max; доставка (RunDelivered) сбрасывает серию.
// first — первое откладывание серии: событие ожидания пишется один раз.
func (s *Store) DeferRun(ctx context.Context, runID int64, taskID string, base, max time.Duration, reason string) (first bool, pause time.Duration, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT defer_count FROM task_step_runs WHERE id=$1 AND verdict IS NULL FOR UPDATE`,
			runID).Scan(&n); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		first = n == 0
		pause = deferPause(n, base, max)
		if _, err := tx.Exec(ctx, `
			UPDATE task_step_runs SET runner_id=NULL, session_id=NULL, agent_id=NULL, assigned_model='',
				defer_count = defer_count + 1, not_before = now() + make_interval(secs => $2)
			WHERE id=$1`, runID, pause.Seconds()); err != nil {
			return err
		}
		// Начало ожидания серии живёт в самом запуске (deferred_since) и
		// пишется в этой же транзакции. Первое откладывание запоминает
		// начало ожидания задачи (уже идущее или с этого момента). Назначения
		// между откладываниями очищают причину ожидания, а следующие
		// откладывания возвращают начало серии.
		if first {
			var since time.Time
			if err := tx.QueryRow(ctx, `UPDATE tasks SET wait_reason=$2, updated_at=now() WHERE id=$1
				RETURNING COALESCE(wait_since, now())`, taskID, clipRunes(reason, 300)).Scan(&since); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE task_step_runs SET deferred_since=$2 WHERE id=$1`, runID, since)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tasks t SET wait_reason=$2, updated_at=now(),
			wait_since = COALESCE((SELECT r.deferred_since FROM task_step_runs r WHERE r.id=$3), t.wait_since, now())
			WHERE t.id=$1`, taskID, clipRunes(reason, 300), runID)
		return err
	})
	return first, pause, err
}

// deferPause — пауза n-го (с нуля) откладывания подряд.
func deferPause(n int, base, max time.Duration) time.Duration {
	p := base
	for i := 0; i < n && p < max; i++ {
		p *= 2
	}
	if p > max {
		p = max
	}
	return p
}

// RunDelivered — стадия доставлена runner'у: серия откладываний закончена,
// событие назначения (p, если есть) пишется только теперь.
func (s *Store) RunDelivered(ctx context.Context, runID int64, p *PendingAssign) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE task_step_runs SET defer_count=0, not_before=NULL, deferred_since=NULL
			WHERE id=$1 AND (defer_count > 0 OR not_before IS NOT NULL OR deferred_since IS NOT NULL)`, runID); err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		_, err := appendEvent(ctx, tx, p.Event)
		return err
	})
}

// UndoAssign — стадия не доставлена: задача, которую назначение перевело
// из ready, возвращается в ready (если её статус с тех пор не менялся).
// Время начала, поставленное этим назначением, снимается: работа не
// начиналась.
func (s *Store) UndoAssign(ctx context.Context, taskID string, p *PendingAssign) error {
	if p == nil || p.To == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='ready',
		started_at = CASE WHEN $3 THEN NULL ELSE started_at END, updated_at=now()
		WHERE id=$1 AND status=$2`, taskID, p.To, p.FirstStart)
	return err
}

// MarkRunnerOffline — runner не подключён (назначение не доставлено):
// планировщик его не выбирает, пока runner не подаст признак жизни. seen —
// last_seen из снимка назначения: если runner с тех пор регистрировался,
// открыл канал или прислал heartbeat, он жив, и отметка не ставится.
// false — отметка не поставлена.
func (s *Store) MarkRunnerOffline(ctx context.Context, runnerID string, seen time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE runners SET status='offline', task_id=NULL, offline_reason='undelivered'
		WHERE id=$1 AND status <> 'offline' AND last_seen <= $2`, runnerID, seen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// reviveRunnerSQL — runner подал признак жизни: отметка offline, которую
// снимает этот признак (cond по offline_reason), снимается, если за
// runner'ом нет стадии и публикации.
func reviveRunnerSQL(cond string) string {
	when := `status = 'offline' AND ` + cond + ` AND task_id IS NULL AND deployment_id IS NULL`
	return `status = CASE WHEN ` + when + ` THEN 'idle' ELSE status END,
		offline_reason = CASE WHEN ` + when + ` THEN '' ELSE offline_reason END`
}

// RunnerAttached — runner открыл канал (Hello): признак жизни, который
// снимает отметку offline с любой причиной.
func (s *Store) RunnerAttached(ctx context.Context, runnerID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen=now(), `+reviveRunnerSQL(`true`)+` WHERE id=$1`, runnerID)
	return err
}
