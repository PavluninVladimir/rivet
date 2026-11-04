package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/discovery"
)

// Контракт fake-llm и ядра исследования: ответы fake-llm, построенные из
// настоящих заданий discovery, проходят настоящую проверку; метка
// [e2e-bad-quote] даёт ровно одну отброшенную цитату.
func TestAnalystRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("../../internal/discovery/testdata/interviews/manager.md")
	if err != nil {
		t.Fatal(err)
	}
	text := discovery.NormalizeText(string(raw))

	run := func(t *testing.T, title string) discovery.SynthesisCheck {
		t.Helper()
		chunk := discovery.SplitInterview(text, discovery.SummaryChunkChars, discovery.ChunkOverlapLines)[0]
		prompt := discovery.SummaryPrompt(discovery.SummaryTask{InterviewID: "iv-1", Title: title, RespondentRole: "Менеджер B2B", Chunk: chunk, Part: 1, Parts: 1})
		reply, ok := analystReply(prompt)
		if !ok {
			t.Fatal("задание конспекта не распознано")
		}
		sc := discovery.CheckSummary("iv-1", text, reply)
		if sc.Retry || len(sc.Dropped) != 0 || len(sc.Summary.Findings) != summaryFindings {
			t.Fatalf("конспект fake-llm должен проходить проверку целиком: %+v", sc)
		}
		if sc.Summary.Respondent.Role != "Менеджер B2B" {
			t.Fatalf("роль респондента из задания: %q", sc.Summary.Respondent.Role)
		}
		ivs := []discovery.InterviewInput{{ID: "iv-1", Title: title, RespondentRole: "Менеджер B2B", Text: text, Summary: sc.Summary}}
		sp, _ := discovery.SynthesisPrompt(discovery.SynthesisTask{Interviews: ivs})
		reply, ok = analystReply(sp)
		if !ok {
			t.Fatal("задание синтеза не распознано")
		}
		return discovery.CheckSynthesis(ivs, nil, reply)
	}

	t.Run("чистое интервью", func(t *testing.T) {
		c := run(t, "Менеджер по продажам")
		if c.Retry || !c.Usable || len(c.Dropped) != 0 {
			t.Fatalf("синтез fake-llm должен проходить проверку: %+v", c)
		}
		if len(c.Result.Personas) != 1 || len(c.Result.Needs) != summaryFindings || len(c.Result.Epics) != 2 {
			t.Fatalf("результат: %+v", c.Result)
		}
		if deps := c.Result.Epics[1].DependsOn; len(deps) != 1 || deps[0] != "e1" {
			t.Fatalf("второй Epic зависит от первого: %v", deps)
		}
		order, err := discovery.TopoOrder(c.Result.Epics)
		if err != nil || strings.Join(order, ",") != "e1,e2" {
			t.Fatalf("порядок: %v %v", order, err)
		}
	})

	t.Run("метка искажённой цитаты", func(t *testing.T) {
		c := run(t, "Менеджер [e2e-bad-quote]")
		if !c.Usable || len(c.Result.Needs) != summaryFindings-1 {
			t.Fatalf("ждали одну отброшенную потребность: %+v", c)
		}
		quotes, needs := 0, 0
		for _, d := range c.Dropped {
			switch {
			case d.Kind == "quote" && d.Reason == discovery.ReasonQuoteNotFound:
				quotes++
			case d.Kind == "need" && d.Reason == discovery.ReasonNoQuotes:
				needs++
			}
		}
		if quotes != 1 || needs != 1 {
			t.Fatalf("отброшенное: %+v", c.Dropped)
		}
	})
}

