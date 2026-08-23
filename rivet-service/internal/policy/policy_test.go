package policy

import (
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestEffectiveInheritsAndOverrides(t *testing.T) {
	inst := Defaults()
	inst.AttemptLimit = 5
	budget := int64(1000)
	inst.DailyTokenBudget = &budget

	// Проект ничего не переопределял — действуют значения установки.
	eff := Effective(inst, Overrides{})
	if eff.AttemptLimit != 5 || eff.DailyTokenBudget == nil || *eff.DailyTokenBudget != 1000 || eff.AutoMerge {
		t.Fatalf("наследование: %+v", eff)
	}
	// Переопределение лимита и снятие бюджета (0 — «без ограничения»).
	eff = Effective(inst, Overrides{AttemptLimit: ptr(2), DailyTokenBudget: ptr(int64(0)), AutoMerge: ptr(true)})
	if eff.AttemptLimit != 2 || eff.DailyTokenBudget != nil || !eff.AutoMerge {
		t.Fatalf("переопределение: %+v", eff)
	}
	// Хэш действующего документа детерминирован и меняется от содержимого.
	h1, h2 := Hash(Effective(inst, Overrides{})), Hash(Effective(inst, Overrides{AttemptLimit: ptr(2)}))
	if h1 == h2 || h1 != Hash(Effective(inst, Overrides{})) || len(h1) != 64 {
		t.Fatalf("hash: %s %s", h1, h2)
	}
}

func TestValidate(t *testing.T) {
	p := Defaults()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.AttemptLimit = 0
	if err := p.Validate(); err == nil {
		t.Fatal("лимит 0 должен отклоняться")
	}
	p = Defaults()
	p.DailyTokenBudget = ptr(int64(-1))
	if err := p.Validate(); err == nil {
		t.Fatal("отрицательный бюджет должен отклоняться")
	}
	p = Defaults()
	p.HumanReviewPaths = []string{"infra/**", "[bad"}
	if err := p.Validate(); err == nil {
		t.Fatal("битый шаблон должен отклоняться")
	}
	o := Overrides{ReviewLimit: ptr(0)}
	if err := o.Validate(); err == nil {
		t.Fatal("лимит отказов review 0 должен отклоняться")
	}
	if err := (Overrides{HumanReviewPaths: ptr([]string{"/abs"})}).Validate(); err == nil {
		t.Fatal("ведущий слэш должен отклоняться")
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"infra/**", "**/*.sql", "deploy/prod/*", "docs/"}
	cases := map[string]bool{
		"infra/main.tf":         true,
		"infra/a/b/c.yaml":      true,
		"migrations/0001.sql":   true,
		"x.sql":                 true,
		"deploy/prod/run.sh":    true,
		"deploy/prod/sub/x.sh":  false,
		"docs/readme.md":        true,
		"src/main.go":           false,
		"/infra/main.tf":        true,
		"infrastructure/x.yaml": false,
	}
	for path, want := range cases {
		if got := MatchAny(pats, path); got != want {
			t.Errorf("%s: want %v got %v", path, want, got)
		}
	}
	if !IsPolicyPath(".rivet/policy.yaml") || IsPolicyPath("src/.rivet/x") || IsPolicyPath("rivet/x") {
		t.Fatal("IsPolicyPath")
	}
}

func TestPathsFromDiff(t *testing.T) {
	diff := "diff --git a/src/main.go b/src/main.go\n" +
		"index 1..2 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/old name.txt b/new name.txt\n" +
		"diff --git \"a/\\320\\277.md\" \"b/\\320\\277.md\"\n" +
		"diff --git a/.rivet/policy.yaml b/.rivet/policy.yaml\n"
	got := PathsFromDiff(diff)
	want := []string{"src/main.go", "old name.txt", "new name.txt", "п.md", ".rivet/policy.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %q got %q", want, got)
	}
	if len(PathsFromDiff("")) != 0 || len(PathsFromDiff("мусор без заголовков")) != 0 {
		t.Fatal("без заголовков путей быть не должно")
	}
}
