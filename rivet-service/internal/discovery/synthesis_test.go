package discovery

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// demoInterviews — два демо-интервью CRM с подтверждёнными конспектами.
func demoInterviews(t *testing.T) []InterviewInput {
	t.Helper()
	manager := loadInterview(t, "manager.md")
	head := loadInterview(t, "head.md")
	return []InterviewInput{
		{ID: "iv-manager", Title: "Менеджер по продажам", RespondentRole: "Менеджер B2B", Text: manager,
			Summary: Summary{Findings: []Finding{{Kind: KindPain, Text: "забывает перезвонить", Importance: ImportanceHigh,
				Quotes: []Quote{{Text: "клиент просил набрать во вторник после совещания", Line: 17}}}}}},
		{ID: "iv-head", Title: "Руководитель продаж", RespondentRole: "Руководитель отдела", Text: head},
	}
}

type synthIn struct {
	Decision
	Personas []map[string]any `json:"personas"`
	Needs    []map[string]any `json:"needs"`
	Epics    []map[string]any `json:"epics"`
}

func baseSynthesis() synthIn {
	return synthIn{Decision: Decision{Outcome: OutcomeBacklogReady, Rationale: "Достаточно подтвержденных требований", Questions: []Question{}, Sources: []DecisionSource{}},
		Personas: []map[string]any{
			{"id": "p1", "name": "Менеджер", "description": "продаёт", "interviews": []string{"iv-manager"}},
			{"id": "p2", "name": "Руководитель", "description": "отвечает за план", "interviews": []string{"iv-head", "iv-ghost"}},
		},
		Needs: []map[string]any{
			{"id": "n1", "title": "Напоминания о звонках", "kind": "pain", "persona": "p1", "importance": "high",
				"evidence": []map[string]any{{"interview": "iv-manager", "quote": "клиент просил набрать во вторник после совещания"}}},
			{"id": "n2", "title": "Воронка целиком", "kind": "need", "persona": "p2", "importance": "high",
				"evidence": []map[string]any{{"interview": "iv-head", "quote": "Потому что я не вижу воронку целиком.", "line": 3}}},
			{"id": "n3", "title": "Отчёт без ручной работы", "kind": "pain", "persona": "p1", "importance": "medium",
				"evidence": []map[string]any{{"interview": "iv-manager", "quote": "Это минут сорок по пятницам"}}},
		},
		Epics: []map[string]any{
			{"key": "e1", "title": "Сделки и воронка", "goal": "Воронка продаж с этапами и суммами для руководителя и менеджеров.",
				"rationale": "n2", "needs": []string{"n2", "n3"}, "priority": "must", "size": "M", "depends_on": []string{}, "existing_epic": nil},
			{"key": "e2", "title": "Напоминания", "goal": "Список звонков на сегодня и напоминания о следующем шаге.",
				"needs": []string{"n1"}, "priority": "should", "size": "S", "depends_on": []string{"e1"}, "existing_epic": nil},
		},
	}
}

func (s synthIn) raw() string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCheckSynthesisValid(t *testing.T) {
	c := CheckSynthesis(demoInterviews(t), nil, baseSynthesis().raw())
	if c.Retry || !c.Usable {
		t.Fatalf("ждали принятый результат: %+v", c)
	}
	if len(c.Result.Personas) != 2 || len(c.Result.Needs) != 3 || len(c.Result.Epics) != 2 {
		t.Fatalf("результат: %+v", c.Result)
	}
	// Неизвестное интервью персоны отброшено, персона осталась.
	if got := c.Result.Personas[1].Interviews; len(got) != 1 || got[0] != "iv-head" {
		t.Fatalf("интервью персоны: %v", got)
	}
	if len(c.Dropped) != 1 || c.Dropped[0].Kind != "persona" {
		t.Fatalf("отброшенное: %+v", c.Dropped)
	}
	// Строка цитаты от системы: модель сказала 3, фраза на строке 17 интервью руководителя.
	ev := c.Result.Needs[1].Evidence[0]
	if ev.Line != 17 || ev.InterviewTitle != "Руководитель продаж" || ev.RespondentRole != "Руководитель отдела" {
		t.Fatalf("цитата: %+v", ev)
	}
}

