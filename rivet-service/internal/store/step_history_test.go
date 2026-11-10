package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// История переходов для графа процесса в деталке (fix-web-console-tails):
// только task.step своей задачи, последние limit в порядке времени, запрос
// идёт по частичному индексу events_task_step из 0044.
func TestTaskStepHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 2)
	task, other := tasks[0], tasks[1]
	ev := func(taskID, typ string, payload map[string]any) {
		t.Helper()
		if _, err := s.AppendEvent(ctx, EventInput{ActorKind: domain.ActorScheduler, Type: typ, TaskID: taskID, Text: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	step := func(taskID, from, outcome, next string) {
		ev(taskID, "task.step", map[string]any{"step": from, "outcome": outcome, "next": next, "verdicts": []any{}})
	}
	step(task.ID, "code", "ok", "test")
	ev(task.ID, "session.step", map[string]any{"tool": "read_file"})
	step(task.ID, "test", "ok", "review")
	step(other.ID, "code", "fail", "escalate")
	step(task.ID, "review", "changes", "code")
	step(task.ID, "code", "ok", "test")

	got := func(limit int) string {
		t.Helper()
		h, err := s.TaskStepHistory(ctx, task.ID, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range h {
			if x.TS.IsZero() {
				t.Fatalf("переход без времени: %+v", x)
			}
			out = append(out, fmt.Sprintf("%s:%s>%s", x.Step, x.Outcome, x.Next))
		}
		return strings.Join(out, " ")
	}
	if g := got(500); g != "code:ok>test test:ok>review review:changes>code code:ok>test" {
		t.Fatalf("история задачи: %s", g)
	}
	// Лимит отдаёт хвост: текущий проход важнее начала.
	if g := got(2); g != "review:changes>code code:ok>test" {
		t.Fatalf("последние два перехода: %s", g)
	}
	if h, err := s.TaskStepHistory(ctx, tasks[1].ID, 500); err != nil || len(h) != 1 || h[0].Next != "escalate" {
		t.Fatalf("история другой задачи: %+v %v", h, err)
	}

	// Частичный индекс миграции 0044 с тем же предикатом, что в запросе.
	var def string
	if err := s.Pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'events_task_step'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "(task_id, id)") || !strings.Contains(def, "'task.step'") {
		t.Fatalf("индекс events_task_step: %s", def)
	}
}
