package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Команда агентов (add-agent-roles, спека agent-roles «Назначение агенту
// команды»): стадию ведёт свободный агент роли на подходящем runner'е.

// withTeam включает тестировщика и заводит агентов команды: id, имя, роль,
// модель (пусто — как в шаблоне).
func withTeam(t *testing.T, f policyFixture, agents ...[4]string) {
	t.Helper()
	withTester(t, f)
	for i, a := range agents {
		if _, err := f.st.Pool.Exec(context.Background(), `
			INSERT INTO team_agents (id, name, role_id, model, position) VALUES ($1,$2,$3,$4,$5)`,
			a[0], a[1], a[2], a[3], (i+1)*10); err != nil {
			t.Fatal(err)
		}
	}
}

func assignAgent(t *testing.T, f policyFixture, sessionID string) store.StepRun {
	t.Helper()
	run, err := f.st.RunBySession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestTeamAgentsCarryTaskThroughPipeline(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"lev", "Лев", "tester", ""}, [4]string{"irina", "Ирина", "reviewer", ""})
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker2", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING || as.GetRole().GetName() != "Макс" || as.GetRole().GetId() != domain.RoleDeveloper {
		t.Fatalf("код ведёт Разработчик Макс: %+v", as.GetRole())
	}
	coder := assignAgent(t, f, as.SessionId).RunnerID
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.AgentID != "maks" || task.AgentName != "Макс" || task.RoleID != domain.RoleDeveloper {
		t.Fatalf("исполнитель задачи: %+v", task)
	}
	evs := eventsOfType(t, f.st, f.task.ID, "task.status")
	last := evs[len(evs)-1]
	if a, _ := last.Payload["agent"].(map[string]any); a["name"] != "Макс" || !strings.Contains(last.Text, "Разработчик Макс") {
		t.Fatalf("событие назначения: %q %+v", last.Text, last.Payload)
	}
	if err := f.e.OnStageResult(ctx, coder, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	// Команда: runner не переиспользуется, агента и runner подбирает планировщик.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	run := assignAgent(t, f, as.SessionId)
	if as.Stage != pb.StageResult_TESTING || as.GetRole().GetName() != "Лев" || run.AgentID != "lev" || run.RunnerID == coder {
		t.Fatalf("тесты ведёт Лев на другом runner'е: %+v %s/%s", as.GetRole(), run.RunnerID, coder)
	}
	if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: true, Verdict: "ok", Detail: "## Тестировщик Лев\nкритерии проверены"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	run = assignAgent(t, f, as.SessionId)
	if as.Stage != pb.StageResult_REVIEW || as.GetRole().GetName() != "Ирина" || run.RunnerID == coder {
		t.Fatalf("review ведёт Ирина не на runner'е кода: %+v %s", as.GetRole(), run.RunnerID)
	}
	// Замечания review: исправление снова у Макса, по возможности на том же runner'е.
	if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_REVIEW, Ok: false, Detail: "поправь обработку ошибок"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	run = assignAgent(t, f, as.SessionId)
	if as.Stage != pb.StageResult_FIXING || run.AgentID != "maks" || run.RunnerID != coder {
		t.Fatalf("исправление у Макса на том же runner'е: %+v %s/%s", as.GetRole(), run.RunnerID, coder)
	}
	// Замечания ревьюера подписаны агентом.
	steps := eventsOfType(t, f.st, f.task.ID, "task.step")
	v, _ := steps[len(steps)-1].Payload["verdicts"].([]any)
	v0, _ := v[0].(map[string]any)
	if a, _ := v0["agent"].(map[string]any); a["name"] != "Ирина" {
		t.Fatalf("агент в вердикте: %+v", v0)
	}
}

func TestTeamAgentBusyWaits(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""})
	second, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker2", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := f.st.GetTask(ctx, f.task.ID)
	b, _ := f.st.GetTask(ctx, second.ID)
	busy, waiting := a, b
	if a.AgentID == "" {
		busy, waiting = b, a
	}
	if busy.AgentID != "maks" || waiting.AgentID != "" {
		t.Fatalf("один разработчик — одна стадия кода: %q %q", a.AgentID, b.AgentID)
	}
	if !strings.Contains(waiting.WaitReason, "нет свободного агента роли «Разработчик»") {
		t.Fatalf("причина ожидания: %q", waiting.WaitReason)
	}
}

func TestTeamAgentModelIsRequired(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", "opus"})
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Models: []string{"sonnet"}, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Нет runner'а с моделью агента — стадия ждёт с причиной.
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.AgentID != "" || !strings.Contains(task.WaitReason, "нет runner'а для свободных агентов роли") {
		t.Fatalf("ожидание runner'а с моделью агента: %q %q", task.AgentID, task.WaitReason)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "opus-box", Agent: "wrap", Models: []string{"sonnet", "opus"}, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if run := assignAgent(t, f, as.SessionId); run.RunnerID != "opus-box" || as.Model != "opus" {
		t.Fatalf("агенту с моделью opus — runner с ней: %s %q", run.RunnerID, as.Model)
	}
}
