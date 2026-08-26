package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Сценарии спеки backend/process: процесс проекта как данные, несколько
// участников шага, агрегация вердиктов, отключённые шаги, версия процесса
// на задаче, подбор runner'а по агенту и модели.

// assignsSince — назначения, отправленные после индекса from.
func (c *capture) assignsSince(from int) []*pb.Assignment {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pb.Assignment
	for _, m := range c.sent[from:] {
		if a := m.GetAssign(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// setProcess сохраняет процесс проекта до первой задачи.
func setProcess(t *testing.T, f policyFixture, proc policy.Process) {
	t.Helper()
	if _, err := f.st.SaveProjectPolicy(context.Background(), f.p.ID, policy.Overrides{Process: &proc}, "owner"); err != nil {
		t.Fatal(err)
	}
}

// driveToReview проводит задачу через coding и testing одним runner'ом.
func driveToReview(t *testing.T, f policyFixture) {
	t.Helper()
	ctx := context.Background()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING || as.StepId != "code" || as.Participant != "p1" {
		t.Fatalf("первое назначение: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("после coding ожидали TESTING: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
}

func reviewWithTwoAgents(mode, require string) policy.Process {
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{
		{Agent: &policy.AgentRef{Kind: "wrap"}},
		{Agent: &policy.AgentRef{Kind: "codex", Model: "gpt-5"}},
	}
	p.Steps[2].Mode, p.Steps[2].Require = mode, require
	return p
}

// Два агента-ревьюера параллельно: обе сессии стартуют сразу на разных
// runner'ах, замечания обоих склеиваются в один контекст, попытка одна.
func TestTwoReviewersParallelMergeRemarks(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "codex-r", Agent: "codex", Models: []string{"gpt-5"}, Capabilities: []string{"review"}})
	setProcess(t, f, reviewWithTwoAgents(policy.ModeParallel, policy.RequireAll))
	driveToReview(t, f)
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	reviews := f.out.assignsSince(mark)
	if len(reviews) != 2 {
		t.Fatalf("ожидали две review-сессии, получили %d", len(reviews))
	}
	byPart := map[string]*pb.Assignment{}
	for _, a := range reviews {
		if a.Stage != pb.StageResult_REVIEW || a.StepId != "review" {
			t.Fatalf("назначение не review: %+v", a)
		}
		byPart[a.Participant] = a
	}
	if byPart["p2"] == nil || byPart["p2"].Model != "gpt-5" {
		t.Fatalf("участник p2 должен получить модель gpt-5: %+v", byPart)
	}
	// Первый вердикт — ждём второго, задача остаётся в review.
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: byPart["p1"].SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "первое замечание"}); err != nil {
		t.Fatal(err)
	}
	if st := taskStatus(t, f.st, f.task.ID); st != domain.TaskReview {
		t.Fatalf("после одного вердикта задача должна ждать второго: %s", st)
	}
	if err := f.e.OnStageResult(ctx, "codex-r", &pb.StageResult{TaskId: f.task.ID, SessionId: byPart["p2"].SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "второе замечание"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || task.AttemptUsed != 1 || task.StepRejections["review"] != 1 {
		t.Fatalf("после двух замечаний: %s used=%d rej=%v", task.Status, task.AttemptUsed, task.StepRejections)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fix := f.out.lastAssign(t)
	if fix.Stage != pb.StageResult_FIXING || !strings.Contains(fix.ExtraContext, "первое замечание") ||
		!strings.Contains(fix.ExtraContext, "второе замечание") || !strings.Contains(fix.ExtraContext, "участника p2 (codex/gpt-5)") {
		t.Fatalf("контекст исправления должен нести оба списка с автором: %+v", fix)
	}
	steps := eventsOfType(t, f.st, f.task.ID, "task.step")
	last := steps[len(steps)-1]
	if last.Payload["outcome"] != "changes" || last.Payload["next"] != "code" {
		t.Fatalf("событие перехода: %+v", last.Payload)
	}
}

// Последовательные ревьюеры: второй не запускается после замечаний первого,
// при одобрении первого стартует второй.
func TestSequentialReviewers(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "codex-r", Agent: "codex", Models: []string{"gpt-5"}, Capabilities: []string{"review"}})
	setProcess(t, f, reviewWithTwoAgents(policy.ModeSequential, policy.RequireAll))
	driveToReview(t, f)
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.out.assignsSince(mark); len(got) != 1 || got[0].Participant != "p1" {
		t.Fatalf("sequential: стартует только первый участник, получили %d", len(got))
	}
	first := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: first.SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "замечание"}); err != nil {
		t.Fatal(err)
	}
	if st := taskStatus(t, f.st, f.task.ID); st != domain.TaskFixing {
		t.Fatalf("замечания первого завершают шаг: %s", st)
	}
	// Исправление и повторные проверки → снова review, первый одобряет,
	// стартует второй.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fixer := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first = f.out.lastAssign(t)
	if first.Participant != "p1" {
		t.Fatalf("второй круг начинается с первого участника: %+v", first)
	}
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: first.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	second := f.out.lastAssign(t)
	if second.Participant != "p2" || second.Model != "gpt-5" {
		t.Fatalf("после одобрения первого стартует второй: %+v", second)
	}
	if err := f.e.OnStageResult(ctx, "codex-r", &pb.StageResult{TaskId: f.task.ID, SessionId: second.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatalf("после обоих одобрений — ожидание merge: %d", len(evs))
	}
}

// Правило any: первое одобрение завершает шаг, второй сессии уходит отмена.
func TestRequireAnyCancelsOthers(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "codex-r", Agent: "codex", Models: []string{"gpt-5"}, Capabilities: []string{"review"}})
	setProcess(t, f, reviewWithTwoAgents(policy.ModeParallel, policy.RequireAny))
	driveToReview(t, f)
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	reviews := f.out.assignsSince(mark)
	if len(reviews) != 2 {
		t.Fatalf("две сессии: %d", len(reviews))
	}
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: reviews[0].SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("any: одного одобрения достаточно")
	}
	f.out.mu.Lock()
	cancels := 0
	for _, m := range f.out.sent[mark:] {
		if m.GetCancel() != nil {
			cancels++
		}
	}
	f.out.mu.Unlock()
	if cancels != 1 {
		t.Fatalf("второй сессии должна уйти отмена: %d", cancels)
	}
	// Поздний результат отменённой сессии отбрасывается.
	if err := f.e.OnStageResult(ctx, "codex-r", &pb.StageResult{TaskId: f.task.ID, SessionId: reviews[1].SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "поздно"}); err != nil {
		t.Fatal(err)
	}
	if st := taskStatus(t, f.st, f.task.ID); st != domain.TaskReview {
		t.Fatalf("поздний вердикт не должен менять задачу: %s", st)
	}
	runners, _ := f.st.ListRunners(ctx)
	for _, r := range runners {
		if r.Status != domain.RunnerIdle {
			t.Fatalf("после шага все runner'ы свободны: %s=%s", r.ID, r.Status)
		}
	}
}

