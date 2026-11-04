package discovery

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalizeText(t *testing.T) {
	// «й» в NFD — «и» + комбинирующая кратка.
	in := "Строка\r\nвторая\rтретья й\n"
	got := NormalizeText(in)
	want := "Строка\nвторая\nтретья й\n"
	if got != want {
		t.Fatalf("NormalizeText: %q, ждали %q", got, want)
	}
	if n := CountLines(got); n != 3 {
		t.Fatalf("строк %d, ждали 3", n)
	}
	if CountLines("") != 0 {
		t.Fatal("пустой текст без строк")
	}
}

func TestFindQuote(t *testing.T) {
	text := NormalizeText(strings.Join([]string{
		"**И:** Как вы ведёте клиентов?",                          // 1
		"**Р:** В Excel. Половина обещаний у меня в голове,",      // 2
		"половина в ежедневнике.",                                 // 3
		"**Р:** Отчёт собираю минут сорок по пятницам.",           // 4
		"**Р:** Половина обещаний у меня в голове, как и у всех.", // 5
	}, "\n"))
	cases := []struct {
		name     string
		quote    string
		hint     int
		ok       bool
		line     int
		fragment string
	}{
		{name: "точная цитата", quote: "В Excel. Половина обещаний", hint: 2, ok: true, line: 2, fragment: "В Excel. Половина обещаний"},
		{name: "через перевод строки", quote: "у меня в голове, половина в ежедневнике", ok: true, line: 2,
			fragment: "у меня в голове,\nполовина в ежедневнике"},
		{name: "неразрывный пробел в тексте", quote: "Отчёт собираю минут сорок", ok: true, line: 4, fragment: "Отчёт собираю минут сорок"},
		{name: "лишние пробелы в цитате", quote: "  Отчёт   собираю  минут сорок ", ok: true, line: 4, fragment: "Отчёт собираю минут сорок"},
		{name: "регистр важен", quote: "в excel. половина обещаний", ok: false},
		{name: "пунктуация важна", quote: "В Excel Половина обещаний", ok: false},
		{name: "пересказ не цитата", quote: "Он ведёт клиентов в Excel", ok: false},
		{name: "короткая цитата", quote: "В Excel", ok: false},
		{name: "ближе к подсказке", quote: "Половина обещаний у меня в голове", hint: 5, ok: true, line: 5, fragment: "Половина обещаний у меня в голове"},
		{name: "без подсказки первое", quote: "Половина обещаний у меня в голове", ok: true, line: 2, fragment: "Половина обещаний у меня в голове"},
		{name: "неверная строка исправляется", quote: "минут сорок по пятницам", hint: 1, ok: true, line: 4, fragment: "минут сорок по пятницам"},
		{name: "NFD в цитате", quote: "Отчёт собираю минут", ok: true, line: 4, fragment: "Отчёт собираю минут"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loc, ok := FindQuote(text, c.quote, c.hint)
			if ok != c.ok {
				t.Fatalf("найдено=%v, ждали %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if loc.Line != c.line || loc.Text != c.fragment {
				t.Fatalf("строка %d «%s», ждали %d «%s»", loc.Line, loc.Text, c.line, c.fragment)
			}
			if text[loc.Start:loc.End] != loc.Text {
				t.Fatal("границы не совпадают с фрагментом")
			}
		})
	}
}

func TestNormalizeTitle(t *testing.T) {
	if NormalizeTitle("  Сделки  и Воронка ЁЖИКОВ ") != NormalizeTitle("сделки и воронка ежиков") {
		t.Fatal("названия должны совпасть после нормализации")
	}
	if NormalizeTitle("Сделки") == NormalizeTitle("Сделка") {
		t.Fatal("разные названия не должны совпасть")
	}
}

func TestSplitInterview(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("ж", 99)) // 100 рун со строкой
	}
	text := strings.Join(lines, "\n")

	if got := SplitInterview(text, 1_000_000, 20); len(got) != 1 || got[0].FirstLine != 1 || got[0].LastLine != 100 {
		t.Fatalf("короткое интервью — одна часть: %+v", got)
	}

	// 30 строк на часть, перекрытие ограничено десятой долей порога: 3 строки.
	chunks := SplitInterview(text, 3000, 20)
	checkChunks(t, chunks, 100, 3000)
	for i := 1; i < len(chunks); i++ {
		if ov := chunks[i-1].LastLine - chunks[i].FirstLine + 1; ov != 3 {
			t.Fatalf("перекрытие частей %d и %d: %d строк", i-1, i, ov)
		}
	}
	if !strings.HasPrefix(chunks[1].Numbered(), "28| ") {
		t.Fatalf("нумерация части идёт от исходной строки: %q", chunks[1].Numbered()[:8])
	}

	// Длинные абзацы: 50 строк по 4000 символов — 4 части, а не десятки.
	var paras []string
	for i := 0; i < 50; i++ {
		paras = append(paras, strings.Repeat("ж", 3999))
	}
	big := SplitInterview(strings.Join(paras, "\n"), SummaryChunkChars, ChunkOverlapLines)
	checkChunks(t, big, 50, SummaryChunkChars)
	if len(big) != 4 {
		t.Fatalf("50 абзацев по 4000 символов: %d частей", len(big))
	}

	// Огромная строка после коротких: части не вкладываются друг в друга.
	var mixed []string
	for i := 0; i < 10; i++ {
		mixed = append(mixed, strings.Repeat("а", 999))
	}
	mixed = append(mixed, strings.Repeat("б", 59000))
	got := SplitInterview(strings.Join(mixed, "\n"), SummaryChunkChars, ChunkOverlapLines)
	checkChunks(t, got, 11, SummaryChunkChars)
	if len(got) != 2 || got[0].LastLine != 10 || got[1].FirstLine != 11 {
		t.Fatalf("смешанный текст: %+v", spans(got))
	}
}

