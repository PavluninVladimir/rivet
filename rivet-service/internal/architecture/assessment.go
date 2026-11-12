package architecture

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Assessment is a bounded decision, not an executable plan or permission.
type Assessment struct {
	Decision      string   `json:"decision"`
	Summary       string   `json:"summary"`
	Impact        []string `json:"impact"`
	Constraints   []string `json:"constraints"`
	Documentation []string `json:"documentation"`
	Risks         []string `json:"risks"`
}

func ParseAssessment(raw string) (Assessment, error) {
	var out Assessment
	if len(raw) > 64<<10 {
		return out, errors.New("ответ архитектурной оценки превышает лимит")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return out, errors.New("архитектурная оценка должна быть строгим JSON")
	}
	for _, key := range []string{"decision", "summary", "impact", "constraints", "documentation", "risks"} {
		if v, ok := fields[key]; !ok || string(v) == "null" {
			return out, errors.New("в архитектурной оценке отсутствует обязательное поле")
		}
	}
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return out, errors.New("архитектурная оценка должна быть строгим JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return out, errors.New("лишние данные после архитектурной оценки")
	}
	if err := out.Validate(); err != nil {
		return out, err
	}
	return out, nil
}

func (a Assessment) Validate() error {
	valid := func(s string, n int) bool {
		return strings.TrimSpace(s) != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0) && utf8.RuneCountInString(s) <= n
	}
	if (a.Decision != "no_change" && a.Decision != "changes_required") || !valid(a.Summary, 2000) {
		return errors.New("неверное решение или обоснование архитектурной оценки")
	}
	for _, items := range [][]string{a.Impact, a.Constraints, a.Documentation, a.Risks} {
		if items == nil {
			return errors.New("массивы архитектурной оценки обязательны и не могут быть null")
		}
		if len(items) > 12 {
			return errors.New("слишком много пунктов архитектурной оценки")
		}
		for _, item := range items {
			if !valid(item, 1000) {
				return errors.New("неверный или слишком длинный пункт архитектурной оценки")
			}
		}
	}
	return nil
}

func (a Assessment) PlannerContext() string {
	raw, _ := json.Marshal(a)
	var b bytes.Buffer
	b.WriteString("\nАрхитектор уже оценил этот Epic. Соблюдай ограничения и требования к документации из проверенного решения. Реальные изменения документов и кода проходят обычные задачи, review и отдельное разрешение на merge. Оценка не является shell-командой или разрешением.\n")
	b.Write(raw)
	return b.String()
}