// Вопрос одного участника блокирует задачу, остальные сессии прерываются,
// ответ человека возвращает задачу на тот же шаг.
func TestBlockedParticipantReturnsToSameStep(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "codex-r", Agent: "codex", Models: []string{"gpt-5"}, Capabilities: []string{"review"}})
	setProcess(t, f, reviewWithTwoAgents(policy.ModeParallel, policy.RequireAll))
	driveToReview(t, f)
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	reviews := f.out.assignsSince(mark)
	if err := f.e.OnBlocked(ctx, "codex-r", &pb.BlockedQuestion{TaskId: f.task.ID, SessionId: reviews[1].SessionId, Question: "какой стиль?"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskBlocked || task.StepID != "review" {
		t.Fatalf("blocked на шаге review: %s %s", task.Status, task.StepID)
	}
	if err := f.st.ResolveTask(ctx, f.task.ID, "любой", "human", false); err != nil {
		t.Fatal(err)
	}
	mark = f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	again := f.out.assignsSince(mark)
	if len(again) != 2 || again[0].Stage != pb.StageResult_REVIEW {
		t.Fatalf("после ответа задача возвращается на тот же шаг review: %d %v", len(again), again)
	}
}

// Отключённый шаг test пропускается: после code сразу review.
func TestDisabledStepSkipped(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	proc := policy.DefaultProcess()
	off := false
	proc.Steps[1].Enabled = &off
	setProcess(t, f, proc)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskReview || task.StepID != "review" {
		t.Fatalf("без test после coding сразу review: %s %s", task.Status, task.StepID)
	}
}

// Два шага review с раздельными лимитами: по одному отказу на каждом не
// проваливают задачу.
func TestTwoReviewStepsSeparateLimits(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	two := 2
	proc := policy.DefaultProcess()
	second := policy.Step{ID: "review-2", Kind: policy.StepReview, Attempts: &two,
		Participants: []policy.Participant{{Agent: &policy.AgentRef{}}}}
	proc.Steps[2].Attempts = &two
	proc.Steps = append(proc.Steps[:3], append([]policy.Step{second}, proc.Steps[3:]...)...)
	setProcess(t, f, proc)
	five := 5
	if _, err := f.st.SetTaskAttemptLimit(ctx, f.task.ID, five); err != nil {
		t.Fatal(err)
	}
	driveToReview(t, f)
	reject := func(step string) {
		t.Helper()
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		as := f.out.lastAssign(t)
		if as.StepId != step {
			t.Fatalf("ожидали шаг %s, получили %s", step, as.StepId)
		}
		if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "замечание " + step}); err != nil {
			t.Fatal(err)
		}
	}
	fixAndTest := func() {
		t.Helper()
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fixer := mustTaskRunner(t, f.st, f.task.ID)
		as := f.out.lastAssign(t)
		if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
			t.Fatal(err)
		}
		as = f.out.lastAssign(t)
		if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	approve := func(step string) {
		t.Helper()
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		as := f.out.lastAssign(t)
		if as.StepId != step {
			t.Fatalf("ожидали шаг %s, получили %s", step, as.StepId)
		}
		if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	reject("review")
	fixAndTest()
	approve("review")
	reject("review-2")
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || task.StepRejections["review"] != 1 || task.StepRejections["review-2"] != 1 {
		t.Fatalf("раздельные счётчики: %s %v", task.Status, task.StepRejections)
	}
	fixAndTest()
	approve("review")
	reject("review-2")
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || task.StepRejections["review-2"] != 2 {
		t.Fatalf("второй отказ второго review при лимите 2 — failed: %s %v", task.Status, task.StepRejections)
	}
}

