package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Виды находок конспекта и важность (design, решение 2).
const (
	KindPain    = "pain"
	KindNeed    = "need"
	KindWish    = "wish"
	KindContext = "context"

	ImportanceHigh   = "high"
	ImportanceMedium = "medium"
	ImportanceLow    = "low"
)

// Quote — цитата в ответе модели: текст и номер строки (подсказка).
// После проверки Text — точный фрагмент исходного текста, Line — строка
// его начала, вычисленная системой.
type Quote struct {
	Text string `json:"text"`
	Line int    `json:"line"`
}

// Respondent — кто отвечал, по мнению Аналитика.
type Respondent struct {
	Role    string `json:"role"`
	Summary string `json:"summary"`
}

// Finding — находка конспекта: боль, потребность, пожелание или контекст.
type Finding struct {
	Kind       string  `json:"kind"`
	Text       string  `json:"text"`
	Importance string  `json:"importance"`
	Quotes     []Quote `json:"quotes"`
}

// Summary — ответ первого этапа: конспект одного интервью или его части.
type Summary struct {
	Respondent Respondent `json:"respondent"`
	Findings   []Finding  `json:"findings"`
}

// Dropped — отброшенное при проверке с причиной (раздел `dropped` версии
// исследования). Kind: quote, finding, persona, need, epic, dependency.
type Dropped struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// Причины отбрасывания: человек видит их в разделе отброшенного.
const (
	ReasonQuoteNotFound     = "цитата не найдена в интервью"
	ReasonQuoteNotInReview  = "цитата не найдена в отзыве"
	ReasonFeedbackNoFinding = "в отзыве нет подтверждённых находок"
	ReasonFeedbackUnparsed  = "ответ модели на отзыв не разобран"
	ReasonQuoteTooShort     = "цитата короче 10 символов"
	ReasonQuoteDuplicate    = "цитата повторяется"
	ReasonOverlapDuplicate  = "находка повторяет соседнюю часть интервью"
	ReasonSelfLinkedDep     = "зависимость на Epic проекта, с которым связано само предложение"
	ReasonDoubleLink        = "Epic проекта уже связан с другим предложением"
	ReasonQuoteLimit        = "больше 5 цитат на потребность"
	ReasonNoQuotes          = "нет подтверждённых цитат"
	ReasonUnknownInterview  = "интервью не найдено"
	ReasonUnknownPersona    = "персона не найдена"
	ReasonUnknownNeed       = "потребность не найдена или отброшена"
	ReasonNoNeeds           = "нет принятых потребностей"
	ReasonUnknownDependency = "зависимость на неизвестный или отброшенный Epic"
	ReasonUnknownEpic       = "Epic проекта не найден или в архиве"
	ReasonDuplicateTitle    = "повтор названия Epic'а"
	ReasonBadKind           = "неизвестный вид"
	ReasonBadImportance     = "неизвестная важность"
	ReasonEmptyText         = "пустой текст"
)

// ErrNoJSON — в ответе модели нет JSON-объекта.
var ErrNoJSON = errors.New("в ответе нет JSON-объекта")

// maxJSONStarts — сколько открывающих скобок пробовать как начало ответа.
// Преамбула модели короткая, перебор всего текста не нужен.
const maxJSONStarts = 64

// ErrTruncated — ответ модели оборван посреди JSON (обычно лимит токенов):
// принимать нечего, нужен повтор.
var ErrTruncated = errors.New("ответ оборван: JSON не закончен, вероятно, кончился лимит токенов")

// extractJSON достаёт из ответа модели объект верхнего уровня с ключом key.
// Модели обрамляют ответ пояснениями и ограждениями кода, в преамбуле
// бывают фигурные скобки и примеры формата, а в цитатах — «```».
//   - Кандидат — каждая «{» по порядку. Разбор декодером учитывает строки
//     JSON, поэтому ограждение внутри цитаты объект не обрезает, а хвост
//     после объекта игнорируется.
//   - Разобранный объект без ключа key (пример в преамбуле) пропускается
//     целиком: скобки внутри него не кандидаты. Иначе вложенный объект
//     ответа выдал бы себя за весь ответ.
//   - Кандидат, упёршийся в конец текста, — оборванный ответ: всё, что
//     дальше, вложено в него, перебирать нечего.
func extractJSON(raw, key string) ([]byte, error) {
	s := raw
	for tries, from := 0, 0; tries < maxJSONStarts; tries++ {
		i := strings.IndexByte(s[from:], '{')
		if i < 0 {
			break
		}
		start := from + i
		dec := json.NewDecoder(strings.NewReader(s[start:]))
		var obj map[string]json.RawMessage
		err := dec.Decode(&obj)
		switch {
		case errors.Is(err, io.ErrUnexpectedEOF):
			return nil, ErrTruncated
		case err != nil:
			from = start + 1
			continue
		}
		end := start + int(dec.InputOffset())
		if _, ok := obj[key]; ok {
			return []byte(s[start:end]), nil
		}
		from = end
	}
	return nil, fmt.Errorf("%w с полем %q", ErrNoJSON, key)
}

