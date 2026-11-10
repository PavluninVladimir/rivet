package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// fix-pr-without-test-step: PR создаётся перед первым шагом, которому он
// нужен (review или merge), при любом составе процесса. Задача без PR не
// переходит в done молча: merge создаёт PR или отказывает.

// processWithout — процесс по умолчанию с отключёнными шагами.
func processWithout(ids ...string) policy.Process {
	proc := policy.DefaultProcess()
	for i := range proc.Steps {
		for _, id := range ids {
			if proc.Steps[i].ID == id {
				off := false
				proc.Steps[i].Enabled = &off
			}
		}
	}
	return proc
}

// saveProcessAutoMerge — процесс проекта и авто-merge одной версией
// политики: версия проекта заменяет предыдущую целиком.
func saveProcessAutoMerge(t *testing.T, f policyFixture, proc policy.Process) {
	t.Helper()
	proc = withoutTesterRole(proc)
	on := true
	if _, err := f.st.SaveProjectPolicy(context.Background(), f.p.ID,
		policy.Overrides{Process: &proc, AutoMerge: &on}, "owner"); err != nil {
		t.Fatal(err)
	}
}

// finishCoding — первое назначение (CODING) завершается успехом.
func finishCoding(t *testing.T, f policyFixture) {
	t.Helper()
	ctx := context.Background()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING {
		t.Fatalf("первое назначение: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
}

func mustTask(t *testing.T, f policyFixture) domain.Task {
	t.Helper()
	task, err := f.st.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// stepEvent — событие task.step шага с исходом.
func stepEvent(t *testing.T, f policyFixture, step, outcome string) domain.Event {
	t.Helper()
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.step") {
		if ev.Payload["step"] == step && ev.Payload["outcome"] == outcome {
			return ev
		}
	}
	t.Fatalf("нет события task.step %s/%s", step, outcome)
	return domain.Event{}
}

// Процесс без test: PR создаётся при переходе на review, ревьюер получает
// diff, событие перехода называет пропущенный шаг, merge человеком
// по-настоящему мержит PR.
func TestPRBeforeReviewWithoutTestStep(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	finishCoding(t, f)

	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepReview {
		t.Fatalf("после coding без test — review: %s %s", task.Status, task.StepID)
	}
	if task.PRURL == "" || len(f.sc.prCreated) != 1 || f.sc.prCreated[0] != task.Branch {
		t.Fatalf("PR ветки задачи не создан: %q %v", task.PRURL, f.sc.prCreated)
	}
	code := stepEvent(t, f, policy.StepCode, policy.OutcomeOk)
	if sk, _ := code.Payload["skipped"].([]any); len(sk) != 1 || sk[0] != policy.StepTest ||
		!strings.Contains(code.Text, "пропущен как отключённый") {
		t.Fatalf("событие перехода должно назвать пропущенный шаг: %q %+v", code.Text, code.Payload)
	}
	var entry bool
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
		if ev.Payload["pr"] == task.PRURL && strings.Contains(ev.Text, "реализация готова, PR создан") {
			entry = true
		}
	}
	if !entry {
		t.Fatal("нет события входа на review с PR и честным текстом")
	}

	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW || !strings.Contains(as.ExtraContext, "diff --git a/src/x.go") {
		t.Fatalf("ревьюер должен получить diff PR: %v %q", as.Stage, as.ExtraContext)
	}
	if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge || len(f.sc.merged) != 0 {
		t.Fatalf("без авто-merge задача ждёт человека: %s %s %v", task.Status, task.StepID, f.sc.merged)
	}
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone || len(f.sc.merged) != 1 || f.sc.merged[0] != 42 {
		t.Fatalf("merge должен вызвать хостинг и закрыть задачу: %s %v", got, f.sc.merged)
	}
	if len(f.sc.prCreated) != 1 {
		t.Fatalf("PR создаётся один раз: %v", f.sc.prCreated)
	}
}

// Процесс без test и review с авто-merge: PR создаётся при входе в merge,
// авто-merge мержит его, и только после этого задача done.
func TestNoTestNoReviewAutoMerge(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
	finishCoding(t, f)

	task := mustTask(t, f)
	if task.Status != domain.TaskDone {
		t.Fatalf("авто-merge должен завершить задачу: %s %s", task.Status, task.StepID)
	}
	if task.PRURL == "" || len(f.sc.prCreated) != 1 {
		t.Fatalf("PR не создан: %q %v", task.PRURL, f.sc.prCreated)
	}
	if len(f.sc.merged) != 1 || f.sc.merged[0] != 42 {
		t.Fatalf("хостинг не получил merge: %v", f.sc.merged)
	}
	code := stepEvent(t, f, policy.StepCode, policy.OutcomeOk)
	if sk, _ := code.Payload["skipped"].([]any); len(sk) != 2 || !strings.Contains(code.Text, "test, review пропущены") {
		t.Fatalf("событие перехода должно назвать оба пропущенных шага: %q %+v", code.Text, code.Payload)
	}
	passed := eventsOfType(t, f.st, f.task.ID, "task.review_passed")
	if len(passed) != 1 || strings.Contains(passed[0].Text, "review пройден") || passed[0].Payload["review_step"] != false ||
		passed[0].Payload["pr"] != task.PRURL {
		t.Fatalf("вход в merge без review: %+v", passed)
	}
	var auto bool
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
		if ev.Payload["auto_merge"] == true {
			auto = true
		}
	}
	if !auto {
		t.Fatal("нет события авто-merge")
	}
}

