package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/overview"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Время задач и Epic'ов проставляется в транзакции смены статуса
// (add-project-lead-view, задачи 1.1 и 1.2): started_at — первое
// назначение, повторные назначения его не перезаписывают.
func TestOverviewTimesOnTransitions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "E", "")
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, s, e.ID, NewTask{Title: "A"})
	get := func() domain.Task {
		t.Helper()
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	if got := get(); got.StartedAt != nil || got.DoneAt != nil {
		t.Fatal("у новой задачи нет времени начала и завершения")
	}
	exec(`UPDATE tasks SET status='running' WHERE id=$1`, task.ID)
	first := get().StartedAt
	if first == nil {
		t.Fatal("переход в running ставит started_at")
	}
	exec(`UPDATE tasks SET status='ready' WHERE id=$1`, task.ID)
	exec(`UPDATE tasks SET status='running' WHERE id=$1`, task.ID)
	if again := get().StartedAt; again == nil || !again.Equal(*first) {
		t.Fatalf("повторное назначение перезаписало started_at: %v → %v", first, again)
	}
	exec(`UPDATE tasks SET status='done' WHERE id=$1`, task.ID)
	if get().DoneAt == nil {
		t.Fatal("переход в done ставит done_at")
	}

	// Ожидание runner'а: момент начала ставится один раз за серию.
	other := mustTask(t, s, e.ID, NewTask{Title: "B"})
	var since1, since2 *time.Time
	exec(`UPDATE tasks SET wait_reason='нет runner''а' WHERE id=$1`, other.ID)
	_ = s.Pool.QueryRow(ctx, `SELECT wait_since FROM tasks WHERE id=$1`, other.ID).Scan(&since1)
	exec(`UPDATE tasks SET wait_reason='нет свободного агента роли' WHERE id=$1`, other.ID)
	_ = s.Pool.QueryRow(ctx, `SELECT wait_since FROM tasks WHERE id=$1`, other.ID).Scan(&since2)
	if since1 == nil || since2 == nil || !since1.Equal(*since2) {
		t.Fatalf("смена текста причины не сбрасывает начало ожидания: %v → %v", since1, since2)
	}
	exec(`UPDATE tasks SET wait_reason='' WHERE id=$1`, other.ID)
	_ = s.Pool.QueryRow(ctx, `SELECT wait_since FROM tasks WHERE id=$1`, other.ID).Scan(&since2)
	if since2 != nil {
		t.Fatal("снятая причина снимает начало ожидания")
	}

	exec(`UPDATE epics SET status='running' WHERE id=$1`, e.ID)
	ge, _ := s.GetEpic(ctx, e.ID)
	if ge.StartedAt == nil {
		t.Fatal("переход Epic'а в running ставит started_at")
	}
	exec(`UPDATE epics SET status='done' WHERE id=$1`, e.ID)
	var done *time.Time
	_ = s.Pool.QueryRow(ctx, `SELECT done_at FROM epics WHERE id=$1`, e.ID).Scan(&done)
	if done == nil {
		t.Fatal("переход Epic'а в done ставит done_at")
	}
}

