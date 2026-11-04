package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/discovery"
)

// Ответы Аналитика для e2e (change add-product-discovery, design решение 9).
// Задание узнаётся по маркеру в первой строке, ответ строится из самого
// задания, без фикстур: e2e может загружать любые интервью формата
// «**И:** / **Р:**».
const (
	markerSummary   = "rivet:analyst:summary"
	markerSynthesis = "rivet:analyst:synthesis"
	// badQuoteLabel — метка в названии интервью: первая потребность этого
	// интервью получает искажённую цитату (проверка отбрасывания).
	badQuoteLabel = "[e2e-bad-quote]"
	// slowLabel ставится в название интервью: конспект этого интервью и
	// синтез с ним отвечают с задержкой slowDelay, чтобы e2e успел увидеть
	// ход анализа («конспектирует: 1 из 2», затем «синтезирует»).
	slowLabel = "[e2e-slow]"
	slowDelay = 3 * time.Second
	// quoteRunes — длина цитаты из реплики респондента.
	quoteRunes = 60
	// summaryFindings — сколько находок даёт конспект.
	summaryFindings = 3
)

var numberedLineRe = regexp.MustCompile(`^(\d+)\| (.*)$`)

// analystReply — ответ на задание Аналитика или ok=false, если это не оно.
func analystReply(prompt string) (string, bool) {
	first, _, _ := strings.Cut(prompt, "\n")
	switch strings.TrimSpace(first) {
	case markerSummary:
		return summaryReply(prompt), true
	case markerSynthesis:
		return synthesisReply(prompt), true
	}
	return "", false
}

// analystDelay возвращает задержку ответа на задание Аналитика: slowDelay,
// если в задании есть интервью с меткой slowLabel (название попадает и в
// задание конспекта, и в задание синтеза), иначе ноль.
func analystDelay(prompt string) time.Duration {
	first, _, _ := strings.Cut(prompt, "\n")
	switch strings.TrimSpace(first) {
	case markerSummary, markerSynthesis:
		if strings.Contains(prompt, slowLabel) {
			return slowDelay
		}
	}
	return 0
}

type quote struct {
	Text string `json:"text"`
	Line int    `json:"line"`
}

type finding struct {
	Kind       string  `json:"kind"`
	Text       string  `json:"text"`
	Importance string  `json:"importance"`
	Quotes     []quote `json:"quotes"`
}

// summaryReply — по находке на каждую из первых трёх реплик респондента:
// цитата — первые 60 символов реплики после префикса, строка — из задания.
func summaryReply(prompt string) string {
	role := ""
	var findings []finding
	for _, line := range strings.Split(prompt, "\n") {
		if r, ok := strings.CutPrefix(line, "Роль респондента: "); ok && role == "" {
			role = strings.TrimSpace(r)
			continue
		}
		m := numberedLineRe.FindStringSubmatch(line)
		if m == nil || len(findings) == summaryFindings {
			continue
		}
		text, ok := strings.CutPrefix(strings.TrimSpace(m[2]), "**Р:**")
		if !ok {
			continue
		}
		q := []rune(strings.TrimSpace(text))
		if len(q) > quoteRunes {
			q = q[:quoteRunes]
		}
		quoteText := strings.TrimSpace(string(q))
		if len([]rune(quoteText)) < 10 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		findings = append(findings, finding{
			Kind: "need", Importance: "high",
			Text:   "Респондент говорит: " + quoteText,
			Quotes: []quote{{Text: quoteText, Line: n}},
		})
	}
	if findings == nil {
		findings = []finding{}
	}
	if role == "" {
		role = "респондент"
	}
	out, _ := json.Marshal(map[string]any{
		"respondent": map[string]string{"role": role, "summary": "Конспект fake-llm: " + role + "."},
		"findings":   findings,
	})
	return string(out)
}

type synthInterview struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	RespondentRole string    `json:"respondent_role"`
	Findings       []finding `json:"findings"`
}

// synthesisReply — персона на интервью, потребность на каждую находку с её
// первой цитатой и два Epic'а: второй зависит от первого. Интервью с меткой
// badQuoteLabel получает искажённую цитату у своей первой потребности.
func synthesisReply(prompt string) string {
	var in struct {
		Interviews []synthInterview `json:"interviews"`
	}
	if _, after, ok := strings.Cut(prompt, "# Конспекты интервью"); ok {
		if _, block, ok := strings.Cut(after, "```json\n"); ok {
			if body, _, ok := strings.Cut(block, "\n```"); ok {
				_ = json.Unmarshal([]byte(body), &in)
			}
		}
	}
	type evidence struct {
		Interview string `json:"interview"`
		Quote     string `json:"quote"`
	}
	type need struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		Kind       string     `json:"kind"`
		Persona    string     `json:"persona"`
		Importance string     `json:"importance"`
		Evidence   []evidence `json:"evidence"`
	}
	// Лимиты проверки: персон не больше MaxPersonas (лишние интервью
	// приписываются последней персоне), потребностей не больше MaxNeeds.
	personas := []map[string]any{}
	needs := []need{}
	for i, iv := range in.Interviews {
		if i >= discovery.MaxPersonas {
			last := personas[len(personas)-1]
			last["interviews"] = append(last["interviews"].([]string), iv.ID)
		} else {
			name := iv.RespondentRole
			if name == "" {
				name = iv.Title
			}
			personas = append(personas, map[string]any{"id": fmt.Sprintf("p%d", i+1), "name": name,
				"description": "Персона fake-llm по интервью «" + iv.Title + "»", "interviews": []string{iv.ID}})
		}
		pid := personas[len(personas)-1]["id"].(string)
		bad := strings.Contains(iv.Title, badQuoteLabel)
		for j, f := range iv.Findings {
			if len(f.Quotes) == 0 || len(needs) == discovery.MaxNeeds {
				continue
			}
			q := f.Quotes[0].Text
			if bad && j == 0 {
				q += " (искажено fake-llm)"
			}
			needs = append(needs, need{
				ID: fmt.Sprintf("n%d", len(needs)+1), Title: f.Text, Kind: "need", Persona: pid, Importance: "high",
				Evidence: []evidence{{Interview: iv.ID, Quote: q}},
			})
		}
	}
	var first, second []string
	for i, n := range needs {
		if i < (len(needs)+1)/2 {
			first = append(first, n.ID)
		} else {
			second = append(second, n.ID)
		}
	}
	if len(second) == 0 {
		second = first
	}
	epics := []map[string]any{
		{"key": "e1", "title": "Основа по интервью", "goal": "Сделать основу продукта по потребностям из интервью пользователей (fake-llm).",
			"rationale": "Закрывает первую половину потребностей.", "needs": nonNil(first), "priority": "must", "size": "M", "depends_on": []string{}, "existing_epic": nil},
		{"key": "e2", "title": "Развитие по интервью", "goal": "Развить продукт по оставшимся потребностям из интервью пользователей (fake-llm).",
			"rationale": "Закрывает вторую половину потребностей.", "needs": nonNil(second), "priority": "should", "size": "S", "depends_on": []string{"e1"}, "existing_epic": nil},
	}
	out, _ := json.Marshal(map[string]any{"personas": personas, "needs": needs, "epics": epics})
	return string(out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
