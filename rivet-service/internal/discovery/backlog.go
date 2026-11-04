package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// FieldError — ошибка правки бэклога с полем вида `epics[2].depends_on`
// (api-contract: `400 invalid` с `field`).
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// ValidateBacklog проверяет бэклог с правками человека так же, как ответ
// Аналитика, кроме требования потребностей у добавленного человеком Epic'а
// (ключ `h<N>`). needs — идентификаторы принятых потребностей версии,
// existing — Epic'и проекта. Возвращает первую ошибку в порядке Epic'ов и
// полей, либо nil. Ацикличность проверяется внутри бэклога; граф вместе с
// зависимостями проекта проверяет store при утверждении (FindCycle).
func ValidateBacklog(epics []BacklogEpic, needs []string, existing []ExistingEpic) *FieldError {
	if len(epics) == 0 || len(epics) > MaxEpics {
		return &FieldError{Field: "epics", Msg: fmt.Sprintf("в бэклоге должно быть от 1 до %d Epic'ов", MaxEpics)}
	}
	knownNeed := map[string]bool{}
	for _, n := range needs {
		knownNeed[n] = true
	}
	existingBy := map[string]ExistingEpic{}
	for _, e := range existing {
		existingBy[e.ID] = e
	}
	// Нулевой символ не хранится в jsonb: отказ с полем, а не сбой записи.
	for i, e := range epics {
		fields := map[string][]string{"key": {e.Key}, "title": {e.Title}, "goal": {e.Goal}, "rationale": {e.Rationale},
			"priority": {e.Priority}, "size": {e.Size}, "needs": e.Needs, "depends_on": e.DependsOn}
		if e.ExistingEpic != nil {
			fields["existing_epic"] = []string{*e.ExistingEpic}
		}
		for _, f := range []string{"key", "title", "goal", "rationale", "priority", "size", "needs", "depends_on", "existing_epic"} {
			for _, v := range fields[f] {
				if strings.ContainsRune(v, 0) {
					return &FieldError{Field: fmt.Sprintf("epics[%d].%s", i, f), Msg: "текст содержит нулевые символы"}
				}
			}
		}
	}
	keys := map[string]bool{}
	for i, e := range epics {
		if !modelKeyRe.MatchString(e.Key) && !humanKeyRe.MatchString(e.Key) {
			return &FieldError{Field: fmt.Sprintf("epics[%d].key", i), Msg: "ключ должен быть вида e1 или h1"}
		}
		if keys[e.Key] {
			return &FieldError{Field: fmt.Sprintf("epics[%d].key", i), Msg: "ключ " + e.Key + " повторяется"}
		}
		keys[e.Key] = true
	}
	titles := map[string]int{}
	links := map[string]int{}
	for i, e := range epics {
		at := func(field string) string { return fmt.Sprintf("epics[%d].%s", i, field) }
		e.Title, e.Goal, e.Rationale = strings.TrimSpace(e.Title), strings.TrimSpace(e.Goal), strings.TrimSpace(e.Rationale)
		if reason := epicFieldReason(e); reason != "" {
			return &FieldError{Field: at(fieldOfReason(reason)), Msg: reason}
		}
		if j, dup := titles[NormalizeTitle(e.Title)]; dup {
			return &FieldError{Field: at("title"), Msg: fmt.Sprintf("название повторяет Epic %s", epics[j].Key)}
		}
		titles[NormalizeTitle(e.Title)] = i
		seenNeed := map[string]bool{}
		for _, n := range e.Needs {
			if !knownNeed[n] {
				return &FieldError{Field: at("needs"), Msg: "потребность " + n + " не найдена в версии исследования"}
			}
			if seenNeed[n] {
				return &FieldError{Field: at("needs"), Msg: "потребность " + n + " указана дважды"}
			}
			seenNeed[n] = true
		}
		if modelKeyRe.MatchString(e.Key) && len(e.Needs) == 0 {
			return &FieldError{Field: at("needs"), Msg: "у предложенного Аналитиком Epic'а должна остаться хотя бы одна потребность"}
		}
		seenDep := map[string]bool{}
		for _, d := range e.DependsOn {
			switch {
			case d == e.Key || (e.ExistingEpic != nil && d == *e.ExistingEpic):
				return &FieldError{Field: at("depends_on"), Msg: "Epic не может зависеть от себя"}
			case seenDep[d]:
				return &FieldError{Field: at("depends_on"), Msg: "зависимость " + d + " указана дважды"}
			case keys[d]:
			default:
				if ex, ok := existingBy[d]; !ok || ex.Status == StatusArchived {
					return &FieldError{Field: at("depends_on"), Msg: "зависимость " + d + " не найдена в бэклоге и среди Epic'ов проекта"}
				}
			}
			seenDep[d] = true
		}
		if e.ExistingEpic != nil {
			ex, ok := existingBy[*e.ExistingEpic]
			if !ok || ex.Status == StatusArchived {
				return &FieldError{Field: at("existing_epic"), Msg: ReasonUnknownEpic}
			}
			if j, dup := links[*e.ExistingEpic]; dup {
				return &FieldError{Field: at("existing_epic"), Msg: "Epic проекта уже связан с " + epics[j].Key}
			}
			links[*e.ExistingEpic] = i
		}
	}
	if cycle := FindCycle(epicGraph(epics)); cycle != nil {
		idx := 0
		for i, e := range epics {
			if e.Key == cycle[0] {
				idx = i
			}
		}
		return &FieldError{Field: fmt.Sprintf("epics[%d].depends_on", idx), Msg: "зависимости образуют цикл: " + strings.Join(cycle, " → ")}
	}
	return nil
}

