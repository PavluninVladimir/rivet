package orchestrator

import (
	"context"
	"encoding/json"
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

// Сценарии политик конвейера (change add-policy-presets, спеки
// task-pipeline «Merge после успешной проверки», orchestration «Лимит
// попыток», «Дневной бюджет токенов», access-policy «Защита от самоослабления»).

// diffSCM — fake SCM с настраиваемым diff PR.
type diffSCM struct {
	fakeSCM
	diff    string
	diffErr error
}

func (d *diffSCM) Diff(ctx context.Context, repo string, number int) (string, error) {
	return d.diff, d.diffErr
}

type policyFixture struct {
	st    *store.Store
	sc    *diffSCM
	out   *capture
	e     *Engine
	owner domain.User
	p     domain.Project
	epic  domain.Epic
	task  domain.Task
}

// driveToReviewPassed проводит задачу через coding → testing → review и
// возвращает результат одобренного review (OnStageResult REVIEW ok).
func driveToReviewPassed(t *testing.T, f policyFixture) error {
	t.Helper()
	ctx := context.Background()
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
	return f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_REVIEW, Ok: true})
}

func newPolicyFixture(t *testing.T, diff string) policyFixture {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	sc := &diffSCM{diff: diff}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Epic", "")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"coding", "review"}})
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	return policyFixture{st: st, sc: sc, out: out, e: e, owner: owner, p: p, epic: epic, task: task}
}