// Хостинг отказал в merge: задача без test и review остаётся в ожидании
// merge, done только после настоящего merge.
func TestNoTestNoReviewDoneOnlyAfterMerge(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
	f.sc.mergeErr = errors.New("github merge: 405: required status check")
	finishCoding(t, f)

	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge || task.PRURL == "" {
		t.Fatalf("отказ хостинга оставляет задачу на merge с PR: %s %s %q", task.Status, task.StepID, task.PRURL)
	}
	if len(eventsOfType(t, f.st, f.task.ID, "task.merge_failed")) != 1 {
		t.Fatal("нет события о неудачном merge")
	}
	f.sc.mergeErr = nil
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone || len(f.sc.merged) != 1 {
		t.Fatalf("после merge задача done: %s %v", got, f.sc.merged)
	}
}

// Процесс без test и review, авто-merge выключен: задача ждёт человека с
// PR, merge кнопкой мержит его.
func TestNoTestNoReviewManualMerge(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	setProcess(t, f, processWithout(policy.StepTest, policy.StepReview))
	finishCoding(t, f)

	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge || task.PRURL == "" || len(f.sc.merged) != 0 {
		t.Fatalf("задача ждёт подтверждения merge с PR: %s %s %q %v", task.Status, task.StepID, task.PRURL, f.sc.merged)
	}
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone || len(f.sc.merged) != 1 {
		t.Fatalf("merge кнопкой: %s %v", got, f.sc.merged)
	}
}

// Задача дошла до merge без PR (до исправления или человек подтвердил
// merge раньше): merge создаёт PR и мержит его, а не закрывает задачу молча.
func TestMergeTaskCreatesMissingPR(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	setProcess(t, f, processWithout(policy.StepTest, policy.StepReview))
	finishCoding(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE tasks SET pr_url='' WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, f)
	if task.Status != domain.TaskDone || task.PRURL == "" || len(f.sc.prCreated) != 2 || len(f.sc.merged) != 1 {
		t.Fatalf("merge без PR должен создать и смержить PR: %s %q %v %v", task.Status, task.PRURL, f.sc.prCreated, f.sc.merged)
	}
}

// Хостинг не создал PR при merge: задача не done, ошибка ErrPRNotCreated.
func TestMergeTaskWithoutPRFailsLoudly(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	setProcess(t, f, processWithout(policy.StepTest, policy.StepReview))
	finishCoding(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE tasks SET pr_url='' WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.sc.createPRErr = errors.New("github create PR: 422: No commits between main and agent/task-1")
	err := f.e.MergeTask(ctx, f.task.ID, "human")
	if !errors.Is(err, ErrPRNotCreated) || !strings.Contains(err.Error(), "No commits") {
		t.Fatalf("ожидали ErrPRNotCreated с ответом хостинга: %v", err)
	}
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("без PR задача остаётся в ожидании merge: %s %v", task.Status, f.sc.merged)
	}
}

