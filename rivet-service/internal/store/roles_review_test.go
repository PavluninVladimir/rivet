package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Находки ревью add-agent-roles: предпочтение модели проекта, гонки
// назначения, ссылки на профиль агента Rivet и подключения, ожидание,
// версии Тимлида, перезапуск runner'а.

// setupConnections — два подключения каталога: модели агента Rivet
// вычисляются из них (add-native-agent), своих привязок у профиля нет.
func setupConnections(t *testing.T, s *Store, adminID string) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), `
		INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
		VALUES ('anthropic', 'Anthropic', 'vendor', 'anthropic', 'https://api.anthropic.com', '[{"id":"opus"},{"id":"sonnet"}]', $1),
		       ('openai', 'OpenAI', 'vendor', 'openai', 'https://api.openai.com', '[{"id":"gpt-5"}]', $1)`, adminID); err != nil {
		t.Fatal(err)
	}
}

// rivetRunner — runner агента Rivet (feature native) с поддержкой ролей.
func rivetRunner(id string, caps ...string) domain.Runner {
	return domain.Runner{ID: id, Agent: AgentRivet, Capabilities: caps, Features: []string{domain.FeatureRoles, "native"}}
}

// runningEpic — проект, Epic в работе и n задач.
func runningEpic(t *testing.T, s *Store, n int) []domain.Task {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	ep, _ := s.CreateEpic(ctx, p.ID, "e", "")
	var tasks []domain.Task
	for i := 0; i < n; i++ {
		tasks = append(tasks, mustTask(t, s, ep.ID, NewTask{Title: "t"}))
	}
	if err := s.TransitionEpic(ctx, ep.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	return tasks
}

// enterCode — вход задач на шаг code процесса по умолчанию с ролью developer.
func enterCode(t *testing.T, s *Store, tasks []domain.Task, override *policy.RoleModel) {
	t.Helper()
	ctx := context.Background()
	proc := policy.Defaults().EffectiveProcess()
	code, _ := proc.Step("code")
	dev, _ := s.GetRole(ctx, "developer")
	team, _ := s.RoleHasTeam(ctx, "developer")
	rb := ResolveRole(dev, true, override, team)
	for _, tk := range tasks {
		if _, err := s.EnterStep(ctx, EnterStep{TaskID: tk.ID, Step: code, Entry: "ok", Process: &proc, ProcessHash: "h",
			Roles: map[string]RoleBinding{"p1": rb}}); err != nil {
			t.Fatal(err)
		}
	}
}

// Переопределение роли в проекте — только предпочтение: runner без его
// модели всё равно получает стадию, предпочтение пишется в запуск.
func TestProjectProfileIsPreference(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	r := rivetRunner("r1", "coding")
	r.Models = []string{"opus"}
	if err := s.UpsertRunner(ctx, r); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, &policy.RoleModel{Agent: AgentRivet, ConnectionID: "openai", Model: "gpt-5"})
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Runner.ID != "r1" || a.Run.PrefAgent != AgentRivet || a.Run.PrefModel != "gpt-5" {
		t.Fatalf("переопределение — предпочтение: %v %v %s %+v", ok, err, a.Runner.ID, a.Run)
	}
}

// Два параллельных назначения за одного агента: агент достаётся одному,
// второе — «нечего назначать» без ошибки (P2 гонки).
func TestAssignRaceSingleAgent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Pool.Exec(ctx, `DELETE FROM team_agents WHERE id='eva'`); err != nil {
		t.Fatal(err)
	}
	tasks := runningEpic(t, s, 2)
	for _, id := range []string{"r1", "r2"} {
		if err := s.UpsertRunner(ctx, domain.Runner{ID: id, Agent: AgentRivet, Models: []string{"sonnet"}, Capabilities: []string{"coding"}}); err != nil {
			t.Fatal(err)
		}
	}
	enterCode(t, s, tasks, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	assigned, errs := 0, 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, ok, err := s.AssignRun(ctx, nil, nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
			}
			if ok && a.Run.AgentID == "maks" {
				assigned++
			}
		}()
	}
	wg.Wait()
	if errs != 0 || assigned != 1 {
		t.Fatalf("один агент — одно назначение без ошибок: assigned=%d errs=%d", assigned, errs)
	}
}

