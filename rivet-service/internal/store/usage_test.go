package store

import (
	"context"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func ptr[T any](v T) *T { return &v }

// seedUsage — владелец/проект/epic/две задачи для usage-тестов.
func seedUsage(t *testing.T, s *Store) (owner domain.User, projectID, epicID string, taskA, taskB domain.Task) {
	t.Helper()
	ctx := context.Background()
	owner = mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "Epic", "цель")
	if err != nil {
		t.Fatal(err)
	}
	return owner, p.ID, e.ID, mustTask(t, s, e.ID, NewTask{Title: "A"}), mustTask(t, s, e.ID, NewTask{Title: "B"})
}

// Идемпотентность billing-grade: повтор source_msg_id не удваивает счёт,
// nullable-поля сохраняются как NULL (спека monetization «Метеринг…»).
func TestRecordUsageIdempotentNullable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, projectID, epicID, task, _ := seedUsage(t, s)

	in := UsageInput{
		SourceMsgID: "msg-1", ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		RunnerID: "r1", Model: "m", TokensIn: ptr(int64(100)), DurationS: 5,
	}
	if err := s.RecordUsage(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, in); err != nil {
		t.Fatal(err)
	}
	var n int
	var tokensOut *int64
	if err := s.Pool.QueryRow(ctx,
		`SELECT COUNT(*), MIN(tokens_out) FROM usage_records WHERE source_msg_id='msg-1'`).
		Scan(&n, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("повтор source_msg_id: строк %d, ожидалась 1", n)
	}
	if tokensOut != nil {
		t.Fatalf("tokens_out должен остаться NULL, получено %d", *tokensOut)
	}
}

