package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Ревью add-agent-roles, круг 2: модель назначения не прилипает, ссылки на
// шаблоны учитывают явный процесс и эффективный профиль, отложенный запуск
// и исключения в рамках тика.

// Модель прежнего агента не переживает отвязку запуска: после перезапуска
// runner'а стадию берёт Ева со своей моделью (круг 2, P2).
func TestAssignedModelNotStickyAfterRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnAndProfiles(t, s, admin.ID)
	if _, err := s.Pool.Exec(ctx, `UPDATE agents SET models='[{"connection_id":"anthropic","model":"opus"},{"connection_id":"anthropic","model":"sonnet"}]' WHERE id='claude-code'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "maks", Name: "Макс", RoleID: "developer", Agent: "claude-code", ConnectionID: "anthropic", Model: "opus"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "eva", Name: "Ева", RoleID: "developer", Agent: "claude-code", ConnectionID: "anthropic", Model: "sonnet"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r-opus", Agent: "claude-code", Models: []string{"opus"}, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Run.AgentID != "maks" {
		t.Fatalf("Макс: %v %v %+v", ok, err, a.Run)
	}
	if err := s.SetRunModel(ctx, a.Run.ID, "opus"); err != nil {
		t.Fatal(err)
	}
	runs := codeRuns(t, s, tasks[0].ID)
	if runs[0].Model != "" || runs[0].AssignedModel != "opus" || runs[0].DisplayModel() != "opus" {
		t.Fatalf("модель назначения отдельно от явной: %+v", runs[0])
	}
	if _, err := s.ReleaseRunnerRuns(ctx, "r-opus", nil); err != nil {
		t.Fatal(err)
	}
	runs = codeRuns(t, s, tasks[0].ID)
	if runs[0].AssignedModel != "" || runs[0].AgentID != "" {
		t.Fatalf("отвязка снимает агента и модель назначения: %+v", runs[0])
	}
	off := false
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "maks", Name: "Макс", RoleID: "developer", Agent: "claude-code", ConnectionID: "anthropic", Model: "opus", Enabled: &off}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET status='offline' WHERE id='r-opus'`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r-sonnet", Agent: "claude-code", Models: []string{"sonnet"}, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	a2, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a2.Run.AgentID != "eva" || a2.Runner.ID != "r-sonnet" {
		t.Fatalf("стадию берёт Ева на r-sonnet: ok=%v err=%v agent=%q runner=%q", ok, err, a2.Run.AgentID, a2.Runner.ID)
	}
}