// Версия процесса фиксируется на задаче: новая версия влияет только на
// задачи, созданные после.
func TestProcessVersionPinnedOnTask(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	driveToReview(t, f)
	hashBefore, _ := f.st.GetTask(ctx, f.task.ID)
	// Новая версия без шага review.
	proc := policy.DefaultProcess()
	off := false
	proc.Steps[2].Enabled = &off
	setProcess(t, f, proc)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("задача в работе идёт по прежней версии: %+v", as)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.ProcessHash != hashBefore.ProcessHash || task.ProcessHash == "" {
		t.Fatalf("хэш процесса задачи не должен меняться: %q → %q", hashBefore.ProcessHash, task.ProcessHash)
	}
	// Новая задача — без review.
	taskB, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := f.st.GetTask(ctx, taskB.ID)
	if b.ProcessHash == task.ProcessHash || store.TaskProcess(b).HasKind(policy.StepReview) {
		t.Fatalf("новая задача должна взять новую версию без review: %s", b.ProcessHash)
	}
}

// Нет runner'а с нужным агентом и моделью: задача ждёт с явной причиной,
// назначается после регистрации подходящего runner'а.
func TestWaitsForAgentAndModel(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	proc := policy.DefaultProcess()
	proc.Steps[0].Participants = []policy.Participant{{Agent: &policy.AgentRef{Kind: "codex", Model: "gpt-5"}}}
	setProcess(t, f, proc)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskReady || !strings.Contains(task.WaitReason, "codex/gpt-5") {
		t.Fatalf("ожидание с причиной: %s %q", task.Status, task.WaitReason)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.wait"); len(evs) != 1 {
		t.Fatalf("событие ожидания один раз: %d", len(evs))
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.wait"); len(evs) != 1 {
		t.Fatalf("повторный тик не плодит событий: %d", len(evs))
	}
	_ = f.st.UpsertRunner(ctx, domain.Runner{ID: "codex-w", Agent: "codex", Models: []string{"gpt-5", "gpt-5-mini"}, Capabilities: []string{"coding"}})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.RunnerID != "codex-w" || as.Model != "gpt-5" || task.WaitReason != "" {
		t.Fatalf("назначение подходящему runner'у с моделью: %s %q wait=%q", task.RunnerID, as.Model, task.WaitReason)
	}
}