// Миграция 0030 заполняет время по event log на базе с историей
// (задача 1.1). Повторный прогон файла миграции идемпотентен.
func TestOverviewMigrationBackfill(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	task := mustTask(t, s, e.ID, NewTask{Title: "A"})
	t1 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(3 * time.Hour)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE tasks SET status='done' WHERE id=$1`, []any{task.ID}},
		{`UPDATE epics SET status='done' WHERE id=$1`, []any{e.ID}},
		{`UPDATE tasks SET started_at=NULL, done_at=NULL WHERE id=$1`, []any{task.ID}},
		{`UPDATE epics SET started_at=NULL, done_at=NULL WHERE id=$1`, []any{e.ID}},
		{`INSERT INTO events (ts, actor_kind, type, project_id, epic_id, task_id, payload) VALUES
			($1,'scheduler','task.status',$3,$4,$5,'{"status":"running"}'),
			($2,'scheduler','task.status',$3,$4,$5,'{"status":"done"}'),
			($1,'user','epic.status',$3,$4,NULL,'{"status":"running"}'),
			($2,'scheduler','epic.status',$3,$4,NULL,'{"status":"done"}'),
			($2,'user','epic.status',$3,$4,NULL,'{"token_budget":100}')`, []any{t1, t2, p.ID, e.ID, task.ID}},
	} {
		if _, err := s.Pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("migrations/0030_project_overview.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("прогон миграции %d: %v", i+1, err)
		}
	}
	var ts, td, es, ed *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT t.started_at, t.done_at, e.started_at, e.done_at
		FROM tasks t JOIN epics e ON e.id = t.epic_id WHERE t.id=$1`, task.ID).Scan(&ts, &td, &es, &ed); err != nil {
		t.Fatal(err)
	}
	if ts == nil || !ts.Equal(t1) || td == nil || !td.Equal(t2) || es == nil || !es.Equal(t1) || ed == nil || !ed.Equal(t2) {
		t.Fatalf("заполнение по событиям: задача %v..%v, Epic %v..%v", ts, td, es, ed)
	}
}