// 21 интервью по 3 находки: персон не больше MaxPersonas, потребностей не
// больше MaxNeeds, ответ проходит проверку лимитов.
func TestAnalystLimits(t *testing.T) {
	raw, err := os.ReadFile("../../internal/discovery/testdata/interviews/head.md")
	if err != nil {
		t.Fatal(err)
	}
	text := discovery.NormalizeText(string(raw))
	chunk := discovery.SplitInterview(text, discovery.SummaryChunkChars, discovery.ChunkOverlapLines)[0]
	var ivs []discovery.InterviewInput
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("iv-%d", i+1)
		prompt := discovery.SummaryPrompt(discovery.SummaryTask{InterviewID: id, Title: "Интервью " + id, RespondentRole: "Руководитель", Chunk: chunk, Part: 1, Parts: 1})
		reply, _ := analystReply(prompt)
		sc := discovery.CheckSummary(id, text, reply)
		ivs = append(ivs, discovery.InterviewInput{ID: id, Title: "Интервью " + id, RespondentRole: "Руководитель", Text: text, Summary: sc.Summary})
	}
	sp, _ := discovery.SynthesisPrompt(discovery.SynthesisTask{Interviews: ivs})
	reply, _ := analystReply(sp)
	c := discovery.CheckSynthesis(ivs, nil, reply)
	if !c.Usable || c.Retry {
		t.Fatalf("ответ fake-llm на 21 интервью должен проходить лимиты: %v", c.Defects)
	}
	if len(c.Result.Personas) != discovery.MaxPersonas || len(c.Result.Needs) != discovery.MaxNeeds {
		t.Fatalf("персон %d, потребностей %d", len(c.Result.Personas), len(c.Result.Needs))
	}
	if last := c.Result.Personas[discovery.MaxPersonas-1]; len(last.Interviews) != 21-discovery.MaxPersonas+1 {
		t.Fatalf("лишние интервью приписываются последней персоне: %v", last.Interviews)
	}
}

func TestAnalystReplyIgnoresOtherPrompts(t *testing.T) {
	for _, p := range []string{"# Epic: Категории\nпостроить план", "", "Ты — планировщик.\nrivet:analyst:summary"} {
		if _, ok := analystReply(p); ok {
			t.Fatalf("не задание Аналитика: %q", p)
		}
	}
}

// Метка [e2e-slow] в названии интервью задерживает конспект и синтез с этим
// интервью, остальные задания отвечают сразу.
func TestAnalystDelay(t *testing.T) {
	raw, err := os.ReadFile("../../internal/discovery/testdata/interviews/manager.md")
	if err != nil {
		t.Fatal(err)
	}
	text := discovery.NormalizeText(string(raw))
	chunk := discovery.SplitInterview(text, discovery.SummaryChunkChars, discovery.ChunkOverlapLines)[0]
	summary := func(title string) string {
		return discovery.SummaryPrompt(discovery.SummaryTask{InterviewID: "iv-1", Title: title, RespondentRole: "Менеджер", Chunk: chunk, Part: 1, Parts: 1})
	}
	if d := analystDelay(summary("Менеджер")); d != 0 {
		t.Fatalf("без метки ответ сразу, задержка %v", d)
	}
	slow := summary("Менеджер " + slowLabel)
	if d := analystDelay(slow); d != slowDelay {
		t.Fatalf("конспект с меткой: задержка %v", d)
	}
	reply, _ := analystReply(slow)
	sc := discovery.CheckSummary("iv-1", text, reply)
	synthesis := func(title string) string {
		ivs := []discovery.InterviewInput{{ID: "iv-1", Title: title, RespondentRole: "Менеджер", Text: text, Summary: sc.Summary}}
		sp, _ := discovery.SynthesisPrompt(discovery.SynthesisTask{Interviews: ivs})
		return sp
	}
	if d := analystDelay(synthesis("Менеджер")); d != 0 {
		t.Fatalf("синтез без метки отвечает сразу, задержка %v", d)
	}
	if d := analystDelay(synthesis("Менеджер " + slowLabel)); d != slowDelay {
		t.Fatalf("синтез с интервью с меткой: задержка %v", d)
	}
	if d := analystDelay("# Epic: " + slowLabel); d != 0 {
		t.Fatalf("задание планировщика не задерживается: %v", d)
	}
}