// Хостинг не создал PR перед review: задача не застревает на пройденном
// шаге, а эскалируется человеку с ответом хостинга.
func TestPRCreateFailureEscalates(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 422: No commits between main and agent/task-1")
	finishCoding(t, f)

	task := mustTask(t, f)
	if task.Status != domain.TaskFailed {
		t.Fatalf("задача без PR эскалируется: %s %s", task.Status, task.StepID)
	}
	atts, err := f.st.ListAttention(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Reason != domain.AttPRNotCreated || !strings.Contains(atts[0].Message, "не удалось создать PR") ||
		!strings.Contains(atts[0].Message, "No commits") {
		t.Fatalf("эскалация PR_NOT_CREATED с ответом хостинга: %+v", atts)
	}
	if task.StepID != policy.StepReview {
		t.Fatalf("шаг задачи переставлен на review, повтор продолжит с него: %s", task.StepID)
	}
	for _, as := range f.out.assignsSince(0) {
		if as.Stage == pb.StageResult_REVIEW {
			t.Fatal("review без PR не назначается")
		}
	}
}

// Процесс по умолчанию: PR создаётся при переходе с test на review, как
// раньше, текст события прежний.
func TestDefaultProcessPRAfterChecks(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	driveToReview(t, f)
	task := mustTask(t, f)
	if task.StepID != policy.StepReview || task.PRURL == "" || len(f.sc.prCreated) != 1 {
		t.Fatalf("PR после проверок: %s %q %v", task.StepID, task.PRURL, f.sc.prCreated)
	}
	var found bool
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
		if ev.Text == "проверки прошли, PR создан — ожидание review" && ev.Payload["pr"] == task.PRURL {
			found = true
		}
	}
	if !found {
		t.Fatal("нет события «проверки прошли, PR создан»")
	}
	if sk := stepEvent(t, f, policy.StepCode, policy.OutcomeOk).Payload["skipped"]; sk != nil {
		t.Fatalf("без отключённых шагов пропусков нет: %v", sk)
	}
}

// fastPRRetries — паузы повторов создания PR в тестах.
func fastPRRetries(t *testing.T) {
	t.Helper()
	old := prRetryPauses
	prRetryPauses = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { prRetryPauses = old })
}

// Пустой diff только что созданного PR — неизвестный список файлов:
// авто-merge откладывается, а не мержит без проверки путей.
func TestEmptyDiffDefersAutoMerge(t *testing.T) {
	f := newPolicyFixture(t, "")
	saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
	finishCoding(t, f)
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("пустой diff не мержится автоматически: %s %v", task.Status, f.sc.merged)
	}
	def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred")
	if len(def) != 1 || def[0].Payload["reason"] != "files_unknown" {
		t.Fatalf("merge отложен как files_unknown: %+v", def)
	}
}

// Хостинг не создал PR при входе в merge: задача ждёт на merge, событие о
// неудачном merge и эскалация PR_NOT_CREATED; подтверждение человека
// создаёт PR, мержит и закрывает эскалацию.
func TestMergeEntryWithoutPRWaits(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
	f.sc.createPRErr = errors.New("github create PR: 422: Validation failed")
	finishCoding(t, f)
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepMerge || task.PRURL != "" || len(f.sc.merged) != 0 {
		t.Fatalf("задача ждёт на merge без PR: %s %s %q %v", task.Status, task.StepID, task.PRURL, f.sc.merged)
	}
	failed := eventsOfType(t, f.st, f.task.ID, "task.merge_failed")
	if len(failed) != 1 || failed[0].Payload["reason"] != "pr_not_created" {
		t.Fatalf("событие о неудачном merge: %+v", failed)
	}
	passed := eventsOfType(t, f.st, f.task.ID, "task.review_passed")
	if len(passed) != 1 || !strings.Contains(passed[0].Text, "PR не создан") || passed[0].Payload["pr"] != nil {
		t.Fatalf("вход в merge без PR честно: %+v", passed)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttPRNotCreated {
		t.Fatalf("эскалация PR_NOT_CREATED: %+v", atts)
	}
	f.sc.createPRErr = nil
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, f)
	atts, _ = f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskDone || len(f.sc.merged) != 1 || len(atts) != 0 {
		t.Fatalf("merge человеком создаёт PR и закрывает эскалацию: %s %v %+v", task.Status, f.sc.merged, atts)
	}
}

