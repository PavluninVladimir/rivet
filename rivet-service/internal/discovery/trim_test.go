package discovery

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// Оценка сокращения входа совпадает с рендером: удаление любой находки
// уменьшает блок ровно на findingCost, а последней находки конспекта — ещё
// на emptyArrayGain. Размер конспекта на своём отступе складывается в
// размер блока.
func TestFindingCostExact(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	texts := []string{"боль", "«цитата» & <тег>", "строка\nс переводом", "ёжик \"в\" кавычках", strings.Repeat("т", 120)}
	render := func(items []synthesisInterview) int {
		return utf8.RuneCountInString(marshalBlock(map[string]any{"interviews": items}))
	}
	for iter := 0; iter < 300; iter++ {
		var items []synthesisInterview
		for i, n := 0, 1+rng.Intn(4); i < n; i++ {
			title := texts[rng.Intn(len(texts))]
			it := synthesisInterview{ID: fmt.Sprintf("iv-%d", i), Title: &title, Findings: []Finding{}}
			for j, m := 0, rng.Intn(4); j < m; j++ {
				it.Findings = append(it.Findings, Finding{Kind: KindPain, Text: texts[rng.Intn(len(texts))], Importance: ImportanceLow,
					Quotes: []Quote{{Text: texts[rng.Intn(len(texts))], Line: rng.Intn(500)}}})
			}
			items = append(items, it)
		}
		full := render(items)
		// Размер блока = обрамление + конспекты на своём отступе.
		sum := 0
		for _, it := range items {
			sum += indentedRunes(it, itemIndent)
		}
		frame := full - sum
		for i := range items {
			for j := range items[i].Findings {
				c := findingCost(items[i].Findings[j])
				if len(items[i].Findings) == 1 {
					c += emptyArrayGain
				}
				saved := items[i].Findings
				items[i].Findings = append(append([]Finding{}, saved[:j]...), saved[j+1:]...)
				if got := full - render(items); got != c {
					t.Fatalf("итерация %d: удаление находки %d конспекта %d экономит %d, оценка %d", iter, j, i, got, c)
				}
				// Обрамление не зависит от находок.
				s := 0
				for _, it := range items {
					s += indentedRunes(it, itemIndent)
				}
				if render(items)-s != frame {
					t.Fatalf("итерация %d: размер конспекта на отступе не складывается в блок", iter)
				}
				items[i].Findings = saved
			}
		}
	}
}

// Удаление единственной находки конспекта не тянет за собой лишнюю.
func TestSynthesisInputMinimal(t *testing.T) {
	big := Finding{Kind: KindNeed, Text: strings.Repeat("б", 300), Importance: ImportanceLow, Quotes: []Quote{{Text: "цитата длинная", Line: 1}}}
	small := Finding{Kind: KindNeed, Text: strings.Repeat("м", 100), Importance: ImportanceLow, Quotes: []Quote{{Text: "цитата короткая", Line: 2}}}
	ivs := []InterviewInput{
		{ID: "a", Summary: Summary{Findings: []Finding{big}}},
		{ID: "b", Summary: Summary{Findings: []Finding{small}}},
	}
	full, _ := SynthesisInput(ivs, 0)
	need := findingCost(big) + emptyArrayGain
	// Порог ровно на экономию от удаления большой находки: хватает одной.
	s, removed := SynthesisInput(ivs, utf8.RuneCountInString(full)-need)
	if removed != 1 || utf8.RuneCountInString(s) != utf8.RuneCountInString(full)-need {
		t.Fatalf("ждали ровно одно удаление: removed=%d, размер %d", removed, utf8.RuneCountInString(s))
	}
	// На символ меньше — нужна и вторая.
	if _, removed = SynthesisInput(ivs, utf8.RuneCountInString(full)-need-1); removed != 2 {
		t.Fatalf("ждали два удаления: %d", removed)
	}
}