// Снимок обзора на фикстуре проекта (задача 1.6): все разделы собраны из
// одной транзакции, роль сессии и записи usage — из запуска шага.
func TestOverviewInputFixture(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "CRM", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.CreateEpic(ctx, p.ID, "Компании", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A", Estimate: 2})
	b := mustTask(t, s, e.ID, NewTask{Title: "B", Estimate: 3})
	c := mustTask(t, s, e.ID, NewTask{Title: "C", Estimate: 1})
	now := time.Now().UTC().Truncate(time.Second)
	h := func(n float64) time.Time { return now.Add(time.Duration(n * float64(time.Hour))) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`UPDATE epics SET status='running' WHERE id=$1`, e.ID)
	exec(`UPDATE tasks SET status='done', started_at=$2, done_at=$3 WHERE id=$1`, a.ID, h(-5), h(-4))
	exec(`UPDATE tasks SET status='running', wait_reason='нет runner''а' WHERE id=$1`, b.ID)
	// C ждёт подтверждения merge: шаг merge в снимке процесса.
	exec(`UPDATE tasks SET status='review', step_id='merge',
		process='{"steps":[{"id":"code","kind":"code"},{"id":"merge","kind":"merge"}]}' WHERE id=$1`, c.ID)
	exec(`INSERT INTO events (ts, actor_kind, type, project_id, epic_id, task_id) VALUES ($1,'scheduler','task.review_passed',$2,$3,$4)`,
		h(-1), p.ID, e.ID, c.ID)
	// Сессия разработчика по A и запись usage с этой сессией.
	var sid string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO sessions (task_id, driver_kind, agent, started_at, ended_at)
		VALUES ($1,'scheduler','rivet',$2,$3) RETURNING id`, a.ID, h(-5), h(-4)).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, session_id, verdict, role, finished_at)
		VALUES ($1,'code','code','p1',$2,'ok','developer',$3)`, a.ID, sid, h(-4))
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, verdict, role, finished_at)
		VALUES ($1,'review','review','p1','ok','reviewer',$2)`, a.ID, h(-4))
	cost := 1.25
	in1, out1 := int64(1000), int64(200)
	if err := s.RecordUsage(ctx, UsageInput{SourceMsgID: "m1", SessionID: sid, ProjectID: p.ID, EpicID: e.ID,
		TaskID: a.ID, Model: "deepseek-v4-flash", TokensIn: &in1, TokensOut: &out1, CostUSD: &cost, DurationS: 60}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, UsageInput{SourceMsgID: "m2", ProjectID: p.ID, EpicID: e.ID,
		TaskID: b.ID, Model: "deepseek-v4-flash", TokensIn: &in1, TokensOut: &out1, DurationS: 30}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO attention (project_id, task_id, reason, message) VALUES ($1,$2,'BLOCKED','вопрос')`, p.ID, b.ID)
	exec(`INSERT INTO events (ts, actor_kind, type, project_id, epic_id, task_id, payload)
		VALUES ($1,'scheduler','task.merge_conflict',$2,$3,$4,'{"exhausted":false}')`, h(-2), p.ID, e.ID, a.ID)
	var envID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO environments (project_id, name) VALUES ($1,'Демо') RETURNING id`, p.ID).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO deployments (env_id, version, status, created_at, ended_at) VALUES ($1,'abc123','done',$2,$3)`, envID, h(-3), h(-2.9))
	if _, err := s.SaveProjectPolicy(ctx, p.ID, policy.Overrides{MonthlyBudgetUSD: ptrF(100)}, owner.Login); err != nil {
		t.Fatal(err)
	}

	loc := time.UTC
	per, _ := overview.NewPeriod("7d", now, loc)
	in, err := s.OverviewInput(ctx, OverviewQuery{ProjectID: p.ID, UserID: owner.ID, Login: owner.Login, Now: now, Period: per, Loc: loc})
	if err != nil {
		t.Fatal(err)
	}
	if in.ProjectName != "CRM" || len(in.Epics) != 1 || len(in.Tasks) != 3 {
		t.Fatalf("Epic'и и задачи: %q %d %d", in.ProjectName, len(in.Epics), len(in.Tasks))
	}
	if len(in.Sessions) != 1 || in.Sessions[0].RoleID != "developer" || in.Sessions[0].Human {
		t.Fatalf("сессии: %+v", in.Sessions)
	}
	roles := map[string]string{}
	for _, u := range in.Usage {
		roles[u.TaskID] = u.RoleID
	}
	if roles[a.ID] != "developer" || roles[b.ID] != "" {
		t.Fatalf("роль записи usage по сессии: %v", roles)
	}
	if len(in.MergeWaits) != 1 || in.MergeWaits[0].TaskID != c.ID || !in.MergeWaits[0].Since.Equal(h(-1)) {
		t.Fatalf("ожидание merge: %+v", in.MergeWaits)
	}
	if len(in.Escalations) != 1 || len(in.MergeConflicts) != 1 || len(in.StepRuns) != 2 {
		t.Fatalf("эскалации %d, конфликты %d, запуски %d", len(in.Escalations), len(in.MergeConflicts), len(in.StepRuns))
	}
	if len(in.Environments) != 1 || in.Environments[0].Last == nil || in.Environments[0].Last.Version != "abc123" {
		t.Fatalf("окружения: %+v", in.Environments)
	}
	if in.MonthlyBudgetUSD == nil || *in.MonthlyBudgetUSD != 100 {
		t.Fatalf("плановый бюджет месяца: %v", in.MonthlyBudgetUSD)
	}
	var waiting *time.Time
	for _, tk := range in.Tasks {
		if tk.ID == b.ID {
			waiting = tk.WaitingRunnerSince
		}
	}
	if waiting == nil {
		t.Fatal("задача с причиной ожидания несёт начало ожидания")
	}

	ov := overview.Build(in)
	if ov.Epics[0].Progress.DoneTasks != 1 || ov.Decisions.Counts.Escalation != 1 || ov.Decisions.Counts.MergeConfirmation != 1 {
		t.Fatalf("обзор: прогресс %+v, решения %+v", ov.Epics[0].Progress, ov.Decisions.Counts)
	}
	if ov.Team.Quality.MergeConflicts.Returned != 1 {
		t.Fatalf("конфликты merge: %+v", ov.Team.Quality.MergeConflicts)
	}
}

func ptrF(v float64) *float64 { return &v }

