package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func depsEvents(t *testing.T, st *store.Store, epicID string) []string {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{EpicID: epicID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type == "epic.waiting_deps" || ev.Type == "epic.deps_ready" {
			out = append(out, ev.Type)
		}
	}
	return out
}

// Задачи Epic'а с незавершённой зависимостью не назначаются, Epic остаётся
// running. Ожидание и его снятие пишутся событиями по смене состояния, первое
// состояние после старта снимается без событий (add-product-discovery,
// спека orchestration «Ожидание зависимостей Epic'а»).
func TestEpicWaitsForDependencies(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	base, _ := st.CreateEpic(ctx, p.ID, "Компании", "")
	if _, err := st.CreateTask(ctx, base.ID, store.NewTask{Title: "B"}); err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Сделки", "")
	if _, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetEpicDeps(ctx, epic.ID, []string{base.ID}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})

	// Базовое состояние: ни один Epic не запущен, событий нет.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	out.mu.Lock()
	for _, m := range out.sent {
		if m.GetAssign() != nil {
			t.Fatalf("задача Epic'а с незавершённой зависимостью назначена: %v", m.GetAssign())
		}
	}
	out.mu.Unlock()
	if ep, _ := st.GetEpic(ctx, epic.ID); ep.Status != domain.EpicRunning {
		t.Fatalf("Epic должен остаться running, got %s", ep.Status)
	}
	if got := depsEvents(t, st, epic.ID); len(got) != 1 || got[0] != "epic.waiting_deps" {
		t.Fatalf("ожидали одно событие epic.waiting_deps, got %v", got)
	}

	// Рестарт: новый движок снимает ожидание как базу, без повторного события.
	e2 := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	if err := e2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := depsEvents(t, st, epic.ID); len(got) != 1 {
		t.Fatalf("после рестарта событие ожидания повторилось: %v", got)
	}

	// Зависимость завершена: ожидание снято, задача назначена.
	if _, err := st.Pool.Exec(ctx, `UPDATE epics SET status='done' WHERE id=$1`, base.ID); err != nil {
		t.Fatal(err)
	}
	if err := e2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := out.lastAssign(t); as.TaskId == "" {
		t.Fatal("задача не назначена после выполнения зависимости")
	}
	if got := depsEvents(t, st, epic.ID); len(got) != 2 || got[1] != "epic.deps_ready" {
		t.Fatalf("ожидали epic.deps_ready, got %v", got)
	}
}

// Зависимость завершилась, пока Epic стоял на паузе: снятие ожидания
// объявляется при возобновлении. Снятие зависимости правкой называется
// иначе, чем выполнение.
func TestEpicDepsAcrossPause(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	e := New(st, &fakeSCM{}, nil, &capture{}, 90*time.Second)
	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	base, _ := st.CreateEpic(ctx, p.ID, "Сделки", "")
	epic, _ := st.CreateEpic(ctx, p.ID, "Дашборд", "")
	if _, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetEpicDeps(ctx, epic.ID, []string{base.ID}); err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tr := func(id string, to domain.EpicStatus) {
		t.Helper()
		if err := st.TransitionEpic(ctx, id, to, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	tr(epic.ID, domain.EpicRunning)
	tick()
	tr(epic.ID, domain.EpicPaused)
	tick()
	if _, err := st.Pool.Exec(ctx, `UPDATE epics SET status='done' WHERE id=$1`, base.ID); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := depsEvents(t, st, epic.ID); len(got) != 1 {
		t.Fatalf("на паузе снятие не объявляется: %v", got)
	}
	tr(epic.ID, domain.EpicRunning)
	tick()
	if got := depsEvents(t, st, epic.ID); len(got) != 2 || got[1] != "epic.deps_ready" {
		t.Fatalf("после возобновления ждали epic.deps_ready: %v", got)
	}

	// Новый незавершённый Epic в зависимостях, затем зависимость снята правкой.
	other, _ := st.CreateEpic(ctx, p.ID, "Компании", "")
	if _, err := st.SetEpicDeps(ctx, epic.ID, []string{other.ID}); err != nil {
		t.Fatal(err)
	}
	tick()
	if _, err := st.SetEpicDeps(ctx, epic.ID, nil); err != nil {
		t.Fatal(err)
	}
	tick()
	evs, _ := st.Events(ctx, store.EventFilter{EpicID: epic.ID, Type: "epic.deps_ready", Limit: 10})
	if len(evs) != 2 || !strings.Contains(evs[1].Text, "зависимости сняты") {
		t.Fatalf("снятие зависимости правкой: %+v", evs)
	}
}

// Архивированная незавершённой зависимость оставляет Epic в ожидании, причина
// называет архивированный Epic.
func TestEpicWaitsForArchivedDependency(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	base, _ := st.CreateEpic(ctx, p.ID, "Сделки и воронка", "")
	epic, _ := st.CreateEpic(ctx, p.ID, "Дашборд продаж", "")
	if _, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetEpicDeps(ctx, epic.ID, []string{base.ID}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, base.ID, domain.EpicArchived, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	n := len(out.sent)
	out.mu.Unlock()
	if n != 0 {
		t.Fatalf("задача назначена при архивированной зависимости: %d сообщений", n)
	}
	evs, _ := st.Events(ctx, store.EventFilter{EpicID: epic.ID, Limit: 500})
	var text string
	for _, ev := range evs {
		if ev.Type == "epic.waiting_deps" {
			text = ev.Text
		}
	}
	if !strings.Contains(text, "Сделки и воронка") || !strings.Contains(text, "в архиве") {
		t.Fatalf("причина ожидания должна называть архивированный Epic: %q", text)
	}
	got := []domain.Epic{{ID: epic.ID}}
	if err := st.EnrichEpics(ctx, got); err != nil {
		t.Fatal(err)
	}
	if len(got[0].WaitingFor) != 1 || got[0].WaitingFor[0].Status != string(domain.EpicArchived) {
		t.Fatalf("WaitingFor: %+v", got[0].WaitingFor)
	}
}
