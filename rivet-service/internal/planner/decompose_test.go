package planner

import "testing"

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
		"битая ссылка":    {{Title: "A", Criteria: []string{"c"}, Deps: []int{7}}},
		"сам от себя":     {{Title: "A", Criteria: []string{"c"}, Deps: []int{0}}},
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
