package planner

import (
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func TestValidatePlan(t *testing.T) {
	valid := []PlannedTask{
		{Title: "A", Criteria: []string{"c"}},
		{Title: "B", Criteria: []string{"c"}, Deps: []int{0}},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("валидный план отклонён: %v", err)
	}

	cases := map[string][]PlannedTask{
		"пустой":        {},
		"без критериев": {{Title: "A"}},
		"дубль":         {{Title: "A", Criteria: []string{"c"}}, {Title: "A", Criteria: []string{"c"}}},
		"цикл": {
			{Title: "A", Criteria: []string{"c"}, Deps: []int{1}},
			{Title: "B", Criteria: []string{"c"}, Deps: []int{0}},
		},
		"битая ссылка": {{Title: "A", Criteria: []string{"c"}, Deps: []int{7}}},
		"сам от себя":  {{Title: "A", Criteria: []string{"c"}, Deps: []int{0}}},
		"дубль зависимости": {
			{Title: "A", Criteria: []string{"c"}},
			{Title: "B", Criteria: []string{"c"}, Deps: []int{0, 0}},
		},
		"пустое название": {{Title: "  ", Criteria: []string{"c"}}},
	}
	for name, plan := range cases {
		if err := Validate(plan); err == nil {
			t.Errorf("%s: план должен быть отклонён", name)
		}
	}
}

func TestParsePlanTolerant(t *testing.T) {
	text := "Вот план:\n```json\n[{\"title\":\"A\",\"criteria\":[\"x\"],\"deps\":[]}]\n```"
	plan, err := parsePlan(text)
	if err != nil || len(plan) != 1 || plan[0].Title != "A" {
		t.Fatalf("парсер не справился: %v %+v", err, plan)
	}
}

// Инструкция Тимлида дополняет задание, требования к формату остаются
// (спека epic-decomposition «Декомпозицию выполняет Тимлид»).
func TestLeadInstructionInPrompt(t *testing.T) {
	epic := domain.Epic{Title: "E", Goal: "цель"}
	plain := buildPrompt(epic, domain.Project{}, "")
	if strings.Contains(plain, "# Роль Тимлида") {
		t.Fatal("без инструкции блока нет")
	}
	p := buildPrompt(epic, domain.Project{}, "Не больше 5 задач на Epic.")
	if !strings.Contains(p, "# Роль Тимлида\nНе больше 5 задач на Epic.") || !strings.Contains(p, "Верни ТОЛЬКО JSON-массив") ||
		strings.Index(p, "# Роль Тимлида") < strings.Index(p, "Верни ТОЛЬКО JSON-массив") {
		t.Fatalf("инструкция после требований к формату: %s", p)
	}
}
