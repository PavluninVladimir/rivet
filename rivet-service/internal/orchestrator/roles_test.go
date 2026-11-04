package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Роли агентов в конвейере (add-agent-roles): инструкция роли в назначении,
// тестировщик на шаге test, роль в событиях.

// withTester включает тестировщика в процессе по умолчанию (фикстуры
// механики конвейера его выключают).
func withTester(t *testing.T, f policyFixture) {
	t.Helper()
	if _, err := f.st.SaveInstallationPolicy(context.Background(), policy.Defaults(), "root"); err != nil {
		t.Fatal(err)
	}
}

func TestTesterOnOtherRunnerReturnsRemarks(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
	// Второй исполнитель с именем: тестировщик предпочтёт его.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker2", Agent: "wrap", Capabilities: []string{"coding"},
		DisplayName: "Ева", Features: []string{domain.FeatureRoles}}); err != nil {
		t.Fatal(err)
	}
	// Ревьюер с coding тоже подошёл бы — заберём его, чтобы выбор был однозначным.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING || as.GetRole().GetId() != domain.RoleDeveloper || as.GetRole().GetInstruction() == "" {
		t.Fatalf("назначение разработчику с ролью: %+v", as.GetRole())
	}
	coder := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, coder, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	// Свободен другой исполнитель: шаг test уходит планировщику, а не тому же runner'у.
	if f.out.lastAssign(t).Stage == pb.StageResult_TESTING {
		t.Fatal("тестировщик не должен сразу достаться runner'у кода при свободном другом")
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING || as.GetRole().GetId() != domain.RoleTester || len(as.TestPaths) == 0 {
		t.Fatalf("назначение тестировщику: %+v", as)
	}
	tester := "worker2"
	if coder == "worker2" {
		tester = "worker"
	}
	run, err := f.st.RunBySession(ctx, as.SessionId)
	if err != nil || run.RunnerID != tester {
		t.Fatalf("тестировщик на другом runner'е: %+v %v", run.RunnerID, err)
	}
	// Шаг сессии несёт роль и имя исполнителя.
	payload := f.e.PerformerPayload(ctx, as.SessionId, map[string]any{})
	if role, _ := payload["role"].(map[string]any); role["id"] != domain.RoleTester {
		t.Fatalf("роль в шаге сессии: %+v", payload)
	}
	// Тестировщик вернул замечания: исправление с его отчётом.
	detail := "## Тестировщик Ева\nкритерий 1 не выполнен\n\n## Проверки\nнет настроенных проверок"
	if err := f.e.OnStageResult(ctx, tester, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: policy.OutcomeChanges, Detail: detail}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFixing {
		t.Fatalf("после замечаний тестировщика: %s", got)
	}
	if ctxText := f.e.peekStageContext(f.task.ID); !strings.HasPrefix(ctxText, "Замечания тестировщика и вывод проверок:") || !strings.Contains(ctxText, "критерий 1") {
		t.Fatalf("контекст исправления: %q", ctxText)
	}
	steps := eventsOfType(t, f.st, f.task.ID, "task.step")
	last := steps[len(steps)-1]
	verdicts, _ := last.Payload["verdicts"].([]any)
	v0, _ := verdicts[0].(map[string]any)
	if role, _ := v0["role"].(map[string]any); role["id"] != domain.RoleTester {
		t.Fatalf("роль в вердикте шага: %+v", last.Payload)
	}
	statuses := eventsOfType(t, f.st, f.task.ID, "task.status")
	found := false
	for _, e := range statuses {
		if strings.Contains(e.Text, "тестировщик вернул замечания") {
			found = true
		}
	}
	if !found {
		t.Fatal("нет события возврата от тестировщика")
	}
}

func TestTesterReusesSingleRunner(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
	// Единственный исполнитель: ревьюер без coding.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING || as.GetRole().GetId() != domain.RoleTester {
		t.Fatalf("тестировщик сразу на том же runner'е: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: true, Verdict: policy.OutcomeOk, Detail: "## Тестировщик worker\nкритерии проверены"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview {
		t.Fatalf("после тестировщика: %s", got)
	}
	if ctxText := f.e.peekStageContext(f.task.ID); !strings.HasPrefix(ctxText, "Отчёт тестировщика и автопроверок:") {
		t.Fatalf("контекст ревьюеру: %q", ctxText)
	}
	// Ревьюер — не исполнитель кода (тест не перезаписал исполнителя задачи).
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.RunnerID != "worker" {
		t.Fatalf("исполнитель задачи: %q", task.RunnerID)
	}
}

func TestDisabledRoleFallsBack(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	// Шаблон из процесса выключить через store нельзя (ссылки); выключенным
	// он может оказаться в гонке или правкой базы — назначение без роли.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=false WHERE id=$1`, domain.RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.Stage != pb.StageResult_CODING || as.GetRole() != nil {
		t.Fatalf("отключённая роль: назначение без роли, got %+v", as.GetRole())
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.RoleID != "" {
		t.Fatalf("роль задачи: %q", task.RoleID)
	}
}
