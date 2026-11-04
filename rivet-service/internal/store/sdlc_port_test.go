package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Ревью переноса add-sdlc-cycle на main с исследованием: Epic
// «Сопровождение» не участвует в зависимостях Epic'ов ни с одной стороны и
// не виден Аналитику как существующий Epic.
func TestMaintenanceEpicOutOfDeps(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var maint string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO epics (project_id, title, goal, status, kind)
		VALUES ($1,'Сопровождение','g','running','maintenance') RETURNING id::text`, p.ID).Scan(&maint); err != nil {
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
	res.Epics[0].Title = "Сопровождение"
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
