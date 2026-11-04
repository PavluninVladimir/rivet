package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Ревью add-agent-roles, круг 3: перенос модели назначения у открытых
// запусков в миграции и профиль шаблона у переопределения роли.

// migrationStatement — оператор миграции, начинающийся с prefix.
func migrationStatement(t *testing.T, name, prefix string) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	i := strings.Index(sql, prefix)
	if i < 0 {
		t.Fatalf("в %s нет оператора %q", name, prefix)
	}
	j := strings.Index(sql[i:], ";\n")
	if j < 0 {
		t.Fatalf("оператор %q не закончен", prefix)
	}
	return sql[i : i+j]
}

// Модель, которую прежний код писал в model открытого запуска, переезжает
// в assigned_model, если участник снимка модель не задаёт; явная модель
// участника остаётся требованием (круг 3, P3-3).
func TestMigrationMovesAssignedModel(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 2)
	enterCode(t, s, tasks, nil)
	// Прежнее состояние: модель назначения в model у обоих запусков, у
	// второй задачи участник шага code задаёт модель явно.
	if _, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET model='opus', assigned_model='' WHERE step_id='code'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE tasks SET process = jsonb_set(process, '{steps,0,participants,0,agent,model}', '"opus"')
		WHERE id=$1`, tasks[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, migrationStatement(t, "0028_agent_roles.sql", "UPDATE task_step_runs r SET assigned_model")); err != nil {
		t.Fatal(err)
	}
	moved := codeRuns(t, s, tasks[0].ID)[0]
	if moved.Model != "" || moved.AssignedModel != "opus" {
		t.Fatalf("модель назначения переехала: model=%q assigned=%q", moved.Model, moved.AssignedModel)
	}
	kept := codeRuns(t, s, tasks[1].ID)[0]
	if kept.Model != "opus" || kept.AssignedModel != "" {
		t.Fatalf("явная модель участника осталась: model=%q assigned=%q", kept.Model, kept.AssignedModel)
	}
}

// Переопределение роли без профиля работает на профиле шаблона: отвязка
// его модели от профиля шаблона видит ссылку (круг 3, P3-4).
func TestRoleModelUsesTemplateProfile(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	setupConnAndProfiles(t, s, admin.ID)
	if _, err := s.Pool.Exec(ctx, `UPDATE agents SET models='[{"connection_id":"anthropic","model":"opus"},{"connection_id":"anthropic","model":"sonnet"}]' WHERE id='claude-code'`); err != nil {
		t.Fatal(err)
	}
	dev, _ := s.GetRole(ctx, "developer")
	if _, err := s.UpdateRole(ctx, RoleInput{ID: "developer", Title: dev.Title, Instruction: dev.Instruction, Color: dev.Color,
		Capability: dev.Capability, Agent: "claude-code"}, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "proj", "o/r", nil, admin.ID)
	if _, err := s.SaveProjectPolicy(ctx, p.ID, policy.Overrides{RoleModels: map[string]policy.RoleModel{
		"developer": {ConnectionID: "anthropic", Model: "sonnet"}}}, admin.Login); err != nil {
		t.Fatal(err)
	}
	cc, _ := s.GetAgent(ctx, "claude-code")
	_, _, err := s.UpsertAgent(ctx, AgentInput{ID: "claude-code", Name: cc.Name, Adapter: cc.Adapter, Command: cc.Command,
		Capabilities: cc.Capabilities, Models: []domain.AgentModelRef{{ConnectionID: "anthropic", Model: "opus"}},
		Env: cc.Env, Args: cc.Args, Secrets: cc.Secrets}, admin.ID, admin.Login)
	var inUse *ErrInUse
	if !errors.As(err, &inUse) || !hasRef(inUse.Refs, "role_model", p.ID+":developer") {
		t.Fatalf("переопределение роли на профиле шаблона: %v", err)
	}
}

// Причина offline: heartbeat снимает только отметку после недоставки, а
// отметку после таймаута heartbeat'ов — только новый канал (круг 4, P2-2).
func TestOfflineReason(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRunner(ctx, "r1")
	if marked, err := s.MarkRunnerOffline(ctx, "r1", r.LastSeen); err != nil || !marked {
		t.Fatalf("недоставка: %v %v", marked, err)
	}
	if stale, err := s.TouchRunner(ctx, "r1", nil); err != nil || stale {
		t.Fatalf("heartbeat после недоставки: stale=%v err=%v", stale, err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerIdle {
		t.Fatalf("heartbeat снимает отметку недоставки: %s", r.Status)
	}
	if _, _, err := s.MarkStaleRunnersOffline(ctx, -60); err != nil {
		t.Fatal(err)
	}
	if stale, err := s.TouchRunner(ctx, "r1", nil); err != nil || !stale {
		t.Fatalf("heartbeat потерянного по таймауту: stale=%v err=%v", stale, err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerOffline {
		t.Fatalf("heartbeat не снимает отметку таймаута: %s", r.Status)
	}
	if err := s.RunnerAttached(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRunner(ctx, "r1"); r.Status != domain.RunnerIdle {
		t.Fatalf("новый канал снимает отметку таймаута: %s", r.Status)
	}
	if stale, err := s.TouchRunner(ctx, "r1", nil); err != nil || stale {
		t.Fatalf("после нового канала heartbeat обычный: %v %v", stale, err)
	}
}