// Подходящий runner занят другим агентом роли — обычное ожидание без
// причины; у свободного агента нет ни одного подключённого runner'а —
// причина «нет runner'а для свободных агентов» (P3-7). С одним профилем
// runner агента Rivet подходит любому агенту команды: его модели — весь
// каталог.
func TestWaitReasonFreeAgentWithoutRunner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnections(t, s, admin.ID)
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "maks", Name: "Макс", RoleID: "developer", Agent: AgentRivet, ConnectionID: "openai", Model: "gpt-5"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	tasks := runningEpic(t, s, 2)
	if err := s.UpsertRunner(ctx, rivetRunner("r1", "coding")); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); !r.Catalog || !containsStr(r.Models, "gpt-5") || !containsStr(r.Models, "opus") {
		t.Fatalf("модели runner'а агента Rivet — каталог: %+v", r)
	}
	enterCode(t, s, tasks, nil)
	if a, ok, err := s.AssignRun(ctx, nil, nil); err != nil || !ok || a.Run.AgentID != "maks" || a.Runner.ID != "r1" {
		t.Fatalf("первую задачу берёт Макс: %v %v %+v", ok, err, a.Run)
	}
	if _, ok, err := s.AssignRun(ctx, nil, nil); err != nil || ok {
		t.Fatalf("вторую некому: %v %v", ok, err)
	}
	waits, err := s.WaitingTeamRuns(ctx, nil, nil, nil)
	if err != nil || len(waits) != 0 {
		t.Fatalf("подходящий runner занят — ожидание без причины: %+v %v", waits, err)
	}
	r1, _ := s.GetRunner(ctx, "r1")
	if marked, err := s.MarkRunnerOffline(ctx, "r1", r1.LastSeen); err != nil || !marked {
		t.Fatalf("отметка offline: %v %v", marked, err)
	}
	waits, err = s.WaitingTeamRuns(ctx, nil, nil, nil)
	if err != nil || len(waits) != 1 || waits[0].Busy || waits[0].Run.TaskID != tasks[1].ID {
		t.Fatalf("причина «нет runner'а для свободных агентов»: %+v %v", waits, err)
	}
	// Проект или Epic, исключённые из назначений по бюджету, причины про
	// агентов не получают.
	projectID, epicID, err := s.TaskRefs(ctx, waits[0].Run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for name, ex := range map[string][2][]string{"проект": {{projectID}, nil}, "Epic": {nil, {epicID}}} {
		if waits, err := s.WaitingTeamRuns(ctx, ex[0], ex[1], nil); err != nil || len(waits) != 0 {
			t.Fatalf("%s исключён по бюджету: %+v %v", name, waits, err)
		}
	}
}

// Ссылки агентов команды и шаблонов на подключение и профиль агента Rivet
// (P2-3, P3-6): модели профиля — каталог, поэтому сохранение профиля
// ссылкам не мешает, а выключение и удаление — мешают.
func TestTeamRefsOnConnectionsAndProfiles(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnections(t, s, admin.ID)
	if _, err := s.Pool.Exec(ctx, `UPDATE model_connections SET enabled=false WHERE id='openai'`); err != nil {
		t.Fatal(err)
	}
	choices, err := s.ModelChoices(ctx)
	if err != nil || len(choices) != 2 {
		t.Fatalf("выбор без выключенного подключения: %+v %v", choices, err)
	}
	for _, c := range choices {
		if c.ConnectionID == "openai" || c.Agent != AgentRivet {
			t.Fatalf("тройка выключенного подключения в выборе: %+v", c)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE model_connections SET enabled=true WHERE id='openai'`); err != nil {
		t.Fatal(err)
	}
	if choices, _ := s.ModelChoices(ctx); len(choices) != 3 {
		t.Fatalf("включённое подключение снова в выборе: %+v", choices)
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "irina", Name: "Ирина", RoleID: "reviewer", ConnectionID: "openai", Model: "gpt-5"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	var inUse *ErrInUse
	if err := s.DeleteConnection(ctx, "openai", admin.Login); !errors.As(err, &inUse) || !hasRef(inUse.Refs, "team_agent", "irina") {
		t.Fatalf("удаление подключения агента команды: %v", err)
	}
	off := false
	// Локальное подключение без ключа: проверка ключа вендора не мешает.
	if _, err := s.Pool.Exec(ctx, `UPDATE model_connections SET kind='local' WHERE id='openai'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertConnection(ctx, ConnectionInput{ID: "openai", Name: "OpenAI", Kind: "local", API: "openai", BaseURL: "http://localhost:1234/v1", Enabled: &off}, nil, admin.ID, admin.Login); !errors.As(err, &inUse) {
		t.Fatalf("выключение подключения агента команды: %v", err)
	}
	if _, err := s.SetConnectionModels(ctx, "openai", []domain.ModelEntry{{ID: "gpt-5", Hidden: true}}, admin.ID, admin.Login); !errors.As(err, &inUse) {
		t.Fatalf("скрытие модели агента команды: %v", err)
	}
	// Агент команды на профиле Rivet: пара проверяется по вычисляемым
	// моделям профиля.
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "irina", Name: "Ирина", RoleID: "reviewer", Agent: AgentRivet, ConnectionID: "openai", Model: "gpt-5"}, admin.ID, admin.Login); err != nil {
		t.Fatalf("агент команды на модели каталога: %v", err)
	}
	rivet, err := s.GetAgent(ctx, AgentRivet)
	if err != nil {
		t.Fatal(err)
	}
	// Сохранение включённого профиля не отвязывает модели: они — каталог.
	in := AgentInput{ID: AgentRivet, Name: rivet.Name, Adapter: AdapterNative, Secrets: rivet.Secrets}
	saved, _, err := s.UpsertAgent(ctx, in, admin.ID, admin.Login)
	if err != nil || len(saved.Models) != 3 {
		t.Fatalf("сохранение профиля со ссылками агентов команды: %+v %v", saved.Models, err)
	}
	in.Enabled = &off
	if _, _, err := s.UpsertAgent(ctx, in, admin.ID, admin.Login); !errors.As(err, &inUse) || !hasRef(inUse.Refs, "team_agent", "irina") {
		t.Fatalf("выключение профиля агента команды: %v", err)
	}
	// Профиль Rivet встроенный: не удаляется даже без ссылок.
	var fe *FieldError
	if err := s.DeleteAgent(ctx, AgentRivet, admin.Login); !errors.As(err, &fe) || fe.Field != "id" {
		t.Fatalf("удаление профиля агента Rivet: %v", err)
	}
}