// После старта задачи на merge без PR получают его сами: с авто-merge
// задача мержится, без него ждёт подтверждения уже с PR.
func TestResumePendingPRs(t *testing.T) {
	ctx := context.Background()
	for _, auto := range []bool{true, false} {
		f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
		if auto {
			saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
		} else {
			setProcess(t, f, processWithout(policy.StepTest, policy.StepReview))
		}
		// Задача дошла до merge без PR (прошлая версия rivetd).
		f.sc.createPRErr = errors.New("github create PR: 422: Validation failed")
		finishCoding(t, f)
		f.sc.createPRErr = nil
		f.e.ResumePendingPRs(ctx)
		task := mustTask(t, f)
		switch {
		case auto && (task.Status != domain.TaskDone || len(f.sc.merged) != 1):
			t.Fatalf("с авто-merge задача мержится после старта: %s %v", task.Status, f.sc.merged)
		case !auto && (task.Status != domain.TaskReview || task.PRURL == "" || len(f.sc.merged) != 0):
			t.Fatalf("без авто-merge задача ждёт с PR: %s %q %v", task.Status, task.PRURL, f.sc.merged)
		}
	}
}

// Хостинг отказал, а открытый PR ветки уже есть (прошлый запрос его
// создал): PR записывается задаче, эскалации нет.
func TestCreatePRAdoptsOpenPR(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = fmt.Errorf("github create PR: %w: 422: A pull request already exists for owner:agent/task-1", scm.ErrPRExists)
	f.sc.openPR = &scm.PR{Number: 7, URL: "https://github.com/owner/repo/pull/7"}
	finishCoding(t, f)
	task := mustTask(t, f)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskReview || task.PRURL != "https://github.com/owner/repo/pull/7" || len(atts) != 0 {
		t.Fatalf("открытый PR записан без эскалации: %s %q %+v", task.Status, task.PRURL, atts)
	}
}

// Временный сбой хостинга повторяется с паузой: PR создаётся без
// эскалации.
func TestCreatePRRetriesTransient(t *testing.T) {
	fastPRRetries(t)
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 502: bad gateway")
	f.sc.createPRFailN = 2
	finishCoding(t, f)
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.PRURL == "" || f.sc.createPRCalls != 3 {
		t.Fatalf("PR после двух повторов: %s %q вызовов %d", task.Status, task.PRURL, f.sc.createPRCalls)
	}
}

// Сбой хостинга не прошёл за повторы: эскалация PR_NOT_CREATED. Повтор при
// всё ещё недоступном хостинге задачу не меняет, а при ответившем создаёт
// PR и продолжает с review без нового прохода агента реализации.
func TestRetryAfterPRFailureCreatesOnlyPR(t *testing.T) {
	ctx := context.Background()
	fastPRRetries(t)
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 503: unavailable")
	finishCoding(t, f)
	if task := mustTask(t, f); task.Status != domain.TaskFailed || f.sc.createPRCalls != 3 {
		t.Fatalf("эскалация после повторов: %s вызовов %d", task.Status, f.sc.createPRCalls)
	}
	if err := f.e.PrepareRetry(ctx, f.task.ID); !errors.Is(err, ErrPRNotCreated) {
		t.Fatalf("хостинг всё ещё недоступен: %v", err)
	}
	if task := mustTask(t, f); task.Status != domain.TaskFailed {
		t.Fatalf("неудачный повтор задачу не меняет: %s", task.Status)
	}
	f.sc.createPRErr = nil
	if err := f.e.PrepareRetry(ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ResolveTask(ctx, f.task.ID, "", "human", false); err != nil {
		t.Fatal(err)
	}
	mark := f.out.count()
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.StepID != policy.StepReview || task.PRURL == "" {
		t.Fatalf("повтор продолжает с review: %s %s %q", task.Status, task.StepID, task.PRURL)
	}
	for _, as := range f.out.assignsSince(mark) {
		if as.Stage != pb.StageResult_REVIEW {
			t.Fatalf("после повтора — только review, без реализации: %v", as.Stage)
		}
	}
	if len(f.out.assignsSince(mark)) != 1 {
		t.Fatalf("назначен ревьюер: %d", len(f.out.assignsSince(mark)))
	}
}

// Тексты краевых путей: переход fail на review называет ошибку шага, вход в
// merge в обход review не пишет «review пройден».
func TestEdgeEntryTexts(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	proc := processWithout(policy.StepTest)
	proc.Steps[0].On = &policy.Transitions{Ok: policy.StepMerge, Fail: policy.StepReview}
	setProcess(t, f, proc)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_CODING, Ok: false, Detail: "упал"}); err != nil {
		t.Fatal(err)
	}
	var viaFail bool
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
		if strings.Contains(ev.Text, "завершился с ошибкой, по процессу — review") {
			viaFail = true
		}
	}
	if !viaFail {
		t.Fatal("вход на review по fail называет ошибку шага")
	}

	g := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	proc = processWithout(policy.StepTest)
	proc.Steps[0].On = &policy.Transitions{Ok: policy.StepMerge}
	setProcess(t, g, proc)
	finishCoding(t, g)
	passed := eventsOfType(t, g.st, g.task.ID, "task.review_passed")
	if len(passed) != 1 || strings.Contains(passed[0].Text, "review пройден") || passed[0].Payload["review_step"] != false {
		t.Fatalf("вход в merge в обход review: %+v", passed)
	}
}

