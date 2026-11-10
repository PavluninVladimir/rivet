package store

import (
	"context"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Ожидание подключения к модели (fix-agent-without-connection): запуск
// откладывается с постоянной паузой, а не с растущей, чтобы задача пошла
// вскоре после появления подключения. Причина ожидания на задаче, runner
// свободен, после паузы запуск снова назначается.
func TestDeferRunFixedPauseForConnection(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	reason := "нет подключения к модели deepseek-v4-flash: добавьте его в Управление → Подключения"
	pause := 15 * time.Second
	for i := 0; i < 4; i++ {
		a, ok, err := s.AssignRun(ctx, nil, nil)
		if err != nil || !ok {
			t.Fatalf("назначение %d: %v %v", i, ok, err)
		}
		if err := s.UndoAssign(ctx, a.Task.ID, a.Pending); err != nil {
			t.Fatal(err)
		}
		first, got, err := s.DeferRun(ctx, a.Run.ID, a.Task.ID, pause, pause, reason)
		if err != nil || first != (i == 0) || got != pause {
			t.Fatalf("откладывание %d: first=%v pause=%v err=%v", i, first, got, err)
		}
		if err := s.FreeRunnerFor(ctx, "r1", a.Task.ID); err != nil {
			t.Fatal(err)
		}
		task, _ := s.GetTask(ctx, a.Task.ID)
		if task.Status != domain.TaskReady || task.WaitReason != reason {
			t.Fatalf("задача ждёт подключения: %s %q", task.Status, task.WaitReason)
		}
		// До паузы запуск не назначается.
		if _, ok, err := s.AssignRun(ctx, nil, nil); err != nil || ok {
			t.Fatalf("до паузы назначения нет: %v %v", ok, err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET not_before = now() WHERE id=$1`, a.Run.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// Серия откладываний считается по причине: другая причина начинает новую
// серию с первой паузы и нового события ожидания.
func TestDeferRunSeriesPerReason(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	enterCode(t, s, tasks, nil)
	runs := codeRuns(t, s, tasks[0].ID)
	id := runs[0].ID
	// Вид причины тот же, подробности меняются (адрес, текст ошибки сети):
	// серия продолжается, пауза растёт.
	for i, detail := range []string{"dial tcp 140.82.1.1: i/o timeout", "connection reset by peer", "dial tcp 140.82.1.2: i/o timeout"} {
		want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}[i]
		first, pause, err := s.DeferRun(ctx, id, tasks[0].ID, time.Minute, 5*time.Minute, "стадия не запустилась: "+detail)
		if err != nil || first != (i == 0) || pause != want {
			t.Fatalf("причина A, откладывание %d: %v %v %v", i, first, pause, err)
		}
	}
	first, pause, err := s.DeferRun(ctx, id, tasks[0].ID, 15*time.Second, 15*time.Second, "нет подключения к модели m")
	if err != nil || !first || pause != 15*time.Second {
		t.Fatalf("смена причины — новая серия: %v %v %v", first, pause, err)
	}
	if first, _, _ = s.DeferRun(ctx, id, tasks[0].ID, 15*time.Second, 15*time.Second, "нет подключения к модели m"); first {
		t.Fatal("та же причина — серия продолжается")
	}
	// Назначенный запуск DeferPendingRun не трогает.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r-x", Agent: AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET runner_id='r-x' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if deferred, _, _, err := s.DeferPendingRun(ctx, id, tasks[0].ID, 15*time.Second, 15*time.Second, "другое"); err != nil || deferred {
		t.Fatalf("назначенный запуск не откладывается: %v %v", deferred, err)
	}
}

// MarkConnectionRejected говорит, перевёл ли вызов подключение в invalid:
// эскалация одна на перевод, даже если отказы пришли разом.
func TestMarkConnectionRejectedOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin, err := s.CreateUser(ctx, "owner-reject", "", "pw-testpass", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertConnection(ctx, ConnectionInput{ID: "lm", Name: "LM", Kind: "local", API: "openai",
		BaseURL: "http://localhost:1234/v1"}, nil, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		marked, err := s.MarkConnectionRejected(ctx, "lm", "проверка control plane: HTTP 401")
		if err != nil || marked != want {
			t.Fatalf("вызов %d: marked=%v err=%v", i, marked, err)
		}
	}
	c, _ := s.GetConnection(ctx, "lm")
	var events int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='connection.checked' AND payload->>'connection_id'='lm'`).Scan(&events)
	if c.State != domain.LLMStateInvalid || events != 1 {
		t.Fatalf("invalid и одно событие: %s %d", c.State, events)
	}
}

// Запуск помнит, с чем стадия ушла runner'у, до возврата в очередь.
func TestRunAssignmentResetOnUnbind(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	if err := s.SetRunAssignment(ctx, a.Run.ID, "m", "lm", true); err != nil {
		t.Fatal(err)
	}
	byID := func() (StepRun, error) {
		return scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_step_runs r WHERE r.id=$1`, a.Run.ID))
	}
	r, err := byID()
	if err != nil || r.AssignedModel != "m" || r.AssignedConnection != "lm" || !r.CatalogKey {
		t.Fatalf("запуск помнит назначение: %+v %v", r, err)
	}
	if err := s.UnbindRun(ctx, a.Run.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = byID()
	if r.AssignedModel != "" || r.AssignedConnection != "" || r.CatalogKey {
		t.Fatalf("возврат в очередь сбрасывает назначение: %+v", r)
	}
}