func eventsOfType(t *testing.T, st *store.Store, taskID, typ string) []domain.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{TaskID: taskID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func enableAutoMerge(t *testing.T, st *store.Store, projectID string, paths []string) store.EffectivePolicy {
	t.Helper()
	on := true
	o := policy.Overrides{AutoMerge: &on}
	if paths != nil {
		o.HumanReviewPaths = &paths
	}
	if _, err := st.SaveProjectPolicy(context.Background(), projectID, o, "owner"); err != nil {
		t.Fatal(err)
	}
	eff, err := st.EffectivePolicy(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// Авто-merge включён, PR не трогает защищённых путей: задача мержится сама,
// событие содержит версию политики.
func TestAutoMergeByPolicy(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n")
	eff := enableAutoMerge(t, f.st, f.p.ID, []string{"infra/**"})
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone {
		t.Fatalf("want done, got %s", got)
	}
	if len(f.sc.merged) != 1 {
		t.Fatalf("PR не смержен: %v", f.sc.merged)
	}
	var found bool
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
		if ev.Payload["auto_merge"] == true {
			found = true
			if ev.Payload["policy_version"] != eff.Hash || ev.ActorKind != domain.ActorSystem {
				t.Fatalf("событие merge без версии политики: %+v", ev)
			}
			if ev.Payload["project_version"] != eff.Project.ID {
				t.Fatalf("нет ссылки на версию проекта: %+v", ev.Payload)
			}
		}
	}
	if !found {
		t.Fatal("нет события авто-merge")
	}
	// Версия проекта восстановима из истории.
	hist, err := f.st.ListPolicyVersions(context.Background(), store.PolicyScopeProject, f.p.ID)
	if err != nil || len(hist) != 1 || hist[0].ID != eff.Project.ID {
		t.Fatalf("история версий: %v %+v", err, hist)
	}
	var saved policy.Overrides
	if err := json.Unmarshal(hist[0].Content, &saved); err != nil || saved.AutoMerge == nil || !*saved.AutoMerge {
		t.Fatalf("содержимое версии: %v %s", err, hist[0].Content)
	}
}

// Защищённый путь: merge откладывается, задача ждёт человека, кнопка работает.
func TestAutoMergeDeferredByProtectedPath(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/infra/main.tf b/infra/main.tf\ndiff --git a/src/x.go b/src/x.go\n")
	eff := enableAutoMerge(t, f.st, f.p.ID, []string{"infra/**"})
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview {
		t.Fatalf("want review (ожидание человека), got %s", got)
	}
	if len(f.sc.merged) != 0 {
		t.Fatal("PR не должен быть смержен")
	}
	def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred")
	if len(def) != 1 || def[0].Payload["reason"] != "human_review_path" || def[0].Payload["policy_version"] != eff.Hash {
		t.Fatalf("ожидали task.merge_deferred по защищённому пути: %+v", def)
	}
	if paths, _ := def[0].Payload["paths"].([]any); len(paths) != 1 || paths[0] != "infra/main.tf" {
		t.Fatalf("paths: %+v", def[0].Payload["paths"])
	}
	// Эскалации нет: защищённый путь — не метаправило.
	atts, _ := f.st.ListAttention(context.Background(), f.owner.ID)
	if len(atts) != 0 {
		t.Fatalf("эскалаций быть не должно: %+v", atts)
	}
	// Человек подтверждает merge кнопкой.
	if err := f.e.MergeTask(context.Background(), f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskDone || len(f.sc.merged) != 1 {
		t.Fatalf("merge кнопкой: %s %v", got, f.sc.merged)
	}
}

// Метаправило: PR меняет .rivet/ — авто-merge не выполняется даже при пустом
// списке защищённых путей, создаётся эскалация POLICY_CHANGE.
func TestAutoMergeBlockedByPolicyFile(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/.rivet/policy.yaml b/.rivet/policy.yaml\n")
	enableAutoMerge(t, f.st, f.p.ID, []string{})
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("метаправило нарушено: %s %v", got, f.sc.merged)
	}
	def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred")
	if len(def) != 1 || def[0].Payload["reason"] != "policy_file" {
		t.Fatalf("ожидали merge_deferred policy_file: %+v", def)
	}
	atts, _ := f.st.ListAttention(context.Background(), f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttPolicyChange || atts[0].TaskID != f.task.ID {
		t.Fatalf("ожидали эскалацию POLICY_CHANGE: %+v", atts)
	}
	// Человек смержил — эскалация закрывается вместе с задачей.
	if err := f.e.MergeTask(context.Background(), f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if atts, _ := f.st.ListAttention(context.Background(), f.owner.ID); len(atts) != 0 {
		t.Fatalf("эскалация должна закрыться после merge человеком: %+v", atts)
	}
}

// Обрезанный diff хостинга — пути неполные, авто-merge не выполняется.
func TestAutoMergeFailClosedOnTruncatedDiff(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	f.sc.diffErr = scm.ErrDiffTruncated
	enableAutoMerge(t, f.st, f.p.ID, nil)
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("обрезанный diff: %s %v", got, f.sc.merged)
	}
	def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred")
	if len(def) != 1 || def[0].Payload["reason"] != "files_unknown" {
		t.Fatalf("ожидали merge_deferred files_unknown: %+v", def)
	}
	// Ревьюер при этом получил начало diff'а с пометкой об обрезке.
	var reviewCtx string
	f.out.mu.Lock()
	for _, m := range f.out.sent {
		if a := m.GetAssign(); a != nil && a.Stage == pb.StageResult_REVIEW {
			reviewCtx = a.ExtraContext
		}
	}
	f.out.mu.Unlock()
	if !strings.Contains(reviewCtx, "diff обрезан") || !strings.Contains(reviewCtx, "src/x.go") {
		t.Fatalf("контекст ревьюера: %q", reviewCtx)
	}
}

// Пути получить не удалось (diff без заголовков) — fail-closed.
func TestAutoMergeFailClosedWithoutPaths(t *testing.T) {
	f := newPolicyFixture(t, "какой-то текст без diff --git заголовков")
	enableAutoMerge(t, f.st, f.p.ID, nil)
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("fail-closed нарушен: %s %v", got, f.sc.merged)
	}
	def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred")
	if len(def) != 1 || def[0].Payload["reason"] != "files_unknown" {
		t.Fatalf("ожидали merge_deferred files_unknown: %+v", def)
	}
}

// Авто-merge выключен (по умолчанию): задача ждёт подтверждения, как прежде.
func TestAutoMergeDisabledWaitsForHuman(t *testing.T) {
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview || len(f.sc.merged) != 0 {
		t.Fatalf("без авто-merge задача должна ждать: %s %v", got, f.sc.merged)
	}
	if def := eventsOfType(t, f.st, f.task.ID, "task.merge_deferred"); len(def) != 0 {
		t.Fatalf("без авто-merge событий отложенного merge нет: %+v", def)
	}
}

// Лимит отказов review меньше лимита попыток: второй отказ проваливает
// задачу с причиной review, хотя попыток осталось.
func TestReviewLimitBelowAttemptLimit(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	two, five := 2, 5
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{ReviewLimit: &two, AttemptLimit: &five}, "owner"); err != nil {
		t.Fatal(err)
	}
	// Задача создана до изменения политики — лимит 3; выставим 5 руками,
	// чтобы лимит отказов review был заведомо меньше.
	if _, err := f.st.SetTaskAttemptLimit(ctx, f.task.ID, 5); err != nil {
		t.Fatal(err)
	}
	// Первый круг: coding → testing → review → отказ.
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
	for round := 1; round <= 2; round++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		as := f.out.lastAssign(t)
		if as.Stage != pb.StageResult_REVIEW {
			t.Fatalf("круг %d: ожидали REVIEW, got %v", round, as.Stage)
		}
		if err := f.e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "замечания"}); err != nil {
			t.Fatal(err)
		}
		if round == 1 {
			task, _ := f.st.GetTask(ctx, f.task.ID)
			if task.Status != domain.TaskFixing || task.ReviewRejections != 1 {
				t.Fatalf("после первого отказа: %s rejections=%d", task.Status, task.ReviewRejections)
			}
			// Исправление и повторные проверки тем же путём.
			if err := f.e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			fixer := mustTaskRunner(t, f.st, f.task.ID)
			if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
				t.Fatal(err)
			}
			if err := f.e.OnStageResult(ctx, fixer, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || task.ReviewRejections != 2 || task.AttemptUsed != 2 {
		t.Fatalf("ожидали failed по лимиту отказов review: %s rejections=%d used=%d/%d",
			task.Status, task.ReviewRejections, task.AttemptUsed, task.AttemptLimit)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttReviewLimit || !strings.Contains(atts[0].Message, "лимит отказов review") {
		t.Fatalf("эскалация: %+v", atts)
	}
	// Решение человека обнуляет оба счётчика.
	if err := f.st.ResolveTask(ctx, f.task.ID, "", "human", false); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.AttemptUsed != 0 || task.ReviewRejections != 0 {
		t.Fatalf("счётчики не сброшены: %+v", task)
	}
}

// Лимит попыток берётся из политики проекта при создании; созданные раньше
// задачи сохраняют свой лимит.
func TestAttemptLimitFromPolicy(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if f.task.AttemptLimit != 3 {
		t.Fatalf("лимит по умолчанию 3, got %d", f.task.AttemptLimit)
	}
	five := 5
	if _, err := f.st.SaveInstallationPolicy(ctx, func() policy.Presets { p := policy.Defaults(); p.AttemptLimit = 5; return p }(), "root"); err != nil {
		t.Fatal(err)
	}
	b, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B"})
	if err != nil || b.AttemptLimit != 5 {
		t.Fatalf("лимит из политики установки: %v %d", err, b.AttemptLimit)
	}
	two := 2
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AttemptLimit: &two}, "owner"); err != nil {
		t.Fatal(err)
	}
	c, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "C"})
	if err != nil || c.AttemptLimit != 2 {
		t.Fatalf("лимит из политики проекта: %v %d", err, c.AttemptLimit)
	}
	old, _ := f.st.GetTask(ctx, f.task.ID)
	if old.AttemptLimit != 3 || func() int { t, _ := f.st.GetTask(ctx, b.ID); return t.AttemptLimit }() != five {
		t.Fatal("изменение политики не должно менять созданные задачи")
	}
	// Правка на задаче: меньше 1 и меньше израсходованных — отклоняется.
	if _, err := f.st.SetTaskAttemptLimit(ctx, f.task.ID, 0); err == nil {
		t.Fatal("лимит 0 должен отклоняться")
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE tasks SET attempt_used=2 WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetTaskAttemptLimit(ctx, f.task.ID, 1); err == nil {
		t.Fatal("лимит меньше израсходованных должен отклоняться")
	}
	if tk, err := f.st.SetTaskAttemptLimit(ctx, f.task.ID, 2); err != nil || tk.AttemptLimit != 2 {
		t.Fatalf("лимит 2: %v %+v", err, tk)
	}
}

