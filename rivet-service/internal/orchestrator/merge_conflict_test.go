package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Сценарии change add-merge-conflict-recovery (спеки task-pipeline
// «Конфликт с базовой веткой при merge», human-escalation «Причины
// эскалации»).

// conflictSCM — хостинг, отклоняющий merge конфликтом заданное число раз
// или прочей ошибкой.
type conflictSCM struct {
	diffSCM
	mu        sync.Mutex
	conflicts int
	mergeErr  error
}

func (c *conflictSCM) Merge(ctx context.Context, repo string, number int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mergeErr != nil {
		return "", c.mergeErr
	}
	if c.conflicts > 0 {
		c.conflicts--
		return "", fmt.Errorf("github merge: %w: Pull Request has merge conflicts", scm.ErrMergeConflict)
	}
	return c.diffSCM.Merge(ctx, repo, number)
}

type conflictFixture struct {
	policyFixture
	sc  *conflictSCM
	dep domain.Task
}

// newConflictFixture — задача A с авто-merge и зависимая от неё B.
// attemptLimit задаёт лимит попыток проекта, а с ним лимит возвратов шага
// merge (0 — значение по умолчанию).
func newConflictFixture(t *testing.T, conflicts, attemptLimit int) conflictFixture {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	sc := &conflictSCM{diffSCM: diffSCM{diff: "diff --git a/src/main.js b/src/main.js\n"}, conflicts: conflicts}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	o := policy.Overrides{AutoMerge: &on}
	if attemptLimit > 0 {
		o.AttemptLimit = &attemptLimit
	}
	if _, err := st.SaveProjectPolicy(ctx, p.ID, o, "owner"); err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Epic", "")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B", Deps: []string{task.ID}, Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: store.AgentRivet, Capabilities: []string{"coding"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: store.AgentRivet, Capabilities: []string{"coding", "review"}})
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	pf := policyFixture{st: st, sc: &sc.diffSCM, out: out, e: e, owner: owner, p: p, epic: epic, task: task}
	return conflictFixture{policyFixture: pf, sc: sc, dep: dep}
}

// driveConflictFix проводит возвращённую задачу через исправление,
// проверки и review: поручение о конфликте должно дойти до агента.
func driveConflictFix(t *testing.T, f conflictFixture) error {
	t.Helper()
	ctx := context.Background()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_FIXING || as.TaskId != f.task.ID {
		t.Fatalf("ожидали FIXING задачи A, got %v %s", as.Stage, as.TaskId)
	}
	if !strings.Contains(as.ExtraContext, "git merge --no-commit --no-ff origin/main") {
		t.Fatalf("нет поручения влить базовую ветку: %q", as.ExtraContext)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("ожидали REVIEW, got %v", as.Stage)
	}
	return f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_REVIEW, Ok: true})
}

