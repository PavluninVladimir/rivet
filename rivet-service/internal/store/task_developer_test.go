package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func TestTaskDeveloperPinPausedPendingRunPreservesPlanAndDeleteConflict(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	enterCode(t, s, tasks, nil)
	id := tasks[0].ID
	before, _ := s.GetTask(ctx, id)
	if err := s.TransitionEpic(ctx, before.EpicID, domain.EpicPaused, EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDeveloper(ctx, id, "eva", "owner"); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetTask(ctx, id)
	if err != nil || after.DeveloperAgentID != "eva" || after.DeveloperAgentName != "Ева" || after.Status != before.Status ||
		!reflect.DeepEqual(after.Process, before.Process) || !reflect.DeepEqual(after.Deps, before.Deps) || after.AttemptUsed != 0 || after.StartedAt != nil {
		t.Fatalf("pin edited plan/process/history: %+v %v", after, err)
	}
	events, err := s.Events(ctx, EventFilter{TaskID: id, Type: "task.developer_changed"})
	if err != nil || len(events) != 1 || events[0].Payload["agent_id"] != "eva" {
		t.Fatalf("pin not audited: %+v %v", events, err)
	}
	if err := s.DeleteTeamAgent(ctx, "eva", "admin"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "закреплён") {
		t.Fatalf("pinned delete not readable conflict: %v", err)
	}
	if err := s.SetTaskDeveloper(ctx, id, "irina", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted nondeveloper: %v", err)
	}
	if err := s.SetTaskDeveloper(ctx, id, "", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeamAgent(ctx, "eva", "admin"); err != nil {
		t.Fatalf("clear pin did not release FK: %v", err)
	}
}

func TestTaskDeveloperPinHistoryCannotBeReset(t *testing.T) {
	for _, which := range []string{"attempt", "started", "finished-run", "session"} {
		t.Run(which, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			tasks := runningEpic(t, s, 1)
			id := tasks[0].ID
			var err error
			switch which {
			case "attempt":
				_, err = s.Pool.Exec(ctx, `UPDATE tasks SET attempt_used=1 WHERE id=$1`, id)
			case "started":
				_, err = s.Pool.Exec(ctx, `UPDATE tasks SET started_at=now() WHERE id=$1`, id)
			case "finished-run":
				enterCode(t, s, tasks, nil)
				_, err = s.Pool.Exec(ctx, `UPDATE task_step_runs SET verdict='ok',finished_at=now() WHERE task_id=$1`, id)
			case "session":
				_, err = s.Pool.Exec(ctx, `INSERT INTO sessions(task_id,driver_kind,agent) VALUES($1,'scheduler','rivet')`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.TaskDeveloperEditable(ctx, id); err != nil || ok {
				t.Fatalf("history was editable: %v %v", ok, err)
			}
			if err := s.SetTaskDeveloper(ctx, id, "eva", "owner"); !errors.Is(err, ErrConflict) {
				t.Fatalf("history pin accepted: %v", err)
			}
		})
	}
}

func TestTaskDeveloperPinLegacyAssignmentBlocked(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "legacy", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDeveloper(ctx, tasks[0].ID, "eva", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx, nil, nil); err != nil || ok {
		t.Fatalf("legacy assignment bypassed pin: %v %v", ok, err)
	}
	if err := s.SetTaskDeveloper(ctx, tasks[0].ID, "", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx, nil, nil); err != nil || !ok {
		t.Fatalf("auto legacy assignment changed: %v %v", ok, err)
	}
}

func TestTaskDeveloperPinConcurrentFirstAssignment(t *testing.T) {
	for range 5 {
		t.Run("race", func(t *testing.T) {
			s := testStore(t)
			tasks := runningEpic(t, s, 1)
			enterCode(t, s, tasks, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			changed := make(chan error, 1)
			type result struct {
				assignment RunAssignment
				ok         bool
				err        error
			}
			assigned := make(chan result, 1)
			go func() { <-start; changed <- s.SetTaskDeveloper(ctx, tasks[0].ID, "eva", "owner") }()
			go func() { <-start; a, ok, err := s.AssignRun(ctx, nil, nil); assigned <- result{a, ok, err} }()
			close(start)
			pinErr, got := <-changed, <-assigned
			if got.err != nil {
				t.Fatal(got.err)
			}
			if pinErr != nil && !errors.Is(pinErr, ErrConflict) {
				t.Fatal(pinErr)
			}
			if !got.ok {
				got.assignment, got.ok, got.err = s.AssignRun(ctx, nil, nil)
			}
			if got.err != nil || !got.ok {
				t.Fatalf("no assignment after race: %v %v", got.ok, got.err)
			}
			if pinErr == nil && got.assignment.Run.AgentID != "eva" {
				t.Fatalf("saved pin bypassed by concurrent assignment: %+v", got.assignment.Run)
			}
			if err := s.SetTaskDeveloper(ctx, tasks[0].ID, "maks", "owner"); !errors.Is(err, ErrConflict) {
				t.Fatalf("assigned task changed pin: %v", err)
			}
		})
	}
}
