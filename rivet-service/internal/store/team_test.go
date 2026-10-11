package store

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Командная видимость (спека team-visibility): реестр активных сессий,
// поиск по истории, пересечения по затронутым файлам.
func TestTeamSessions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-team", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := s.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "Кэширование каталога"})
	taskB, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "Индекс поиска"})

	mk := func(taskID, prompt string, depth domain.SessionDepth) string {
		t.Helper()
		id, err := s.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "scheduler", Agent: "claude-code", Depth: depth, Scope: "CODING", Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sesA := mk(taskA.ID, "Кэширование каталога\nСделать кэш горячих записей", domain.DepthFull)
	sesB := mk(taskB.ID, "Индекс поиска\nПостроить обратный индекс", domain.DepthFull)
	sesMin := mk(taskB.ID, "Минимальная", domain.DepthMinimal)

	if err := s.AppendSessionFiles(ctx, sesA, []string{"internal/cache.go", "go.mod"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSessionFiles(ctx, sesB, []string{"go.mod", "internal/index.go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionLastStep(ctx, sesA, "Edit internal/cache.go"); err != nil {
		t.Fatal(err)
	}

	// Реестр активных: три сессии, у полных глубин пересечение по go.mod,
	// у минимальной Overlaps == nil (недоступно ≠ пусто).
	reg, err := s.ActiveProjectSessions(ctx, p.ID, "owner-team")
	if err != nil || len(reg) != 3 {
		t.Fatalf("реестр: %v %d", err, len(reg))
	}
	byID := map[string]SessionEntry{}
	for _, e := range reg {
		byID[e.ID] = e
	}
	a := byID[sesA]
	if a.TaskNum != taskA.Num || a.TaskTitle != "Кэширование каталога" || a.LastStep != "Edit internal/cache.go" {
		t.Fatalf("запись реестра: %+v", a)
	}
	if len(a.Overlaps) != 1 || a.Overlaps[0].TaskID != taskB.ID || len(a.Overlaps[0].Files) != 1 || a.Overlaps[0].Files[0] != "go.mod" {
		t.Fatalf("пересечения A: %+v", a.Overlaps)
	}
	if b := byID[sesB]; len(b.Overlaps) != 1 || b.Overlaps[0].TaskID != taskA.ID {
		t.Fatalf("пересечения B: %+v", b.Overlaps)
	}
	if m := byID[sesMin]; m.Overlaps != nil || m.Files != nil {
		t.Fatalf("минимальная глубина: overlaps/files должны быть nil: %+v", m)
	}

	// OverlappingSessions: другой активной сессии той же задачи нет в hits.
	self, hits, err := s.OverlappingSessions(ctx, sesA, []string{"go.mod"})
	if err != nil || self.TaskID != taskA.ID || len(hits) != 1 || hits[0].SessionID != sesB {
		t.Fatalf("overlap hits: %v %+v %+v", err, self, hits)
	}

	// История и поиск: закрываем сессии с итогами.
	if _, err := s.EndSession(ctx, sesA, "", "изменения в ветке agent/task-1, кэш готов"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSession(ctx, sesB, "", "review вернул замечания"); err != nil {
		t.Fatal(err)
	}
	// По слову из запроса (FTS, словоформа отличается).
	found, err := s.SearchProjectSessions(ctx, p.ID, "кэш", "owner-team", 0)
	if err != nil || len(found) == 0 {
		t.Fatalf("поиск по запросу: %v %d", err, len(found))
	}
	for _, e := range found {
		if e.ID == sesB {
			t.Fatalf("поиск по «кэш» не должен находить сессию индекса: %+v", found)
		}
	}
	// По названию задачи (ILIKE-канал).
	found, err = s.SearchProjectSessions(ctx, p.ID, "Индекс", "owner-team", 0)
	if err != nil || len(found) < 2 { // sesB и sesMin — обе задачи B
		t.Fatalf("поиск по названию задачи: %v %d", err, len(found))
	}
	// По итогу.
	found, err = s.SearchProjectSessions(ctx, p.ID, "замечания", "owner-team", 0)
	if err != nil || len(found) != 1 || found[0].ID != sesB || found[0].Outcome != "review вернул замечания" {
		t.Fatalf("поиск по итогу: %v %+v", err, found)
	}
	// Активных больше двух нет: реестр после закрытия.
	reg, _ = s.ActiveProjectSessions(ctx, p.ID, "owner-team")
	if len(reg) != 1 || reg[0].ID != sesMin {
		t.Fatalf("реестр после закрытия: %+v", reg)
	}
}