// BacklogQuoteLeak — дословный фрагмент цитаты версии в названии, цели или
// обосновании строки бэклога (ошибка поля для владельца). Эти поля после
// утверждения уходят в задание Тимлиду, задачи и pull request'ы, поэтому
// строку правят до утверждения.
func BacklogQuoteLeak(epics []BacklogEpic, quotes []string) *FieldError {
	ix := NewQuoteIndex(quotes)
	for i, e := range epics {
		for _, f := range []struct{ field, text string }{{"title", e.Title}, {"goal", e.Goal}, {"rationale", e.Rationale}} {
			if frag, ok := ix.Leak(f.text); ok {
				return &FieldError{Field: fmt.Sprintf("epics[%d].%s", i, f.field),
					Msg: "перескажите своими словами: здесь дословный фрагмент цитаты интервью «" + frag + "»"}
			}
		}
	}
	return nil
}

// fieldOfReason — поле Epic'а, к которому относится причина epicFieldReason.
func fieldOfReason(reason string) string {
	switch {
	case strings.Contains(reason, "название"):
		return "title"
	case strings.Contains(reason, "цель"):
		return "goal"
	case strings.Contains(reason, "обоснование"):
		return "rationale"
	case strings.Contains(reason, "приоритет"):
		return "priority"
	default:
		return "size"
	}
}

// priorityRank — порядок внутри уровня топологической сортировки.
func priorityRank(p string) int {
	switch p {
	case PriorityMust:
		return 0
	case PriorityShould:
		return 1
	default:
		return 2
	}
}

// TopoOrder — порядок декомпозиции Epic'ов бэклога после утверждения:
// топологический по зависимостям внутри бэклога, внутри уровня по
// приоритету (must, should, could), затем по порядку в бэклоге.
// Зависимости на существующие Epic'и проекта порядок не меняют.
// Ошибка — цикл.
func TopoOrder(epics []BacklogEpic) ([]string, error) {
	g := epicGraph(epics)
	if cycle := FindCycle(g); cycle != nil {
		return nil, fmt.Errorf("зависимости образуют цикл: %s", strings.Join(cycle, " → "))
	}
	index := map[string]int{}
	for i, e := range epics {
		index[e.Key] = i
	}
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for _, e := range epics {
		indeg[e.Key] += 0
		for _, d := range g[e.Key] {
			indeg[e.Key]++
			dependents[d] = append(dependents[d], e.Key)
		}
	}
	less := func(a, b string) bool {
		pa, pb := priorityRank(epics[index[a]].Priority), priorityRank(epics[index[b]].Priority)
		if pa != pb {
			return pa < pb
		}
		return index[a] < index[b]
	}
	var ready []string
	for _, e := range epics {
		if indeg[e.Key] == 0 {
			ready = append(ready, e.Key)
		}
	}
	var order []string
	for len(ready) > 0 {
		sort.SliceStable(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		k := ready[0]
		ready = ready[1:]
		order = append(order, k)
		for _, dep := range dependents[k] {
			if indeg[dep]--; indeg[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	return order, nil
}
