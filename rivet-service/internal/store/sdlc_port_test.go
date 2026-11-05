package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Ревью переноса add-sdlc-cycle на main с исследованием: Epic
// «Сопровождение» не участвует в зависимостях Epic'ов ни с одной стороны и
// не виден Аналитику как существующий Epic. Ревью переноса архитектуры
// (P2-1): то же для системного Epic'а «Архитектура проекта», в
// зависимостях участвуют только Epic'и вида work.
func TestMaintenanceEpicOutOfDeps(t *testing.T) {
	for _, c := range []struct{ kind, title string }{
		{"maintenance", "Сопровождение"},
		{"architecture", "Архитектура проекта"},
	} {
		t.Run(c.kind, func(t *testing.T) { systemEpicOutOfDeps(t, c.kind, c.title) })
	}
}

func systemEpicOutOfDeps(t *testing.T, kind, title string) {
	ctx := context.Background()
	s := testStore(t)
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var maint string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO epics (project_id, title, goal, status, kind)
		VALUES ($1,$2,'g','running',$3) RETURNING id::text`, p.ID, title, kind).Scan(&maint); err != nil {
		t.Fatal(err)
	}
	work, err := s.CreateEpic(ctx, p.ID, "CRM", "контакты")
	if err != nil {
		t.Fatal(err)
	}
	// Обе стороны зависимости отклоняются ошибкой поля.
	var fe *DiscoveryFieldError
	if _, err := s.SetEpicDeps(ctx, maint, []string{work.ID}); !errors.As(err, &fe) || fe.Field != "depends_on" {
		t.Fatalf("Сопровождение зависит от Epic'а: %v", err)
	}
	if _, err := s.SetEpicDeps(ctx, work.ID, []string{maint}); !errors.As(err, &fe) || fe.Field != "depends_on" {
		t.Fatalf("Epic зависит от Сопровождения: %v", err)
	}
	// Пустой список у Сопровождения разрешён: снимает старые зависимости.
	if _, err := s.SetEpicDeps(ctx, maint, nil); err != nil {
		t.Fatalf("снятие зависимостей Сопровождения: %v", err)
	}
	// Старые данные с любой стороны не держат ни Сопровождение, ни Epic.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO epic_deps (epic_id, depends_on) VALUES ($1,$2), ($2,$1)`, maint, work.ID); err != nil {
		t.Fatal(err)
	}
	w, err := s.WaitingDeps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(w[maint]) > 0 || len(w[work.ID]) > 0 {
		t.Fatalf("ожидание зависимостей с Сопровождением: %v", w)
	}
	// Консоль видит то же ожидание, что и планировщик.
	shown := []domain.Epic{{ID: maint, Kind: kind}, {ID: work.ID, Kind: domain.EpicWork}}
	if err := s.EnrichEpics(ctx, shown); err != nil {
		t.Fatal(err)
	}
	if len(shown[0].WaitingFor) > 0 || len(shown[1].WaitingFor) > 0 {
		t.Fatalf("ожидание в DTO Epic'а с системным Epic'ом: %+v %+v", shown[0].WaitingFor, shown[1].WaitingFor)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM epic_deps`); err != nil {
		t.Fatal(err)
	}
	// Аналитик Сопровождения не видит.
	ex, err := existingEpics(ctx, s.Pool, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ex {
		if e.ID == maint {
			t.Fatal("Сопровождение среди существующих Epic'ов для Аналитика")
		}
	}
	// Строка бэклога «Сопровождение» создаёт новый Epic, а не связь.
	res := syntheticResult()
	res.Epics[0].Title = title
	res.Epics[0].Goal = strings.Repeat("Поддержка работающего приложения. ", 3)
	r, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRunReady(ctx, r.ID, res, nil, nil); err != nil {
		t.Fatal(err)
	}
	ap, err := s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ap.Epics {
		if a.EpicID == maint || a.Action != "created" {
			t.Fatalf("строка бэклога связана с Сопровождением: %+v", ap.Epics)
		}
	}
	var deps int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM epic_deps WHERE epic_id=$1 OR depends_on=$1`, maint).Scan(&deps)
	if deps != 0 {
		t.Fatalf("зависимостей у Сопровождения после утверждения: %d", deps)
	}
}
