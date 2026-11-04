package discovery

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Регрессии по ревью: оборванные ответы, примеры формата в преамбуле,
// скорость сокращения входа, фаззинг поиска цитат, нарезки и инварианта
// синтеза.

func TestTruncatedAnswers(t *testing.T) {
	text := NormalizeText("**Р:** Это минут сорок по пятницам, честно.\n")
	cut := `{"respondent":{"role":"менеджер","summary":"ведёт клиентов"},"findings":[{"kind":"pain","text":"отчёт","importance":"high","quotes":[{"text":"Это минут сорок по пя`
	for name, raw := range map[string]string{"без ограждения": cut, "в ограждении": "```json\n" + cut} {
		c := CheckSummary("iv", text, raw)
		if !c.Retry || c.Usable || len(c.Defects) != 1 || !strings.Contains(c.Defects[0], "оборван") {
			t.Fatalf("%s: оборванный конспект не должен приниматься пустым: %+v", name, c)
		}
	}
	ivs := []InterviewInput{{ID: "iv", Title: "t", Text: text}}
	syn := `{"personas":[{"id":"p1","name":"М","description":"","interviews":["iv"]}],"needs":[{"id":"n1","title":"отчёт","kind":"pain","persona":"p1","importance":"high","evidence":[{"interview":"iv","quote":"Это ми`
	if c := CheckSynthesis(ivs, nil, syn); !c.Retry || c.Usable || !strings.Contains(strings.Join(c.Defects, " "), "оборван") {
		t.Fatalf("оборванный синтез: %+v", c)
	}
	if _, err := extractJSON(cut, "findings"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("ждали ErrTruncated: %v", err)
	}
}

func TestPreambleExample(t *testing.T) {
	text := NormalizeText("**Р:** Это минут сорок по пятницам, честно.\n")
	answer := `{"respondent":{"role":"","summary":""},"findings":[{"kind":"pain","text":"отчёт","importance":"high","quotes":[{"text":"Это минут сорок по пятницам","line":1}]}]}`
	// Пример формата в преамбуле перед настоящим ответом: берётся ответ.
	c := CheckSummary("iv", text, "Формат {\"kind\": \"pain\", \"quotes\": [{\"text\": \"…\"}]} соблюдаю.\n"+answer)
	if c.Retry || !c.Usable || len(c.Summary.Findings) != 1 {
		t.Fatalf("ответ после примера: %+v", c)
	}
	// Только пример без ответа — повтор, а не пустой конспект.
	c = CheckSummary("iv", text, "Формат {\"kind\": \"pain\"} соблюдаю. Ответ ниже.")
	if !c.Retry || c.Usable {
		t.Fatalf("пример без ответа: %+v", c)
	}
	// Вложенный объект без верхнего уровня не выдаёт себя за ответ.
	c = CheckSummary("iv", text, `{"respondent":{"role":"м","summary":"с"}}`)
	if !c.Retry || c.Usable {
		t.Fatalf("объект без findings: %+v", c)
	}
}

func TestSynthesisInputPerf(t *testing.T) {
	var ivs []InterviewInput
	for i := 0; i < 50; i++ {
		var fs []Finding
		for j := 0; j < 60; j++ {
			imp := []string{ImportanceHigh, ImportanceMedium, ImportanceLow}[j%3]
			fs = append(fs, Finding{Kind: KindNeed, Text: strings.Repeat("т", 150), Importance: imp,
				Quotes: []Quote{{Text: strings.Repeat("ц", 200), Line: j + 1}, {Text: strings.Repeat("ч", 150), Line: j + 2}}})
		}
		ivs = append(ivs, InterviewInput{ID: fmt.Sprintf("iv-%d", i), Title: "интервью", Summary: Summary{Findings: fs}})
	}
	start := time.Now()
	s, removed := SynthesisInput(ivs, SynthesisInputChars)
	el := time.Since(start)
	if utf8.RuneCountInString(s) > SynthesisInputChars || removed == 0 {
		t.Fatalf("вход %d рун при пороге, убрано %d", utf8.RuneCountInString(s), removed)
	}
	// Без квадрата это доли секунды; запас на -race и медленные машины.
	if el > 3*time.Second {
		t.Fatalf("сокращение входа на 3000 находках заняло %v", el)
	}
	t.Logf("3000 находок, убрано %d, %v", removed, el)
}

