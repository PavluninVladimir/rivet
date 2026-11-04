package policy

import (
	"errors"
	"testing"
)

// Роли в процессе (add-agent-roles, спеки process «Участники шага» и
// «Процесс по умолчанию»).

func TestDefaultProcessRoles(t *testing.T) {
	want := map[string]string{StepCode: RoleDeveloper, StepTest: RoleTester, StepReview: RoleReviewer}
	for _, s := range DefaultProcess().Steps {
		role := ""
		if len(s.Participants) > 0 && s.Participants[0].Agent != nil {
			role = s.Participants[0].Agent.Role
		}
		if role != want[s.Kind] {
			t.Fatalf("шаг %s: роль %q, ожидали %q", s.ID, role, want[s.Kind])
		}
	}
}

func TestTesterAgentOnlyAffectsDefaultProcess(t *testing.T) {
	testRole := func(r Resolved) string {
		for _, s := range r.Steps {
			if s.Kind == StepTest {
				return s.Participants[0].Agent.Role
			}
		}
		return "?"
	}
	p := Defaults()
	if got := testRole(p.EffectiveProcess()); got != RoleTester {
		t.Fatalf("по умолчанию тестировщик включён: %q", got)
	}
	off := false
	p.TesterAgent = &off
	if got := testRole(p.EffectiveProcess()); got != "" {
		t.Fatalf("tester_agent: false снимает роль в процессе по умолчанию: %q", got)
	}
	// Явный процесс выражает намерение сам: флаг его не трогает.
	proc := DefaultProcess()
	p.Process = &proc
	if got := testRole(p.EffectiveProcess()); got != RoleTester {
		t.Fatalf("явный процесс не меняется флагом: %q", got)
	}
	// DefaultProcess не должен мутироваться снятием роли.
	if DefaultProcess().Steps[1].Participants[0].Agent.Role != RoleTester {
		t.Fatal("DefaultProcess испорчен")
	}
}

func TestEffectiveRoleFields(t *testing.T) {
	inst := Defaults()
	inst.TestPaths = []string{"spec/**"}
	if got := inst.EffectiveTestPaths(); len(got) != 1 || got[0] != "spec/**" {
		t.Fatalf("шаблоны установки: %v", got)
	}
	off := false
	paths := []string{"e2e/**"}
	o := Overrides{TesterAgent: &off, TestPaths: &paths, RoleModels: map[string]RoleModel{"developer": {Agent: "claude-code", ConnectionID: "anthropic", Model: "claude-sonnet-5"}}}
	eff := Effective(inst, o)
	if eff.TesterEnabled() {
		t.Fatal("проект выключил тестировщика")
	}
	if got := eff.EffectiveTestPaths(); len(got) != 1 || got[0] != "e2e/**" {
		t.Fatalf("шаблоны проекта: %v", got)
	}
	if eff.RoleModels["developer"].Model != "claude-sonnet-5" {
		t.Fatalf("модель роли: %v", eff.RoleModels)
	}
	if got := Defaults().EffectiveTestPaths(); len(got) != len(DefaultTestPaths) {
		t.Fatalf("умолчания: %v", got)
	}
}

func TestRoleValidation(t *testing.T) {
	bad := DefaultProcess()
	bad.Steps[2].Participants = []Participant{{Agent: &AgentRef{Role: "Ревьюер"}}}
	var pe *ProcessError
	if err := bad.Validate(); !errors.As(err, &pe) || pe.Field != "participants[0].agent.role" || pe.Step != StepReview {
		t.Fatalf("роль с недопустимыми символами: %v", err)
	}
	o := Overrides{RoleModels: map[string]RoleModel{"developer": {ConnectionID: "x"}}}
	if err := o.Validate(); !errors.As(err, &pe) || pe.Field != "role_models.developer.model" {
		t.Fatalf("модель роли без модели: %v", err)
	}
	paths := []string{"/abs/**"}
	if err := (Overrides{TestPaths: &paths}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("абсолютный шаблон тестов: %v", err)
	}
}

func TestOldDocumentsStillParse(t *testing.T) {
	// Документ без ролей и новых полей разбирается строго, как раньше.
	raw := []byte(`
process:
  steps:
    - {id: code, kind: code, participants: [{agent: {kind: claude-code}}]}
    - {id: merge, kind: merge}
`)
	o, err := ParseOverrides(raw)
	if err != nil {
		t.Fatal(err)
	}
	if o.Process.Steps[0].Participants[0].Agent.Role != "" {
		t.Fatal("роль не задавалась")
	}
	// Хэш пресетов без новых полей не меняется: поля omitempty.
	if h1, h2 := Hash(Defaults()), Hash(Defaults().Normalize()); h1 != h2 {
		t.Fatal("хэш умолчаний нестабилен")
	}
	withRole := []byte(`
process:
  steps:
    - {id: code, kind: code, participants: [{agent: {role: developer}}]}
    - {id: merge, kind: merge}
tester_agent: false
test_paths: ["spec/**"]
role_models: {reviewer: {connection_id: anthropic, model: claude-opus-5}}
`)
	if _, err := ParseOverrides(withRole); err != nil {
		t.Fatalf("документ с ролями: %v", err)
	}
}
