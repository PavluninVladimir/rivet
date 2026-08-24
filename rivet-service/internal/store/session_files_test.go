package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Затронутые файлы сессии: NULL у минимальной глубины против [] у полной,
// дедупликация и порядок первого появления (спека agent-integration).
func TestSessionFiles(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-files", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := s.CreateEpic(ctx, p.ID, "E", "")
	task, err := s.CreateTask(ctx, epic.ID, NewTask{Title: "T"})
	if err != nil {
		t.Fatal(err)
	}

	full, err := s.CreateSession(ctx, domain.Session{TaskID: task.ID, Attempt: 1,
		DriverKind: "scheduler", Agent: "claude-code", Depth: domain.DepthFull, Scope: "CODING"})
	if err != nil {
		t.Fatal(err)
	}
	minimal, err := s.CreateSession(ctx, domain.Session{TaskID: task.ID, Attempt: 1,
		DriverKind: "scheduler", Agent: "fake", Depth: domain.DepthMinimal, Scope: "REVIEW"})
	if err != nil {
		t.Fatal(err)
	}

	// Дедупликация с сохранением порядка первого появления.
	for _, batch := range [][]string{{"b.go", "a.go"}, {"a.go", "c.go", "b.go"}} {
		if err := s.AppendSessionFiles(ctx, full, batch); err != nil {
			t.Fatal(err)
		}
	}
	// Файлы в сессию минимальной глубины не пишутся (files IS NULL).
	if err := s.AppendSessionFiles(ctx, minimal, []string{"x.go"}); err != nil {
		t.Fatal(err)
	}

	sessions, err := s.ListTaskSessions(ctx, task.ID, "")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("%v %d", err, len(sessions))
	}
	byID := map[string]domain.Session{sessions[0].ID: sessions[0], sessions[1].ID: sessions[1]}
	f := byID[full]
	if f.Depth != domain.DepthFull || fmt.Sprint(f.Files) != "[b.go a.go c.go]" {
		t.Fatalf("files полной глубины: %+v", f.Files)
	}
	m := byID[minimal]
	if m.Depth != domain.DepthMinimal || m.Files != nil {
		t.Fatalf("минимальная глубина: files должен быть nil, got %+v", m.Files)
	}

	// Кап на количество путей.
	many := make([]string, 600)
	for i := range many {
		many[i] = fmt.Sprintf("gen/f%03d.go", i)
	}
	if err := s.AppendSessionFiles(ctx, full, many); err != nil {
		t.Fatal(err)
	}
	sessions, _ = s.ListTaskSessions(ctx, task.ID, "")
	for _, v := range sessions {
		if v.ID == full && len(v.Files) != sessionFilesCap {
			t.Fatalf("кап файлов: %d", len(v.Files))
		}
	}
}
