package architecture

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Сводка снимка для Аналитика: система, элементы C4 одной строкой, HLD
// обрезается по символам, а не по байтам.
func TestContextSummary(t *testing.T) {
	d := SnapshotData{
		System: SystemInfo{Name: "CRM", Description: "учёт\nконтактов"},
		Elements: []C4Element{
			{ID: "crm", Kind: "system", Name: "CRM"},
			{ID: "db", Kind: "container", Name: "Хранилище\nконтактов", Technology: "JSON-файл", Parent: "crm"},
			{ID: "pay", Kind: "system", Name: "Платежи", External: true, Parent: "missing"},
		},
		HLD: strings.Repeat("я", ContextHLDRunes+5),
	}
	got := ContextSummary(d)
	for _, want := range []string{
		"Система: CRM. учёт контактов\n",
		"- container «Хранилище контактов», технология: JSON-файл, внутри «CRM»\n",
		"- system «Платежи», внешний\n",
		"(HLD обрезан)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет %q в сводке:\n%s", want, got)
		}
	}
	_, hld, _ := strings.Cut(got, "HLD:\n")
	hld, _, _ = strings.Cut(hld, "\n")
	if n := utf8.RuneCountInString(hld); n != ContextHLDRunes {
		t.Fatalf("HLD в сводке: %d символов", n)
	}

	empty := ContextSummary(SnapshotData{})
	if empty != "Система: без названия\n" {
		t.Fatalf("пустой снимок: %q", empty)
	}
}

// Длинный список элементов не вытесняет HLD: сводка укладывается в лимит
// раздела архитектуры задания синтеза (20 000 символов) вместе с первыми
// 10 000 символами HLD.
func TestContextSummaryManyElements(t *testing.T) {
	d := SnapshotData{System: SystemInfo{Name: "CRM", Description: strings.Repeat("о", 2000)}, HLD: strings.Repeat("h", 15000)}
	for i := 0; i < 300; i++ {
		d.Elements = append(d.Elements, C4Element{ID: fmt.Sprintf("e%d", i), Kind: "component", Name: fmt.Sprintf("Компонент %d", i),
			Technology: "Go", Description: strings.Repeat("д", 400)})
	}
	got := ContextSummary(d)
	if n := utf8.RuneCountInString(got); n > 20000 {
		t.Fatalf("сводка длиннее раздела задания: %d", n)
	}
	if !strings.Contains(got, "\nHLD:\n"+strings.Repeat("h", ContextHLDRunes)+"\n(HLD обрезан)") {
		t.Fatal("HLD вытеснен элементами")
	}
	if !strings.Contains(got, "элементов не показано)") || !strings.Contains(got, "- component «Компонент 0»") {
		t.Fatalf("блок элементов без пометки об остатке:\n%.400s", got)
	}
}