// decodeAnswer разбирает ответ модели. Неизвестные поля не валят разбор:
// модель добавила «confidence» — данные целы. Но опечатку в имени поля
// человек должен видеть, поэтому строгий второй проход возвращает
// предупреждение (warning), которое идёт в дефекты без повтора.
func decodeAnswer[T any](raw, key string) (v T, warning string, err error) {
	b, err := extractJSON(raw, key)
	if err != nil {
		return v, "", err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, "", fmt.Errorf("ответ не разбирается: %w", err)
	}
	var probe T
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&probe); err != nil {
		return v, "предупреждение: в ответе есть поля вне формата (" + err.Error() + ")", nil
	}
	return v, "", nil
}

// SummaryCheck — итог проверки ответа первого этапа.
type SummaryCheck struct {
	// Summary — принятая часть: цитаты заменены точными фрагментами текста
	// и номерами строк, находки без цитат убраны.
	Summary Summary
	Dropped []Dropped
	// Defects — перечень для повторного задания и для `failed`.
	Defects []string
	// Retry — повторить запрос: ответ не разобрался или отброшено больше
	// половины находок.
	Retry bool
	// Usable — принятую часть можно использовать (ответ разобрался и в нём
	// осталась хотя бы одна находка, либо модель честно вернула пустой
	// конспект).
	Usable bool
	// Parsed — ответ разобрался как JSON конспекта. Неразобранный ответ
	// (сбой провайдера, текст вместо JSON) не говорит о самом материале.
	Parsed bool
}

// CheckSummary разбирает и проверяет ответ первого этапа по тексту
// интервью (нормализованному, целиком: цитата из части ищется во всём
// интервью, номер строки от модели — подсказка). ref — подпись интервью в
// разделе отброшенного.
func CheckSummary(ref, text, raw string) SummaryCheck {
	return CheckMaterialSummary(ref, MaterialInterview, text, raw)
}

// CheckMaterialSummary — CheckSummary для материала вида kind: у отзыва
// причины и дефекты называют отзыв, а не интервью. Дефект повтора
// различает короткую цитату и цитату, которой нет в тексте: модели нужен
// разный совет.
func CheckMaterialSummary(ref, kind, text, raw string) SummaryCheck {
	s, warning, err := decodeAnswer[Summary](raw, "findings")
	if err != nil {
		return SummaryCheck{Defects: []string{err.Error()}, Retry: true}
	}
	out := SummaryCheck{Parsed: true}
	if warning != "" {
		out.Defects = append(out.Defects, ref+": "+warning)
	}
	out.Summary.Respondent = Respondent{Role: strings.TrimSpace(s.Respondent.Role), Summary: strings.TrimSpace(s.Respondent.Summary)}
	for i, f := range s.Findings {
		fref := fmt.Sprintf("%s: находка %d", ref, i+1)
		f.Text = strings.TrimSpace(f.Text)
		switch {
		case f.Text == "":
			out.Dropped = append(out.Dropped, Dropped{Kind: "finding", Ref: fref, Reason: ReasonEmptyText})
			continue
		case !validFindingKind(f.Kind):
			out.Dropped = append(out.Dropped, Dropped{Kind: "finding", Ref: fref, Reason: ReasonBadKind + ": " + f.Kind})
			continue
		case !validImportance(f.Importance):
			out.Dropped = append(out.Dropped, Dropped{Kind: "finding", Ref: fref, Reason: ReasonBadImportance + ": " + f.Importance})
			continue
		}
		var locs []Located
		for _, q := range f.Quotes {
			loc, reason, ok := locate(text, q.Text, q.Line)
			if !ok {
				reason = materialReason(reason, kind)
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: fref + ": «" + short(q.Text) + "»", Reason: reason})
				out.Defects = append(out.Defects, quoteDefect(fref, q.Text, reason, kind))
				continue
			}
			var dup *Located
			locs, dup = addQuote(locs, loc)
			if dup != nil {
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: fref + ": «" + short(dup.Text) + "»", Reason: ReasonQuoteDuplicate})
			}
		}
		var quotes []Quote
		for _, l := range locs {
			quotes = append(quotes, Quote{Text: l.Text, Line: l.Line})
		}
		if len(quotes) == 0 {
			out.Dropped = append(out.Dropped, Dropped{Kind: "finding", Ref: fref + ": " + short(f.Text), Reason: ReasonNoQuotes})
			continue
		}
		f.Quotes = quotes
		out.Summary.Findings = append(out.Summary.Findings, f)
	}
	dropped := len(s.Findings) - len(out.Summary.Findings)
	out.Retry = len(s.Findings) > 0 && dropped*2 > len(s.Findings)
	// Пустой конспект от модели честен; конспект, где всё отброшено, — нет.
	out.Usable = len(s.Findings) == 0 || len(out.Summary.Findings) > 0
	if out.Retry {
		out.Defects = append(out.Defects, fmt.Sprintf("%s: отброшено %d из %d находок", ref, dropped, len(s.Findings)))
	}
	return out
}