func TestCheckSynthesisRules(t *testing.T) {
	ivs := demoInterviews(t)
	existing := []ExistingEpic{
		{ID: "ep-contacts", Title: "Компании и контакты", Status: "done"},
		{ID: "ep-old", Title: "Напоминания", Status: "archived"},
		{ID: "ep-x", Title: "Икс", Status: "running"},
	}
	cases := []struct {
		name  string
		edit  func(s *synthIn)
		check func(t *testing.T, c SynthesisCheck)
	}{
		{"выдуманная цитата", func(s *synthIn) {
			s.Needs[0]["evidence"] = []map[string]any{{"interview": "iv-manager", "quote": "Мне нужна CRM с искусственным интеллектом"}}
		}, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Needs) != 2 || !hasDropped(c, "need", ReasonNoQuotes) || !hasDropped(c, "quote", ReasonQuoteNotFound) {
				t.Fatalf("потребность без цитат должна уйти в отброшенное: %+v", c.Dropped)
			}
			// e2 держался только на n1 — Epic без потребностей отброшен, зависимость ушла вместе с ним.
			if len(c.Result.Epics) != 1 || !hasDropped(c, "epic", ReasonNoNeeds) || c.Retry {
				t.Fatalf("Epic'и: %+v", c)
			}
		}},
		{"цитата из чужого интервью", func(s *synthIn) {
			s.Needs[2]["evidence"] = []map[string]any{{"interview": "iv-head", "quote": "Это минут сорок по пятницам"}}
		}, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Needs) != 2 || !hasDropped(c, "quote", ReasonQuoteNotFound) {
				t.Fatalf("%+v", c)
			}
		}},
		{"неизвестная персона", func(s *synthIn) { s.Needs[2]["persona"] = "p9" }, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Needs) != 2 || !hasDropped(c, "need", ReasonUnknownPersona+": p9") {
				t.Fatalf("%+v", c.Dropped)
			}
		}},
		{"цикл зависимостей", func(s *synthIn) { s.Epics[0]["depends_on"] = []string{"e2"} }, func(t *testing.T, c SynthesisCheck) {
			if !c.Retry || c.Usable || !containsDefect(c, "цикл: e1 → e2 → e1") {
				t.Fatalf("ждали роковой цикл: %+v", c.Defects)
			}
		}},
		{"повтор названия", func(s *synthIn) { s.Epics[1]["title"] = "  СДЕЛКИ и воронка" }, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Epics) != 1 || !hasDropped(c, "epic", ReasonDuplicateTitle+" (e1)") {
				t.Fatalf("%+v", c)
			}
		}},
		{"неверные поля Epic'а", func(s *synthIn) { s.Epics[1]["priority"] = "high"; s.Epics[0]["size"] = "XL" }, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Epics) != 0 || c.Usable {
				t.Fatalf("без Epic'ов результат не принимается: %+v", c)
			}
		}},
		{"зависимость на неизвестный Epic", func(s *synthIn) { s.Epics[1]["depends_on"] = []string{"e1", "e7", "e1"} }, func(t *testing.T, c SynthesisCheck) {
			if got := c.Result.Epics[1].DependsOn; len(got) != 1 || got[0] != "e1" || !hasDropped(c, "dependency", ReasonUnknownDependency) {
				t.Fatalf("зависимости: %v %+v", got, c.Dropped)
			}
		}},
		{"зависимость на Epic проекта", func(s *synthIn) { s.Epics[0]["depends_on"] = []string{"ep-contacts"} }, func(t *testing.T, c SynthesisCheck) {
			if got := c.Result.Epics[0].DependsOn; len(got) != 1 || got[0] != "ep-contacts" || c.Retry {
				t.Fatalf("%v %+v", got, c)
			}
		}},
		{"связь с существующим по названию, архивный не связывается", func(s *synthIn) {
			s.Epics[0]["title"] = "Компании и контакты"
		}, func(t *testing.T, c SynthesisCheck) {
			if e := c.Result.Epics[0]; e.ExistingEpic == nil || *e.ExistingEpic != "ep-contacts" {
				t.Fatalf("e1 связывается с ep-contacts: %+v", e)
			}
			if e := c.Result.Epics[1]; e.ExistingEpic != nil {
				t.Fatalf("архивный Epic не связывается: %+v", e)
			}
		}},
		{"ссылка модели на архивный Epic снимается", func(s *synthIn) { s.Epics[1]["existing_epic"] = "ep-old" }, func(t *testing.T, c SynthesisCheck) {
			if c.Result.Epics[1].ExistingEpic != nil || !hasDropped(c, "epic", ReasonUnknownEpic) {
				t.Fatalf("%+v", c)
			}
		}},
		{"больше половины потребностей отброшено — повтор, но результат годен", func(s *synthIn) {
			s.Needs[0]["evidence"] = []map[string]any{{"interview": "iv-manager", "quote": "выдумка номер один про CRM"}}
			s.Needs[2]["evidence"] = []map[string]any{{"interview": "iv-manager", "quote": "выдумка номер два про отчёты"}}
		}, func(t *testing.T, c SynthesisCheck) {
			if !c.Retry || !c.Usable || len(c.Result.Needs) != 1 {
				t.Fatalf("%+v", c)
			}
		}},
		{"повтор идентификатора потребности", func(s *synthIn) { s.Needs[2]["id"] = "n1" }, func(t *testing.T, c SynthesisCheck) {
			if c.Usable || !containsDefect(c, "потребность n1 повторяется") {
				t.Fatalf("%+v", c.Defects)
			}
		}},
		{"больше пяти цитат обрезается", func(s *synthIn) {
			var ev []map[string]any
			for _, q := range []string{"Утром открываю почту и Excel", "Смотрю, кому сегодня обещал перезвонить",
				"Половина обещаний у меня в голове", "клиент просил набрать во вторник", "Он уже купил у конкурента",
				"Сделка была на миллион двести", "Обидно до сих пор."} {
				ev = append(ev, map[string]any{"interview": "iv-manager", "quote": q})
			}
			s.Needs[0]["evidence"] = ev
		}, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Needs[0].Evidence) != MaxQuotesPerNeed || !hasDropped(c, "quote", ReasonQuoteLimit) {
				t.Fatalf("%+v", c.Result.Needs[0].Evidence)
			}
		}},
		{"лимит Epic'ов", func(s *synthIn) {
			for i := 3; i <= 13; i++ {
				s.Epics = append(s.Epics, map[string]any{"key": fmt.Sprintf("e%d", i), "title": fmt.Sprintf("Epic %d", i),
					"goal": strings.Repeat("цель ", 10), "needs": []string{"n1"}, "priority": "could", "size": "S", "depends_on": []string{}, "existing_epic": nil})
			}
		}, func(t *testing.T, c SynthesisCheck) {
			if c.Usable || !containsDefect(c, "Epic'ов 13") {
				t.Fatalf("%+v", c.Defects)
			}
		}},
		{"неверный ключ Epic'а", func(s *synthIn) { s.Epics[0]["key"] = "epic-1" }, func(t *testing.T, c SynthesisCheck) {
			if c.Usable {
				t.Fatalf("%+v", c)
			}
		}},
		{"короткая цель модели", func(s *synthIn) { s.Epics[1]["goal"] = "Напоминания" }, func(t *testing.T, c SynthesisCheck) {
			if len(c.Result.Epics) != 1 || !hasDropped(c, "epic", "цель короче 40 символов") {
				t.Fatalf("%+v", c)
			}
		}},
		{"зависимость на себя — цикл", func(s *synthIn) { s.Epics[0]["depends_on"] = []string{"e1"} }, func(t *testing.T, c SynthesisCheck) {
			if c.Usable || !containsDefect(c, "цикл: e1 → e1") {
				t.Fatalf("%+v", c.Defects)
			}
		}},
		{"цикл через связанный Epic проекта", func(s *synthIn) {
			// e1 связан с ep-x по названию и зависит от e2, e2 зависит от ep-x.
			s.Epics[0]["title"] = "Икс"
			s.Epics[0]["depends_on"] = []string{"e2"}
			s.Epics[1]["depends_on"] = []string{"ep-x"}
		}, func(t *testing.T, c SynthesisCheck) {
			if c.Usable || !containsDefect(c, "цикл") {
				t.Fatalf("ждали цикл e1 → e2 → ep-x(e1): %+v", c.Defects)
			}
		}},
		{"зависимость на свой связанный Epic снимается", func(s *synthIn) {
			s.Epics[0]["existing_epic"] = "ep-x"
			s.Epics[0]["depends_on"] = []string{"ep-x"}
		}, func(t *testing.T, c SynthesisCheck) {
			if !c.Usable || len(c.Result.Epics[0].DependsOn) != 0 || !hasDropped(c, "dependency", ReasonSelfLinkedDep) {
				t.Fatalf("%+v", c)
			}
		}},
		{"две ссылки на один Epic проекта", func(s *synthIn) {
			s.Epics[0]["existing_epic"] = "ep-x"
			s.Epics[1]["existing_epic"] = "ep-x"
		}, func(t *testing.T, c SynthesisCheck) {
			if !c.Usable || c.Result.Epics[1].ExistingEpic != nil || !hasDropped(c, "epic", ReasonDoubleLink+" (e1)") {
				t.Fatalf("%+v", c)
			}
		}},
		{"лишнее поле — предупреждение", func(s *synthIn) { s.Epics[0]["confidence"] = 0.9 }, func(t *testing.T, c SynthesisCheck) {
			if !c.Usable || c.Retry || !containsDefect(c, "предупреждение") {
				t.Fatalf("%+v", c)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSynthesis()
			tc.edit(&s)
			c := CheckSynthesis(ivs, existing, s.raw())
			tc.check(t, c)
			// Инвариант: годный результат синтеза проходит проверку правки
			// бэклога, иначе первая же правка человека упала бы.
			if c.Usable {
				var needs []string
				for _, n := range c.Result.Needs {
					needs = append(needs, n.ID)
				}
				if err := ValidateBacklog(c.Result.Epics, needs, existing); err != nil {
					t.Fatalf("годный синтез не проходит ValidateBacklog: %v", err)
				}
			}
		})
	}
}

