package discovery

import (
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
			it := synthesisInterview{ID: fmt.Sprintf("iv-%d", i), Title: texts[rng.Intn(len(texts))], Findings: []Finding{}}
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
