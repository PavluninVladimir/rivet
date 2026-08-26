package policy

import (
	"errors"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

// Процесс по умолчанию валиден и раскрывается в текущий конвейер (спека
// process «Процесс по умолчанию»).
func TestDefaultProcessResolves(t *testing.T) {
	p := DefaultProcess()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	ids := []string{}
	for _, s := range r.Steps {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "code,test,review,merge,deploy" {
		t.Fatalf("шаги: %v", ids)
	}
	code, test, review, merge, deploy := r.Steps[0], r.Steps[1], r.Steps[2], r.Steps[3], r.Steps[4]
	if code.On != (Transitions{Ok: "test", Changes: "code", Fail: TargetEscalate}) {
		t.Fatalf("переходы code: %+v", code.On)
	}
	if test.On != (Transitions{Ok: "review", Changes: "code", Fail: TargetEscalate}) {
		t.Fatalf("переходы test: %+v", test.On)
	}
	if review.On.Ok != "merge" || review.On.Changes != "code" || review.Attempts != 3 || review.Capabilities[0] != "review" {
		t.Fatalf("review: %+v", review)
	}
	if merge.On.Ok != "deploy" || deploy.On.Ok != TargetDone {
		t.Fatalf("merge/deploy: %+v %+v", merge.On, deploy.On)
	}
	if code.Mode != ModeParallel || code.Require != RequireAll || code.Attempts != 3 || len(code.Participants) != 1 || code.Participants[0].ID != "p1" {
		t.Fatalf("code: %+v", code)
	}
	// Лимиты из пресетов.
	r = Resolve(p, Presets{AttemptLimit: 5, ReviewLimit: 2})
	if r.Steps[1].Attempts != 5 || r.Steps[2].Attempts != 2 {
		t.Fatalf("лимиты из пресетов: %d %d", r.Steps[1].Attempts, r.Steps[2].Attempts)
	}
}

// Отключённый шаг пропускается, переходы перескакивают через него.
func TestResolveSkipsDisabled(t *testing.T) {
	p := DefaultProcess()
	p.Steps[1].Enabled = boolp(false)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	if len(r.Steps) != 4 || r.Steps[0].On.Ok != "review" || r.Skipped[0] != "test" {
		t.Fatalf("пропуск: %+v skipped=%v", r.Steps[0].On, r.Skipped)
	}
}

// Два шага review с разными агентами и явными переходами.
func TestTwoReviewSteps(t *testing.T) {
	p := DefaultProcess()
	second := Step{ID: "review-2", Kind: StepReview, Attempts: intp(2),
		Participants: []Participant{{Agent: &AgentRef{Kind: "codex", Model: "gpt-5"}}, {Agent: &AgentRef{Kind: "claude-code"}}},
		Mode:         ModeSequential, Require: RequireAll}
	p.Steps = append(p.Steps[:3], append([]Step{second}, p.Steps[3:]...)...)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	s, ok := r.Step("review-2")
	if !ok || s.On.Ok != "merge" || s.On.Changes != "code" || s.Attempts != 2 || s.Participants[0].Agent.Model != "gpt-5" || s.Participants[1].ID != "p2" {
		t.Fatalf("review-2: %+v", s)
	}
	if r.Steps[2].On.Ok != "review-2" {
		t.Fatalf("review → review-2: %+v", r.Steps[2].On)
	}
}

func TestProcessValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Process)
		want string
	}{
		{"нет шагов", func(p *Process) { p.Steps = nil }, "ни одного шага"},
		{"плохой id", func(p *Process) { p.Steps[0].ID = "Code Step" }, "идентификатор шага"},
		{"дубликат id", func(p *Process) { p.Steps[1].ID = "code" }, "повторяется"},
		{"неизвестный тип", func(p *Process) { p.Steps[0].Kind = "lint" }, "неизвестный тип"},
		{"без участников", func(p *Process) { p.Steps[2].Participants = nil }, "хотя бы один участник"},
		{"человек без логина и роли", func(p *Process) { p.Steps[2].Participants = []Participant{{User: &UserRef{}}} }, "без логина и роли"},
		{"человек с логином и ролью", func(p *Process) { p.Steps[2].Participants = []Participant{{User: &UserRef{Login: "v", Role: "owner"}}} }, "либо логином, либо ролью"},
		{"неизвестная роль", func(p *Process) { p.Steps[2].Participants = []Participant{{User: &UserRef{Role: "admin"}}} }, "роль"},
		{"агент и человек", func(p *Process) {
			p.Steps[2].Participants = []Participant{{Agent: &AgentRef{}, User: &UserRef{Role: "owner"}}}
		}, "либо агентом, либо человеком"},
		{"пустой участник", func(p *Process) { p.Steps[2].Participants = []Participant{{}} }, "не указан ни агент"},
		{"участники у merge", func(p *Process) { p.Steps[3].Participants = []Participant{{Agent: &AgentRef{}}} }, "control plane"},
		{"два merge", func(p *Process) { p.Steps = append(p.Steps, Step{ID: "merge-2", Kind: StepMerge}) }, "ровно один"},
		{"merge отключён", func(p *Process) { p.Steps[3].Enabled = boolp(false) }, "ровно один"},
		{"режим", func(p *Process) { p.Steps[0].Mode = "serial" }, "режим"},
		{"правило", func(p *Process) { p.Steps[0].Require = "two" }, "правило"},
		{"лимит 0", func(p *Process) { p.Steps[0].Attempts = intp(0) }, "не меньше 1"},
		{"переход в никуда", func(p *Process) { p.Steps[2].On = &Transitions{Changes: "nope"} }, "несуществующий шаг"},
		{"переход на отключённый", func(p *Process) {
			p.Steps[1].Enabled = boolp(false)
			p.Steps[2].On = &Transitions{Ok: "test"}
		}, "отключённый шаг"},
		{"changes не на code", func(p *Process) { p.Steps[2].On = &Transitions{Changes: "test"} }, "только шагом code"},
		{"merge с changes", func(p *Process) { p.Steps[3].On = &Transitions{Changes: "code"} }, "только переход ok"},
		{"модель с пробелом", func(p *Process) { p.Steps[0].Participants = []Participant{{Agent: &AgentRef{Model: "opus 5"}}} }, "недопустимые символы"},
	}
	for _, c := range cases {
		p := DefaultProcess()
		c.mut(&p)
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: ожидали ошибку с %q, получили %v", c.name, c.want, err)
		}
		var pe *ProcessError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: ошибка без привязки к шагу: %v", c.name, err)
		}
	}
}

