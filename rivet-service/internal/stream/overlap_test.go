package stream

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Пересечение работ: событие session.overlap в timeline обеих задач,
// повтор той же пары не дублируется (спека team-visibility).
func TestEmitOverlaps(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "owner-ov", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	taskB, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B"})
	mk := func(taskID string) string {
		id, err := st.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "scheduler", Agent: "claude-code", Depth: domain.DepthFull, Scope: "CODING"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sesA, sesB := mk(taskA.ID), mk(taskB.ID)
	if err := st.AppendSessionFiles(ctx, sesA, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendSessionFiles(ctx, sesB, []string{"shared.go", "own.go"}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{St: st}
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go", "own.go"}); err != nil {
		t.Fatal(err)
	}
	// Повтор той же пары (ещё один шаг с тем же файлом) — без новых событий.
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{taskA.ID, taskB.ID} {
		evs, err := st.Events(ctx, store.EventFilter{TaskID: taskID, Type: "session.overlap", Limit: 10})
		if err != nil || len(evs) != 1 {
			t.Fatalf("task %s: %v %d событий", taskID, err, len(evs))
		}
		if files, _ := evs[0].Payload["files"].([]any); len(files) != 1 || files[0] != "shared.go" {
			t.Fatalf("общие файлы: %+v", evs[0].Payload)
		}
	}
	// Стороны ссылаются друг на друга.
	evsA, _ := st.Events(ctx, store.EventFilter{TaskID: taskA.ID, Type: "session.overlap", Limit: 10})
	if evsA[0].Payload["other_task_id"] != taskB.ID || evsA[0].Payload["session_id"] != sesA {
		t.Fatalf("payload стороны A: %+v", evsA[0].Payload)
	}
}