// materialReason — причина отказа цитаты с названием материала.
func materialReason(reason, kind string) string {
	if kind == MaterialFeedback && reason == ReasonQuoteNotFound {
		return ReasonQuoteNotInReview
	}
	return reason
}

// quoteDefect — дефект цитаты для повторного задания по причине отказа.
func quoteDefect(ref, quote, reason, kind string) string {
	where := "текста интервью"
	if kind == MaterialFeedback {
		where = "текста отзыва"
	}
	if reason == ReasonQuoteTooShort {
		return fmt.Sprintf("%s: цитата «%s» короче %d символов: возьми более длинный дословный фрагмент %s или не приводи находку", ref, short(quote), MinQuoteRunes, where)
	}
	return fmt.Sprintf("%s: цитата «%s» не найдена дословно: копируй фрагмент %s символ в символ", ref, short(quote), where)
}

// addQuote добавляет найденную цитату. Цитаты с одним началом в тексте —
// повтор: остаётся более длинная (она подтверждает больше), вторая
// возвращается как отброшенная.
func addQuote(locs []Located, loc Located) ([]Located, *Located) {
	for i, l := range locs {
		if l.Start != loc.Start {
			continue
		}
		if loc.End > l.End {
			locs[i] = loc
			return locs, &l
		}
		return locs, &loc
	}
	return append(locs, loc), nil
}

// locate ищет цитату и называет причину отказа.
func locate(text, quote string, hint int) (Located, string, bool) {
	if len([]rune(normalizeQuote(quote))) < MinQuoteRunes {
		return Located{}, ReasonQuoteTooShort, false
	}
	loc, ok := FindQuote(text, quote, hint)
	if !ok {
		return Located{}, ReasonQuoteNotFound, false
	}
	return loc, "", true
}

func validFindingKind(k string) bool {
	switch k {
	case KindPain, KindNeed, KindWish, KindContext:
		return true
	}
	return false
}

func validImportance(v string) bool {
	switch v {
	case ImportanceHigh, ImportanceMedium, ImportanceLow:
		return true
	}
	return false
}

// short — начало длинного текста для подписи в отброшенном и дефектах.
func short(s string) string {
	t, _ := truncateRunes(strings.TrimSpace(s), 80)
	return t
}

// MergeSummaries склеивает проверенные конспекты частей одного интервью:
// роль — первая непустая, резюме частей через пробел, находки по порядку
// частей. Повторы из зоны перекрытия схлопываются только между частями:
// цитата (строка и нормализованный текст), уже встреченная в предыдущей
// части, отбрасывается, находка без оставшихся цитат — тоже. Внутри одной
// части несколько находок могут опираться на одну цитату. Отброшенное
// возвращается с причиной; ref — подпись интервью.
func MergeSummaries(ref string, parts []Summary) (Summary, []Dropped) {
	var out Summary
	var dropped []Dropped
	var summaries []string
	earlier := map[string]bool{}
	for pi, p := range parts {
		if out.Respondent.Role == "" {
			out.Respondent.Role = p.Respondent.Role
		}
		if s := strings.TrimSpace(p.Respondent.Summary); s != "" {
			summaries = append(summaries, s)
		}
		current := map[string]bool{}
		for fi, f := range p.Findings {
			fref := fmt.Sprintf("%s: часть %d, находка %d", ref, pi+1, fi+1)
			var quotes []Quote
			for _, q := range f.Quotes {
				key := fmt.Sprintf("%d\x00%s", q.Line, normalizeQuote(q.Text))
				if earlier[key] {
					dropped = append(dropped, Dropped{Kind: "quote", Ref: fref + ": «" + short(q.Text) + "»", Reason: ReasonQuoteDuplicate})
					continue
				}
				current[key] = true
				quotes = append(quotes, q)
			}
			if len(quotes) == 0 {
				dropped = append(dropped, Dropped{Kind: "finding", Ref: fref + ": " + short(f.Text), Reason: ReasonOverlapDuplicate})
				continue
			}
			f.Quotes = quotes
			out.Findings = append(out.Findings, f)
		}
		for k := range current {
			earlier[k] = true
		}
	}
	out.Respondent.Summary = strings.Join(summaries, " ")
	return out, dropped
}
