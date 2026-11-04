package discovery

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "перезаписать golden-файлы заданий")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("нет golden-файла %s (go test -update): %v", path, err)
	}
	if string(want) != got {
		t.Fatalf("задание %s расходится с golden-файлом (go test -update, затем просмотреть дифф)", name)
	}
}

var vera = Analyst{
	Title: "Аналитик", Name: "Вера",
	Instruction: "Ты бизнес-аналитик. Разбираешь интервью пользователей и ничего не выдумываешь.",
	Extra:       "Пиши коротко.",
}

func TestSummaryPromptGolden(t *testing.T) {
	text := loadInterview(t, "manager.md")
	chunks := SplitInterview(text, SummaryChunkChars, ChunkOverlapLines)
	if len(chunks) != 1 {
		t.Fatalf("демо-интервью короче порога, частей %d", len(chunks))
	}
	p := SummaryPrompt(SummaryTask{Analyst: vera, InterviewID: "iv-manager", Title: "Менеджер\nпо продажам",
		RespondentRole: "Менеджер B2B", Chunk: chunks[0], Part: 1, Parts: 1})
	if !strings.HasPrefix(p, MarkerSummary+"\n") || !strings.Contains(p, "Название: Менеджер по продажам\n") {
		t.Fatal("маркер в первой строке, название в одну строку")
	}
	if !strings.Contains(p, "17| **Р:** Постоянно.") {
		t.Fatal("текст интервью пронумерован исходными строками")
	}
	golden(t, "summary_prompt.txt", p)

	// Повтор: часть длинного интервью и дефекты.
	retry := SummaryPrompt(SummaryTask{Analyst: Analyst{}, InterviewID: "iv-x", Title: "Длинное", Chunk: Chunk{FirstLine: 41, LastLine: 42, Lines: []string{"**Р:** раз", "**Р:** два"}},
		Part: 2, Parts: 3, Defects: []string{"цитата «x» не найдена"}})
	golden(t, "summary_prompt_retry.txt", retry)
}

func TestSynthesisPromptGolden(t *testing.T) {
	ivs := demoInterviews(t)
	p, removed := SynthesisPrompt(SynthesisTask{
		Analyst:    vera,
		Interviews: ivs,
		Context: ProductContext{
			README:       "# CRM\nУчёт контактов: список, поиск, добавление.",
			Architecture: "Система CRM: веб-клиент (ES-модули), API-сервер (node:http), хранилище (JSON-файл).",
			Epics:        []ExistingEpic{{ID: "ep-1", Title: "Контакты", Goal: "Список контактов\nс поиском", Status: "done", Needs: []string{"б", "а"}}},
		},
	})
	if removed != 0 || !strings.HasPrefix(p, MarkerSynthesis+"\n") {
		t.Fatalf("маркер и отсутствие сокращения: removed=%d", removed)
	}
	if !strings.Contains(p, "- ep-1 · Контакты · done: Список контактов с поиском (потребности: а; б)") {
		t.Fatal("Epic'и проекта одной строкой, потребности по алфавиту")
	}
	golden(t, "synthesis_prompt.txt", p)

	empty, _ := SynthesisPrompt(SynthesisTask{Interviews: ivs[:1], Defects: []string{"цикл: e1 → e2 → e1"}})
	for _, want := range []string{"README нет.", "Корректного описания архитектуры нет.", "Epic'ов пока нет.", "# Дефекты прошлого ответа", "- цикл: e1 → e2 → e1"} {
		if !strings.Contains(empty, want) {
			t.Fatalf("в задании без контекста нет «%s»", want)
		}
	}
}

func TestSynthesisInputTrim(t *testing.T) {
	mk := func(imp string, n int) []Finding {
		var fs []Finding
		for i := 0; i < n; i++ {
			fs = append(fs, Finding{Kind: KindNeed, Text: strings.Repeat("т", 200), Importance: imp, Quotes: []Quote{{Text: "цитата", Line: 1}}})
		}
		return fs
	}
	ivs := []InterviewInput{
		{ID: "a", Summary: Summary{Findings: append(append(mk(ImportanceHigh, 5), mk(ImportanceMedium, 5)...), mk(ImportanceLow, 5)...)}},
		{ID: "b", Summary: Summary{Findings: append(mk(ImportanceHigh, 5), mk(ImportanceLow, 5)...)}},
	}
	full, removed := SynthesisInput(ivs, 0)
	if removed != 0 {
		t.Fatal("без порога ничего не убирается")
	}
	size := utf8.RuneCountInString(full)

	// Порог чуть ниже полного размера: уходит одна находка low, с конца
	// самого длинного конспекта (a), остальное на месте.
	s, removed := SynthesisInput(ivs, size-100)
	if removed != 1 || strings.Count(s, `"low"`) != 9 || strings.Count(s, `"medium"`) != 5 {
		t.Fatalf("ждали удаление одной находки low: removed=%d", removed)
	}
	// Порог ниже, чем без всех low: medium уходят по одной, пока не уложится.
	noLow, _ := SynthesisInput([]InterviewInput{
		{ID: "a", Summary: Summary{Findings: append(mk(ImportanceHigh, 5), mk(ImportanceMedium, 5)...)}},
		{ID: "b", Summary: Summary{Findings: mk(ImportanceHigh, 5)}},
	}, 0)
	limit := utf8.RuneCountInString(noLow) - 100
	s, removed = SynthesisInput(ivs, limit)
	if utf8.RuneCountInString(s) > limit || strings.Contains(s, `"low"`) || strings.Count(s, `"medium"`) != 4 || removed != 11 {
		t.Fatalf("размер %d при пороге %d, removed=%d, medium=%d", utf8.RuneCountInString(s), limit, removed, strings.Count(s, `"medium"`))
	}
	// Совсем маленький порог: уходят и high, но вход укладывается.
	s, _ = SynthesisInput(ivs, size/4)
	if utf8.RuneCountInString(s) > size/4 || strings.Contains(s, `"medium"`) {
		t.Fatalf("размер %d при пороге %d", utf8.RuneCountInString(s), size/4)
	}
	// Входные конспекты не меняются.
	if len(ivs[0].Summary.Findings) != 15 {
		t.Fatal("SynthesisInput не должен менять вход")
	}
}

func TestInstructionSHA(t *testing.T) {
	a := vera
	b := vera
	b.Name = "Ольга"
	if a.InstructionSHA() != b.InstructionSHA() {
		t.Fatal("имя агента не влияет на кэш конспекта")
	}
	b.Extra = "Пиши подробно."
	if a.InstructionSHA() == b.InstructionSHA() {
		t.Fatal("дополнение агента влияет на кэш конспекта")
	}
	k1 := SummaryCacheKey(TextSHA256("текст"), "model-a", a.InstructionSHA())
	k2 := SummaryCacheKey(TextSHA256("текст"), "model-b", a.InstructionSHA())
	if k1 == k2 || len(k1) != 64 {
		t.Fatal("модель входит в ключ кэша")
	}
}