// Бюджет проекта исчерпан: назначения не выполняются, событие один раз,
// новые сутки снимают паузу.
func TestDailyBudgetPausesAssignments(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	// Бюджет ровно равен засчитанному (150): исчерпан, назначений нет.
	budget := int64(150)
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{DailyTokenBudget: &budget}, "owner"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	f.e.Now = func() time.Time { return now }
	// Засчитанные токены за сегодня: 150 > 100. Запись без токенов бюджет
	// не расходует.
	in, out := int64(100), int64(50)
	if err := f.st.RecordUsage(ctx, store.UsageInput{SourceMsgID: "m1", ProjectID: f.p.ID, TokensIn: &in, TokensOut: &out}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordUsage(ctx, store.UsageInput{SourceMsgID: "m2", ProjectID: f.p.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE usage_records SET ts=$1`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReady {
		t.Fatalf("при исчерпанном бюджете задача не назначается: %s", got)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{ProjectID: f.p.ID, Type: "policy.budget_exceeded", Limit: 10})
	if len(evs) != 1 || evs[0].Payload["scope"] != "project" || evs[0].Payload["used"] != float64(150) {
		t.Fatalf("ожидали одно событие бюджета: %+v", evs)
	}
	st, err := f.st.ProjectBudget(ctx, f.p.ID, now)
	if err != nil || st.PausedUntil == nil || st.UsedToday != 150 || *st.DailyTokens != 150 ||
		!st.PausedUntil.Equal(time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("состояние бюджета: %v %+v", err, st)
	}
	// Новые сутки: назначение возобновляется само.
	f.e.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskRunning {
		t.Fatalf("на новые сутки задача должна назначиться: %s", got)
	}
	// Бюджет установки исключает все проекты.
	f2 := newPolicyFixture(t, "")
	f2.e.Now = func() time.Time { return now }
	small := int64(10)
	if _, err := f2.st.SaveInstallationPolicy(ctx, func() policy.Presets { p := policy.Defaults(); p.DailyTokenBudget = &small; return p }(), "root"); err != nil {
		t.Fatal(err)
	}
	if err := f2.st.RecordUsage(ctx, store.UsageInput{SourceMsgID: "m3", ProjectID: f2.p.ID, TokensIn: &in}); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.st.Pool.Exec(ctx, `UPDATE usage_records SET ts=$1`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f2.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f2.st, f2.task.ID); got != domain.TaskReady {
		t.Fatalf("бюджет установки: задача не назначается: %s", got)
	}
	evs, _ = f2.st.Events(ctx, store.EventFilter{ProjectID: f2.p.ID, Type: "policy.budget_exceeded", Limit: 10})
	if len(evs) != 1 || evs[0].Payload["scope"] != "installation" {
		t.Fatalf("событие бюджета установки: %+v", evs)
	}
}

// Автопубликация запрещена политикой: окружения с trigger=auto не ставятся,
// пишется deploy.deferred.
func TestAutoPublishDeniedByPolicy(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	env, err := f.st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: f.p.ID, Name: "staging", ExecType: "ssh", Trigger: "auto",
		Config: domain.EnvConfig{Host: "h", DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AutoPublish: &off}, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	if err := f.e.MergeTask(ctx, f.task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	deps, err := f.st.ListDeployments(ctx, env.ID, 10)
	if err != nil || len(deps) != 0 {
		t.Fatalf("публикаций быть не должно: %v %+v", err, deps)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{ProjectID: f.p.ID, Type: "deploy.deferred", Limit: 10})
	if len(evs) != 1 || fmt.Sprint(evs[0].Payload["environments"]) != "[staging]" || evs[0].Payload["policy_version"] == "" {
		t.Fatalf("ожидали deploy.deferred: %+v", evs)
	}
	// Политика разрешает — публикация ставится.
	on := true
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AutoPublish: &on}, "owner"); err != nil {
		t.Fatal(err)
	}
	f.e.enqueueAutoDeploys(ctx, f.p.ID, "sha-2")
	deps, _ = f.st.ListDeployments(ctx, env.ID, 10)
	if len(deps) != 1 {
		t.Fatalf("после разрешения публикация должна встать: %+v", deps)
	}
}

// Запрос и итог сессии (спека team-visibility «История сессий»): prompt —
// снимок задачи при запуске, outcome — результат стадии или вопрос blocked.
func TestSessionPromptAndOutcome(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/src/x.go b/src/x.go\n")
	if err := driveToReviewPassed(t, f); err != nil {
		t.Fatal(err)
	}
	sessions, err := f.st.ListTaskSessions(ctx, f.task.ID, "")
	if err != nil || len(sessions) < 3 {
		t.Fatalf("%v: %d сессий", err, len(sessions))
	}
	coding := sessions[0]
	if !strings.Contains(coding.Prompt, "A") || coding.Scope != "CODING" {
		t.Fatalf("prompt coding-сессии: %+v", coding)
	}
	// Тестовый StageResult без Detail: итог — заглушка успеха.
	if coding.Outcome != "стадия завершена успешно" {
		t.Fatalf("outcome coding-сессии: %q", coding.Outcome)
	}
	review := sessions[len(sessions)-1]
	if review.Scope != "REVIEW" || review.Outcome == "" {
		t.Fatalf("outcome review-сессии: %+v", review)
	}
	// Blocked: вопрос агента становится итогом сессии.
	f2 := newPolicyFixture(t, "")
	if err := f2.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f2.st, f2.task.ID)
	if err := f2.e.OnBlocked(ctx, worker, &pb.BlockedQuestion{TaskId: f2.task.ID,
		SessionId: f2.out.lastAssign(t).SessionId, Question: "какой формат ответа?"}); err != nil {
		t.Fatal(err)
	}
	ss, _ := f2.st.ListTaskSessions(ctx, f2.task.ID, "")
	if len(ss) != 1 || !strings.Contains(ss[0].Outcome, "какой формат ответа?") {
		t.Fatalf("outcome blocked-сессии: %+v", ss)
	}
}

// Бюджет Epic (спека orchestration «Бюджет Epic»): исчерпание останавливает
// назначения на границе стадии, событие один раз, поднятие бюджета
// возобновляет без событий и смены статуса.
func TestEpicBudgetPausesAssignments(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	budget := int64(100)
	if _, err := f.st.SetEpicBudget(ctx, f.epic.ID, &budget); err != nil {
		t.Fatal(err)
	}
	in := int64(150)
	if err := f.st.RecordUsage(ctx, store.UsageInput{SourceMsgID: "ebp-1", ProjectID: f.p.ID,
		EpicID: f.epic.ID, TaskID: f.task.ID, TokensIn: &in}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReady {
		t.Fatalf("при исчерпанном бюджете Epic задача не назначается: %s", got)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: f.epic.ID, Type: "epic.budget_exceeded", Limit: 10})
	if len(evs) != 1 || evs[0].Payload["used"] != float64(150) || evs[0].Payload["budget"] != float64(100) {
		t.Fatalf("ожидали одно событие бюджета Epic: %+v", evs)
	}
	// Человек поднял бюджет — назначение возобновляется, событий больше нет.
	bigger := int64(10000)
	if _, err := f.st.SetEpicBudget(ctx, f.epic.ID, &bigger); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskRunning {
		t.Fatalf("после поднятия бюджета задача должна назначиться: %s", got)
	}
	if evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: f.epic.ID, Type: "epic.budget_exceeded", Limit: 10}); len(evs) != 1 {
		t.Fatalf("новых событий быть не должно: %+v", evs)
	}
	ep, _ := f.st.GetEpic(ctx, f.epic.ID)
	if ep.Status != domain.EpicRunning {
		t.Fatalf("статус Epic не должен меняться: %s", ep.Status)
	}
}
