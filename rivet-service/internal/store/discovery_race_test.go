package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/discovery"
)

// Epic с тем же названием создан вручную после синтеза: утверждение
// связывает строку бэклога с ним, а не создаёт дубль (репро ревью Opus).
func TestApproveLinksEpicCreatedAfterSynthesis(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRunReady(ctx, r.ID, syntheticResult(), nil, nil); err != nil {
		t.Fatal(err)
	}
	manual, err := s.CreateEpic(ctx, p.ID, "Сделки и воронка", "создан вручную после синтеза")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	epics, _ := s.ListEpics(ctx, p.ID)
	n := 0
	for _, e := range epics {
		if discovery.NormalizeTitle(e.Title) == discovery.NormalizeTitle("Сделки и воронка") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("утверждение создало дубль: Epic'ов «Сделки и воронка» %d", n)
	}
	linked := false
	for _, a := range res.Epics {
		if a.EpicID == manual.ID && a.Action == "linked" {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("строка не связана с Epic'ом проекта: %+v", res.Epics)
	}
}

// Утверждение и правка зависимостей идут под одним замком проекта:
// правка ждёт утверждения и видит его рёбра, цикл не возникает (репро
// ревью Opus). Транзакция ниже повторяет порядок замков ApproveRun.
func TestApproveAndSetDepsSerialized(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	L, _ := s.CreateEpic(ctx, p.ID, "L", "")
	T, _ := s.CreateEpic(ctx, p.ID, "T", "")
	X, _ := s.CreateEpic(ctx, p.ID, "X", "")
	if _, err := s.SetEpicDeps(ctx, T.ID, []string{X.ID}); err != nil { // T → X
		t.Fatal(err)
	}
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, _ := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM projects WHERE id=$1 FOR UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM discovery_runs WHERE id=$1 FOR UPDATE`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE epics SET discovery_run_id=$2, priority=COALESCE($3, priority) WHERE id=$1`, L.ID, r.ID, "must"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO epic_deps (epic_id, depends_on) VALUES ($1,$2) ON CONFLICT DO NOTHING`, L.ID, T.ID); err != nil {
		t.Fatal(err)
	}
	if err := projectDepsAcyclic(ctx, tx, p.ID); err != nil {
		t.Fatalf("утверждение видит цикл раньше времени: %v", err)
	}

	// Параллельная правка X → L ждёт замка проекта.
	done := make(chan error, 1)
	go func() {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, derr := s.SetEpicDeps(dctx, X.ID, []string{L.ID})
		done <- derr
	}()
	select {
	case err := <-done:
		t.Fatalf("правка зависимостей прошла во время утверждения: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	derr := <-done
	var fe *DiscoveryFieldError
	if !errors.As(derr, &fe) || fe.Field != "depends_on" {
		t.Fatalf("правка после утверждения должна увидеть цикл: %v", derr)
	}
	rows, _ := s.Pool.Query(ctx, `SELECT epic_id::text, depends_on::text FROM epic_deps`)
	g := map[string][]string{}
	for rows.Next() {
		var a, b string
		_ = rows.Scan(&a, &b)
		g[a] = append(g[a], b)
	}
	rows.Close()
	if c := discovery.FindCycle(g); c != nil {
		t.Fatalf("цикл в epic_deps после обеих транзакций: %v", c)
	}
}

// План создаётся одной транзакцией и только у Epic'а без задач.
func TestCreatePlanOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	plan := []PlanTask{{NewTask: NewTask{Title: "A"}}, {NewTask: NewTask{Title: "B"}, DepIdx: []int{0}}}
	tasks, err := s.CreatePlan(ctx, e.ID, plan)
	if err != nil || len(tasks) != 2 || len(tasks[1].Deps) != 1 || tasks[1].Deps[0] != tasks[0].ID {
		t.Fatalf("план: %+v %v", tasks, err)
	}
	if _, err := s.CreatePlan(ctx, e.ID, plan); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("второй план: %v", err)
	}
	// Ошибка посередине не оставляет половину плана.
	e2, _ := s.CreateEpic(ctx, p.ID, "E2", "")
	bad := []PlanTask{{NewTask: NewTask{Title: "A"}}, {NewTask: NewTask{Title: "B"}, DepIdx: []int{5}}}
	if _, err := s.CreatePlan(ctx, e2.ID, bad); err == nil {
		t.Fatal("план с неверной зависимостью принят")
	}
	if got, _ := s.ListEpicTasks(ctx, e2.ID); len(got) != 0 {
		t.Fatalf("половина плана осталась: %d задач", len(got))
	}
}