// Открытый PR ищется только на ответ «уже есть»: при прочих отказах чужой
// PR ветки задаче не записывается. Несколько подходящих PR — эскалация.
func TestOpenPROnlyOnAlreadyExists(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 422: No commits between main and agent/task-1")
	f.sc.openPR = &scm.PR{Number: 7, URL: "https://github.com/owner/repo/pull/7"}
	finishCoding(t, f)
	task := mustTask(t, f)
	if task.Status != domain.TaskFailed || task.PRURL != "" || f.sc.openPRCalls != 0 {
		t.Fatalf("на «нет изменений» открытый PR не ищется: %s %q поисков %d", task.Status, task.PRURL, f.sc.openPRCalls)
	}

	g := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, g, processWithout(policy.StepTest))
	g.sc.createPRErr = fmt.Errorf("github create PR: %w: 422: A pull request already exists for owner:agent/task-1", scm.ErrPRExists)
	g.sc.openPRAmbiguous = true
	finishCoding(t, g)
	task = mustTask(t, g)
	atts, _ := g.st.ListAttention(ctx, g.owner.ID)
	if task.Status != domain.TaskFailed || task.PRURL != "" || len(atts) != 1 || atts[0].Reason != domain.AttPRNotCreated ||
		!strings.Contains(atts[0].Message, "несколько") {
		t.Fatalf("несколько кандидатов — эскалация, а не выбор: %s %q %+v", task.Status, task.PRURL, atts)
	}
}

// После старта PR не создаётся задачам приостановленного Epic'а и задачам,
// чей PR человек закрыл (PR_CLOSED); созданный PR закрывает эскалацию
// PR_NOT_CREATED.
func TestResumePendingPRsFilters(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	setProcess(t, f, processWithout(policy.StepTest, policy.StepReview))
	f.sc.createPRErr = errors.New("github create PR: 422: Validation failed")
	finishCoding(t, f)
	f.sc.createPRErr = nil
	if atts, _ := f.st.ListAttention(ctx, f.owner.ID); len(atts) != 1 || atts[0].Reason != domain.AttPRNotCreated {
		t.Fatalf("эскалация PR_NOT_CREATED на merge: %+v", atts)
	}
	// PR закрыт человеком: новый не создаётся.
	if err := f.st.Escalate(ctx, f.p.ID, f.task.ID, domain.AttPRClosed, "PR закрыт"); err != nil {
		t.Fatal(err)
	}
	f.e.ResumePendingPRs(ctx)
	if task := mustTask(t, f); task.PRURL != "" {
		t.Fatalf("PR, закрытый человеком, заново не создаётся: %q", task.PRURL)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE attention SET status='resolved' WHERE reason=$1`, string(domain.AttPRClosed)); err != nil {
		t.Fatal(err)
	}
	// Epic на паузе: не трогается.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE epics SET status='paused' WHERE id=$1`, f.epic.ID); err != nil {
		t.Fatal(err)
	}
	f.e.ResumePendingPRs(ctx)
	if task := mustTask(t, f); task.PRURL != "" {
		t.Fatalf("задача приостановленного Epic'а не трогается: %q", task.PRURL)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, f.epic.ID); err != nil {
		t.Fatal(err)
	}
	f.e.ResumePendingPRs(ctx)
	task := mustTask(t, f)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.PRURL == "" || len(atts) != 0 {
		t.Fatalf("PR создан, эскалация PR_NOT_CREATED закрыта: %q %+v", task.PRURL, atts)
	}
}