// Шаблоны: агенты и процесс по умолчанию — ссылки; перевод на control plane
// при ссылках отклоняется; role_models установки и выключенных шаблонов
// отклоняются (живой API, P3-5, P3-9).
func TestTemplateRefsAndRoleModels(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnections(t, s, admin.ID)
	dev, _ := s.GetRole(ctx, "developer")
	off := false
	_, err := s.UpdateRole(ctx, RoleInput{ID: "developer", Title: dev.Title, Instruction: dev.Instruction, Color: dev.Color, Capability: dev.Capability, Enabled: &off}, admin.ID, admin.Login)
	var inUse *ErrRoleInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("выключение шаблона с агентами и процессом по умолчанию: %v", err)
	}
	kinds := map[string]int{}
	for _, r := range inUse.Refs {
		kinds[r.Kind]++
		if r.Kind == "participant" && (r.Scope != PolicyScopeInstallation || r.Step != "code") {
			t.Fatalf("ссылка процесса по умолчанию: %+v", r)
		}
	}
	if kinds["participant"] != 1 || kinds["agent"] != 2 {
		t.Fatalf("ссылки: %+v", inUse.Refs)
	}
	// Выключенный тестировщик в установке — шаблон tester не в процессе.
	pr := policy.Defaults()
	pr.TesterAgent = &off
	if _, err := s.SaveInstallationPolicy(ctx, pr, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM team_agents WHERE role_id='tester'`); err != nil {
		t.Fatal(err)
	}
	tester, _ := s.GetRole(ctx, "tester")
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "tester", Title: tester.Title, Instruction: tester.Instruction, Color: tester.Color, Capability: tester.Capability, Enabled: &off}, admin.ID, admin.Login); err != nil {
		t.Fatalf("тестировщик без ссылок выключается: %v", err)
	}
	// Перевод собственного шаблона на control plane при ссылке из процесса.
	if _, err := s.CreateRole(ctx, RoleInput{ID: "security", Title: "Sec", Instruction: "x", Color: "slate", Capability: "review"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	proc := policy.DefaultProcessFor(false)
	proc.Steps[2].Participants = append(proc.Steps[2].Participants, policy.Participant{Agent: &policy.AgentRef{Role: "security"}})
	pr.Process = &proc
	if _, err := s.SaveInstallationPolicy(ctx, pr, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "security", Title: "Sec", Instruction: "x", Color: "slate", Capability: ""}, admin.ID, admin.Login); !errors.As(err, &inUse) {
		t.Fatalf("перевод на control plane при ссылке: %v", err)
	}
	// role_models: не в установке; выключенный и control plane шаблоны — нет.
	pr.RoleModels = map[string]policy.RoleModel{"developer": {ConnectionID: "anthropic", Model: "opus"}}
	var pe *policy.ProcessError
	if _, err := s.SaveInstallationPolicy(ctx, pr, admin.Login); !errors.As(err, &pe) || pe.Field != "role_models" {
		t.Fatalf("role_models в установке: %v", err)
	}
	for role, m := range map[string]policy.RoleModel{
		"tester": {ConnectionID: "anthropic", Model: "opus"},
		"lead":   {ConnectionID: "anthropic", Model: "opus"},
	} {
		if err := s.ValidateRoleModels(ctx, map[string]policy.RoleModel{role: m}); !errors.As(err, &pe) {
			t.Fatalf("role_models %s: %v", role, err)
		}
	}
}

// Смена модели планировщика сдвигает версию шаблона и агентов-тимлидов:
// устаревшая вкладка получает stale (P2-5, P3-4).
func TestPlannerChangeMakesLeadStale(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnections(t, s, admin.ID)
	lead, _ := s.GetRole(ctx, "lead")
	timur, _ := s.GetTeamAgent(ctx, "timur")
	time.Sleep(5 * time.Millisecond)
	if err := s.SetPlannerModel(ctx, &domain.PlannerModel{ConnectionID: "anthropic", Model: "sonnet"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	old := &domain.AgentModelRef{ConnectionID: "anthropic", Model: "opus"}
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "lead", Title: lead.Title, Instruction: lead.Instruction, Color: lead.Color,
		DefaultModel: old, ExpectedUpdatedAt: &lead.UpdatedAt}, admin.ID, admin.Login); !errors.Is(err, ErrStale) {
		t.Fatalf("устаревшая форма шаблона Тимлида: %v", err)
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "timur", Name: "Тимур", RoleID: "lead", ConnectionID: "anthropic", Model: "opus",
		ExpectedUpdatedAt: &timur.UpdatedAt}, admin.ID, admin.Login); !errors.Is(err, ErrStale) {
		t.Fatalf("устаревшая форма агента-тимлида: %v", err)
	}
	if pm, _, _ := s.PlannerModel(ctx); pm.Model != "sonnet" {
		t.Fatalf("модель планировщика не откатилась: %+v", pm)
	}
}

// Перезапуск runner'а снимает его открытые запуски и освобождает агентов
// (P2-6); повтор после сбоя инфраструктуры ограничен (P3-4); переиспользование
// занятого другой задачей runner'а отклоняется (P2-3).
func TestRunnerRestartAndReuseGuards(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 2)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks[:1], nil)
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Run.AgentID == "" {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	sid, err := s.CreateSession(ctx, domain.Session{TaskID: a.Task.ID, DriverKind: "scheduler", Scope: "CODING", Depth: domain.DepthMinimal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunSession(ctx, a.Run.ID, sid); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ReleaseRunnerRuns(ctx, "r1", nil)
	if err != nil || len(runs) != 1 {
		t.Fatalf("снятые запуски: %v %v", runs, err)
	}
	if busy, _ := s.GetTeamAgent(ctx, a.Run.AgentID); busy.TaskID != "" {
		t.Fatal("агент свободен после перезапуска runner'а")
	}
	run, _ := s.RunBySession(ctx, sid)
	if run.ID != 0 {
		t.Fatal("сессия отвязана от запуска")
	}
	if r, n, err := s.RecordInfraRetry(ctx, a.Task.ID, "test", 1); err != nil || !r || n != 1 {
		t.Fatalf("первый повтор: %v %d %v", r, n, err)
	}
	if r, _, err := s.RecordInfraRetry(ctx, a.Task.ID, "test", 1); err != nil || r {
		t.Fatalf("повторы исчерпаны: %v %v", r, err)
	}
	// Runner за другой задачей не переиспользуется: запуск ждёт планировщика.
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET status='running', task_id=$2 WHERE id=$1`, "r1", tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	proc := policy.Defaults().EffectiveProcess()
	code, _ := proc.Step("code")
	got, err := s.EnterStep(ctx, EnterStep{TaskID: tasks[1].ID, Step: code, Entry: "ok", Process: &proc, ProcessHash: "h", ReuseRunner: "r1"})
	if err != nil || len(got) != 1 || got[0].RunnerID != "" {
		t.Fatalf("занятый runner не переиспользуется: %+v %v", got, err)
	}
}

func hasRef(refs []Ref, kind, id string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.ID == id {
			return true
		}
	}
	return false
}