// Стоимость записи usage (design, решение 5): ноль у модели без прайса —
// «без стоимости», ноль у модели с ценой и положительная стоимость
// известны, пустая — неизвестна. То же правило в расходе Epic'а.
func TestOverviewCostKnown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "CRM", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	if _, err := s.Pool.Exec(ctx, `INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
		VALUES ('ds', 'DeepSeek', 'vendor', 'openai', 'https://api.example',
		        '[{"id":"priced","input_price":270000,"output_price":1100000},{"id":"listed"}]', $1)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model string
		cost  *float64
		known bool
	}{
		{"priced", ptrF(0), true},
		{"listed", ptrF(0), false},
		{"unknown", ptrF(0), false},
		{"unknown", ptrF(0.5), true},
		{"priced", nil, false},
	}
	for i, c := range cases {
		if err := s.RecordUsage(ctx, UsageInput{SourceMsgID: "m" + string(rune('a'+i)), ProjectID: p.ID, EpicID: e.ID,
			TaskID: a.ID, Model: c.model, CostUSD: c.cost, DurationS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	per, _ := overview.NewPeriod("7d", now, time.UTC)
	in, err := s.OverviewInput(ctx, OverviewQuery{ProjectID: p.ID, Now: now, Period: per, Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Usage) != len(cases) {
		t.Fatalf("записей usage: %d", len(in.Usage))
	}
	for i, c := range cases {
		if in.Usage[i].Model != c.model || in.Usage[i].CostKnown != c.known {
			t.Fatalf("запись %d (%s): стоимость известна %v, ждали %v", i, in.Usage[i].Model, in.Usage[i].CostKnown, c.known)
		}
	}
	if sp := in.Epics[0].Spend; sp.Records != 5 || sp.CostRecords != 2 {
		t.Fatalf("расход Epic'а: записей %d, со стоимостью %d", sp.Records, sp.CostRecords)
	}
}

// Целевая дата Epic'а: событие с прежним и новым значением, архивный Epic
// не правится (задача 1.3).
func TestPatchEpicDueAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	due := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	got, err := s.PatchEpic(ctx, e.ID, EpicPatch{SetDue: true, Due: &due}, owner.Login)
	if err != nil || got.DueAt == nil || !got.DueAt.Equal(due) {
		t.Fatalf("целевая дата: %+v %v", got.DueAt, err)
	}
	var before, after any
	if err := s.Pool.QueryRow(ctx, `SELECT payload->'before', payload->'after' FROM events
		WHERE epic_id=$1 AND type='epic.due_changed' ORDER BY id DESC LIMIT 1`, e.ID).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if before != nil || after != "2026-12-01T09:00:00Z" {
		t.Fatalf("событие: before=%v after=%v", before, after)
	}
	if got, err = s.PatchEpic(ctx, e.ID, EpicPatch{SetDue: true, Due: nil}, owner.Login); err != nil || got.DueAt != nil {
		t.Fatalf("снятие даты: %+v %v", got.DueAt, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='archived' WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchEpic(ctx, e.ID, EpicPatch{SetDue: true, Due: &due}, owner.Login); !errors.Is(err, ErrConflict) {
		t.Fatalf("архивный Epic: %v", err)
	}
	if _, err := s.PatchEpic(ctx, "00000000-0000-0000-0000-000000000000", EpicPatch{SetDue: true, Due: &due}, owner.Login); !errors.Is(err, ErrNotFound) {
		t.Fatalf("неизвестный Epic: %v", err)
	}
}

// Плановый бюджет месяца — свойство проекта: политика установки с ним
// отклоняется с полем monthly_budget_usd.
func TestInstallationRejectsMonthlyBudget(t *testing.T) {
	s := testStore(t)
	p := policy.Defaults()
	p.MonthlyBudgetUSD = ptrF(100)
	_, err := s.SaveInstallationPolicy(context.Background(), p, "admin")
	var pe *policy.ProcessError
	if !errors.As(err, &pe) || pe.Field != "monthly_budget_usd" {
		t.Fatalf("политика установки с бюджетом месяца: %v", err)
	}
}
