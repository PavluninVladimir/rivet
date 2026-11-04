package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Каталог ролей (add-agent-roles, спека agent-roles «Каталог ролей»).
func TestRolesCatalog(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)

	roles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range roles {
		ids = append(ids, r.ID)
		if !r.Builtin || !r.Enabled || r.Instruction == "" || r.Title == "" {
			t.Fatalf("встроенная роль %s: %+v", r.ID, r)
		}
	}
	if strings.Join(ids, ",") != "analyst,architect,lead,developer,tester,reviewer" {
		t.Fatalf("порядок встроенных ролей: %v", ids)
	}

	// Правка инструкции встроенной роли — событие с администратором.
	rev, _ := s.GetRole(ctx, "reviewer")
	upd, created, err := s.UpsertRole(ctx, RoleInput{ID: "reviewer", Title: rev.Title, Description: rev.Description,
		Instruction: rev.Instruction + " Проверяй обратимость миграций.", Color: rev.Color, Capability: rev.Capability}, admin.ID, admin.Login)
	if err != nil || created || !strings.Contains(upd.Instruction, "миграций") || upd.UpdatedBy != admin.Login {
		t.Fatalf("правка роли: %+v %v %v", upd, created, err)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: "role.updated"})
	if len(evs) != 1 || evs[0].ActorID != admin.Login {
		t.Fatalf("событие правки роли: %+v", evs)
	}

	// Ошибки формы с полем.
	cases := []struct {
		in    RoleInput
		field string
	}{
		{RoleInput{ID: "Bad", Title: "x", Instruction: "x", Color: "blue", Capability: "coding"}, "id"},
		{RoleInput{ID: "qa", Title: "", Instruction: "x", Color: "blue", Capability: "coding"}, "title"},
		{RoleInput{ID: "qa", Title: "QA", Instruction: " ", Color: "blue", Capability: "coding"}, "instruction"},
		{RoleInput{ID: "qa", Title: "QA", Instruction: "x", Color: "pink", Capability: "coding"}, "color"},
		{RoleInput{ID: "qa", Title: "QA", Instruction: "x", Color: "blue", Capability: "deploy"}, "capability"},
		{RoleInput{ID: "tester", Title: "QA", Instruction: "x", Color: "blue"}, "capability"},
		{RoleInput{ID: "lead", Title: "Тимлид", Instruction: "x", Color: "amber", Capability: "coding"}, "capability"},
		{RoleInput{ID: "analyst", Title: "Аналитик", Instruction: "x", Color: "indigo", Capability: "review"}, "capability"},
		{RoleInput{ID: "qa", Title: "QA", Instruction: "x", Color: "blue", Agent: "claude-code"}, "agent"},
		{RoleInput{ID: "qa", Title: "QA", Instruction: "x", Color: "blue", Capability: "coding", Agent: "nope"}, "agent"},
	}
	for _, c := range cases {
		var fe *FieldError
		if _, _, err := s.UpsertRole(ctx, c.in, admin.ID, admin.Login); !errors.As(err, &fe) || fe.Field != c.field {
			t.Fatalf("%+v: ожидали ошибку поля %s, got %v", c.in, c.field, err)
		}
	}

	// Собственная роль, процесс проекта со ссылкой на неё.
	sec, created, err := s.UpsertRole(ctx, RoleInput{ID: "security", Title: "Безопасник", Instruction: "Ищи уязвимости.",
		Color: "slate", Capability: "review"}, admin.ID, admin.Login)
	if err != nil || !created || sec.Builtin || !sec.Enabled {
		t.Fatalf("собственная роль: %+v %v", sec, err)
	}
	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	proc := policy.DefaultProcess()
	proc.Steps[2].Participants = []policy.Participant{{Agent: &policy.AgentRef{Role: "security"}}}
	if _, err := s.SaveProjectPolicy(ctx, p.ID, policy.Overrides{Process: &proc}, admin.Login); err != nil {
		t.Fatalf("процесс с собственной ролью: %v", err)
	}
	var inUse *ErrRoleInUse
	if err := s.DeleteRole(ctx, "security", admin.Login); !errors.As(err, &inUse) || len(inUse.Refs) != 1 {
		t.Fatalf("удаление роли со ссылкой: %v", err)
	}
	if r := inUse.Refs[0]; r.Scope != PolicyScopeProject || r.ProjectID != p.ID || r.ProjectName != "demo" || r.Step != "review" || r.Kind != "participant" {
		t.Fatalf("ссылка на шаг процесса: %+v", r)
	}
	off := false
	if _, _, err := s.UpsertRole(ctx, RoleInput{ID: "security", Title: "Безопасник", Instruction: "x", Color: "slate", Capability: "review", Enabled: &off}, admin.ID, admin.Login); !errors.As(err, &inUse) {
		t.Fatalf("выключение роли со ссылкой: %v", err)
	}
	if err := s.DeleteRole(ctx, "tester", admin.Login); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("удаление встроенной роли: %v", err)
	}

	// Проверка процесса по каталогу: неизвестная, выключенная, роль control plane.
	var pe *policy.ProcessError
	for role, want := range map[string]string{"ghost": "нет в каталоге", "lead": "control plane", "analyst": "control plane"} {
		bad := policy.DefaultProcess()
		bad.Steps[0].Participants = []policy.Participant{{Agent: &policy.AgentRef{Role: role}}}
		if err := s.ValidateProcessRoles(ctx, &bad); !errors.As(err, &pe) || pe.Field != "participants[0].agent.role" || !strings.Contains(pe.Msg, want) {
			t.Fatalf("роль %s: %v", role, err)
		}
	}
	if _, _, err := s.UpsertRole(ctx, RoleInput{ID: "qa", Title: "QA", Instruction: "x", Color: "teal", Capability: "coding", Enabled: &off}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	bad := policy.DefaultProcess()
	bad.Steps[1].Participants = []policy.Participant{{Agent: &policy.AgentRef{Role: "qa"}}}
	if err := s.ValidateProcessRoles(ctx, &bad); !errors.As(err, &pe) || !strings.Contains(pe.Msg, "отключена") {
		t.Fatalf("выключенная роль: %v", err)
	}

	// Убрали ссылку — роль удаляется.
	plain := policy.DefaultProcess()
	if _, err := s.SaveProjectPolicy(ctx, p.ID, policy.Overrides{Process: &plain}, admin.Login); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRole(ctx, "security", admin.Login); err != nil {
		t.Fatalf("удаление без ссылок: %v", err)
	}
	if _, err := s.GetRole(ctx, "security"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("роль удалена: %v", err)
	}
}