// Задача вернулась на review без PR в обход повтора и хостинг отказывает:
// после нескольких попыток она эскалируется, а не ждёт в ready вечно.
func TestResumeWithoutPREscalates(t *testing.T) {
	ctx := context.Background()
	old := prResumePause
	prResumePause = 0
	t.Cleanup(func() { prResumePause = old })
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 422: No commits between main and agent/task-1")
	finishCoding(t, f)
	// Повтор мимо PrepareRetry: задача в ready на шаге review без PR.
	if err := f.st.ResolveTask(ctx, f.task.ID, "", "human", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < prResumeAttempts; i++ {
		if task := mustTask(t, f); i > 0 && (task.Status != domain.TaskReady || !strings.HasPrefix(task.WaitReason, "PR ветки не создан")) {
			t.Fatalf("попытка %d: задача ждёт с причиной: %s %q", i, task.Status, task.WaitReason)
		}
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task := mustTask(t, f)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskFailed || task.StepID != policy.StepReview || len(atts) != 1 || atts[0].Reason != domain.AttPRNotCreated {
		t.Fatalf("после попыток — эскалация PR_NOT_CREATED: %s %s %+v", task.Status, task.StepID, atts)
	}
	for _, as := range f.out.assignsSince(0) {
		if as.Stage == pb.StageResult_REVIEW {
			t.Fatal("review без PR не назначается")
		}
	}
}

// Переход fail на merge называет ошибку шага; повторный вход в merge не
// помечает задачу как прошедшую мимо review.
func TestMergeEntryTextsAfterFail(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	proc := processWithout(policy.StepTest, policy.StepReview)
	proc.Steps[0].On = &policy.Transitions{Fail: policy.StepMerge}
	setProcess(t, f, proc)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_CODING, Ok: false, Detail: "упал"}); err != nil {
		t.Fatal(err)
	}
	passed := eventsOfType(t, f.st, f.task.ID, "task.review_passed")
	if len(passed) != 1 || !strings.Contains(passed[0].Text, "завершился с ошибкой, по процессу — merge") {
		t.Fatalf("вход в merge по fail: %+v", passed)
	}
	text, reviewed := mergeEntryText(store.TaskProcess(mustTask(t, f)), policy.StepMerge, true, false)
	if text != "ожидание подтверждения merge" || !reviewed {
		t.Fatalf("повторный вход в merge: %q %v", text, reviewed)
	}
}

// Вторичный лимит GitHub на создание PR — временный отказ: повтор с паузой,
// а не сразу PR_NOT_CREATED.
func TestCreatePRRetriesRateLimit(t *testing.T) {
	fastPRRetries(t)
	oldMin, oldMax := prRateLimitPause, prRateLimitMaxPause
	prRateLimitPause, prRateLimitMaxPause = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { prRateLimitPause, prRateLimitMaxPause = oldMin, oldMax })
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = &scm.RateLimitError{Msg: "github create PR: лимит запросов хостинга: 403: secondary rate limit"}
	f.sc.createPRFailN = 2
	finishCoding(t, f)
	task := mustTask(t, f)
	if task.Status != domain.TaskReview || task.PRURL == "" || f.sc.createPRCalls != 3 {
		t.Fatalf("PR после повторов по лимиту: %s %q вызовов %d", task.Status, task.PRURL, f.sc.createPRCalls)
	}
}

// Пауза повтора после отказа по лимиту: не меньше нижней границы, по
// Retry-After, но не больше верхней; прочие сбои — обычная пауза.
func TestRetryPauseRateLimit(t *testing.T) {
	limit := func(after time.Duration) error { return &scm.RateLimitError{After: after, Msg: "лимит"} }
	for _, c := range []struct {
		err  error
		want time.Duration
	}{
		{errors.New("github create PR: 502: x"), 2 * time.Second},
		{limit(0), prRateLimitPause},
		{limit(15 * time.Second), 15 * time.Second},
		{limit(time.Hour), prRateLimitMaxPause},
	} {
		if got := retryPause(2*time.Second, c.err); got != c.want {
			t.Errorf("retryPause(%v) = %v, ждали %v", c.err, got, c.want)
		}
	}
}