func hasDropped(c SynthesisCheck, kind, reason string) bool {
	for _, d := range c.Dropped {
		if d.Kind == kind && d.Reason == reason {
			return true
		}
	}
	return false
}

func containsDefect(c SynthesisCheck, part string) bool {
	for _, d := range c.Defects {
		if strings.Contains(d, part) {
			return true
		}
	}
	return false
}

func TestValidateBacklog(t *testing.T) {
	ptr := func(s string) *string { return &s }
	goal := "Воронка продаж с этапами и суммами для руководителя и менеджеров."
	base := func() []BacklogEpic {
		return []BacklogEpic{
			{Key: "e1", Title: "Сделки", Goal: goal, Needs: []string{"n1"}, Priority: "must", Size: "M", DependsOn: []string{}},
			{Key: "h1", Title: "Импорт из Excel", Goal: goal, Priority: "could", Size: "S", DependsOn: []string{"e1", "ep-contacts"}},
		}
	}
	existing := []ExistingEpic{{ID: "ep-contacts", Title: "Контакты", Status: "done"}, {ID: "ep-old", Status: "archived"}}
	if err := ValidateBacklog(base(), []string{"n1"}, existing); err != nil {
		t.Fatalf("валидный бэклог: %v", err)
	}
	cases := []struct {
		name  string
		edit  func(b []BacklogEpic) []BacklogEpic
		field string
	}{
		{"пусто", func([]BacklogEpic) []BacklogEpic { return nil }, "epics"},
		{"ключ", func(b []BacklogEpic) []BacklogEpic { b[1].Key = "x1"; return b }, "epics[1].key"},
		{"повтор ключа", func(b []BacklogEpic) []BacklogEpic { b[1].Key = "e1"; return b }, "epics[1].key"},
		{"короткая цель", func(b []BacklogEpic) []BacklogEpic { b[0].Goal = "мало"; return b }, "epics[0].goal"},
		{"повтор названия", func(b []BacklogEpic) []BacklogEpic { b[1].Title = "сделки "; return b }, "epics[1].title"},
		{"приоритет", func(b []BacklogEpic) []BacklogEpic { b[1].Priority = "high"; return b }, "epics[1].priority"},
		{"Epic Аналитика без потребностей", func(b []BacklogEpic) []BacklogEpic { b[0].Needs = nil; return b }, "epics[0].needs"},
		{"неизвестная потребность", func(b []BacklogEpic) []BacklogEpic { b[1].Needs = []string{"n9"}; return b }, "epics[1].needs"},
		{"зависимость на себя", func(b []BacklogEpic) []BacklogEpic { b[0].DependsOn = []string{"e1"}; return b }, "epics[0].depends_on"},
		{"зависимость на архив", func(b []BacklogEpic) []BacklogEpic { b[0].DependsOn = []string{"ep-old"}; return b }, "epics[0].depends_on"},
		{"цикл", func(b []BacklogEpic) []BacklogEpic { b[0].DependsOn = []string{"h1"}; return b }, "epics[0].depends_on"},
		{"связь с архивом", func(b []BacklogEpic) []BacklogEpic { b[1].ExistingEpic = ptr("ep-old"); return b }, "epics[1].existing_epic"},
		{"двойная связь", func(b []BacklogEpic) []BacklogEpic {
			b[0].ExistingEpic, b[1].ExistingEpic = ptr("ep-contacts"), ptr("ep-contacts")
			b[1].DependsOn = []string{"e1"}
			return b
		}, "epics[1].existing_epic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBacklog(c.edit(base()), []string{"n1"}, existing)
			if err == nil || err.Field != c.field {
				t.Fatalf("ждали ошибку поля %s, получили %v", c.field, err)
			}
		})
	}
}

