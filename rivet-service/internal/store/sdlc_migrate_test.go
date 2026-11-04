package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// emptyDB — пустая база без миграций (тест порядка миграций).
func emptyDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RIVET_DATABASE_URL")
	if base == "" {
		base = "postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("postgres недоступен: %v", err)
	}
	name := fmt.Sprintf("rivet_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(base)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// withArchitecture — миграции сборки и 0029 change'а архитектуры (копия в
// testdata: блок вида Epic'а и задачи дословно совпадает с 0032).
func withArchitecture(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/0029_project_architecture.sql")
	if err != nil {
		t.Fatal(err)
	}
	files["0029_project_architecture.sql"] = raw
	return files
}

// checkKinds — умолчания и полные списки видов после обеих миграций.
func checkKinds(t *testing.T, url string) {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := s.CreateEpic(ctx, p.ID, "CRM", "контакты")
	if err != nil {
		t.Fatalf("Epic с умолчанием вида: %v", err)
	}
	var kind string
	_ = s.Pool.QueryRow(ctx, `SELECT kind FROM epics WHERE id=$1`, ep.ID).Scan(&kind)
	if kind != "work" {
		t.Fatalf("вид Epic'а по умолчанию: %q", kind)
	}
	for _, k := range []string{"architecture", "explore", "maintenance"} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO epics (project_id, title, goal, kind) VALUES ($1,'x','g',$2)`, p.ID, k); err != nil {
			t.Fatalf("вид Epic'а %s: %v", k, err)
		}
	}
	for _, k := range []string{"work", "architecture", "explore", "bug"} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO tasks (epic_id, title, kind) VALUES ($1,'t',$2)`, ep.ID, k); err != nil {
			t.Fatalf("вид задачи %s: %v", k, err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO epics (project_id, title, goal, kind) VALUES ($1,'x','g','regular')`, p.ID); err == nil {
		t.Fatal("вид regular принят")
	}
}

// Порядок A: 0029 до 0032 на пустой базе.
func TestMigrationsArchitectureFirst(t *testing.T) {
	url := emptyDB(t)
	if err := migrateFiles(context.Background(), url, withArchitecture(t)); err != nil {
		t.Fatal(err)
	}
	checkKinds(t, url)
}

// Порядок B: база уже на 0032 с Epic'ом «Сопровождение» и багом, затем 0029.
func TestMigrationsArchitectureAfter(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var epicID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO epics (project_id, title, goal, status, kind)
		VALUES ($1,'Сопровождение','g','running','maintenance') RETURNING id`, p.ID).Scan(&epicID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO tasks (epic_id, title, kind) VALUES ($1,'баг','bug')`, epicID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := migrateFiles(ctx, url, withArchitecture(t)); err != nil {
		t.Fatalf("0029 после 0032: %v", err)
	}
	checkKinds(t, url)
}

// Стенд main: 0033 (занятость агентов, tasks.wait_kind) уже применена, 0032
// идёт после неё. Обратный порядок (0032, затем 0033) — пустая база,
// его проходит каждый testStore. Триггер 0033 работает и после 0032, а
// колонки обеих миграций в tasks уживаются.
func TestMigrationsTeamWorkFirst(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	files, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for name, raw := range files {
		if name != "0032_sdlc_cycle.sql" {
			before[name] = raw
		}
	}
	if err := migrateFiles(ctx, url, before); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var taskID string
	if err := conn.QueryRow(ctx, `
		WITH p AS (INSERT INTO projects (name, repo_path) VALUES ('demo', 'o/r') RETURNING id),
		     e AS (INSERT INTO epics (project_id, title, goal) SELECT id, 'CRM', 'g' FROM p RETURNING id)
		INSERT INTO tasks (epic_id, title, wait_reason, wait_kind) SELECT id, 't', 'нет свободного агента роли «Разработчик»: все заняты', 'team' FROM e
		RETURNING id::text`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO team_agents (id, name, role_id) VALUES ('max', 'Макс', 'developer')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO task_step_runs (task_id, step_id, participant, agent_id) VALUES ($1, 'code', 'p1', 'max')`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := migrateFiles(ctx, url, files); err != nil {
		t.Fatalf("0032 после 0033: %v", err)
	}
	var kind, waitKind string
	if err := conn.QueryRow(ctx, `SELECT kind, wait_kind FROM tasks WHERE id=$1`, taskID).Scan(&kind, &waitKind); err != nil {
		t.Fatal(err)
	}
	if kind != "work" || waitKind != "team" {
		t.Fatalf("задача после 0032: kind=%q wait_kind=%q", kind, waitKind)
	}
	// Триггер занятости срабатывает и после 0032.
	if _, err := conn.Exec(ctx, `UPDATE task_step_runs SET verdict='ok' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	var busy, free, agents int
	_ = conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type='team.agent_busy'), count(*) FILTER (WHERE type='team.agent_free') FROM events`).Scan(&busy, &free)
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM team_agents WHERE id IN ('dina', 'gleb')`).Scan(&agents)
	if busy != 1 || free != 1 || agents != 2 {
		t.Fatalf("занятость агентов: busy=%d free=%d, Дина и Глеб: %d", busy, free, agents)
	}
	var nullable string
	_ = conn.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name='usage_records' AND column_name='role'`).Scan(&nullable)
	if nullable != "YES" {
		t.Fatalf("usage_records.role nullable: %q", nullable)
	}
}