// Сотни отзывов не вытесняют интервью (add-user-feedback): отзывы
// укладываются в свою долю порога, находки интервью при этом целы, блок в
// пределах порога, отзывы без оставшихся находок уходят из блока.
func TestSynthesisInputFeedbackQuota(t *testing.T) {
	finding := func(text, quote string) Finding {
		return Finding{Kind: KindPain, Text: text, Importance: ImportanceHigh, Quotes: []Quote{{Text: quote, Line: 3}}}
	}
	var ivs []InterviewInput
	interviewFindings := 0
	for i := 0; i < 10; i++ {
		iv := InterviewInput{ID: fmt.Sprintf("iv-%d", i), Title: fmt.Sprintf("Интервью %d", i), RespondentRole: "менеджер",
			Summary: Summary{Respondent: Respondent{Summary: strings.Repeat("р", 200)}}}
		// 150 находок интервью занимают около 50 000 символов: меньше
		// половины порога, которая остаётся интервью.
		for j := 0; j < 15; j++ {
			iv.Summary.Findings = append(iv.Summary.Findings, finding(strings.Repeat("н", 50), strings.Repeat("ц", 60)))
			interviewFindings++
		}
		ivs = append(ivs, iv)
	}
	for i := 0; i < 500; i++ {
		fb := InterviewInput{ID: fmt.Sprintf("fb-%03d", i), Kind: MaterialFeedback, Title: "Отзыв", RespondentRole: "чат поддержки"}
		for j := 0; j < 2; j++ {
			fb.Summary.Findings = append(fb.Summary.Findings, finding(strings.Repeat("о", 60), strings.Repeat("ц", 80)))
		}
		ivs = append(ivs, fb)
	}
	block, removed := SynthesisInput(ivs, SynthesisInputChars)
	if n := utf8.RuneCountInString(block); n > SynthesisInputChars {
		t.Fatalf("блок длиннее порога: %d", n)
	}
	var got struct {
		Interviews []struct {
			ID       string    `json:"id"`
			Kind     string    `json:"kind"`
			Findings []Finding `json:"findings"`
		} `json:"interviews"`
	}
	if err := json.Unmarshal([]byte(block), &got); err != nil {
		t.Fatal(err)
	}
	kept, feedback, feedbackItems := 0, 0, 0
	for _, it := range got.Interviews {
		if it.Kind == MaterialFeedback {
			feedbackItems++
			feedback += len(it.Findings)
			if len(it.Findings) == 0 {
				t.Fatalf("отзыв без находок в блоке: %s", it.ID)
			}
			continue
		}
		kept += len(it.Findings)
	}
	if kept != interviewFindings {
		t.Fatalf("находки интервью вытеснены отзывами: осталось %d из %d", kept, interviewFindings)
	}
	if feedback == 0 || feedback == 1000 || removed != 1000-feedback || feedbackItems >= 500 {
		t.Fatalf("отзывы не сокращены в свою долю: находок %d, отзывов %d, убрано %d", feedback, feedbackItems, removed)
	}
}

// Бюджет отзывов — потолок, а не резерв (раунд 2 ревью add-user-feedback):
// пока интервью сами помещаются в порог, ни одна их находка не теряется,
// даже если интервью занимают больше половины порога, а находки отзывов
// с важностью low уходят первыми.
func TestSynthesisInputFeedbackBudget(t *testing.T) {
	levels := []string{ImportanceHigh, ImportanceMedium, ImportanceLow}
	build := func(interviews, perInterview, feedback int, fbLevel string, mixed bool) ([]InterviewInput, int) {
		var ivs []InterviewInput
		count := 0
		for i := 0; i < interviews; i++ {
			iv := InterviewInput{ID: fmt.Sprintf("iv-%d", i), Title: fmt.Sprintf("Интервью %d", i), RespondentRole: "менеджер",
				Summary: Summary{Respondent: Respondent{Summary: strings.Repeat("р", 300)}}}
			for j := 0; j < perInterview; j++ {
				imp := ImportanceHigh
				if mixed {
					imp = levels[j%3]
				}
				iv.Summary.Findings = append(iv.Summary.Findings, Finding{Kind: KindPain, Text: strings.Repeat("н", 90), Importance: imp,
					Quotes: []Quote{{Text: strings.Repeat("ц", 60), Line: 3}}})
				count++
			}
			ivs = append(ivs, iv)
		}
		for i := 0; i < feedback; i++ {
			fb := InterviewInput{ID: fmt.Sprintf("fb-%03d", i), Kind: MaterialFeedback, RespondentRole: "чат"}
			for j := 0; j < 2; j++ {
				imp := fbLevel
				if fbLevel == "" {
					imp = levels[(i+j)%3]
				}
				fb.Summary.Findings = append(fb.Summary.Findings, Finding{Kind: KindPain, Text: strings.Repeat("о", 90), Importance: imp,
					Quotes: []Quote{{Text: strings.Repeat("ц", 60), Line: 1}}})
			}
			ivs = append(ivs, fb)
		}
		return ivs, count
	}
	type kept struct{ interview, feedback map[string]int }
	parse := func(block string) kept {
		var got struct {
			Interviews []struct {
				Kind     string    `json:"kind"`
				Findings []Finding `json:"findings"`
			} `json:"interviews"`
		}
		if err := json.Unmarshal([]byte(block), &got); err != nil {
			t.Fatal(err)
		}
		k := kept{map[string]int{}, map[string]int{}}
		for _, it := range got.Interviews {
			for _, f := range it.Findings {
				if it.Kind == MaterialFeedback {
					k.feedback[f.Importance]++
				} else {
					k.interview[f.Importance]++
				}
			}
		}
		return k
	}
	for _, c := range []struct {
		name                      string
		interviews, per, feedback int
		fbLevel                   string
		mixed                     bool
	}{
		{"10 интервью high, 500 отзывов low", 10, 20, 500, ImportanceLow, false},
		{"10 интервью смешанные, 200 отзывов high", 10, 20, 200, ImportanceHigh, true},
		{"10 интервью смешанные, 500 отзывов смешанные", 10, 20, 500, "", true},
		{"14 интервью high, 300 отзывов low", 14, 20, 300, ImportanceLow, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			onlyIv, total := build(c.interviews, c.per, 0, "", c.mixed)
			ivBlock, _ := SynthesisInput(onlyIv, 0)
			ivSize := utf8.RuneCountInString(ivBlock)
			if ivSize < 70000 || ivSize > SynthesisInputChars {
				t.Fatalf("интервью %d символов, нужно 70–120 тыс.", ivSize)
			}
			ivs, _ := build(c.interviews, c.per, c.feedback, c.fbLevel, c.mixed)
			block, trim := SynthesisInputTrim(ivs, SynthesisInputChars)
			if n := utf8.RuneCountInString(block); n > SynthesisInputChars {
				t.Fatalf("блок длиннее порога: %d", n)
			}
			k := parse(block)
			got := k.interview[ImportanceHigh] + k.interview[ImportanceMedium] + k.interview[ImportanceLow]
			if got != total || trim.Removed != trim.Feedback {
				t.Fatalf("интервью потеряли находки: %d из %d, %+v", got, total, trim)
			}
			// Внутри бюджета отзывов low уходят раньше medium и high.
			if c.fbLevel == "" && k.feedback[ImportanceLow] > 0 && (k.feedback[ImportanceMedium] == 0 || k.feedback[ImportanceHigh] == 0) {
				t.Fatalf("у отзывов остались low при убранных важных: %v", k.feedback)
			}
			if !strings.Contains(trim.Text(), "находки интервью целы") {
				t.Fatalf("пояснение сокращения: %s", trim.Text())
			}
			// Отзывы в наборе версии, но во вход вошли не все: это видно.
			fbItems := 0
			for _, it := range parseItems(t, block) {
				if it == MaterialFeedback {
					fbItems++
				}
			}
			want := fmt.Sprintf("в синтез вошли %d из %d отзывов: убрано сокращением %d", fbItems, c.feedback, c.feedback-fbItems)
			if trim.FeedbackTotal != c.feedback || trim.FeedbackSynthesized != fbItems || fbItems >= c.feedback || trim.FeedbackText() != want {
				t.Fatalf("вошедшие отзывы: %+v, %q, ждали %q", trim, trim.FeedbackText(), want)
			}
		})
	}

	// Интервью сами длиннее порога: отзывы уходят целиком, интервью
	// сокращаются по важности, пояснение называет убранное.
	ivs, _ := build(30, 20, 100, ImportanceHigh, true)
	block, trim := SynthesisInputTrim(ivs, SynthesisInputChars)
	k := parse(block)
	if len(k.feedback) != 0 || trim.Feedback != 200 || utf8.RuneCountInString(block) > SynthesisInputChars {
		t.Fatalf("отзывы при переполненных интервью: %v %+v", k.feedback, trim)
	}
	if !strings.Contains(trim.Text(), "из отзывов пользователей") || !strings.Contains(trim.Text(), "из интервью") {
		t.Fatalf("пояснение: %s", trim.Text())
	}
}

