package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Проверки проекта без тестировщика, упавшие из-за окружения песочницы
// (нет сети, инструмент под $HOME runner'а), приходят с вердиктом error
// (финальное ревью add-native-agent, P2-1). Шаг test повторяется без
// расхода попытки разработчика, повторный сбой эскалирует человеку с
// подсказкой из отчёта, а не уходит разработчику на исправление.
func TestChecksInfraErrorRetriesThenEscalates(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	as := driveToTest(t, f)
	if as.Stage != pb.StageResult_TESTING || as.GetRole() != nil {
		t.Fatalf("шаг проверок без тестировщика: %+v", as)
	}
	const detail = "== test ==\nsh: 1: npm: not found\nпроверка test: код выхода 127: команда не найдена в песочнице: " +
		"проверке нужна сеть или инструмент вне рабочей копии, см. README: подготовка кешей и флаги runner'а"
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: verdictInfraError, Detail: detail}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskTesting || task.AttemptUsed != before.AttemptUsed || task.StepRejections["test"] != before.StepRejections["test"] {
		t.Fatalf("повтор без расхода попытки: %s, попытки %d→%d, отказы test %d→%d",
			task.Status, before.AttemptUsed, task.AttemptUsed, before.StepRejections["test"], task.StepRejections["test"])
	}
	if ctxText := f.e.peekStageContext(f.task.ID); strings.Contains(ctxText, "npm: not found") {
		t.Fatalf("сбой окружения не должен уходить разработчику как вывод проверок: %q", ctxText)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("шаг test назначен заново: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: verdictInfraError, Detail: detail}); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || task.StepRejections["test"] != before.StepRejections["test"] {
		t.Fatalf("повторный сбой эскалирует без исправления: %s, отказы test %d", task.Status, task.StepRejections["test"])
	}
	if text := lastStatusText(t, f); !strings.Contains(text, "README") {
		t.Fatalf("эскалация с подсказкой: %q", text)
	}
}