func openConflictEscalations(t *testing.T, st *store.Store, taskID string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM attention WHERE task_id=$1 AND reason=$2 AND status <> 'resolved'`,
		taskID, string(domain.AttMergeConflict)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Конфликт при авто-merge возвращает задачу агенту без расхода попыток;
// после разрешения она доходит до done, зависимая задача стартует.
func TestMergeConflictReturnsToCodeAndCompletes(t *testing.T) {
	f := newConflictFixture(t, 1, 0)
	ctx := context.Background()
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	task, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskFixing || task.StepID != policy.StepCode {
		t.Fatalf("задача должна вернуться на code: %s %s", task.Status, task.StepID)
	}
	if task.AttemptUsed != 0 || task.ReviewRejections != 0 {
		t.Fatalf("конфликт не должен тратить попытки: used=%d review=%d", task.AttemptUsed, task.ReviewRejections)
	}
	evs := eventsOfType(t, f.st, f.task.ID, "task.merge_conflict")
	if len(evs) != 1 {
		t.Fatalf("ожидалось одно событие о конфликте, got %d", len(evs))
	}
	pl := evs[0].Payload
	if pl["base"] != "main" || pl["returns"] != float64(1) || pl["limit"] != float64(3) ||
		pl["exhausted"] != false || pl["manual"] != false || !strings.Contains(fmt.Sprint(pl["detail"]), "git merge") {
		t.Fatalf("payload события: %+v", pl)
	}
	if !strings.Contains(evs[0].Text, "(1/3)") {
		t.Fatalf("текст события: %q", evs[0].Text)
	}
	if n := openConflictEscalations(t, f.st, f.task.ID); n != 0 {
		t.Fatalf("конфликт в пределах лимита не эскалируется: %d", n)
	}
	if got := taskStatus(t, f.st, f.dep.ID); got != domain.TaskQueued {
		t.Fatalf("зависимая задача не должна стартовать до merge: %s", got)
	}

	if err := driveConflictFix(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("после разрешения конфликта задача должна быть done: %s", got)
	}
	if len(f.sc.merged) != 1 {
		t.Fatalf("PR должен смержиться один раз: %v", f.sc.merged)
	}
	if got := taskStatus(t, f.st, f.dep.ID); got == domain.TaskQueued || got == domain.TaskBlocked {
		t.Fatalf("зависимая задача должна стать готовой: %s", got)
	}
}

// Сверх лимита задача остаётся на merge с одной эскалацией; ручной merge
// при конфликте сбрасывает счётчик и снова отдаёт задачу агенту, а ручной
// merge без конфликта завершает задачу и закрывает эскалацию.
func TestMergeConflictLimitAndManualMerge(t *testing.T) {
	f := newConflictFixture(t, 100, 1)
	ctx := context.Background()
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFixing {
		t.Fatalf("первый конфликт возвращает задачу: %s", got)
	}
	if err := driveConflictFix(t, f); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge {
		t.Fatalf("сверх лимита задача ждёт на merge: %s %s", task.Status, task.StepID)
	}
	if task.AttemptUsed != 0 {
		t.Fatalf("попытки не тронуты: %d", task.AttemptUsed)
	}
	if n := openConflictEscalations(t, f.st, f.task.ID); n != 1 {
		t.Fatalf("ожидалась одна эскалация MERGE_CONFLICT, got %d", n)
	}
	evs := eventsOfType(t, f.st, f.task.ID, "task.merge_conflict")
	if last := evs[len(evs)-1]; last.Payload["exhausted"] != true {
		t.Fatalf("последнее событие должно отметить исчерпание: %+v", last.Payload)
	}

	// Человек жмёт merge, конфликт на месте: счётчик сброшен, задача у агента.
	err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID)
	var mc *MergeConflictError
	if !errors.As(err, &mc) || !mc.Returned || !errors.Is(err, scm.ErrMergeConflict) {
		t.Fatalf("ручной merge при конфликте: %v", err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFixing {
		t.Fatalf("ручной merge возвращает задачу агенту: %s", got)
	}
	if n := openConflictEscalations(t, f.st, f.task.ID); n != 0 {
		t.Fatalf("эскалация закрывается при возврате: %d", n)
	}
	evs = eventsOfType(t, f.st, f.task.ID, "task.merge_conflict")
	if last := evs[len(evs)-1]; last.Payload["manual"] != true || last.ActorID != f.owner.ID || last.Payload["returns"] != float64(1) {
		t.Fatalf("событие ручного merge: %+v %s", last.Payload, last.ActorID)
	}

	// Снова конфликт после исправления: лимит 1 исчерпан, эскалация одна.
	if err := driveConflictFix(t, f); err != nil {
		t.Fatal(err)
	}
	if n := openConflictEscalations(t, f.st, f.task.ID); n != 1 {
		t.Fatalf("ожидалась одна эскалация, got %d", n)
	}

	// Человек разрешил конфликт на хостинге и жмёт merge.
	f.sc.conflicts = 0
	if err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("после ручного merge задача done: %s", got)
	}
	if n := openConflictEscalations(t, f.st, f.task.ID); n != 0 {
		t.Fatalf("merge закрывает эскалацию: %d", n)
	}
}

// Прочий отказ хостинга (защита ветки) ведёт себя по-прежнему: задача
// ждёт человека на merge, на code не возвращается.
func TestMergeNonConflictFailureUnchanged(t *testing.T) {
	f := newConflictFixture(t, 0, 0)
	f.sc.mergeErr = errors.New("github merge: 405: Required status check is expected")
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(context.Background(), f.task.ID)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge {
		t.Fatalf("задача ждёт человека на merge: %s %s", task.Status, task.StepID)
	}
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.merge_failed")); n != 1 {
		t.Fatalf("ожидалось событие merge_failed: %d", n)
	}
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.merge_conflict")); n != 0 {
		t.Fatalf("не конфликт — нет события о конфликте: %d", n)
	}
	err := f.e.MergeTask(context.Background(), f.task.ID, f.owner.ID)
	var mc *MergeConflictError
	if err == nil || errors.As(err, &mc) {
		t.Fatalf("ручной merge при прочем отказе: %v", err)
	}
}

// Два одновременных подтверждения merge одного конфликтующего PR идут по
// очереди: первое возвращает задачу агенту, второе видит, что задача уже
// не в review, и ничего не мержит и не уводит.
func TestMergeConflictConcurrentManualMerge(t *testing.T) {
	f := newConflictFixture(t, 100, 1)
	ctx := context.Background()
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	if err := driveConflictFix(t, f); err != nil {
		t.Fatal(err)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.StepID != policy.StepMerge {
		t.Fatalf("задача должна ждать на merge: %s", task.StepID)
	}
	before := len(eventsOfType(t, f.st, f.task.ID, "task.merge_conflict"))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.e.MergeTask(ctx, f.task.ID, f.owner.ID)
		}(i)
	}
	wg.Wait()
	returned, rejected := 0, 0
	for _, err := range errs {
		var mc *MergeConflictError
		var bad domain.ErrBadTransition
		switch {
		case errors.As(err, &mc) && mc.Returned:
			returned++
		case errors.As(err, &bad):
			rejected++
		default:
			t.Fatalf("неожиданный исход: %v", err)
		}
	}
	if returned != 1 || rejected != 1 {
		t.Fatalf("ожидался один возврат и один отказ, got %d/%d", returned, rejected)
	}
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.merge_conflict")) - before; n != 1 {
		t.Fatalf("ожидалось одно новое событие о конфликте, got %d", n)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFixing {
		t.Fatalf("задача у агента: %s", got)
	}
}

// Конфликт и успешный merge одновременно: хостинг отклоняет только первый
// вызов. Под блокировкой второй подтверждающий видит, что задача уже у
// агента, и PR не мержит — смерженного PR при незавершённой задаче нет.
func TestMergeConflictRacesWithSuccessfulMerge(t *testing.T) {
	f := newConflictFixture(t, 0, 0)
	ctx := context.Background()
	// Авто-merge выключен: задача ждёт человека на merge.
	off := false
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AutoMerge: &off}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	f.sc.mu.Lock()
	f.sc.conflicts = 1
	f.sc.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.e.MergeTask(ctx, f.task.ID, f.owner.ID)
		}()
	}
	wg.Wait()
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFixing || len(f.sc.merged) != 0 {
		t.Fatalf("задача у агента, PR не смержен: %s, merged=%v", task.Status, f.sc.merged)
	}
}

// Блокировки операций merge: одна задача — по очереди, разные задачи друг
// друга не ждут, записи о снятых блокировках не копятся.
func TestTaskLocks(t *testing.T) {
	var l taskLocks
	unlockA := l.lock("a")

	other := make(chan struct{})
	go func() { l.lock("b")(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("блокировка другой задачи не должна ждать")
	}

	entered := make(chan func())
	go func() { entered <- l.lock("a") }()
	select {
	case <-entered:
		t.Fatal("вторая операция той же задачи не должна войти, пока первая идёт")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case unlock := <-entered:
		unlock()
	case <-time.After(time.Second):
		t.Fatal("после снятия блокировки вторая операция должна войти")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) != 0 {
		t.Fatalf("снятые блокировки должны удаляться: %v", l.m)
	}
}

// Человек подтверждает merge, пока идёт review, а PR конфликтует: задача
// уходит агенту на разрешение конфликта, запуск ревьюера отменяется.
func TestMergeConflictManualDuringReview(t *testing.T) {
	f := newConflictFixture(t, 1, 0)
	ctx := context.Background()
	off := false
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AutoMerge: &off}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("ожидали REVIEW, got %v", as.Stage)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.StepID != policy.StepReview {
		t.Fatalf("задача должна быть на шаге review: %s", task.StepID)
	}

	err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID)
	var mc *MergeConflictError
	if !errors.As(err, &mc) || !mc.Returned {
		t.Fatalf("ручной merge на шаге review при конфликте: %v", err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFixing {
		t.Fatalf("задача должна уйти агенту: %s", got)
	}
	f.out.mu.Lock()
	cancelled := false
	for _, m := range f.out.sent {
		if m.GetCancel() != nil && m.GetCancel().TaskId == f.task.ID {
			cancelled = true
		}
	}
	f.out.mu.Unlock()
	if !cancelled {
		t.Fatal("запуск ревьюера должен быть отменён")
	}
	if err := driveConflictFix(t, f); err != nil {
		t.Fatal(err)
	}
	if err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("после разрешения задача done: %s", got)
	}
}

// Человек подтверждает merge, пока идёт review, и merge проходит: задача
// done, запуск ревьюера прерывается (CancelTask, сессия закрыта), поздний
// результат ревьюера отбрасывается. Раньше он записывал шаг review у
// задачи в done и падал на переходе done → review (e2e, overview.spec).
func TestManualMergeDuringReviewCancelsReviewer(t *testing.T) {
	f := newConflictFixture(t, 0, 0)
	ctx := context.Background()
	off := false
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AutoMerge: &off}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	review := f.out.lastAssign(t)
	if review.Stage != pb.StageResult_REVIEW {
		t.Fatalf("ожидали REVIEW, got %v", review.Stage)
	}

	if err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID); err != nil {
		t.Fatalf("ручной merge на шаге review: %v", err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("после merge задача done: %s", got)
	}
	f.out.mu.Lock()
	cancelled := false
	for _, m := range f.out.sent {
		if m.GetCancel() != nil && m.GetCancel().TaskId == f.task.ID {
			cancelled = true
		}
	}
	f.out.mu.Unlock()
	if !cancelled {
		t.Fatal("ревьюер должен получить отмену стадии")
	}
	var outcome string
	var ended bool
	if err := f.st.Pool.QueryRow(ctx, `SELECT coalesce(outcome, ''), ended_at IS NOT NULL FROM sessions WHERE id=$1`,
		review.SessionId).Scan(&outcome, &ended); err != nil {
		t.Fatal(err)
	}
	if !ended || outcome != store.RunsCancelledByMerge {
		t.Fatalf("сессия ревьюера закрыта merge: ended=%v outcome=%q", ended, outcome)
	}

	// Поздний результат ревьюера: без ошибки и без шага review в журнале.
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: review.SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatalf("поздний результат ревьюера: %v", err)
	}
	var reviewSteps int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id=$1 AND type='task.step' AND payload->>'step'=$2`,
		f.task.ID, policy.StepReview).Scan(&reviewSteps); err != nil {
		t.Fatal(err)
	}
	if reviewSteps != 0 {
		t.Fatalf("шаг review у задачи в done: %d событий", reviewSteps)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("поздний результат не меняет задачу: %s", got)
	}
}
