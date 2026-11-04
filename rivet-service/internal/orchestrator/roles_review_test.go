package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Находки ревью add-agent-roles в оркестраторе: недоставленное назначение,
// сбой инфраструктуры тестировщика, подписи исхода, переиспользование
// освобождённого runner'а, перезапуск runner'а, подключение агента.

// downSender — runner недоступен: Assignment не доставляется.
type downSender struct{ capture }

func (d *downSender) Send(runnerID string, msg *pb.PlaneMsg) bool {
	d.capture.Send(runnerID, msg)
	return false
}

// Недоставленное назначение: запуск отвязан от runner'а и агента, runner
// свободен, сессия закрыта (P2-6).
func TestUndeliveredAssignmentReleasesAgent(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""})
	down := &downSender{}
	f.e.Out = down
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(down.sent) == 0 {
		t.Fatal("назначение должно было уйти")
	}
	agent, _ := f.st.GetTeamAgent(ctx, "maks")
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if agent.TaskID != "" || task.AgentID != "" {
		t.Fatalf("агент свободен после недоставки: %+v %+v", agent.TaskID, task.AgentID)
	}
	runners, _ := f.st.ListRunners(ctx)
	for _, r := range runners {
		if r.TaskID != "" {
			t.Fatalf("runner свободен после недоставки: %+v", r)
		}
	}
}

// Сбой инфраструктуры тестировщика: один повтор без расхода попытки,
// повторный сбой эскалирует (P3-4 оркестратора).
func TestTesterInfraErrorRetriesThenEscalates(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
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
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("тестировщик: %+v", as)
	}
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "error", Detail: "git: push отклонён"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskTesting || task.AttemptUsed != before.AttemptUsed || task.StepRejections["test"] != before.StepRejections["test"] {
		t.Fatalf("повтор без расхода попытки: %s %d→%d", task.Status, before.AttemptUsed, task.AttemptUsed)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("шаг test назначен заново: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "error", Detail: "git: push отклонён"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFailed {
		t.Fatalf("повторный сбой эскалирует: %s", got)
	}
}

// driveToTest — задача прошла код и стоит на назначенном шаге test.
func driveToTest(t *testing.T, f policyFixture) *pb.Assignment {
	t.Helper()
	ctx := context.Background()
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
	return f.out.lastAssign(t)
}

func lastStatusText(t *testing.T, f policyFixture) string {
	t.Helper()
	evs := eventsOfType(t, f.st, f.task.ID, "task.status")
	return evs[len(evs)-1].Text
}

// Подписи исхода test: тестировщик одобрил, а проверки красные — «проверки
// упали»; runner без ролей — без отчёта тестировщика (P3-2).
func TestTesterLabelsFollowAgentVerdict(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
	as := driveToTest(t, f)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "ok", Detail: "## Тестировщик\nкритерии проверены\n\n## Проверки\nFAIL"}); err != nil {
		t.Fatal(err)
	}
	if text := lastStatusText(t, f); !strings.Contains(text, "проверки упали") {
		t.Fatalf("одобрение тестировщика при красных проверках: %q", text)
	}
	if ctxText := f.e.peekStageContext(f.task.ID); !strings.HasPrefix(ctxText, "Замечания тестировщика и вывод проверок:") {
		t.Fatalf("контекст исправления с отчётом тестировщика: %q", ctxText)
	}

	g := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, g)
	as = driveToTest(t, g)
	// Старый runner: роль не поддерживает, вердикта агента нет.
	if err := g.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: g.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Detail: "== tests ==\nFAIL"}); err != nil {
		t.Fatal(err)
	}
	if text := lastStatusText(t, g); !strings.Contains(text, "проверки упали") {
		t.Fatalf("старый runner: %q", text)
	}
	if ctxText := g.e.peekStageContext(g.task.ID); !strings.HasPrefix(ctxText, "Вывод проверок:") {
		t.Fatalf("старый runner — только вывод проверок: %q", ctxText)
	}
}

// Освобождённый runner не переиспользуется: его могли отдать другой задаче
// между решением и входом на шаг (P2-3).
func TestReuseRequiresRunnerStillBound(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	proc, _, err := f.e.processFor(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	test, _ := proc.Step("test")
	if !f.e.reuseTarget(ctx, task, test, "worker") {
		t.Fatal("runner за задачей переиспользуется")
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET task_id=NULL, status='idle' WHERE id='worker'`); err != nil {
		t.Fatal(err)
	}
	if f.e.reuseTarget(ctx, task, test, "worker") {
		t.Fatal("освобождённый runner не переиспользуется")
	}
}

// Перезапуск runner'а: открытые запуски снимаются, агент свободен, запуск
// назначается заново (P2-6).
func TestRunnerRestartReassigns(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first := f.out.lastAssign(t)
	if err := f.e.ReleaseRunnerRuns(ctx, "worker", nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.st.GetTeamAgent(ctx, "maks"); a.TaskID != "" {
		t.Fatal("агент свободен после перезапуска runner'а")
	}
	if f.e.SessionMatches(ctx, f.task.ID, first.SessionId) {
		t.Fatal("сессия прежнего процесса закрыта")
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL WHERE id='worker'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again := f.out.lastAssign(t); again.SessionId == first.SessionId || again.GetRole().GetName() != "Макс" {
		t.Fatalf("назначение заново: %+v", again)
	}
}

// Подключение агента команды едет в назначение парой (P3-3).
func TestAgentConnectionInAssignment(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	owner := f.owner
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
		VALUES ('a', 'A', 'local', 'openai', 'http://a/v1', '[{"id":"m"}]', $1),
		       ('b', 'B', 'local', 'openai', 'http://b/v1', '[{"id":"m"}]', $1)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agents SET models='[{"connection_id":"a","model":"m"},{"connection_id":"b","model":"m"}]',
		default_model='{"connection_id":"a","model":"m"}' WHERE id='claude-code'`); err != nil {
		t.Fatal(err)
	}
	withTeam(t, f)
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO team_agents (id, name, role_id, agent, connection_id, model) VALUES ('maks','Макс','developer','claude-code','b','m')`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "claude-code", Models: []string{"m"}, Capabilities: []string{"coding"}, Protocol: "12"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.ConnectionId != "b" || as.Model != "m" {
		t.Fatalf("подключение агента в назначении: %q %q", as.ConnectionId, as.Model)
	}
	_ = policy.OutcomeOk
}