// NULL не смешивается с нулём: группа без данных о токенах отдаёт null,
// смешанная группа суммирует только известные значения (спека observability).
func TestUsageSummaryNullAggregation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, projectID, epicID, taskA, taskB := seedUsage(t, s)

	rec := func(msgID, taskID string, in *int64, cost *float64) {
		t.Helper()
		if err := s.RecordUsage(ctx, UsageInput{
			SourceMsgID: msgID, ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			RunnerID: "r1", Model: "m", TokensIn: in, CostUSD: cost, DurationS: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec("m1", taskA.ID, ptr(int64(100)), ptr(0.5))
	rec("m2", taskA.ID, nil, nil) // агент не отчитался
	rec("m3", taskB.ID, nil, nil) // по задаче B данных нет вовсе

	rows, err := s.UsageSummary(ctx, owner.ID, "task", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]UsageRow{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	a, b := byKey[taskA.ID], byKey[taskB.ID]
	if a.TokensIn == nil || *a.TokensIn != 100 || a.CostUSD == nil || *a.CostUSD != 0.5 {
		t.Fatalf("задача A: ожидались tokens_in=100 cost=0.5, получено %+v", a)
	}
	if a.Duration != 20 {
		t.Fatalf("задача A: длительность 20, получено %d", a.Duration)
	}
	if b.TokensIn != nil || b.CostUSD != nil {
		t.Fatalf("задача B: токены и стоимость должны быть null, получено %+v", b)
	}

	// Период: будущий from отсекает всё.
	rows, err = s.UsageSummary(ctx, owner.ID, "task", time.Now().Add(time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("будущий from: ожидалось 0 групп, получено %d", len(rows))
	}
	// Прошедший to отсекает всё.
	rows, err = s.UsageSummary(ctx, owner.ID, "task", time.Time{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("прошедший to: ожидалось 0 групп, получено %d", len(rows))
	}
}

// Вклад задач и итог Epic; nil + значение = значение (design, EpicUsage).
func TestEpicUsageTotals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, projectID, epicID, taskA, taskB := seedUsage(t, s)

	if err := s.RecordUsage(ctx, UsageInput{
		SourceMsgID: "m1", ProjectID: projectID, EpicID: epicID, TaskID: taskA.ID,
		Model: "m", TokensIn: ptr(int64(70)), TokensOut: ptr(int64(30)), DurationS: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, UsageInput{
		SourceMsgID: "m2", ProjectID: projectID, EpicID: epicID, TaskID: taskB.ID,
		Model: "m", DurationS: 3,
	}); err != nil {
		t.Fatal(err)
	}

	rows, total, err := s.EpicUsage(ctx, epicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ожидались 2 задачи, получено %d", len(rows))
	}
	if total == nil || total.Key != epicID {
		t.Fatalf("итог: ожидался key=%s, получено %+v", epicID, total)
	}
	if total.TokensIn == nil || *total.TokensIn != 70 || total.TokensOut == nil || *total.TokensOut != 30 {
		t.Fatalf("итог: ожидались токены 70/30, получено %+v", total)
	}
	if total.CostUSD != nil {
		t.Fatalf("итог: стоимость должна быть null, получено %v", *total.CostUSD)
	}
	if total.Duration != 10 {
		t.Fatalf("итог: длительность 10, получено %d", total.Duration)
	}
}

// EndSession подводит итог токенов по usage-записям задачи с момента старта
// сессии; без данных о токенах — NULL (design, решение 6).
func TestEndSessionTokens(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, projectID, epicID, task, _ := seedUsage(t, s)

	sessionID, err := s.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: 1, DriverKind: "scheduler", DriverID: "sched",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, UsageInput{
		SourceMsgID: "m1", ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Model: "m", TokensIn: ptr(int64(40)), TokensOut: ptr(int64(2)), DurationS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSession(ctx, sessionID, "ref"); err != nil {
		t.Fatal(err)
	}
	var tokens *int64
	if err := s.Pool.QueryRow(ctx, `SELECT tokens FROM sessions WHERE id=$1`, sessionID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens == nil || *tokens != 42 {
		t.Fatalf("ожидались 42 токена, получено %v", tokens)
	}

	// Сессия без usage-данных о токенах закрывается с NULL.
	empty, err := s.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: 2, DriverKind: "scheduler", DriverID: "sched",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSession(ctx, empty, ""); err != nil {
		t.Fatal(err)
	}
	// Записи задачи были до старта этой сессии... они попали бы в интервал
	// [started_at, ∞) только при ts >= started_at — проверяем строгую границу:
	// новых записей нет, но старая запись m1 сделана раньше started_at.
	if err := s.Pool.QueryRow(ctx, `SELECT tokens FROM sessions WHERE id=$1`, empty).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != nil {
		t.Fatalf("токены сессии без usage должны быть NULL, получено %d", *tokens)
	}
}

// TouchRunner: nil = заполненность неизвестна, NULL в БД.
func TestTouchRunnerNullableCtx(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "fake", Model: "m", Host: "h", Capabilities: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchRunner(ctx, "r1", ptr(37)); err != nil {
		t.Fatal(err)
	}
	runners, err := s.ListRunners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].CtxPct == nil || *runners[0].CtxPct != 37 {
		t.Fatalf("ожидался ctx_pct=37, получено %+v", runners)
	}
	if err := s.TouchRunner(ctx, "r1", nil); err != nil {
		t.Fatal(err)
	}
	runners, err = s.ListRunners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runners[0].CtxPct != nil {
		t.Fatalf("ожидался неизвестный ctx_pct (nil), получено %d", *runners[0].CtxPct)
	}
}

// История сессий задачи: порядок по started_at, стадия в Scope, nullable
// tokens (дельта observability «Просмотр сохранённых транскриптов»).
func TestListTaskSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, projectID, epicID, task, other := seedUsage(t, s)

	first, err := s.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal, Scope: "CODING",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, UsageInput{
		SourceMsgID: "ls1", ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Model: "m", TokensIn: ptr(int64(10)), TokensOut: ptr(int64(5)), DurationS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSession(ctx, first, "s3://rivet/tasks/1/attempt-1-CODING.log"); err != nil {
		t.Fatal(err)
	}
	// Вторая сессия: без usage и без транскрипта, ещё открыта.
	second, err := s.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal, Scope: "TESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Сессия чужой задачи в выборку не попадает.
	if _, err := s.CreateSession(ctx, domain.Session{
		TaskID: other.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Depth: domain.DepthMinimal, Scope: "CODING",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListTaskSessions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != first || got[1].ID != second {
		t.Fatalf("ожидались 2 сессии задачи по порядку, получено %+v", got)
	}
	if got[0].Scope != "CODING" || got[0].TranscriptRef == "" || got[0].Ended == nil {
		t.Fatalf("первая сессия: %+v", got[0])
	}
	if got[0].Tokens == nil || *got[0].Tokens != 15 {
		t.Fatalf("токены первой сессии: %v", got[0].Tokens)
	}
	if got[1].Tokens != nil {
		t.Fatalf("открытая сессия без usage: tokens должны быть nil, получено %d", *got[1].Tokens)
	}
	if got[1].TranscriptRef != "" || got[1].Ended != nil {
		t.Fatalf("вторая сессия должна быть открытой без транскрипта: %+v", got[1])
	}

	// OpenSession видит только открытую сессию задачи.
	open, err := s.OpenSession(ctx, task.ID)
	if err != nil || open != second {
		t.Fatalf("OpenSession: %q, %v (ожидалась %q)", open, err, second)
	}
	if _, err := s.EndSession(ctx, second, ""); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenSession(ctx, task.ID); err != nil || open != "" {
		t.Fatalf("после закрытия открытых сессий нет: %q, %v", open, err)
	}
}

// Обрыв стадии вне StageResult/Blocked закрывает открытые сессии задачи:
// отмена (ResolveTask cancel) и потеря runner'а (MarkStaleRunnersOffline).
func TestOpenSessionsClosedOnCancelAndRunnerLost(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, _, _, task, other := seedUsage(t, s)

	// Отмена выполняющейся задачи.
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='running' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Depth: domain.DepthMinimal, Scope: "CODING",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveTask(ctx, task.ID, "", "human", true); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenSession(ctx, task.ID); err != nil || open != "" {
		t.Fatalf("после отмены сессия осталась открытой: %q, %v", open, err)
	}

	// Потеря runner'а: молчащий runner с задачей.
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='running' WHERE id=$1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "lost", Agent: "fake", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE runners SET status='running', task_id=$2, last_seen=now()-interval '10 minutes' WHERE id=$1`,
		"lost", other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, domain.Session{
		TaskID: other.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Depth: domain.DepthMinimal, Scope: "CODING",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkStaleRunnersOffline(ctx, 60); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenSession(ctx, other.ID); err != nil || open != "" {
		t.Fatalf("после потери runner'а сессия осталась открытой: %q, %v", open, err)
	}
}