// Флаг тестировщика проекта не создаёт ссылку на шаблон, если у установки
// явный процесс: проект наследует его, флаг на него не влияет (круг 2, P3-2).
func TestRoleRefsIgnoreTesterFlagUnderExplicitProcess(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	pr := policy.DefaultProcess()
	pr.Steps[1].Participants = []policy.Participant{{Agent: &policy.AgentRef{}}}
	pres := policy.Defaults()
	pres.Process = &pr
	if _, err := s.SaveInstallationPolicy(ctx, pres, admin.Login); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "proj", "o/r", nil, admin.ID)
	on := true
	if _, err := s.SaveProjectPolicy(ctx, p.ID, policy.Overrides{TesterAgent: &on}, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM team_agents WHERE role_id='tester'`); err != nil {
		t.Fatal(err)
	}
	tr, _ := s.GetRole(ctx, "tester")
	off := false
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "tester", Title: tr.Title, Instruction: tr.Instruction, Color: tr.Color,
		Capability: tr.Capability, Enabled: &off}, admin.ID, admin.Login); err != nil {
		t.Fatalf("шаблон без ссылок отключается: %v", err)
	}
}

// Агент команды без своего профиля работает на профиле шаблона: отвязка
// его модели от профиля и смена профиля шаблона видят эту ссылку, а
// сохранение агента проверяет модель по профилю шаблона (круг 2, P3-3).
func TestTemplateProfileIsEffectiveForTeamAgent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnAndProfiles(t, s, admin.ID)
	if _, err := s.Pool.Exec(ctx, `UPDATE agents SET models='[{"connection_id":"anthropic","model":"opus"},{"connection_id":"anthropic","model":"sonnet"}]' WHERE id='claude-code'`); err != nil {
		t.Fatal(err)
	}
	dev, _ := s.GetRole(ctx, "developer")
	devInput := func(agent string) RoleInput {
		return RoleInput{ID: "developer", Title: dev.Title, Instruction: dev.Instruction, Color: dev.Color,
			Capability: dev.Capability, Agent: agent}
	}
	if _, err := s.UpdateRole(ctx, devInput("claude-code"), admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	// Модель, которой нет в профиле шаблона, не сохраняется.
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "maks", Name: "Макс", RoleID: "developer", ConnectionID: "openai", Model: "gpt-5"}, admin.ID, admin.Login); err == nil {
		t.Fatal("модель вне профиля шаблона отклоняется")
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "maks", Name: "Макс", RoleID: "developer", ConnectionID: "anthropic", Model: "sonnet"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	cc, _ := s.GetAgent(ctx, "claude-code")
	_, _, err := s.UpsertAgent(ctx, AgentInput{ID: "claude-code", Name: cc.Name, Adapter: cc.Adapter, Command: cc.Command,
		Capabilities: cc.Capabilities, Models: []domain.AgentModelRef{{ConnectionID: "anthropic", Model: "opus"}},
		Env: cc.Env, Args: cc.Args, Secrets: cc.Secrets}, admin.ID, admin.Login)
	var inUse *ErrInUse
	if !errors.As(err, &inUse) || !hasRef(inUse.Refs, "team_agent", "maks") {
		t.Fatalf("отвязка модели агента шаблона: %v", err)
	}
	// Смена профиля шаблона на профиль без модели Макса отклоняется.
	_, err = s.UpdateRole(ctx, devInput("codex"), admin.ID, admin.Login)
	var roleInUse *ErrRoleInUse
	if !errors.As(err, &roleInUse) || len(roleInUse.Refs) != 1 || roleInUse.Refs[0].AgentID != "maks" {
		t.Fatalf("смена профиля шаблона: %v", err)
	}
}

// Git-синхронизация политики проверяет роли процесса: отключённый шаблон
// в файле репозитория не сохраняется (круг 2, P3-4).
func TestGitPolicyValidatesRoles(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "proj", "o/r", nil, admin.ID)
	if _, err := s.Pool.Exec(ctx, `UPDATE projects SET policy_source=$2 WHERE id=$1`, p.ID, policy.SourceGit); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=false WHERE id='reviewer'`); err != nil {
		t.Fatal(err)
	}
	pr := policy.DefaultProcess()
	for i := range pr.Steps {
		if pr.Steps[i].ID == "review" {
			pr.Steps[i].Participants = []policy.Participant{{Agent: &policy.AgentRef{Role: "reviewer"}}}
		}
	}
	saved, err := s.SaveProjectPolicyFromGit(ctx, p.ID, policy.Overrides{Process: &pr}, "f1", admin.Login)
	if err == nil || saved {
		t.Fatalf("отключённый шаблон в git-политике отклоняется: %v %v", saved, err)
	}
}

