package discovery

import (
	"encoding/json"
	"strings"
	"testing"
)

func decisionRaw(d Decision) string {
	b, _ := json.Marshal(struct {
		Decision
		Personas []Persona     `json:"personas"`
		Needs    []Need        `json:"needs"`
		Epics    []BacklogEpic `json:"epics"`
	}{d, []Persona{}, []Need{}, []BacklogEpic{}})
	return string(b)
}
func TestDecisionOutcomesAndEvidence(t *testing.T) {
	text := "Вводная\nНе знаем, в какой системе хранятся договоры."
	d := Decision{Outcome: OutcomeNeedsClarification, Rationale: "Нужен контекст интеграции", Questions: []Question{{ID: "system", Text: "Где хранятся договоры?", Reason: "Определяет интеграцию", Sources: []DecisionSource{{MaterialID: "material", Line: 999, Quote: "в какой системе хранятся договоры"}}}}, Sources: []DecisionSource{}}
	ivs := []InterviewInput{{ID: "material", Text: text}}
	c := CheckSynthesis(ivs, nil, decisionRaw(d))
	if !c.Usable || c.Retry || c.Result.Questions[0].Sources[0].Line != 2 {
		t.Fatalf("%+v", c)
	}
	if len(c.Result.Epics) != 0 {
		t.Fatal("clarification fabricated work")
	}
	for _, edit := range []func(*Decision){
		func(d *Decision) { d.Outcome = "unknown" }, func(d *Decision) { d.Outcome = "" }, func(d *Decision) { d.Questions = nil }, func(d *Decision) { d.Sources = nil }, func(d *Decision) { d.Rationale = "" }, func(d *Decision) { d.Rationale = strings.Repeat("я", 2001) }, func(d *Decision) { d.Questions[0].ID = "UPPER" }, func(d *Decision) { d.Questions[0].Sources[0].MaterialID = "foreign" }, func(d *Decision) {
			d.Questions[0].Sources[0].Quote = "Выдуманная цитата не из материала"
		},
	} {
		var copy Decision
		b, _ := json.Marshal(d)
		_ = json.Unmarshal(b, &copy)
		edit(&copy)
		if c := CheckSynthesis(ivs, nil, decisionRaw(copy)); c.Usable || !c.Retry {
			t.Fatalf("invalid decision accepted %+v", copy)
		}
	}
	d = Decision{Outcome: OutcomeNoDevelopment, Rationale: "Существующий сервис решает задачу, разработка не требуется", Questions: []Question{}, Sources: []DecisionSource{}}
	if c := CheckSynthesis(nil, nil, decisionRaw(d)); !c.Usable || c.Retry {
		t.Fatalf("%+v", c)
	}
	d.Outcome = OutcomeBacklogReady
	if c := CheckSynthesis(nil, nil, decisionRaw(d)); c.Usable {
		t.Fatal("empty actionable backlog accepted")
	}
}
func TestSavedOutcomeCompatibility(t *testing.T) {
	legacyModel := Synthesis{Personas: []Persona{{ID: "p1", Name: "Пользователь", Interviews: []string{"deleted"}}}, Needs: []Need{{ID: "n1", Title: "Функция", Kind: KindNeed, Importance: ImportanceHigh, Persona: "p1", Evidence: []QuoteRef{{Interview: "deleted", Line: 2, Text: "Нужна проверяемая полезная функция"}}}}, Epics: []BacklogEpic{{Key: "e1", Title: "Сервис", Goal: "Сделать полезную функцию для проверки рабочего процесса пользователем.", Priority: PriorityMust, Size: "S", Needs: []string{"n1"}, DependsOn: []string{}}}}
	legacyBytes, _ := json.Marshal(legacyModel)
	legacy := string(legacyBytes)
	s, old, err := ReadSavedSynthesis([]byte(legacy))
	if err != "" || !old || s.Outcome != OutcomeBacklogReady {
		t.Fatalf("%+v %v %s", s, old, err)
	}
	s, old, err = ReadSavedSynthesis([]byte(`{"personas":[],"needs":[],"epics":[]}`))
	if err == "" || !old || s.Outcome != "" {
		t.Fatal("empty legacy became actionable")
	}
	// The same historic shape is rejected as a new model response.
	if c := CheckSynthesis(nil, nil, legacy); c.Usable {
		t.Fatal("new answer got legacy fallback")
	}
	d := Decision{Outcome: OutcomeNoDevelopment, Rationale: "Повторная разработка не нужна", Questions: []Question{}, Sources: []DecisionSource{}}
	s, old, err = ReadSavedSynthesis([]byte(decisionRaw(d)))
	if err != "" || old || s.Outcome != OutcomeNoDevelopment {
		t.Fatalf("%+v %v %s", s, old, err)
	}
}

func TestSavedDecisionMultipleQuotesSameMaterialRoundTrip(t *testing.T) {
	ivs := []InterviewInput{{ID: "material", Text: "Первая строка о существующей системе.\nВторая строка о неизвестном интерфейсе."}}
	d := Decision{Outcome: OutcomeNeedsClarification, Rationale: "Не определена интеграция", Sources: []DecisionSource{{MaterialID: "material", Line: 999, Quote: "Первая строка о существующей системе."}}, Questions: []Question{{ID: "api", Text: "Какой интерфейс?", Reason: "Неизвестна интеграция", Sources: []DecisionSource{{MaterialID: "material", Line: 999, Quote: "Вторая строка о неизвестном интерфейсе."}}}}}
	c := CheckSynthesis(ivs, nil, decisionRaw(d))
	if !c.Usable {
		t.Fatalf("%+v", c)
	}
	raw, err := json.Marshal(c.Result)
	if err != nil {
		t.Fatal(err)
	}
	saved, legacy, why := ReadSavedSynthesis(raw)
	if why != "" || legacy || saved.Outcome != OutcomeNeedsClarification {
		t.Fatalf("roundtrip %+v %v %s", saved, legacy, why)
	}
	if saved.Sources[0].Line != 1 || saved.Questions[0].Sources[0].Line != 2 {
		t.Fatal("source lines changed")
	}
	// No current materials are supplied to the history reader, including after deletion.
	if _, _, why = ReadSavedSynthesis(raw); why != "" {
		t.Fatal(why)
	}
}

func TestMalformedLegacyReferencesCannotBeApproved(t *testing.T) {
	for _, raw := range []string{
		`{"personas":[],"needs":[],"epics":[{"key":"h1","title":"Bypass","goal":"Fake work without an original research need or persona at all.","priority":"must","size":"S","needs":[],"depends_on":[]}]}`,
		`{"personas":[{"id":"p1","name":"User"}],"needs":[{"id":"n1","title":"Need","kind":"need","importance":"high","persona":"ghost","evidence":[{"interview":"deleted","line":1,"text":"Verified historical quote"}]}],"epics":[{"key":"e1","title":"Bypass","goal":"Fake work with a broken persona reference that must be rejected.","priority":"must","size":"S","needs":["n1"],"depends_on":[]}]}`,
	} {
		s, legacy, err := ReadSavedSynthesis([]byte(raw))
		if !legacy || err == "" || s.Outcome != "" {
			t.Fatalf("invalid legacy became actionable %+v %v %s", s, legacy, err)
		}
	}
}