// parseItems — виды конспектов во входе синтеза по порядку.
func parseItems(t *testing.T, block string) []string {
	t.Helper()
	var got struct {
		Interviews []struct {
			Kind string `json:"kind"`
		} `json:"interviews"`
	}
	if err := json.Unmarshal([]byte(block), &got); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range got.Interviews {
		out = append(out, it.Kind)
	}
	return out
}

// Все отзывы вошли во вход: пояснения о вошедших нет. Отзыв без находок во
// вход не входит и в число вошедших не считается.
func TestFeedbackCoverageWithoutTrim(t *testing.T) {
	f := Finding{Kind: KindPain, Text: "т", Importance: ImportanceHigh, Quotes: []Quote{{Text: "цитата отзыва", Line: 1}}}
	ivs := []InterviewInput{
		{ID: "fb-1", Kind: MaterialFeedback, Summary: Summary{Findings: []Finding{f}}},
		{ID: "fb-2", Kind: MaterialFeedback, Summary: Summary{Findings: []Finding{f}}},
	}
	_, trim := SynthesisInputTrim(ivs, SynthesisInputChars)
	if trim.FeedbackTotal != 2 || trim.FeedbackSynthesized != 2 || trim.FeedbackText() != "" {
		t.Fatalf("все отзывы во входе: %+v", trim)
	}
	ivs = append(ivs, InterviewInput{ID: "fb-3", Kind: MaterialFeedback})
	_, trim = SynthesisInputTrim(ivs, SynthesisInputChars)
	if trim.FeedbackText() != "в синтез вошли 2 из 3 отзывов: без находок 1" || trim.FeedbackTrimmed() != 0 {
		t.Fatalf("отзыв без находок: %+v %q", trim, trim.FeedbackText())
	}
	if got := FeedbackCoverageText(10, 6, 1); got != "в синтез вошли 6 из 10 отзывов: без находок 1, убрано сокращением 3" {
		t.Fatalf("обе причины: %q", got)
	}
	if got := FeedbackCoverageText(10, 10, 0); got != "" {
		t.Fatalf("все вошли: %q", got)
	}
}
