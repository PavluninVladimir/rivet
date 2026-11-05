package orchestrator

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Регрессия ревью add-native-agent P1-1: установку обновляют, пока задача
// идёт на runner'е со сторонним агентом. Миграция 0034 снимает runner с
// работы, но не ставит ему offline напрямую: первый тик watchdog'а снимает
// его штатно, задача с открытым запуском уходит другому runner'у, а не
// висит на старом навсегда.
func TestMigration0034ReleasesRetiredRunner(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	// До обновления runner работал сторонним агентом.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET agent='claude-code' WHERE id=$1`, worker); err != nil {
		t.Fatal(err)
	}
	// Миграция целиком: повторное выполнение на мигрированной базе
	// безопасно, а runner'а claude-code она снимает так же, как при обновлении.
	sql, err := os.ReadFile("../store/migrations/0034_native_agent.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("0034: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var open int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM task_step_runs WHERE task_id=$1 AND verdict IS NULL AND runner_id=$2`,
		f.task.ID, worker).Scan(&open); err != nil {
		t.Fatal(err)
	}
	var status, taskID string
	var retired bool
	_ = f.st.Pool.QueryRow(ctx, `SELECT status, COALESCE(task_id::text, ''), retired FROM runners WHERE id=$1`, worker).Scan(&status, &taskID, &retired)
	if open > 0 || taskID != "" || status != "offline" || !retired {
		t.Fatalf("снятый runner держит задачу: открытых запусков %d, task_id %q, статус %s, retired %v", open, taskID, status, retired)
	}
	if task.RunnerID == "" || task.RunnerID == worker || task.AttemptUsed != 1 {
		t.Fatalf("задача не перешла на другой runner: runner %q, попыток %d, статус %s", task.RunnerID, task.AttemptUsed, task.Status)
	}
}

// Runner выкатки (агента нет, capability deploy) агентских стадий не
// получает (add-native-agent, задача 1.12.2, ревью P2-6): шаг без роли и
// capabilities подходил ему по пустому набору capabilities, и проверки
// проекта или цикл агента пошли бы на машине выкатки. Задача ждёт
// runner'а агента Rivet с причиной ожидания и уходит ему, когда он
// появляется.
func TestDeployRunnerGetsNoAgentStage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Свой процесс проекта: участник без роли, у шага нет capabilities.
	if _, err := st.SaveProjectPolicy(ctx, p.ID, policy.Overrides{Process: &policy.Process{Steps: []policy.Step{
		{ID: "code", Kind: policy.StepCode, Participants: []policy.Participant{{Agent: &policy.AgentRef{}}}},
		{ID: "merge", Kind: policy.StepMerge},
	}}}, owner.Login); err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	// Задача без capabilities: шагу подходит любой runner.
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET capabilities='{}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.GetTask(ctx, task.ID)
	out.mu.Lock()
	sent := len(out.sent)
	out.mu.Unlock()
	if sent != 0 || got.RunnerID != "" || got.WaitReason == "" {
		t.Fatalf("runner выкатки получил агентскую стадию: отправлено %d, runner %q, причина ожидания %q", sent, got.RunnerID, got.WaitReason)
	}
	// Человеку сессия доработки на runner'е выкатки тоже не открывается.
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='blocked' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartUserSession(ctx, task.ID, owner.Login, false); !errors.Is(err, store.ErrNoRunner) {
		t.Fatalf("сессия доработки без runner'а агента: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='ready' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: store.AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetTask(ctx, task.ID); got.RunnerID != "worker" {
		t.Fatalf("задача не ушла runner'у агента Rivet: %q (%s)", got.RunnerID, got.Status)
	}
}
