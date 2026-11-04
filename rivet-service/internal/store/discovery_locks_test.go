package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Регрессии второго круга ревью Opus: замки исследования против
// планировщика, отметка плана в бэклоге, NUL в правке бэклога.

func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// Планировщик держит строку Epic'а и пишет событие (FK events.project_id
// берёт KEY SHARE на проект). Правка зависимостей того же Epic'а берёт
// проект, затем Epic: дедлока нет, правка дожидается планировщика.
func TestSetDepsNoDeadlockWithScheduler(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	E, _ := s.CreateEpic(ctx, p.ID, "E", "")
	X, _ := s.CreateEpic(ctx, p.ID, "X", "")

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT project_id, status FROM epics WHERE id=$1 FOR UPDATE`, E.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, derr := s.SetEpicDeps(ctx, E.ID, []string{X.ID})
		done <- derr
	}()
	time.Sleep(300 * time.Millisecond)
	_, evErr := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "task.status",
		ProjectID: p.ID, EpicID: E.ID, Text: "ready"})
	if evErr == nil {
		evErr = tx.Commit(ctx)
	}
	derr := <-done
	if isDeadlock(evErr) || isDeadlock(derr) || evErr != nil || derr != nil {
		t.Fatalf("планировщик: %v, правка зависимостей: %v", evErr, derr)
	}
}

// Утверждение: новый Epic зависит от Epic'а, который держит планировщик.
func TestApproveNoDeadlockWithScheduler(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	E, _ := s.CreateEpic(ctx, p.ID, "Существующий Epic проекта", "")
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	res := syntheticResult()
	res.Epics[0].DependsOn = []string{E.ID}
	if _, err := s.FinishRunReady(ctx, r.ID, res, nil, nil); err != nil {
		t.Fatal(err)
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT project_id, status FROM epics WHERE id=$1 FOR UPDATE`, E.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, aerr := s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, false)
		done <- aerr
	}()
	time.Sleep(300 * time.Millisecond)
	_, evErr := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "task.status",
		ProjectID: p.ID, EpicID: E.ID, Text: "ready"})
	if evErr == nil {
		evErr = tx.Commit(ctx)
	}
	aerr := <-done
	if isDeadlock(evErr) || isDeadlock(aerr) || evErr != nil || aerr != nil {
		t.Fatalf("планировщик: %v, утверждение: %v", evErr, aerr)
	}
}

// Замок исследования не задерживает запись событий проекта.
func TestDiscoveryLockDoesNotBlockEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	tx, _ := s.Pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockProjectDiscovery(ctx, tx, p.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := s.AppendEvent(cctx, EventInput{ActorKind: domain.ActorSystem, Type: "x", ProjectID: p.ID}); err != nil {
		t.Fatalf("событие ждёт замка исследования: %v", err)
	}
}

// FinishRunReady и удаление интервью идут под одним замком проекта:
// удаление ждёт, дедлока нет. Транзакция ниже повторяет порядок
// FinishRunReady: проект, затем версии.
func TestFinishReadyAndDeleteInterviewSerialized(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	iv, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "Менеджер", Text: "**Р:** путаюсь в дублях"})
	if err != nil {
		t.Fatal(err)
	}
	set := []RunInterview{{ID: iv.ID, Title: iv.Title, SHA256: iv.SHA256}}
	y, _ := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if _, err := s.FinishRunReady(ctx, y.ID, syntheticResult(), nil, nil); err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.Pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockProjectDiscovery(ctx, tx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE discovery_runs SET status='ready', stage='done' WHERE id=$1 AND status='running'`, x.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, derr := s.DeleteInterview(ctx, p.ID, iv.ID)
		done <- derr
	}()
	time.Sleep(300 * time.Millisecond)
	_, ferr := tx.Exec(ctx, `UPDATE discovery_runs SET status='superseded' WHERE project_id=$1 AND version < $2 AND status IN ('ready','failed')`, p.ID, x.Version)
	if ferr == nil {
		ferr = tx.Commit(ctx)
	}
	derr := <-done
	if isDeadlock(ferr) || isDeadlock(derr) || ferr != nil || derr != nil {
		t.Fatalf("FinishRunReady: %v, удаление интервью: %v", ferr, derr)
	}
}

// План и отметка «план готов» в строке бэклога пишутся одной
// транзакцией: рестарт после плана не делает строку failed.
func TestPlanMarksBacklogRowDone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, _ := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if _, err := s.FinishRunReady(ctx, r.ID, syntheticResult(), nil, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	key := res.Order[0]
	var epicID string
	for _, a := range res.Epics {
		if a.Key == key {
			epicID = a.EpicID
		}
	}
	_ = s.SetRunDecompose(ctx, r.ID, key, "running", "")
	if _, err := s.CreatePlan(ctx, epicID, []PlanTask{{NewTask: NewTask{Title: "A"}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, p.ID, r.ID)
	if got.Decompose[key].Status != "done" {
		t.Fatalf("строка после плана: %+v", got.Decompose[key])
	}
	// Страховка рестарта: строка running у Epic'а с планом становится done.
	_ = s.SetRunDecompose(ctx, r.ID, key, "running", "")
	if _, err := s.FailInterruptedDecompositions(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun(ctx, p.ID, r.ID)
	if got.Decompose[key].Status != "done" {
		t.Fatalf("строка с планом после рестарта: %+v", got.Decompose[key])
	}
}

// Цитата версии в цели строки бэклога: сохранение и утверждение
// отклоняются ошибкой поля, владелец правит строку до утверждения.
func TestBacklogQuoteLeakBlocksSaveAndApprove(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, _ := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	res := syntheticResult()
	quote := "клиент просил набрать во вторник после совещания, я набрал в четверг"
	res.Needs[0].Evidence[0].Text = quote
	res.Epics[0].Goal = "Сделать так, чтобы " + quote + " больше не повторялось"
	if _, err := s.FinishRunReady(ctx, r.ID, res, nil, nil); err != nil {
		t.Fatal(err)
	}
	var fe *DiscoveryFieldError
	if _, err := s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, false); !errors.As(err, &fe) || fe.Field != "epics[0].goal" {
		t.Fatalf("утверждение с цитатой в цели: %v", err)
	}
	if _, err := s.SaveBacklog(ctx, p.ID, r.ID, 1, res.Epics); !errors.As(err, &fe) || fe.Field != "epics[0].goal" {
		t.Fatalf("сохранение с цитатой в цели: %v", err)
	}
	res.Epics[0].Goal = "Менеджер видит, кому перезвонить сегодня, и не теряет сделки из-за забытых звонков."
	if _, err := s.SaveBacklog(ctx, p.ID, r.ID, 1, res.Epics); err != nil {
		t.Fatalf("сохранение пересказа: %v", err)
	}
}

// NUL в правке бэклога — ошибка поля, а не сбой jsonb.
func TestSaveBacklogNUL(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, _ := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if _, err := s.FinishRunReady(ctx, r.ID, syntheticResult(), nil, nil); err != nil {
		t.Fatal(err)
	}
	epics := syntheticResult().Epics
	epics[0].Title = "Компании\x00контакты"
	_, err := s.SaveBacklog(ctx, p.ID, r.ID, 1, epics)
	var fe *DiscoveryFieldError
	if !errors.As(err, &fe) || fe.Field != "epics[0].title" {
		t.Fatalf("NUL в бэклоге: %v", err)
	}
}