// Переход changes без явного значения ведёт на ближайший предыдущий code;
// без code впереди — эскалация.
func TestResolveChangesTarget(t *testing.T) {
	p := Process{Steps: []Step{
		{ID: "review", Kind: StepReview, Participants: []Participant{{Agent: &AgentRef{}}}},
		{ID: "code", Kind: StepCode, Participants: []Participant{{Agent: &AgentRef{}}}},
		{ID: "test", Kind: StepTest, Participants: []Participant{{Agent: &AgentRef{}}}},
		{ID: "merge", Kind: StepMerge},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	if r.Steps[0].On.Changes != TargetEscalate || r.Steps[2].On.Changes != "code" || r.Steps[1].On.Changes != "code" {
		t.Fatalf("changes: %s %s %s", r.Steps[0].On.Changes, r.Steps[1].On.Changes, r.Steps[2].On.Changes)
	}
	if r.Steps[3].On.Ok != TargetDone {
		t.Fatalf("последний шаг → done: %s", r.Steps[3].On.Ok)
	}
}

// Процесс проекта перекрывает процесс установки целиком, хэш меняется.
func TestEffectiveProcessOverride(t *testing.T) {
	inst := Defaults()
	base := Hash(inst)
	custom := DefaultProcess()
	custom.Steps[2].Participants = append(custom.Steps[2].Participants, Participant{Agent: &AgentRef{Kind: "codex"}})
	eff := Effective(inst, Overrides{Process: &custom})
	if eff.Process == nil || len(eff.Process.Steps[2].Participants) != 2 {
		t.Fatalf("процесс проекта не применился: %+v", eff.Process)
	}
	if Hash(eff) == base {
		t.Fatal("хэш действующей политики должен учитывать процесс")
	}
	// Без переопределения процесс не входит в JSON: хэши прежних версий
	// не меняются.
	if Hash(Effective(inst, Overrides{})) != base {
		t.Fatal("политика без процесса изменила хэш")
	}
	if eff.EffectiveProcess().Steps[2].Participants[1].Agent.Kind != "codex" {
		t.Fatal("разрешённый процесс не из переопределения")
	}
	if len(Defaults().EffectiveProcess().Steps) != 5 {
		t.Fatal("без документа действует процесс по умолчанию")
	}
}

// Участники-люди принимаются документом: логин или роль; в установке —
// только роль.
func TestUserParticipants(t *testing.T) {
	p := DefaultProcess()
	p.Steps[2].Participants = []Participant{{Agent: &AgentRef{}}, {User: &UserRef{Role: RoleOwner}}, {User: &UserRef{Login: "vladimir"}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	if !r.Steps[2].Participants[1].IsUser() || r.Steps[2].Participants[2].User.Login != "vladimir" || r.Steps[2].Participants[0].IsUser() {
		t.Fatalf("участники: %+v", r.Steps[2].Participants)
	}
	if logins := p.UserLogins(); logins["vladimir"] != "review" {
		t.Fatalf("логины: %v", logins)
	}
	inst := Defaults()
	inst.Process = &p
	if err := inst.Validate(); err == nil || !strings.Contains(err.Error(), "политике установки") {
		t.Fatalf("установка с участником по логину: %v", err)
	}
}

// Шаг prompt: текст обязателен, только у prompt; ограничения установки.
func TestPromptStepAndLocks(t *testing.T) {
	p := DefaultProcess()
	p.Steps = append(p.Steps[:3], append([]Step{{ID: "migrations", Kind: StepPrompt, Prompt: "проверь миграции",
		Participants: []Participant{{Agent: &AgentRef{}}}}}, p.Steps[3:]...)...)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	r := Resolve(p, Defaults())
	s, _ := r.Step("migrations")
	if s.Prompt != "проверь миграции" || s.On.Ok != "merge" || s.On.Changes != "code" || s.Title != "Задание агенту" {
		t.Fatalf("prompt: %+v", s)
	}
	bad := p
	bad.Steps = append([]Step{}, p.Steps...)
	bad.Steps[3].Prompt = "  "
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "текст задания") {
		t.Fatalf("prompt без текста: %v", err)
	}
	bad.Steps[3].Prompt = "x"
	bad.Steps[0].Prompt = "лишний"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "только у шага prompt") {
		t.Fatalf("prompt у code: %v", err)
	}

	locks := ProcessLocks{RequiredKinds: []string{StepReview}, MinParticipants: map[string]int{StepReview: 2}, HumanReview: true}
	if err := locks.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := CheckLocks(locks, DefaultProcess()); err == nil || !strings.Contains(err.Error(), "не меньше 2") {
		t.Fatalf("минимум участников: %v", err)
	}
	two := DefaultProcess()
	two.Steps[2].Participants = []Participant{{Agent: &AgentRef{}}, {Agent: &AgentRef{Kind: "codex"}}}
	if err := CheckLocks(locks, two); err == nil || !strings.Contains(err.Error(), "человека") {
		t.Fatalf("человек на review: %v", err)
	}
	two.Steps[2].Participants[1] = Participant{User: &UserRef{Role: RoleOwner}}
	if err := CheckLocks(locks, two); err != nil {
		t.Fatalf("процесс соответствует ограничениям: %v", err)
	}
	off := false
	two.Steps[2].Enabled = &off
	if err := CheckLocks(locks, two); err == nil || !strings.Contains(err.Error(), "включённый шаг типа review") {
		t.Fatalf("обязательный review: %v", err)
	}
	if err := (ProcessLocks{RequiredKinds: []string{"merge"}}).Validate(); err == nil {
		t.Fatal("merge как обязательный тип с участниками не имеет смысла")
	}
}
