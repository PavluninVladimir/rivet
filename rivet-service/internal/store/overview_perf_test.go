package store

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/overview"
)

// Замер GET /overview на синтетическом проекте (add-project-lead-view,
// задача 1.9): 500 задач, 20 тыс. записей usage, 50 тыс. событий, цель
// меньше 200 мс. Тест тяжёлый и включается переменной RIVET_OVERVIEW_PERF=1.
func TestOverviewPerf(t *testing.T) {
	if os.Getenv("RIVET_OVERVIEW_PERF") != "1" {
		t.Skip("замер включается RIVET_OVERVIEW_PERF=1")
	}
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "perf", "o/perf", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	// 10 Epic'ов по 50 задач: часть завершена, часть в работе.
	// INSERT триггер времени не вызывает: done_at завершённых задаётся явно.
	exec(`INSERT INTO epics (project_id, title, status, created_at, started_at, done_at)
		SELECT $1, 'E' || g, CASE WHEN g <= 3 THEN 'done' ELSE 'running' END, now() - interval '40 days', now() - interval '30 days',
		       CASE WHEN g <= 3 THEN now() - interval '20 days' END
		FROM generate_series(1, 10) g`, p.ID)
	exec(`INSERT INTO tasks (epic_id, title, status, estimate, created_at, started_at, done_at, attempt_used)
		SELECT e.id, 'T' || g, CASE WHEN g % 3 = 0 THEN 'running' ELSE 'done' END, 1 + g % 5,
		       now() - interval '30 days', now() - (g % 28) * interval '1 day' - interval '2 hours',
		       CASE WHEN g % 3 = 0 THEN NULL ELSE now() - (g % 28) * interval '1 day' END, 1
		FROM epics e, generate_series(1, 50) g WHERE e.project_id = $1`, p.ID)
	exec(`INSERT INTO sessions (task_id, driver_kind, agent, started_at, ended_at)
		SELECT t.id, 'scheduler', 'rivet', t.started_at, t.started_at + interval '40 minutes'
		FROM tasks t JOIN epics e ON e.id = t.epic_id WHERE e.project_id = $1`, p.ID)
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, session_id, verdict, role, finished_at)
		SELECT s.task_id, 'code', 'code', 'p1', s.id, 'ok', 'developer', s.ended_at
		FROM sessions s JOIN tasks t ON t.id = s.task_id JOIN epics e ON e.id = t.epic_id WHERE e.project_id = $1`, p.ID)
	exec(`INSERT INTO usage_records (ts, project_id, epic_id, task_id, model, tokens_in, tokens_out, cost_usd, duration_s, session_id)
		SELECT now() - (g % 40) * interval '1 day', $1, t.epic_id, t.id, 'deepseek-v4-flash', 1000, 200, 0.01, 30, s.id
		FROM generate_series(1, 20000) g
		JOIN LATERAL (SELECT t2.id, t2.epic_id FROM tasks t2 JOIN epics e ON e.id = t2.epic_id
		              WHERE e.project_id = $1 ORDER BY t2.num OFFSET (g % 500) LIMIT 1) t ON true
		LEFT JOIN LATERAL (SELECT id FROM sessions WHERE task_id = t.id LIMIT 1) s ON true`, p.ID)
	exec(`INSERT INTO events (ts, actor_kind, type, project_id, epic_id, task_id, payload)
		SELECT now() - (g % 40) * interval '1 day', 'scheduler', 'task.status', $1, t.epic_id, t.id, '{"status":"running"}'
		FROM generate_series(1, 50000) g
		JOIN LATERAL (SELECT t2.id, t2.epic_id FROM tasks t2 JOIN epics e ON e.id = t2.epic_id
		              WHERE e.project_id = $1 ORDER BY t2.num OFFSET (g % 500) LIMIT 1) t ON true`, p.ID)
	exec(`ANALYZE`)

	loc := time.UTC
	var took []time.Duration
	for i := 0; i < 7; i++ {
		now := time.Now()
		per, _ := overview.NewPeriod("7d", now, loc)
		start := time.Now()
		in, err := s.OverviewInput(ctx, OverviewQuery{ProjectID: p.ID, UserID: owner.ID, Login: owner.Login, Now: now, Period: per, Loc: loc})
		if err != nil {
			t.Fatal(err)
		}
		_ = overview.Build(in)
		took = append(took, time.Since(start))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	t.Logf("обзор: медиана %v, худший %v (500 задач, 20 тыс. usage, 50 тыс. событий)", took[len(took)/2], took[len(took)-1])
	if took[len(took)/2] > 200*time.Millisecond {
		t.Errorf("медиана %v больше цели 200 мс", took[len(took)/2])
	}
}
