package discovery

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	OutcomeBacklogReady       = "backlog_ready"
	OutcomeNeedsClarification = "needs_clarification"
	OutcomeNoDevelopment      = "no_development_recommended"
)

var questionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type DecisionSource struct {
	MaterialID string `json:"material_id"`
	Line       int    `json:"line"`
	Quote      string `json:"quote"`
}
type Question struct {
	ID      string           `json:"id"`
	Text    string           `json:"text"`
	Reason  string           `json:"reason"`
	Sources []DecisionSource `json:"sources"`
}
type Decision struct {
	Outcome   string           `json:"outcome,omitempty"`
	Rationale string           `json:"rationale"`
	Questions []Question       `json:"questions"`
	Sources   []DecisionSource `json:"sources"`
}

func validDecisionText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}

// checkDecision checks the new model contract. Source lines are recomputed
// from the actual run material, never trusted from the model.
func checkDecision(d *Decision, epics []BacklogEpic, materials map[string]InterviewInput) []string {
	var defects []string
	bad := func(s string) { defects = append(defects, s) }
	if !validDecisionText(d.Rationale, 2000) {
		bad("rationale: требуется обоснование 1–2000 символов")
	}
	if d.Questions == nil || d.Sources == nil {
		bad("questions, sources: обязательны массивы, не null")
	}
	switch d.Outcome {
	case OutcomeBacklogReady:
		if len(epics) < 1 || len(epics) > MaxEpics || len(d.Questions) != 0 {
			bad("backlog_ready: нужны 1–12 Epic и ноль вопросов")
		}
	case OutcomeNeedsClarification:
		if len(epics) != 0 || len(d.Questions) < 1 || len(d.Questions) > 5 {
			bad("needs_clarification: нужны 1–5 вопросов и ноль Epic")
		}
	case OutcomeNoDevelopment:
		if len(epics) != 0 || len(d.Questions) != 0 {
			bad("no_development_recommended: Epic и вопросы должны быть пустыми")
		}
	default:
		bad("outcome: требуется backlog_ready, needs_clarification или no_development_recommended")
	}
	checkSources := func(sources []DecisionSource) {
		if len(sources) > 5 {
			bad("sources: не больше 5 ссылок")
		}
		for i := range sources {
			src := &sources[i]
			iv, ok := materials[src.MaterialID]
			if !ok {
				bad("sources: материал отсутствует в текущем исследовании")
				continue
			}
			loc, reason, ok := locate(iv.Text, src.Quote, src.Line)
			if !ok {
				bad("sources: " + reason)
				continue
			}
			src.Line, src.Quote = loc.Line, loc.Text
		}
	}
	checkSources(d.Sources)
	seen := map[string]bool{}
	for i := range d.Questions {
		q := &d.Questions[i]
		if !questionID.MatchString(q.ID) || seen[q.ID] {
			bad("questions: неверный или повторный id")
		}
		seen[q.ID] = true
		if !validDecisionText(q.Text, 1000) || !validDecisionText(q.Reason, 1000) {
			bad("questions: text и reason должны содержать 1–1000 символов")
		}
		if q.Sources == nil {
			bad("questions.sources: требуется массив")
		}
		checkSources(q.Sources)
	}
	return defects
}

