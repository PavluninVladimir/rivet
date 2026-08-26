package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Участники-люди (спека process «Очередь шагов человека», «Review с
// хостинга как вердикт участника»).

func reviewAgentAndOwner() policy.Process {
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{
		{Agent: &policy.AgentRef{}},
		{User: &policy.UserRef{Role: policy.RoleOwner}},
	}
	return p
}

// openHumanRun — открытый запуск человека текущего входа.
func openHumanRun(t *testing.T, f policyFixture) store.StepRun {
	t.Helper()
	task, _ := f.st.GetTask(context.Background(), f.task.ID)
	runs, err := f.st.CurrentStepRuns(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.IsUser() && r.Verdict == "" {
			return r
		}
	}
	t.Fatalf("нет открытого запуска человека: %+v", runs)
	return store.StepRun{}
}

// Агент и владелец на шаге review: сессия агента стартует, запуск человека
// ждёт в очереди владельца; шаг закрывается вердиктами обоих.
func TestHumanAndAgentReview(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	setProcess(t, f, reviewAgentAndOwner())
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW || as.Participant != "p1" {
		t.Fatalf("агент-ревьюер: %+v", as)
	}
	items, err := f.st.MySteps(ctx, f.owner.ID, f.owner.Login)
	if err != nil || len(items) != 1 || items[0].Run.Participant != "p2" || items[0].Addressed != "role:owner" {
		t.Fatalf("очередь владельца: %v %+v", err, items)
	}
	// Агент одобрил — шаг ждёт человека.
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 0 {
		t.Fatal("без вердикта человека шаг не закрыт")
	}
	run := openHumanRun(t, f)
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.ApplyVerdict(ctx, task, run, policy.OutcomeOk, "", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("после вердикта владельца — ожидание merge")
	}
	verdicts := eventsOfType(t, f.st, f.task.ID, "task.verdict")
	if len(verdicts) != 1 || verdicts[0].ActorID != f.owner.Login {
		t.Fatalf("событие вердикта от имени владельца: %+v", verdicts)
	}
	if items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login); len(items) != 0 {
		t.Fatalf("очередь пуста после вердикта: %+v", items)
	}
	// Второй вердикт по тому же запуску — запуск закрыт.
	if err := f.e.ApplyVerdict(ctx, task, run, policy.OutcomeOk, "", f.owner.Login); !errors.Is(err, ErrRunClosed) {
		t.Fatalf("повторный вердикт: %v", err)
	}
}

// Замечания человека уходят на исправление с автором, отказ расходует проход.
func TestHumanChangesRemarks(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{{User: &policy.UserRef{Login: f.owner.Login}}}
	setProcess(t, f, p)
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	run := openHumanRun(t, f)
	if run.UserLogin != f.owner.Login {
		t.Fatalf("запуск по логину: %+v", run)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.ApplyVerdict(ctx, task, run, policy.OutcomeChanges, "переименуй функцию", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || task.StepRejections["review"] != 1 {
		t.Fatalf("после замечаний человека: %s %v", task.Status, task.StepRejections)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fix := f.out.lastAssign(t)
	if fix.Stage != pb.StageResult_FIXING || !strings.Contains(fix.ExtraContext, "переименуй функцию") {
		t.Fatalf("контекст исправления: %+v", fix)
	}
}

// Человек исполняет шаг code: без сессии агента, «готово» ведёт на проверки.
func TestHumanExecutesCodeStep(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	p := policy.DefaultProcess()
	p.Steps[0].Participants = []policy.Participant{{User: &policy.UserRef{Role: policy.RoleOwner}}}
	setProcess(t, f, p)
	before := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.out.assignsSince(before); len(got) != 0 {
		t.Fatalf("человеку Assignment не шлётся: %d", len(got))
	}
	items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login)
	if len(items) != 1 || items[0].Run.StepKind != policy.StepCode {
		t.Fatalf("очередь: %+v", items)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskRunning || task.RunnerID != "" || task.Branch == "" {
		t.Fatalf("шаг code у человека: статус running без runner'а и с веткой, got %s runner=%q branch=%q", task.Status, task.RunnerID, task.Branch)
	}
	if err := f.e.ApplyVerdict(ctx, task, items[0].Run, policy.OutcomeOk, "сделано в ветке", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("после «готово» — проверки агентом: %+v", as)
	}
}

// Review с хостинга при участнике-человеке: одобрение закрывает его запуск.
func TestHostingApprovalClosesHumanRun(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{{User: &policy.UserRef{Role: policy.RoleOwner}}}
	setProcess(t, f, p)
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	// Чужой ревьюер на хостинге запуск владельца не закрывает.
	reacted, err := f.e.OnExternalReview(ctx, task, "approved", "stranger", "LGTM", "https://gh/r/pull/1#r0")
	if err != nil || reacted {
		t.Fatalf("одобрение не участника проекта: %v %v", err, reacted)
	}
	if items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login); len(items) != 1 {
		t.Fatalf("запуск владельца должен остаться: %+v", items)
	}
	reacted, err = f.e.OnExternalReview(ctx, task, "approved", f.owner.Login, "LGTM", "https://gh/r/pull/1#r1")
	if err != nil || !reacted {
		t.Fatalf("одобрение с хостинга: %v %v", err, reacted)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("одобрение с хостинга должно закрыть шаг review")
	}
	if items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login); len(items) != 0 {
		t.Fatalf("очередь пуста: %+v", items)
	}
}

// Отмена задачи закрывает запуски людей: очередь пустеет.
func TestCancelClosesHumanRuns(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{{User: &policy.UserRef{Role: policy.RoleOwner}}}
	setProcess(t, f, p)
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login); len(items) != 1 {
		t.Fatalf("очередь до отмены: %+v", items)
	}
	// Отмена через решение человека: задача в review не отменяется напрямую,
	// поэтому блокируем вопросом и отменяем.
	if err := f.st.BlockTask(ctx, f.task.ID, "вопрос", store.EventInput{ActorKind: domain.ActorSystem}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ResolveTask(ctx, f.task.ID, "", f.owner.Login, true); err != nil {
		t.Fatal(err)
	}
	if items, _ := f.st.MySteps(ctx, f.owner.ID, f.owner.Login); len(items) != 0 {
		t.Fatalf("очередь после отмены: %+v", items)
	}
}

// Вердикт записан, продвижение не случилось (сбой): тик дожимает шаг.
func TestReconcileClosedStep(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	p := policy.DefaultProcess()
	p.Steps[2].Participants = []policy.Participant{{User: &policy.UserRef{Role: policy.RoleOwner}}}
	setProcess(t, f, p)
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	run := openHumanRun(t, f)
	// Вердикт напрямую в store, без evaluateStep — как при сбое после записи.
	if ok, err := f.st.RecordUserVerdict(ctx, run.ID, f.owner.Login, policy.OutcomeOk, ""); err != nil || !ok {
		t.Fatalf("вердикт: %v %v", err, ok)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 0 {
		t.Fatal("до тика шаг не продвинут")
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("тик должен дожать исход шага")
	}
	// Повторный тик ничего не делает.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := eventsOfType(t, f.st, f.task.ID, "task.review_passed"); len(evs) != 1 {
		t.Fatal("дожим идемпотентен")
	}
}