// refFind — эталон поиска: все вхождения, ближайшее к подсказке, при
// равенстве первое, номер строки пересчитывается от начала текста.
func refFind(text, quote string, hint int) (Located, bool) {
	q := normalizeQuote(quote)
	if utf8.RuneCountInString(q) < MinQuoteRunes {
		return Located{}, false
	}
	c := collapse(text)
	var best Located
	found, bestDist := false, 0
	for from := 0; from <= len(c.s)-len(q); {
		idx := strings.Index(c.s[from:], q)
		if idx < 0 {
			break
		}
		pos := from + idx
		start, end := c.start[pos], c.end[pos+len(q)-1]
		loc := Located{Line: 1 + strings.Count(text[:start], "\n"), Text: text[start:end], Start: start, End: end}
		dist := 0
		if hint > 0 {
			dist = abs(loc.Line - hint)
		}
		if !found || dist < bestDist {
			best, bestDist, found = loc, dist, true
		}
		if hint <= 0 {
			break
		}
		_, size := utf8.DecodeRuneInString(c.s[pos:])
		from = pos + size
	}
	return best, found
}

func TestFindQuoteFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"а", "б", " ", "\n", "\u00a0", "\u00ad", "ё", "«", "»", "  ", "\n\n", "\u200b"}
	for iter := 0; iter < 20000; iter++ {
		var b strings.Builder
		for i, n := 0, 20+rng.Intn(200); i < n; i++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		text := NormalizeText(b.String())
		r := []rune(text)
		if len(r) < 12 {
			continue
		}
		s := rng.Intn(len(r) - 11)
		quote := string(r[s : s+10+rng.Intn(min(20, len(r)-s-10))])
		hint := rng.Intn(CountLines(text)+3) - 1
		got, ok1 := FindQuote(text, quote, hint)
		want, ok2 := refFind(text, quote, hint)
		if ok1 != ok2 || got != want {
			t.Fatalf("hint=%d quote=%q: получили %+v/%v, эталон %+v/%v", hint, quote, got, ok1, want, ok2)
		}
	}
}

func TestSplitFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	words := []string{"слово", "ёж", "Привет.", "да!", "что?", "x", " ", "й", "…", "🙂"}
	for iter := 0; iter < 3000; iter++ {
		maxChars := 20 + rng.Intn(300)
		var lines []string
		for i, nl := 0, 1+rng.Intn(60); i < nl; i++ {
			var b strings.Builder
			for j, nw := 0, rng.Intn(1+rng.Intn(3)*maxChars/3); j < nw; j++ {
				b.WriteString(words[rng.Intn(len(words))])
				if rng.Intn(3) > 0 {
					b.WriteByte(' ')
				}
			}
			lines = append(lines, b.String())
		}
		text := strings.Join(lines, "\n")
		ls := Lines(text)
		chunks := SplitInterview(text, maxChars, 20)
		covered := make([]bool, len(ls))
		prevLast := 0
		for ci, c := range chunks {
			if c.LastLine < prevLast {
				t.Fatalf("итерация %d: порядок частей нарушен", iter)
			}
			multi := c.FirstLine != c.LastLine
			if ci > 0 && multi && chunks[ci-1].FirstLine != chunks[ci-1].LastLine && c.LastLine <= chunks[ci-1].LastLine {
				t.Fatalf("итерация %d: вложенная часть %d", iter, ci)
			}
			prevLast = c.LastLine
			size := 0
			for _, l := range c.Lines {
				size += utf8.RuneCountInString(l)
			}
			if size > maxChars {
				t.Fatalf("итерация %d: часть %d длиннее порога %d > %d", iter, ci, size, maxChars)
			}
			if !utf8.ValidString(c.Text()) {
				t.Fatalf("итерация %d: часть %d рвёт UTF-8", iter, ci)
			}
			for l := c.FirstLine; l <= c.LastLine; l++ {
				covered[l-1] = true
			}
		}
		for i, ok := range covered {
			if !ok {
				t.Fatalf("итерация %d: строка %d не покрыта", iter, i+1)
			}
		}
	}
}

