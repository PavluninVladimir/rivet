package store

import (
	"context"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/history"
)

// Импорт истории проекта (спека domain-model «Импорт истории проекта»):
// выполненные Epic'и и задачи с исходными датами, повтор без дубликатов,
// история не видна планировщику и оценке.
func TestImportHistory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-hist", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	created := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	done := time.Date(2026, 8, 12, 15, 30, 0, 0, time.UTC)
	m := history.Manifest{Source: "openspec", Epics: []history.Epic{{
		Key: "2026-08-12-harden-core", Title: "Ядро", Goal: "надёжность", CreatedAt: created, DoneAt: done,
		Tasks: []history.Task{
			{Title: "1.1 Попытки", Done: true, Repo: "rivet", PRURL: "https://gh/r/pull/8"},
			{Title: "1.2 Не успели", Done: false, Repo: "rivet"},
		},
	}}}
	res, err := s.ImportHistory(ctx, p.ID, m, "owner-hist")
	if err != nil {
		t.Fatal(err)
	}
	if res.EpicsCreated != 1 || res.TasksCreated != 2 || res.EpicsUpdated != 0 {
		t.Fatalf("итог импорта: %+v", res)
	}
	epics, err := s.ListEpics(ctx, p.ID)
	if err != nil || len(epics) != 1 {
		t.Fatalf("Epic'и: %v %d", err, len(epics))
	}
	e := epics[0]
	if e.Status != domain.EpicDone || e.SourceKey != "2026-08-12-harden-core" || !e.Created.Equal(created) {
		t.Fatalf("Epic из истории: %+v", e)
	}
	tasks, err := s.ListEpicTasks(ctx, e.ID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("задачи: %v %d", err, len(tasks))
	}
	if tasks[0].Status != domain.TaskDone || tasks[0].PRURL != "https://gh/r/pull/8" {
		t.Fatalf("выполненная задача: %+v", tasks[0])
	}
	if tasks[1].Status != domain.TaskCancelled || tasks[1].Description == "" {
		t.Fatalf("невыполненная задача: %+v", tasks[1])
	}
	// События — с исходными датами, а не now().
	evs, err := s.Events(ctx, EventFilter{ProjectID: p.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var statusEvents, imported int
	for _, ev := range evs {
		if ev.Type == "task.status" || ev.Type == "epic.status" {
			statusEvents++
			if !ev.TS.Equal(done) {
				t.Fatalf("дата события истории: %v, ожидали %v", ev.TS, done)
			}
			if ev.Payload["imported"] != true {
				t.Fatalf("метка импорта в payload: %+v", ev.Payload)
			}
		}
		if ev.Type == "history.imported" {
			imported++
		}
	}
	if statusEvents != 3 || imported != 1 {
		t.Fatalf("событий статусов %d, итогов импорта %d", statusEvents, imported)
	}

	// Повторный импорт с правкой названия: обновление, не дубликат, без
	// новых событий.
	m.Epics[0].Title = "Ядро конвейера"
	res, err = s.ImportHistory(ctx, p.ID, m, "owner-hist")
	if err != nil {
		t.Fatal(err)
	}
	if res.EpicsCreated != 0 || res.EpicsUpdated != 1 || res.TasksCreated != 0 || res.TasksUpdated != 2 {
		t.Fatalf("повторный импорт: %+v", res)
	}
	epics, _ = s.ListEpics(ctx, p.ID)
	if len(epics) != 1 || epics[0].Title != "Ядро конвейера" {
		t.Fatalf("после повтора: %+v", epics)
	}
	evs, _ = s.Events(ctx, EventFilter{ProjectID: p.ID, Type: "task.status", Limit: 20})
	if len(evs) != 2 {
		t.Fatalf("повтор не должен плодить события задач: %d", len(evs))
	}

	// Лента без курсора отдаёт последние события, а не первые по id:
	// иначе после импорта истории живая активность до экрана не доходит.
	latest, err := s.Events(ctx, EventFilter{ProjectID: p.ID, Limit: 2, Latest: true})
	if err != nil || len(latest) != 2 {
		t.Fatalf("последние события: %v %d", err, len(latest))
	}
	if latest[1].Type != "history.imported" || latest[0].ID >= latest[1].ID {
		t.Fatalf("ожидали два последних события по возрастанию id: %s, %s", latest[0].Type, latest[1].Type)
	}
	first, _ := s.Events(ctx, EventFilter{ProjectID: p.ID, Limit: 2})
	if len(first) != 2 || first[0].ID >= latest[0].ID {
		t.Fatalf("без Latest порядок с начала: %+v", first)
	}

	// История не видна планировщику: назначать нечего.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r-hist", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx, nil, nil); err != nil || ok {
		t.Fatalf("история не должна назначаться: %v %v", err, ok)
	}
	// И не входит в оценку стоимости: done-задачи истории без usage не
	// делают историю «источником».
	est, err := s.EpicCostEstimate(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if est.Available {
		t.Fatalf("история не должна становиться источником оценки: %+v", est)
	}
}
