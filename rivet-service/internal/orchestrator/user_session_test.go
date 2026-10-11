package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Сессия доработки (спека agent-integration «Сессия из интерфейса Rivet»):
// blocked → fixing на свободном runner'е, промпт человека в Assignment,
// эскалация закрыта, счётчики сброшены, попытка не расходуется.
func TestUserSessionFromBlocked(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnBlocked(ctx, worker, &pb.BlockedQuestion{TaskId: f.task.ID,
		SessionId: f.out.lastAssign(t).SessionId, Question: "что делать?"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskBlocked {
		t.Fatalf("want blocked, got %s", task.Status)
	}

	sessionID, err := f.e.StartUserSession(ctx, f.task.ID, "Поправь только README, ничего больше", "alice", false)
	if err != nil || sessionID == "" {
		t.Fatalf("запуск сессии: %v %q", err, sessionID)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || task.RunnerID == "" {
		t.Fatalf("want fixing с runner'ом: %+v", task)
	}
	if task.AttemptUsed != 0 || task.BlockReason != "" {
		t.Fatalf("счётчики и блокировка должны сброситься: %+v", task)
	}
	if atts, _ := f.st.ListAttention(ctx, f.owner.ID); len(atts) != 0 {
		t.Fatalf("эскалация должна закрыться: %+v", atts)
	}
	// Assignment несёт промпт пользователя, стадия FIXING.
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_FIXING || as.UserPrompt != "Поправь только README, ничего больше" {
		t.Fatalf("assignment: %+v", as)
	}
	// Сессия — водитель-пользователь с промптом.
	sessions, _ := f.st.ListTaskSessions(ctx, f.task.ID, "alice")
	last := sessions[len(sessions)-1]
	if last.ID != sessionID || last.DriverKind != "user" || last.DriverID != "alice" ||
		!strings.Contains(last.Prompt, "README") {
		t.Fatalf("сессия: %+v", last)
	}
	// Результат идёт обычным конвейером: FIXING ok → testing; попытка не тронута.
	if err := f.e.OnStageResult(ctx, task.RunnerID, &pb.StageResult{TaskId: f.task.ID,
		SessionId: as.SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskTesting || task.AttemptUsed != 0 {
		t.Fatalf("после сессии: %+v", task)
	}
}

// Отказ без свободного runner'а и на недопустимом статусе.
func TestUserSessionRejections(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	// Задача в queued — недопустимый статус.
	if _, err := f.e.StartUserSession(ctx, f.task.ID, "промпт", "alice", false); err == nil {
		t.Fatal("queued должен отклоняться")
	}
	// Blocked, но все runner'ы заняты.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnBlocked(ctx, worker, &pb.BlockedQuestion{TaskId: f.task.ID,
		SessionId: f.out.lastAssign(t).SessionId, Question: "?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='running'`); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.StartUserSession(ctx, f.task.ID, "промпт", "alice", false)
	if !errors.Is(err, store.ErrNoRunner) {
		t.Fatalf("ожидали ErrNoRunner, got %v", err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskBlocked {
		t.Fatalf("задача должна остаться blocked: %s", task.Status)
	}
}