// Куски длинной строки покрывают её без дыр: каждый следующий начинается
// внутри уже покрытого.
func TestSliceLongLineCoverage(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	seps := []string{" ", ". ", "! ", "? ", "… ", "", "  ", "\u00a0"}
	for iter := 0; iter < 5000; iter++ {
		maxChars := 10 + rng.Intn(200)
		var b strings.Builder
		for j, nw := 0, 1+rng.Intn(100); j < nw; j++ {
			fmt.Fprintf(&b, "ж%dё", j)
			b.WriteString(seps[rng.Intn(len(seps))])
		}
		l := b.String()
		if utf8.RuneCountInString(l)+1 <= maxChars {
			continue
		}
		pos, prevK := 0, -1
		pieces := sliceLongLine(l, maxChars)
		for i, p := range pieces {
			if n := utf8.RuneCountInString(p); n > maxChars {
				t.Fatalf("итерация %d: кусок длиннее порога %d > %d", iter, n, maxChars)
			}
			// Самое позднее вхождение куска в покрытой зоне: короткий кусок
			// может встречаться в строке несколько раз.
			k := -1
			for s := min(pos, len(l)-len(p)); s > prevK; s-- {
				if l[s:s+len(p)] == p {
					k = s
					break
				}
			}
			if k < 0 {
				t.Fatalf("итерация %d: дыра перед куском %d: max=%d строка=%q куски=%q", iter, i, maxChars, l, pieces)
			}
			prevK = k
			if k+len(p) > pos {
				pos = k + len(p)
			}
		}
		if pos != len(l) {
			t.Fatalf("итерация %d: строка покрыта до %d из %d", iter, pos, len(l))
		}
	}
}

func TestSynthesisInvariantFuzz(t *testing.T) {
	text := NormalizeText("**Р:** Это минут сорок по пятницам, честно.\n**Р:** Клиент просил набрать во вторник.\n")
	ivs := []InterviewInput{{ID: "iv", Title: "t", Text: text}}
	existing := []ExistingEpic{
		{ID: "ep-a", Title: "Отчёты", Status: "running"},
		{ID: "ep-b", Title: "Звонки", Status: "done"},
		{ID: "ep-c", Title: "Архив", Status: "archived"},
	}
	rng := rand.New(rand.NewSource(3))
	titles := []string{"Отчёты", "Звонки", "Архив", "Новый", "новый ", "Третий", "Четвёртый", "Четвертый"}
	refs := []string{"e1", "e2", "e3", "e4", "ep-a", "ep-b", "ep-c", "e9", " e1"}
	exs := []string{"", "ep-a", "ep-b", "ep-c", "ep-z"}
	goal := "Сделать отчёты для менеджеров автоматически каждую пятницу без ручной работы."
	usable := 0
	for iter := 0; iter < 20000; iter++ {
		var eps []string
		for i, n := 0, 1+rng.Intn(4); i < n; i++ {
			var deps []string
			for k := rng.Intn(3); k > 0; k-- {
				deps = append(deps, fmt.Sprintf("%q", refs[rng.Intn(len(refs))]))
			}
			ex := "null"
			if e := exs[rng.Intn(len(exs))]; e != "" {
				ex = fmt.Sprintf("%q", e)
			}
			g := goal
			if rng.Intn(6) == 0 {
				g = "коротко"
			}
			needs := `["n1"]`
			if rng.Intn(8) == 0 {
				needs = `["n9"]`
			}
			eps = append(eps, fmt.Sprintf(`{"key":"e%d","title":%q,"goal":%q,"needs":%s,"priority":"must","size":"M","depends_on":[%s],"existing_epic":%s}`,
				i+1, titles[rng.Intn(len(titles))], g, needs, strings.Join(deps, ","), ex))
		}
		raw := `{"personas":[{"id":"p1","name":"М","description":"","interviews":["iv"]}],
"needs":[{"id":"n1","title":"отчёт","kind":"pain","persona":"p1","importance":"high","evidence":[{"interview":"iv","quote":"Это минут сорок по пятницам"}]}],
"epics":[` + strings.Join(eps, ",") + `]}`
		c := CheckSynthesis(ivs, existing, raw)
		if !c.Usable {
			continue
		}
		usable++
		var needIDs []string
		for _, nd := range c.Result.Needs {
			needIDs = append(needIDs, nd.ID)
		}
		if err := ValidateBacklog(c.Result.Epics, needIDs, existing); err != nil {
			t.Fatalf("годный синтез не проходит ValidateBacklog: %v\n%s", err, raw)
		}
		if _, err := TopoOrder(c.Result.Epics); err != nil {
			t.Fatalf("TopoOrder на годном синтезе: %v", err)
		}
	}
	if usable < 1000 {
		t.Fatalf("фаззинг почти не дал годных результатов: %d", usable)
	}
}
