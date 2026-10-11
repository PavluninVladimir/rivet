package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Шаг prompt (спека process «Шаг prompt с замечаниями», «Шаг prompt без
// маркера», orchestration: runner без стадии PROMPT).

func withPromptAfterReview() policy.Process {
	p := policy.DefaultProcess()
	step := policy.Step{ID: "migrations", Kind: policy.StepPrompt, Prompt: "проверь миграции на обратимость",
		Participants: []policy.Participant{{Agent: &policy.AgentRef{}}}}
	p.Steps = append(p.Steps[:3], append([]policy.Step{step}, p.Steps[3:]...)...)
	return p
}

func TestPromptStepVerdicts(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	// Runner'ы стенда объявляют PROMPT.
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}, Stages: []string{"CODING", "TESTING", "REVIEW", "FIXING", "PROMPT"}})
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"coding", "review"}, Stages: []string{"CODING", "TESTING", "REVIEW", "FIXING", "PROMPT"}})
	setProcess(t, f, withPromptAfterReview())
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_PROMPT || as.StepId != "migrations" || !strings.Contains(as.StepPrompt, "обратимость") {
		t.Fatalf("назначение prompt: %+v", as)
	}
	runner := mustTaskRunner(t, f.st, f.task.ID)
	// Маркер CHANGES: исправление с текстом, проход шага расходуется.
	if err := f.e.OnStageResult(ctx, runner, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_PROMPT, Ok: true, Verdict: "changes", Detail: "миграция 0023 необратима"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || task.StepRejections["migrations"] != 1 {
		t.Fatalf("после CHANGES: %s %v", task.Status, task.StepRejections)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fix := f.out.lastAssign(t)
	if fix.Stage != pb.StageResult_FIXING || !strings.Contains(fix.ExtraContext, "0023") {
		t.Fatalf("исправление с замечаниями prompt: %+v", fix)
	}
	// Второй круг до prompt, теперь без маркера: ok.
	fixer := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: fix.SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_PROMPT {
		t.Fatalf("второй prompt: %+v", as)
	}
	runner = mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, runner, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_PROMPT, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("после prompt без маркера — ожидание merge")
	}
}

// Runner без стадии PROMPT шаг prompt не получает: задача ждёт с причиной.
func TestPromptWaitsForCapableRunner(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	p := policy.DefaultProcess()
	p.Steps[0] = policy.Step{ID: "code", Kind: policy.StepPrompt, Prompt: "напиши changelog",
		Participants: []policy.Participant{{Agent: &policy.AgentRef{}}}}
	setProcess(t, f, p)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskReady || !strings.Contains(task.WaitReason, "PROMPT") {
		t.Fatalf("ожидание runner'а со стадией PROMPT: %s %q", task.Status, task.WaitReason)
	}
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}, Stages: []string{"CODING", "TESTING", "REVIEW", "FIXING", "PROMPT"}})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.Stage != pb.StageResult_PROMPT {
		t.Fatalf("после регистрации runner'а с PROMPT: %+v", as)
	}
}
