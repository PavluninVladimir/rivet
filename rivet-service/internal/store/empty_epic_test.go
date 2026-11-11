package store

import (
	"context"
	"errors"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func TestEmptyEpicPlan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	newEpic := func() domain.Epic {
		t.Helper()
		e, err := s.CreateEpic(ctx, p.ID, "Epic", "goal")
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	start := func(id string) error {
		return s.TransitionEpic(ctx, id, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: owner.Login, Type: "epic.status"})
	}
	t.Run("reject empty start and allow retry after adding a task", func(t *testing.T) {
		e := newEpic()
		if err := start(e.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("want conflict, got %v", err)
		}
		got, err := s.GetEpic(ctx, e.ID)
		if err != nil || got.Status != domain.EpicPlanned {
			t.Fatalf("status changed: %v, %v", got.Status, err)
		}
		var denied int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE epic_id=$1 AND type='epic.transition_denied'`, e.ID).Scan(&denied); err != nil {
			t.Fatal(err)
		}
		if denied != 1 {
			t.Fatalf("want one denied event, got %d", denied)
		}
		mustTask(t, s, e.ID, NewTask{Title: "A"})
		if err := start(e.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RecomputeEpic(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetEpic(ctx, e.ID)
		if err != nil || got.Status != domain.EpicRunning {
			t.Fatalf("want running, got %v, %v", got.Status, err)
		}
	})
	t.Run("legacy empty running epic does not complete", func(t *testing.T) {
		e := newEpic()
		if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, e.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RecomputeEpic(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetEpic(ctx, e.ID)
		if err != nil || got.Status != domain.EpicRunning {
			t.Fatalf("empty epic completed: %v, %v", got.Status, err)
		}
	})
	for _, status := range []string{"done", "cancelled"} {
		t.Run("nonempty terminal plan still completes "+status, func(t *testing.T) {
			e := newEpic()
			task := mustTask(t, s, e.ID, NewTask{Title: "A"})
			if err := start(e.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, task.ID, status); err != nil {
				t.Fatal(err)
			}
			if err := s.RecomputeEpic(ctx, e.ID); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetEpic(ctx, e.ID)
			if err != nil || got.Status != domain.EpicDone {
				t.Fatalf("want done, got %v, %v", got.Status, err)
			}
		})
	}
	for _, kind := range []string{domain.EpicMaintenance, domain.KindArchitecture} {
		t.Run("system epic can start without tasks "+kind, func(t *testing.T) {
			e := newEpic()
			if _, err := s.Pool.Exec(ctx, `UPDATE epics SET kind=$2 WHERE id=$1`, e.ID, kind); err != nil {
				t.Fatal(err)
			}
			if err := start(e.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