func TestTopoOrder(t *testing.T) {
	epics := []BacklogEpic{
		{Key: "e1", Priority: "could"},
		{Key: "e2", Priority: "must", DependsOn: []string{"e3"}},
		{Key: "e3", Priority: "should"},
		{Key: "e4", Priority: "must", DependsOn: []string{"ep-existing"}},
		{Key: "e5", Priority: "must", DependsOn: []string{"e1", "e3"}},
	}
	got, err := TopoOrder(epics)
	if err != nil {
		t.Fatal(err)
	}
	want := "e4 e3 e2 e1 e5"
	if strings.Join(got, " ") != want {
		t.Fatalf("порядок %v, ждали %s", got, want)
	}
	epics[2].DependsOn = []string{"e2"}
	if _, err := TopoOrder(epics); err == nil || !strings.Contains(err.Error(), "e2 → e3 → e2") {
		t.Fatalf("ждали цикл: %v", err)
	}
}

func TestFindCycle(t *testing.T) {
	if c := FindCycle(map[string][]string{"a": {"b"}, "b": {"c"}, "c": {}, "d": {"x"}}); c != nil {
		t.Fatalf("цикла нет: %v", c)
	}
	if c := FindCycle(map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}}); strings.Join(c, ",") != "a,b,c,a" {
		t.Fatalf("цикл: %v", c)
	}
}
