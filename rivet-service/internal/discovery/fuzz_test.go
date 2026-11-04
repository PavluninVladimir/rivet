package discovery

import (
	"os"
	"testing"
)

// Фаззинг разбора ответов модели (недоверенный вход) и нарезки интервью:
// паника здесь роняла бы анализ (цели из второго круга ревью Opus).

func fuzzText(f *testing.F) string {
	b, err := os.ReadFile("testdata/interviews/manager.md")
	if err != nil {
		f.Fatal(err)
	}
	return NormalizeText(string(b))
}

func FuzzCheckSummary(f *testing.F) {
	text := fuzzText(f)
	f.Add(`{"respondent":{"role":"x"},"findings":[{"kind":"pain","text":"t","importance":"high","quotes":[{"line":3,"text":"Excel"}]}]}`)
	f.Add("```json\n{\"findings\":[]}\n```")
	f.Add(`{"findings":[{"quotes":[{"line":-1,"text":""}]}]}`)
	f.Fuzz(func(t *testing.T, raw string) {
		c := CheckSummary("«x»", text, raw)
		if c.Usable {
			_, _ = MergeSummaries("«x»", []Summary{c.Summary, c.Summary})
		}
	})
}

func FuzzCheckSynthesis(f *testing.F) {
	text := fuzzText(f)
	in := []InterviewInput{{ID: "i1", Title: "M", Text: text}}
	f.Add(`{"personas":[{"id":"p1","name":"n","interviews":["i1"]}],"needs":[{"id":"n1","title":"t","kind":"pain","persona":"p1","importance":"high","evidence":[{"interview":"i1","quote":"Excel"}]}],"epics":[{"key":"e1","title":"E","goal":"g","needs":["n1"],"priority":"must","size":"M","depends_on":["e1"],"existing_epic":null}]}`)
	f.Add(`{"epics":[{"key":"e1","depends_on":["e2"]},{"key":"e2","depends_on":["e1"]}]}`)
	f.Fuzz(func(t *testing.T, raw string) {
		_ = CheckSynthesis(in, []ExistingEpic{{ID: "x", Title: "E", Status: "done"}}, raw)
	})
}

func FuzzSplitInterview(f *testing.F) {
	f.Add("a\nb\nc")
	f.Fuzz(func(t *testing.T, s string) {
		_ = SplitInterview(NormalizeText(s), SummaryChunkChars, ChunkOverlapLines)
	})
}

func FuzzQuoteLeak(f *testing.F) {
	f.Add("клиент просил набрать во вторник после совещания", "Менеджер: клиент просил набрать во вторник после совещания")
	f.Fuzz(func(t *testing.T, text, quote string) {
		_, _ = QuoteLeak(text, []string{quote})
	})
}