// Модель Тимлида — настройка планировщика (спека epic-decomposition
// «Настройка модели для декомпозиции»).
func TestLeadRoleModelIsPlannerModel(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
		VALUES ('anthropic', 'Anthropic', 'vendor', 'anthropic', 'https://api.anthropic.com', '[{"id":"claude-opus-5"}]', $1)`, admin.ID); err != nil {
		t.Fatal(err)
	}
	lead, _ := s.GetRole(ctx, "lead")
	if lead.DefaultModel != nil {
		t.Fatalf("модель не выбрана: %+v", lead.DefaultModel)
	}
	m := &domain.AgentModelRef{ConnectionID: "anthropic", Model: "claude-opus-5"}
	if _, _, err := s.UpsertRole(ctx, RoleInput{ID: "lead", Title: lead.Title, Instruction: lead.Instruction, Color: lead.Color, DefaultModel: m}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	pm, ok, err := s.PlannerModel(ctx)
	if err != nil || !ok || pm.Model != "claude-opus-5" {
		t.Fatalf("модель планировщика из роли: %+v %v %v", pm, ok, err)
	}
	lead, _ = s.GetRole(ctx, "lead")
	if lead.DefaultModel == nil || lead.DefaultModel.Model != "claude-opus-5" {
		t.Fatalf("роль показывает модель планировщика: %+v", lead.DefaultModel)
	}
	// Модель вне подключения отклоняется до записи роли.
	var fe *FieldError
	bad := &domain.AgentModelRef{ConnectionID: "anthropic", Model: "nope"}
	if _, _, err := s.UpsertRole(ctx, RoleInput{ID: "lead", Title: "Другое", Instruction: lead.Instruction, Color: lead.Color, DefaultModel: bad}, admin.ID, admin.Login); !errors.As(err, &fe) {
		t.Fatalf("неизвестная модель: %v", err)
	}
	if got, _ := s.GetRole(ctx, "lead"); got.Title != lead.Title {
		t.Fatalf("роль не должна меняться при отказе: %q", got.Title)
	}
	// Сохранение шаблона без модели не трогает модель планировщика: её могли
	// выбрать в другом месте (сброс — только настройкой планировщика).
	if _, _, err := s.UpsertRole(ctx, RoleInput{ID: "lead", Title: lead.Title, Instruction: lead.Instruction, Color: lead.Color}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if pm, ok, _ := s.PlannerModel(ctx); !ok || pm.Model != "claude-opus-5" {
		t.Fatalf("модель планировщика сохраняется: %+v %v", pm, ok)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: "team.agent_updated"})
	if len(evs) == 0 || evs[len(evs)-1].Payload["id"] != "timur" {
		t.Fatalf("смена модели планировщика обновляет агента-тимлида: %+v", evs)
	}
}

// Имя runner'а (спека agent-roles «Имя агента»).
func TestRunnerDisplayName(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	reg := func(name string) {
		t.Helper()
		if err := s.UpsertRunner(ctx, domain.Runner{ID: "claude-sonnet-1", Agent: "claude-code", Capabilities: []string{"coding"},
			DisplayName: name, Features: []string{domain.FeatureRoles}}); err != nil {
			t.Fatal(err)
		}
	}
	reg("Макс")
	r, _ := s.GetRunner(ctx, "claude-sonnet-1")
	if r.DisplayName != "Макс" || r.DisplayNameLocked || !r.HasFeature(domain.FeatureRoles) || r.Label() != "Макс" {
		t.Fatalf("имя из регистрации: %+v", r)
	}
	if r, err := s.RenameRunner(ctx, "claude-sonnet-1", "Ева", "admin"); err != nil || r.DisplayName != "Ева" || !r.DisplayNameLocked {
		t.Fatalf("имя из консоли: %+v %v", r, err)
	}
	reg("Макс")
	if r, _ := s.GetRunner(ctx, "claude-sonnet-1"); r.DisplayName != "Ева" {
		t.Fatalf("регистрация не перезаписывает имя из консоли: %q", r.DisplayName)
	}
	if r, err := s.RenameRunner(ctx, "claude-sonnet-1", "", "admin"); err != nil || r.DisplayName != "Макс" || r.DisplayNameLocked {
		t.Fatalf("снятие имени из консоли: %+v %v", r, err)
	}
	var fe *FieldError
	if _, err := s.RenameRunner(ctx, "claude-sonnet-1", strings.Repeat("я", 41), "admin"); !errors.As(err, &fe) || fe.Field != "display_name" {
		t.Fatalf("длинное имя: %v", err)
	}
	if _, err := s.RenameRunner(ctx, "ghost", "Ева", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("неизвестный runner: %v", err)
	}
	// Старый runner без имени показывает идентификатор.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "old", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "old"); r.Label() != "old" || r.HasFeature(domain.FeatureRoles) {
		t.Fatalf("runner без имени: %+v", r)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: "runner.renamed"})
	if len(evs) != 2 {
		t.Fatalf("события переименования: %d", len(evs))
	}
}

// Роль участника на входе в шаг и назначение с её учётом (спеки
// agent-roles «Модель роли» и process «Шаг test с тестировщиком»).
func TestRoleBindingAndAssignment(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	// Путь без команды: роль по шаблону через capability.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM team_agents`); err != nil {
		t.Fatal(err)
	}
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	ep, _ := s.CreateEpic(ctx, p.ID, "e", "")
	task := mustTask(t, s, ep.ID, NewTask{Title: "t"})
	if err := s.TransitionEpic(ctx, ep.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	for _, r := range []domain.Runner{
		{ID: "a-plain", Agent: "claude-code", Models: []string{"sonnet"}, Capabilities: []string{"coding"}, DisplayName: "Макс", Features: []string{domain.FeatureRoles}},
		{ID: "b-opus", Agent: "claude-code", Models: []string{"sonnet", "opus"}, Capabilities: []string{"coding"}, DisplayName: "Ева"},
	} {
		if err := s.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	proc := policy.Defaults().EffectiveProcess()
	code, _ := proc.Step("code")
	dev, _ := s.GetRole(ctx, "developer")
	// Модель роли opus — предпочтение: из двух свободных берётся runner с ней.
	rb := ResolveRole(dev, true, &policy.RoleModel{ConnectionID: "anthropic", Model: "opus"}, false)
	runs, err := s.EnterStep(ctx, EnterStep{TaskID: task.ID, Step: code, Entry: "ok", Process: &proc, ProcessHash: "h",
		Roles: map[string]RoleBinding{"p1": rb}})
	if err != nil || len(runs) != 1 {
		t.Fatalf("вход на шаг: %v", err)
	}
	if runs[0].Role != "developer" || runs[0].PrefModel != "opus" || !containsStr(runs[0].Capabilities, "coding") {
		t.Fatalf("запуск с ролью: %+v", runs[0])
	}
	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Runner.ID != "b-opus" {
		t.Fatalf("предпочтение модели роли: %+v %v %v", a.Runner.ID, ok, err)
	}
	got, _ := s.GetTask(ctx, task.ID)
	if got.RoleID != "developer" || got.RunnerName != "Ева" {
		t.Fatalf("текущий исполнитель задачи: %q %q", got.RoleID, got.RunnerName)
	}
	if r, _ := s.GetRunner(ctx, "b-opus"); r.RoleID != "developer" {
		t.Fatalf("роль runner'а: %q", r.RoleID)
	}
	// Событие назначения пишется после доставки стадии.
	if err := s.RunDelivered(ctx, a.Run.ID, a.Pending); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events(ctx, EventFilter{TaskID: task.ID, Type: "task.status"})
	last := evs[len(evs)-1]
	role, _ := last.Payload["role"].(map[string]any)
	if role["id"] != "developer" || last.Payload["runner_name"] != "Ева" || last.Payload["role_delivered"] != false ||
		!strings.Contains(last.Text, "Разработчик (runner b-opus)") {
		t.Fatalf("событие назначения: %q %+v", last.Text, last.Payload)
	}

	// Тестировщик предпочитает runner, который не писал код: b-opus — исполнитель.
	if _, err := s.RecordVerdict(ctx, a.Run.ID, policy.OutcomeOk, "", false); err != nil {
		t.Fatal(err)
	}
	testStep, _ := proc.Step("test")
	tester, _ := s.GetRole(ctx, "tester")
	if _, err := s.EnterStep(ctx, EnterStep{TaskID: task.ID, Step: testStep, Entry: "ok",
		Roles: map[string]RoleBinding{"p1": ResolveRole(tester, true, nil, false)}}); err != nil {
		t.Fatal(err)
	}
	a2, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a2.Runner.ID != "a-plain" || a2.Run.Role != "tester" {
		t.Fatalf("тестировщик на другом runner'е: %+v %v %v", a2.Runner.ID, ok, err)
	}
	if got, _ := s.GetTask(ctx, task.ID); got.RunnerID != "b-opus" || got.RoleID != "tester" || got.RunnerName != "Макс" {
		t.Fatalf("исполнитель кода сохраняется, текущий — тестировщик: %+v", got)
	}

	// Отключённая роль — участник без роли.
	if b := ResolveRole(tester, false, nil, true); b.Role != "" {
		t.Fatalf("неизвестная роль: %+v", b)
	}
	tester.Enabled = false
	if b := ResolveRole(tester, true, nil, true); b.Role != "" {
		t.Fatalf("выключенная роль: %+v", b)
	}
}

// Создание и изменение раздельно, оптимистическая проверка версии.
func TestRoleCreateUpdateStale(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	in := RoleInput{ID: "tester", Title: "QA", Instruction: "x", Color: "teal", Capability: "coding"}
	if _, err := s.CreateRole(ctx, in, admin.ID, admin.Login); !errors.Is(err, ErrExists) {
		t.Fatalf("создание с занятым id: %v", err)
	}
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "ghost", Title: "x", Instruction: "x", Color: "teal", Capability: "coding"}, admin.ID, admin.Login); !errors.Is(err, ErrNotFound) {
		t.Fatalf("изменение несуществующей: %v", err)
	}
	cur, _ := s.GetRole(ctx, "tester")
	old := cur.UpdatedAt.Add(-time.Second)
	in = RoleInput{ID: "tester", Title: cur.Title, Instruction: cur.Instruction, Color: cur.Color, Capability: cur.Capability, ExpectedUpdatedAt: &old}
	if _, err := s.UpdateRole(ctx, in, admin.ID, admin.Login); !errors.Is(err, ErrStale) {
		t.Fatalf("устаревшая версия: %v", err)
	}
	in.ExpectedUpdatedAt = &cur.UpdatedAt
	if _, err := s.UpdateRole(ctx, in, admin.ID, admin.Login); err != nil {
		t.Fatalf("актуальная версия: %v", err)
	}
}

