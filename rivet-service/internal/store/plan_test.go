package store

import (
	"context"
	"errors"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func strp(s string) *string       { return &s }
func slicep(v []string) *[]string { return &v }

// Правка плана (спека epic-decomposition «Правка плана человеком»).
func TestPlanEditing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-plan", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "A"})
	b, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "B", Deps: []string{a.ID}})
	c, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "C"})

	// Правка полей: title/description/criteria; отметки сбрасываются.
	if _, err := s.UpdateTaskPlan(ctx, c.ID, PlanEdit{
		Title: strp("C: уточнено"), Description: strp("новое описание"),
		Criteria: slicep([]string{"критерий 1", "критерий 2"}),
	}, "owner"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, c.ID)
	if got.Title != "C: уточнено" || got.Description != "новое описание" ||
		len(got.Criteria) != 2 || got.Criteria[0].OK {
		t.Fatalf("правка полей: %+v", got)
	}
	evs, _ := s.Events(ctx, EventFilter{TaskID: c.ID, Type: "task.plan_edited", Limit: 5})
	if len(evs) != 1 {
		t.Fatalf("событие правки: %+v", evs)
	}

	// Цикл отклоняется: A → B при существующем B → A.
	if _, err := s.UpdateTaskPlan(ctx, a.ID, PlanEdit{Deps: slicep([]string{b.ID})}, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("цикл должен отклоняться: %v", err)
	}
	// Самоссылка и чужой Epic.
	if _, err := s.UpdateTaskPlan(ctx, a.ID, PlanEdit{Deps: slicep([]string{a.ID})}, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("самоссылка: %v", err)
	}
	otherEpic, _ := s.CreateEpic(ctx, p.ID, "E2", "")
	x, _ := s.CreateTask(ctx, otherEpic.ID, NewTask{Title: "X"})
	if _, err := s.UpdateTaskPlan(ctx, a.ID, PlanEdit{Deps: slicep([]string{x.ID})}, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("чужой Epic: %v", err)
	}

	// Запуск: A и C ready, B queued (зависит от A).
	if err := s.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, epic.ID); err != nil {
		t.Fatal(err)
	}
	st := func(id string) domain.TaskStatus { tk, _ := s.GetTask(ctx, id); return tk.Status }
	if st(a.ID) != domain.TaskReady || st(b.ID) != domain.TaskQueued || st(c.ID) != domain.TaskReady {
		t.Fatalf("готовность до правки: %s %s %s", st(a.ID), st(b.ID), st(c.ID))
	}

	// ready → queued: у C появляется зависимость от невыполненной A.
	epicID, err := s.UpdateTaskPlan(ctx, c.ID, PlanEdit{Deps: slicep([]string{a.ID})}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, epicID); err != nil {
		t.Fatal(err)
	}
	if st(c.ID) != domain.TaskQueued {
		t.Fatalf("C должна вернуться в queued: %s", st(c.ID))
	}
	// queued → ready: у B зависимости сняты.
	if _, err := s.UpdateTaskPlan(ctx, b.ID, PlanEdit{Deps: slicep([]string{})}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, epic.ID); err != nil {
		t.Fatal(err)
	}
	if st(b.ID) != domain.TaskReady {
		t.Fatalf("B должна стать ready: %s", st(b.ID))
	}

	// Архивный Epic неизменяем.
	arch, _ := s.CreateEpic(ctx, p.ID, "Arch", "")
	at, _ := s.CreateTask(ctx, arch.ID, NewTask{Title: "AT"})
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='archived' WHERE id=$1`, arch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskPlan(ctx, at.ID, PlanEdit{Title: strp("нет")}, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("архивный Epic должен отклоняться: %v", err)
	}
	// Лимит и план — одна транзакция: отклонённый план не меняет лимит.
	limit := 7
	if _, err := s.UpdateTaskPlan(ctx, c.ID, PlanEdit{AttemptLimit: &limit, Deps: slicep([]string{c.ID})}, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("самоссылка должна отклоняться: %v", err)
	}
	if tk, _ := s.GetTask(ctx, c.ID); tk.AttemptLimit == 7 {
		t.Fatal("лимит не должен примениться при отклонённой правке плана")
	}

	// Начатая задача не правится.
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='running' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	var bad domain.ErrBadTransition
	if _, err := s.UpdateTaskPlan(ctx, a.ID, PlanEdit{Title: strp("нет")}, "owner"); !errors.As(err, &bad) {
		t.Fatalf("running должен отклоняться: %v", err)
	}
}

// Удаление из чернового плана: рёбра сняты, история — страж.
func TestDeletePlannedTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-del", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "A"})
	b, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "B", Deps: []string{a.ID}})

	if err := s.DeletePlannedTask(ctx, a.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("задача должна исчезнуть: %v", err)
	}
	got, _ := s.GetTask(ctx, b.ID)
	if len(got.Deps) != 0 {
		t.Fatalf("рёбра должны сняться: %+v", got.Deps)
	}
	evs, _ := s.Events(ctx, EventFilter{EpicID: epic.ID, Type: "epic.plan_edited", Limit: 5})
	if len(evs) != 1 || evs[0].Payload["action"] != "deleted" {
		t.Fatalf("событие удаления: %+v", evs)
	}

	// Задача с историей (событие plan_edited ссылается на неё) — 409.
	if _, err := s.UpdateTaskPlan(ctx, b.ID, PlanEdit{Title: strp("B+")}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlannedTask(ctx, b.ID, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("история должна блокировать удаление: %v", err)
	}
	// Запущенный Epic — 409.
	c, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "C"})
	_ = c
	if err := s.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlannedTask(ctx, c.ID, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("запущенный Epic должен блокировать удаление: %v", err)
	}
}