func TestSplitLongLine(t *testing.T) {
	// Одна строка на 200 000 символов из предложений: куски по концам
	// предложений с перекрытием в символах.
	sentence := "Мы ведём клиентов в таблице и теряем звонки каждую неделю. "
	line := strings.Repeat(sentence, 200000/len([]rune(sentence))+1)
	chunks := SplitInterview(line, SummaryChunkChars, ChunkOverlapLines)
	if len(chunks) < 4 || len(chunks) > 5 {
		t.Fatalf("кусков %d", len(chunks))
	}
	for i, c := range chunks {
		if c.FirstLine != 1 || c.LastLine != 1 || len(c.Lines) != 1 {
			t.Fatalf("кусок %d: %+v", i, spans([]Chunk{c}))
		}
		piece := c.Lines[0]
		if n := len([]rune(piece)); n > SummaryChunkChars {
			t.Fatalf("кусок %d длиннее порога: %d", i, n)
		}
		if i < len(chunks)-1 && !strings.HasSuffix(strings.TrimSpace(piece), ".") {
			t.Fatalf("кусок %d режется не по концу предложения: …%q", i, piece[len(piece)-20:])
		}
		if i > 0 && !strings.HasPrefix(piece, "Мы ведём") {
			t.Fatalf("кусок %d начинается с обрывка: %q", i, piece[:20])
		}
		if !strings.HasPrefix(c.Numbered(), "1| ") {
			t.Fatal("кусок нумеруется своей строкой")
		}
	}
	// Без пробелов режется по порогу и всё равно продвигается.
	solid := SplitInterview(strings.Repeat("ы", 200000), SummaryChunkChars, ChunkOverlapLines)
	if len(solid) < 4 || len(solid) > 5 {
		t.Fatalf("строка без пробелов: %d кусков", len(solid))
	}
}

// checkChunks — инварианты нарезки: части в пределах порога, покрывают
// все строки, каждая следующая заканчивается дальше предыдущей.
func checkChunks(t *testing.T, chunks []Chunk, lines, max int) {
	t.Helper()
	covered := map[int]bool{}
	for i, c := range chunks {
		size := 0
		for _, l := range c.Lines {
			size += len([]rune(l)) + 1
		}
		if size > max+1 {
			t.Fatalf("часть %d длиннее порога: %d", i, size)
		}
		if c.FirstLine != c.LastLine && c.LastLine-c.FirstLine+1 != len(c.Lines) {
			t.Fatalf("часть %d: границы не совпадают со строками", i)
		}
		if i > 0 && c.LastLine <= chunks[i-1].LastLine && c.FirstLine != c.LastLine {
			t.Fatalf("часть %d не продвигается: %v", i, spans(chunks))
		}
		for l := c.FirstLine; l <= c.LastLine; l++ {
			covered[l] = true
		}
	}
	if len(covered) != lines {
		t.Fatalf("покрыто %d строк из %d", len(covered), lines)
	}
}

func spans(chunks []Chunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, fmt.Sprintf("%d-%d", c.FirstLine, c.LastLine))
	}
	return out
}

func TestInvisibleCharacters(t *testing.T) {
	bom := string(rune(0xFEFF))
	text := NormalizeText(bom + "**Р:** Это минут" + string(rune(0xAD)) + " сорок по пят" + string(rune(0x200B)) + "ницам.\n")
	if strings.HasPrefix(text, bom) {
		t.Fatal("BOM в начале убирается при нормализации")
	}
	loc, ok := FindQuote(text, "Это минут сорок по пятницам", 1)
	if !ok || loc.Line != 1 || loc.Text != "Это минут"+string(rune(0xAD))+" сорок по пят"+string(rune(0x200B))+"ницам" {
		t.Fatalf("невидимые символы: %v %q", ok, loc.Text)
	}
}

func TestFindQuoteManyOccurrences(t *testing.T) {
	var b strings.Builder
	for b.Len() < 390000 {
		b.WriteString("ага ага ага\n")
	}
	text := b.String()
	start := time.Now()
	loc, ok := FindQuote(text, "ага ага ага", 15000)
	if !ok || loc.Line != 15000 {
		t.Fatalf("ближайшее к подсказке вхождение: %v %d", ok, loc.Line)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("поиск среди тысяч вхождений слишком долгий: %v", el)
	}
}