// Команда агентов (спека agent-roles «Команда агентов»).
func TestTeamAgents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	team, err := s.ListTeam(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range team {
		names = append(names, a.RoleID+":"+a.Name)
	}
	if strings.Join(names, ",") != "analyst:Вера,architect:Ада,lead:Тимур,developer:Макс,developer:Ева,tester:Лев,reviewer:Ирина" {
		t.Fatalf("команда по умолчанию: %v", names)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
		VALUES ('anthropic', 'Anthropic', 'vendor', 'anthropic', 'https://api.anthropic.com', '[{"id":"claude-opus-5"},{"id":"claude-sonnet-5"}]', $1)`, admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE agents SET models='[{"connection_id":"anthropic","model":"claude-opus-5"}]' WHERE id='claude-code'`); err != nil {
		t.Fatal(err)
	}
	// Третий разработчик из шаблона с моделью-тройкой.
	yan, err := s.CreateTeamAgent(ctx, TeamAgentInput{ID: "yan", Name: "Ян", RoleID: "developer",
		Agent: "claude-code", ConnectionID: "anthropic", Model: "claude-opus-5"}, admin.ID, admin.Login)
	if err != nil || yan.Model != "claude-opus-5" || !yan.Enabled || yan.Position <= 70 {
		t.Fatalf("новый агент: %+v %v", yan, err)
	}
	if _, err := s.CreateTeamAgent(ctx, TeamAgentInput{ID: "yan", Name: "Ян", RoleID: "developer"}, admin.ID, admin.Login); !errors.Is(err, ErrExists) {
		t.Fatalf("занятый id: %v", err)
	}
	var fe *FieldError
	for _, bad := range []TeamAgentInput{
		{ID: "x1", Name: "", RoleID: "developer"},
		{ID: "x1", Name: "X", RoleID: "ghost"},
		{ID: "x1", Name: "X", RoleID: "developer", Agent: "claude-code", ConnectionID: "anthropic", Model: "claude-sonnet-5"},
		{ID: "x1", Name: "X", RoleID: "developer", Agent: "nope", ConnectionID: "anthropic", Model: "claude-opus-5"},
		{ID: "x1", Name: "X", RoleID: "developer", ConnectionID: "anthropic"},
		{ID: "x1", Name: "X", RoleID: "analyst", Agent: "claude-code", ConnectionID: "anthropic", Model: "claude-opus-5"},
	} {
		if _, err := s.CreateTeamAgent(ctx, bad, admin.ID, admin.Login); !errors.As(err, &fe) {
			t.Fatalf("%+v: ожидали ошибку поля, got %v", bad, err)
		}
	}
	// Смена модели — событие с прежним и новым значением; устаревшая версия — ErrStale.
	stale := yan.UpdatedAt.Add(-time.Second)
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "yan", Name: "Ян", RoleID: "developer", ExpectedUpdatedAt: &stale}, admin.ID, admin.Login); !errors.Is(err, ErrStale) {
		t.Fatalf("устаревшая версия агента: %v", err)
	}
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "yan", Name: "Ян", RoleID: "developer", InstructionExtra: "Пиши короче.", ExpectedUpdatedAt: &yan.UpdatedAt}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: "team.agent_updated"})
	last := evs[len(evs)-1]
	before, _ := last.Payload["before"].(map[string]any)
	if before["model"] != "claude-code / anthropic / claude-opus-5" || last.Payload["model"] != "" {
		t.Fatalf("событие смены модели: %+v", last.Payload)
	}
	// Агент-тимлид: модель — настройка планировщика, только явная смена.
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "timur", Name: "Тимур", RoleID: "lead", ConnectionID: "anthropic", Model: "claude-sonnet-5"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if pm, ok, _ := s.PlannerModel(ctx); !ok || pm.Model != "claude-sonnet-5" {
		t.Fatalf("модель планировщика от Тимлида: %+v", pm)
	}
	off := false
	if _, err := s.UpdateTeamAgent(ctx, TeamAgentInput{ID: "timur", Name: "Тимур", RoleID: "lead", Enabled: &off}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if pm, ok, _ := s.PlannerModel(ctx); !ok || pm.Model != "claude-sonnet-5" {
		t.Fatalf("отключение Тимлида не сбрасывает модель планировщика: %+v %v", pm, ok)
	}
	lead, ok, err := s.FirstTeamAgent(ctx, "lead")
	if err != nil || ok {
		t.Fatalf("включённого Тимлида нет: %+v %v", lead, err)
	}
	// Шаблон с агентами не удаляется.
	if _, err := s.CreateRole(ctx, RoleInput{ID: "security", Title: "Безопасник", Instruction: "x", Color: "slate", Capability: "review"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeamAgent(ctx, TeamAgentInput{ID: "sec", Name: "Сева", RoleID: "security"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	var inUse *ErrRoleInUse
	if err := s.DeleteRole(ctx, "security", admin.Login); !errors.As(err, &inUse) || inUse.Refs[0].Kind != "agent" || inUse.Refs[0].AgentName != "Сева" {
		t.Fatalf("шаблон с агентом: %v", err)
	}
	if err := s.DeleteTeamAgent(ctx, "sec", admin.Login); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRole(ctx, "security", admin.Login); err != nil {
		t.Fatalf("шаблон без агентов: %v", err)
	}
	// Выбор модели: тройки включённых профилей с признаком runner'а.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	choices, err := s.ModelChoices(ctx)
	if err != nil || len(choices) == 0 {
		t.Fatalf("выбор моделей: %v %v", choices, err)
	}
	for _, c := range choices {
		if c.Agent == "claude-code" && (!c.RunnerOnline || c.Model != "claude-opus-5") {
			t.Fatalf("тройка claude-code: %+v", c)
		}
	}
}