// Ревью, проход 4: в проходе планировщика PR создаётся одной попыткой без
// пауз, а вся серия попыток ограничена общим пределом.
func TestEnsurePRTickAndDeadline(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	old, oldSeries := prRetryPauses, prSeriesTimeout
	prRetryPauses = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { prRetryPauses, prSeriesTimeout = old, oldSeries })
	f.sc.createPRErr = errors.New("github create PR: 502: bad gateway")
	task := mustTask(t, f)
	start := time.Now()
	if _, err := f.e.ensurePR(tickContext(ctx), task); err == nil || f.sc.createPRCalls != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("проход планировщика: одна попытка без пауз: %v вызовов %d за %v", err, f.sc.createPRCalls, time.Since(start))
	}
	prSeriesTimeout = 50 * time.Millisecond
	start = time.Now()
	if _, err := f.e.ensurePR(ctx, task); err == nil || time.Since(start) > 5*time.Second || f.sc.createPRCalls != 2 {
		t.Fatalf("серия попыток в пределе: %v вызовов %d за %v", err, f.sc.createPRCalls, time.Since(start))
	}
}

// Ревью, проход 4: временный сбой хостинга при переходе на review в
// проходе планировщика (дожим исхода) не эскалирует сразу: задача ждёт в
// ready на шаге review, следующий проход после паузы создаёт PR и входит на
// review.
func TestTickReviewTransientWaits(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	setProcess(t, f, processWithout(policy.StepTest))
	f.sc.createPRErr = errors.New("github create PR: 503: unavailable")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(tickContext(ctx), worker, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, f)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskReady || task.StepID != policy.StepReview || len(atts) != 0 || f.sc.createPRCalls != 1 ||
		!strings.HasPrefix(task.WaitReason, "PR ветки не создан") {
		t.Fatalf("задача ждёт PR в ready: %s %s эскалаций %d вызовов %d %q", task.Status, task.StepID, len(atts), f.sc.createPRCalls, task.WaitReason)
	}
	// Пауза не прошла: следующий проход хостинг не дёргает.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sc.createPRCalls != 1 {
		t.Fatalf("до паузы повтора нет: %d", f.sc.createPRCalls)
	}
	f.sc.createPRErr = nil
	f.e.mu.Lock()
	f.e.prWait[f.task.ID] = prWaitState{fails: 1}
	f.e.mu.Unlock()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, f)
	if task.Status != domain.TaskReview || task.PRURL == "" || f.out.lastAssign(t).Stage != pb.StageResult_REVIEW {
		t.Fatalf("после паузы PR создан, review назначен: %s %q", task.Status, task.PRURL)
	}
}

// Ревью, проход 4: задача ждёт на merge без PR, хостинг ответил — проход
// планировщика после паузы создаёт PR, и авто-merge мержит его. Первый
// проход после старта не повторяет: это делает ResumePendingPRs.
func TestRetryMergePRs(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n")
	saveProcessAutoMerge(t, f, processWithout(policy.StepTest, policy.StepReview))
	f.sc.createPRErr = errors.New("github create PR: 422: Validation failed")
	finishCoding(t, f)
	if task := mustTask(t, f); task.StepID != policy.StepMerge || task.PRURL != "" {
		t.Fatalf("задача ждёт на merge без PR: %s %q", task.StepID, task.PRURL)
	}
	f.sc.createPRErr = nil
	calls := f.sc.createPRCalls
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sc.createPRCalls != calls {
		t.Fatalf("до паузы повтора нет: %d", f.sc.createPRCalls)
	}
	f.e.mu.Lock()
	f.e.prMergeRetryAt = time.Now().Add(-time.Second)
	f.e.mu.Unlock()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, f)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskDone || len(f.sc.merged) != 1 || len(atts) != 0 {
		t.Fatalf("PR создан и смержен, эскалация закрыта: %s %v %+v", task.Status, f.sc.merged, atts)
	}
}