// ReadSavedSynthesis is deliberately separate from model decoding. Only a
// structurally valid historical backlog may receive the legacy projection.
// Historical quotes are not revalidated against materials that may be deleted.
func ReadSavedSynthesis(raw []byte) (*Synthesis, bool, string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, "результат не разобран"
	}
	_, hasOutcome := fields["outcome"]
	legacy := !hasOutcome
	var s Synthesis
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, legacy, "результат не разобран"
	}
	if legacy {
		if !validSavedLegacyStructure(s) {
			s.Outcome = ""
			return &s, true, "исторический результат не содержит корректных персон, потребностей и бэклога"
		}
		s.Outcome, s.Rationale = OutcomeBacklogReady, ""
		s.Questions, s.Sources = []Question{}, []DecisionSource{}
		ids := make([]string, 0, len(s.Needs))
		for _, n := range s.Needs {
			ids = append(ids, n.ID)
		}
		// Preserve historical existing Epic references while checking structure.
		ex := map[string]bool{}
		var existing []ExistingEpic
		for _, e := range s.Epics {
			if e.ExistingEpic != nil {
				ex[*e.ExistingEpic] = true
			}
			for _, d := range e.DependsOn {
				if !modelKeyRe.MatchString(d) && !humanKeyRe.MatchString(d) {
					ex[d] = true
				}
			}
		}
		for id := range ex {
			existing = append(existing, ExistingEpic{ID: id})
		}
		if err := ValidateBacklog(s.Epics, ids, existing); err != nil || s.Personas == nil || s.Needs == nil || len(s.Personas) > MaxPersonas || len(s.Needs) > MaxNeeds {
			s.Outcome = ""
			return &s, true, "исторический результат не содержит корректного бэклога"
		}
		return &s, true, ""
	}
	if s.Questions == nil || s.Sources == nil || s.Personas == nil || s.Needs == nil || s.Epics == nil {
		s.Outcome = ""
		return &s, false, "некорректные массивы результата"
	}
	// New saved data already has verified sources. Validate structure, bounds,
	// source IDs and positive lines without requiring current source availability.
	copyDecision := s.Decision
	copyDecision.Sources = []DecisionSource{}
	copyDecision.Questions = append([]Question{}, s.Questions...)
	// Sources were verified on acceptance. History must not locate quotes
	// against an invented text or depend on currently available materials.
	validSources := func(ss []DecisionSource) bool {
		if len(ss) > 5 {
			return false
		}
		for _, src := range ss {
			if strings.TrimSpace(src.MaterialID) == "" || src.Line < 1 || !utf8.ValidString(src.Quote) || strings.ContainsRune(src.Quote, 0) || utf8.RuneCountInString(normalizeQuote(src.Quote)) < MinQuoteRunes {
				return false
			}
		}
		return true
	}
	valid := validSources(s.Sources)
	for i, q := range s.Questions {
		valid = valid && q.Sources != nil && validSources(q.Sources)
		copyDecision.Questions[i].Sources = []DecisionSource{}
	}
	if !valid {
		s.Outcome = ""
		return &s, false, "некорректные источники сохраненного результата"
	}
	if ds := checkDecision(&copyDecision, s.Epics, nil); len(ds) > 0 {
		s.Outcome = ""
		return &s, false, fmt.Sprintf("некорректный исход исследования: %s", ds[0])
	}

	return &s, false, ""
}

// Legacy history keeps verified quotes without consulting live materials, but
// malformed IDs/references and invented empty model work are never actionable.
func validSavedLegacyStructure(s Synthesis) bool {
	personas := map[string]bool{}
	needs := map[string]bool{}
	for _, p := range s.Personas {
		if !itemIDRe.MatchString(p.ID) || strings.TrimSpace(p.Name) == "" || personas[p.ID] {
			return false
		}
		personas[p.ID] = true
	}
	for _, n := range s.Needs {
		if !itemIDRe.MatchString(n.ID) || strings.TrimSpace(n.Title) == "" || needs[n.ID] || !personas[n.Persona] || !validImportance(n.Importance) || (n.Kind != KindPain && n.Kind != KindNeed && n.Kind != KindWish) || len(n.Evidence) < 1 || len(n.Evidence) > MaxQuotesPerNeed {
			return false
		}
		needs[n.ID] = true
		for _, q := range n.Evidence {
			if strings.TrimSpace(q.Interview) == "" || q.Line < 1 || utf8.RuneCountInString(normalizeQuote(q.Text)) < MinQuoteRunes {
				return false
			}
		}
	}
	for _, e := range s.Epics {
		if !modelKeyRe.MatchString(e.Key) || len(e.Needs) == 0 {
			return false
		}
	}
	return true
}
