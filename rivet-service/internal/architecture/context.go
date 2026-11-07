package architecture

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Лимиты сводки снимка для задания синтеза Аналитику (design
// add-product-discovery, решение 5). Раздел архитектуры в задании режется
// на 20 000 символов: описание системы и элементы C4 ограничены так, чтобы
// первые 10 000 символов HLD всегда оставались в задании.
const (
	ContextHLDRunes         = 10000
	ContextSystemRunes      = 500
	ContextElementsRunes    = 8000
	ContextElementDescRunes = 200
)

// ContextSummary — сводка корректного снимка архитектуры для задания
// синтеза Аналитику (спека product-discovery «Анализ интервью
// Аналитиком»): система, элементы C4 с видом, технологией и родителем,
// первые ContextHLDRunes символов HLD. Названия и описания из описания
// архитектуры идут одной строкой: это данные, а не разметка задания.
func ContextSummary(d SnapshotData) string {
	var b strings.Builder
	name := contextLine(d.System.Name)
	if name == "" {
		name = "без названия"
	}
	fmt.Fprintf(&b, "Система: %s", clipRunes(name, ContextSystemRunes))
	if desc := contextLine(d.System.Description); desc != "" {
		b.WriteString(". " + clipRunes(desc, ContextSystemRunes))
	}
	b.WriteString("\n")
	names := make(map[string]string, len(d.Elements))
	for _, e := range d.Elements {
		names[e.ID] = contextLine(e.Name)
	}
	if len(d.Elements) > 0 {
		b.WriteString("\nЭлементы C4:\n")
	}
	// Блок элементов не длиннее ContextElementsRunes: не вошедшие
	// называются числом, HLD ниже не вытесняется.
	used := 0
	for i, e := range d.Elements {
		var l strings.Builder
		fmt.Fprintf(&l, "- %s «%s»", e.Kind, clipRunes(contextLine(e.Name), ContextElementDescRunes))
		if t := contextLine(e.Technology); t != "" {
			l.WriteString(", технология: " + clipRunes(t, ContextElementDescRunes))
		}
		if e.External {
			l.WriteString(", внешний")
		}
		if p := names[e.Parent]; p != "" {
			l.WriteString(", внутри «" + clipRunes(p, ContextElementDescRunes) + "»")
		}
		if desc := contextLine(e.Description); desc != "" {
			l.WriteString(": " + clipRunes(desc, ContextElementDescRunes))
		}
		l.WriteString("\n")
		n := utf8.RuneCountInString(l.String())
		if used+n > ContextElementsRunes {
			fmt.Fprintf(&b, "(ещё %d элементов не показано)\n", len(d.Elements)-i)
			break
		}
		used += n
		b.WriteString(l.String())
	}
	if hld := strings.TrimSpace(d.HLD); hld != "" {
		cut := false
		if utf8.RuneCountInString(hld) > ContextHLDRunes {
			hld, cut = string([]rune(hld)[:ContextHLDRunes]), true
		}
		b.WriteString("\nHLD:\n" + hld + "\n")
		if cut {
			b.WriteString("(HLD обрезан)\n")
		}
	}
	return b.String()
}

// clipRunes — не больше n символов, обрезанное помечается многоточием.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// contextLine — текст одной строкой без лишних пробелов.
func contextLine(s string) string { return strings.Join(strings.Fields(s), " ") }