// Отложенный запуск не назначается до паузы, исключённые в рамках тика
// runner и запуск пропускаются, offline runner не получает работу.
func TestDeferRunAndTickExclusions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 2)
	for _, id := range []string{"r1", "r2"} {
		if err := s.UpsertRunner(ctx, domain.Runner{ID: id, Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
			t.Fatal(err)
		}
	}
	enterCode(t, s, tasks, nil)
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	// Пауза растёт с каждым откладыванием подряд до потолка.
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		first, pause, err := s.DeferRun(ctx, a.Run.ID, a.Task.ID, time.Minute, 5*time.Minute, "шаг недоступен")
		if err != nil || first != (i == 0) || pause != want {
			t.Fatalf("откладывание %d: first=%v pause=%v err=%v, ждали %v", i, first, pause, err, want)
		}
	}
	// Доставка закрывает серию и пишет отложенное событие назначения.
	ev := EventInput{ActorKind: domain.ActorScheduler, Type: "task.assign", TaskID: a.Task.ID, Text: "назначен исполнитель: тест"}
	if err := s.RunDelivered(ctx, a.Run.ID, &PendingAssign{Event: ev}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Events(ctx, EventFilter{TaskID: a.Task.ID, Type: "task.assign"}); len(evs) != 1 {
		t.Fatalf("событие назначения после доставки: %+v", evs)
	}
	if first, pause, err := s.DeferRun(ctx, a.Run.ID, a.Task.ID, time.Hour, 2*time.Hour, "шаг недоступен"); err != nil || !first || pause != time.Hour {
		t.Fatalf("после доставки — новая серия: %v %v %v", first, pause, err)
	}
	if err := s.FreeRunnerFor(ctx, a.Runner.ID, a.Task.ID); err != nil {
		t.Fatal(err)
	}
	task, _ := s.GetTask(ctx, a.Task.ID)
	if task.WaitReason != "шаг недоступен" {
		t.Fatalf("причина ожидания: %q", task.WaitReason)
	}
	runs := codeRuns(t, s, a.Task.ID)
	if runs[0].AgentID != "" || runs[0].RunnerID != "" {
		t.Fatalf("отложенный запуск отвязан: %+v", runs[0])
	}
	// Отложенный не назначается, второй исключён в рамках тика.
	b, ok, err := s.AssignRunExcept(ctx, nil, nil, nil, []int64{otherRun(t, s, tasks, a.Task.ID)})
	if err != nil || ok {
		t.Fatalf("исключённый и отложенный не назначаются: %v %v %+v", ok, err, b.Run)
	}
	// Исключён runner r1 и r2 offline: назначать некому.
	r2, _ := s.GetRunner(ctx, "r2")
	if marked, err := s.MarkRunnerOffline(ctx, "r2", r2.LastSeen); err != nil || !marked {
		t.Fatalf("отметка offline: %v %v", marked, err)
	}
	if _, ok, err := s.AssignRunExcept(ctx, nil, nil, []string{"r1"}, nil); err != nil || ok {
		t.Fatalf("исключённый runner и offline не получают работу: %v %v", ok, err)
	}
	c, ok, err := s.AssignRunExcept(ctx, nil, nil, nil, nil)
	if err != nil || !ok || c.Runner.ID != "r1" || c.Task.ID == a.Task.ID {
		t.Fatalf("второй запуск на r1: %v %v %+v", ok, err, c)
	}
	// После паузы отложенный снова назначается.
	if _, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET not_before=now() - interval '1 second' WHERE id=$1`, a.Run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r2", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || d.Run.ID != a.Run.ID {
		t.Fatalf("после паузы: %v %v %+v", ok, err, d.Run)
	}
}

// Перезапуск runner'а не снимает сессии, чьи результаты ещё лежат в его
// журнале: они придут после регистрации (круг 2, P3-3).
func TestReleaseKeepsPendingSessions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	sid, err := s.CreateSession(ctx, domain.Session{TaskID: a.Task.ID, DriverKind: "scheduler", Scope: "CODING", Depth: domain.DepthMinimal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunSession(ctx, a.Run.ID, sid); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ReleaseRunnerRuns(ctx, "r1", []string{sid})
	if err != nil || len(runs) != 0 {
		t.Fatalf("сессия из журнала не снимается: %v %v", runs, err)
	}
	if run, _ := s.RunBySession(ctx, sid); run.ID != a.Run.ID {
		t.Fatal("запуск остался за сессией")
	}
}

// otherRun — запуск задачи, отличной от taskID.
func otherRun(t *testing.T, s *Store, tasks []domain.Task, taskID string) int64 {
	t.Helper()
	for _, tk := range tasks {
		if tk.ID == taskID {
			continue
		}
		return codeRuns(t, s, tk.ID)[0].ID
	}
	t.Fatal("нет второй задачи")
	return 0
}

// codeRuns — запуски задачи на шаге code.
func codeRuns(t *testing.T, s *Store, taskID string) []StepRun {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `SELECT `+runCols+` FROM task_step_runs r
		WHERE r.task_id=$1 AND r.step_id='code' ORDER BY r.id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := collectRuns(rows)
	if err != nil || len(runs) == 0 {
		t.Fatalf("запуски задачи: %v", err)
	}
	return runs
}

// Отметка offline после недоставки не ставится runner'у, который подал
// признак жизни после снимка назначения, и снимается heartbeat'ом и
// открытием канала (круг 3, P2-2).
func TestRunnerOfflineMarkIsConditional(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.GetRunner(ctx, "r1")
	time.Sleep(10 * time.Millisecond)
	if _, err := s.TouchRunner(ctx, "r1", nil); err != nil {
		t.Fatal(err)
	}
	if marked, err := s.MarkRunnerOffline(ctx, "r1", snap.LastSeen); err != nil || marked {
		t.Fatalf("живой runner не помечается: %v %v", marked, err)
	}
	cur, _ := s.GetRunner(ctx, "r1")
	if marked, err := s.MarkRunnerOffline(ctx, "r1", cur.LastSeen); err != nil || !marked {
		t.Fatalf("молчащий runner помечается: %v %v", marked, err)
	}
	if _, err := s.TouchRunner(ctx, "r1", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerIdle {
		t.Fatalf("heartbeat снимает отметку: %s", r.Status)
	}
	cur, _ = s.GetRunner(ctx, "r1")
	if _, err := s.MarkRunnerOffline(ctx, "r1", cur.LastSeen); err != nil {
		t.Fatal(err)
	}
	if err := s.RunnerAttached(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerIdle {
		t.Fatalf("открытый канал снимает отметку: %s", r.Status)
	}
	// Runner, за которым числится стадия, heartbeat не освобождает.
	tasks := runningEpic(t, s, 1)
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET status='offline', task_id=$1 WHERE id='r1'`, tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchRunner(ctx, "r1", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerOffline {
		t.Fatalf("runner со стадией остаётся offline: %s", r.Status)
	}
}
