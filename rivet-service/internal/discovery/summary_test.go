package discovery

import (
	"os"
	"strings"
	"testing"
)

func loadInterview(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/interviews/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return NormalizeText(string(raw))
}

func TestCheckSummary(t *testing.T) {
	text := loadInterview(t, "manager.md")
	t.Run("ответ в ограждении кода, строка исправляется", func(t *testing.T) {
		raw := "Вот конспект:\n```json\n" + `{"respondent":{"role":"менеджер","summary":"ведёт клиентов в Excel"},
 "findings":[{"kind":"pain","text":"забывает перезвонить","importance":"high",
   "quotes":[{"text":"клиент просил набрать во вторник после совещания","line":3}]}]}` + "\n```"
		c := CheckSummary("manager", text, raw)
		if c.Retry || !c.Usable || len(c.Summary.Findings) != 1 || len(c.Dropped) != 0 {
			t.Fatalf("проверка: %+v", c)
		}
		q := c.Summary.Findings[0].Quotes[0]
		if q.Line != 17 || q.Text != "клиент просил набрать во вторник после совещания" {
			t.Fatalf("цитата: %+v", q)
		}
	})
	t.Run("выдуманная цитата отбрасывается вместе с находкой", func(t *testing.T) {
		raw := `{"respondent":{"role":"менеджер","summary":""},"findings":[
  {"kind":"need","text":"нужны напоминания","importance":"high","quotes":[{"text":"Покажи, кому я сегодня должен позвонить","line":65}]},
  {"kind":"wish","text":"хочет мобильное приложение","importance":"low","quotes":[{"text":"Хочу приложение на телефоне с push","line":40}]},
  {"kind":"pain","text":"отчёт по пятницам","importance":"medium","quotes":[{"text":"Это минут сорок по пятницам","line":25}]}]}`
		c := CheckSummary("manager", text, raw)
		if c.Retry || len(c.Summary.Findings) != 2 {
			t.Fatalf("ждали 2 находки без повтора: %+v", c)
		}
		if len(c.Dropped) != 2 || c.Dropped[0].Kind != "quote" || c.Dropped[0].Reason != ReasonQuoteNotFound || c.Dropped[1].Reason != ReasonNoQuotes {
			t.Fatalf("отброшенное: %+v", c.Dropped)
		}
	})
	t.Run("больше половины отброшено — повтор", func(t *testing.T) {
		raw := `{"respondent":{"role":"","summary":""},"findings":[
  {"kind":"need","text":"а","importance":"high","quotes":[{"text":"этой фразы нет в интервью вообще","line":1}]},
  {"kind":"need","text":"б","importance":"high","quotes":[{"text":"и этой фразы тоже нет нигде","line":1}]},
  {"kind":"pain","text":"в","importance":"high","quotes":[{"text":"Это минут сорок по пятницам","line":25}]}]}`
		c := CheckSummary("manager", text, raw)
		if !c.Retry || !c.Usable || len(c.Summary.Findings) != 1 {
			t.Fatalf("ждали повтор с принятой частью: %+v", c)
		}
		if len(c.Defects) < 3 || !strings.Contains(c.Defects[len(c.Defects)-1], "отброшено 2 из 3") {
			t.Fatalf("дефекты: %v", c.Defects)
		}
	})
	t.Run("ответ не разбирается — повтор", func(t *testing.T) {
		for _, raw := range []string{"не JSON", `{"findings": [`, `{"findings": "строка"}`} {
			c := CheckSummary("manager", text, raw)
			if !c.Retry || c.Usable || len(c.Defects) != 1 {
				t.Fatalf("%q: %+v", raw, c)
			}
		}
	})
	t.Run("лишнее поле — предупреждение без повтора", func(t *testing.T) {
		raw := `{"respondent":{"role":"","summary":""},"findings":[{"kind":"pain","text":"а","importance":"high","confidence":0.9,
  "quotes":[{"text":"Это минут сорок по пятницам","line":25}]}]}`
		c := CheckSummary("manager", text, raw)
		if c.Retry || !c.Usable || len(c.Summary.Findings) != 1 || len(c.Defects) != 1 || !strings.Contains(c.Defects[0], "предупреждение") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("ограждение кода внутри цитаты и скобки в преамбуле", func(t *testing.T) {
		code := NormalizeText("**Р:** Я пишу в чат ```make deploy``` и жду полчаса, это ужасно.\n")
		body := `{"respondent":{"role":"dev","summary":""},"findings":[{"kind":"pain","text":"долгий деплой","importance":"high",
  "quotes":[{"text":"Я пишу в чат ` + "```make deploy```" + ` и жду полчаса","line":1}]}]}`
		for name, raw := range map[string]string{
			"в ограждении":     "```json\n" + body + "\n```",
			"без ограждения":   body,
			"преамбула {json}": "Ответ в формате {json}:\n" + body + "\nГотово {ок}.",
		} {
			c := CheckSummary("iv", code, raw)
			if c.Retry || !c.Usable || len(c.Summary.Findings) != 1 {
				t.Fatalf("%s: %+v", name, c)
			}
		}
	})
	t.Run("цитаты с одним началом — остаётся длинная", func(t *testing.T) {
		raw := `{"respondent":{"role":"","summary":""},"findings":[{"kind":"pain","text":"а","importance":"high",
  "quotes":[{"text":"Это минут сорок","line":25},{"text":"Это минут сорок по пятницам","line":25}]}]}`
		c := CheckSummary("manager", text, raw)
		q := c.Summary.Findings[0].Quotes
		if len(q) != 1 || q[0].Text != "Это минут сорок по пятницам" {
			t.Fatalf("цитаты: %+v", q)
		}
		if len(c.Dropped) != 1 || c.Dropped[0].Reason != ReasonQuoteDuplicate || !strings.Contains(c.Dropped[0].Ref, "«Это минут сорок»") {
			t.Fatalf("отброшенное: %+v", c.Dropped)
		}
	})
	t.Run("всё отброшено — результат негоден", func(t *testing.T) {
		raw := `{"respondent":{"role":"","summary":""},"findings":[{"kind":"pain","text":"а","importance":"high","quotes":[{"text":"выдумка совсем другая фраза","line":1}]}]}`
		// Модель дважды вернула только выдуманные цитаты: и первый, и
		// повторный ответ негодны, анализ уйдёт в failed.
		for attempt := 1; attempt <= 2; attempt++ {
			if c := CheckSummary("manager", text, raw); !c.Retry || c.Usable {
				t.Fatalf("попытка %d: %+v", attempt, c)
			}
		}
	})
	t.Run("неизвестный вид и важность", func(t *testing.T) {
		raw := `{"respondent":{"role":"","summary":""},"findings":[
  {"kind":"idea","text":"а","importance":"high","quotes":[{"text":"Это минут сорок по пятницам","line":25}]},
  {"kind":"pain","text":"б","importance":"urgent","quotes":[{"text":"Это минут сорок по пятницам","line":25}]},
  {"kind":"pain","text":"","importance":"high","quotes":[{"text":"Это минут сорок по пятницам","line":25}]}]}`
		c := CheckSummary("manager", text, raw)
		if len(c.Summary.Findings) != 0 || len(c.Dropped) != 3 || !c.Retry {
			t.Fatalf("ждали три отброшенных находки: %+v", c)
		}
	})
	t.Run("пустой конспект принимается без повтора", func(t *testing.T) {
		c := CheckSummary("manager", text, `{"respondent":{"role":"","summary":""},"findings":[]}`)
		if c.Retry || !c.Usable {
			t.Fatalf("%+v", c)
		}
	})
}

func TestMergeSummaries(t *testing.T) {
	a := Summary{Respondent: Respondent{Role: "менеджер", Summary: "часть 1."}, Findings: []Finding{
		{Kind: KindPain, Text: "x", Importance: ImportanceHigh, Quotes: []Quote{{Text: "общая цитата в перекрытии", Line: 30}}},
		// Внутри одной части две находки на одной цитате — не повтор.
		{Kind: KindWish, Text: "x как пожелание", Importance: ImportanceLow, Quotes: []Quote{{Text: "общая цитата в перекрытии", Line: 30}}},
	}}
	b := Summary{Respondent: Respondent{Role: "", Summary: "часть 2."}, Findings: []Finding{
		{Kind: KindPain, Text: "x повтор", Importance: ImportanceHigh, Quotes: []Quote{{Text: "общая  цитата в перекрытии", Line: 30}}},
		{Kind: KindNeed, Text: "y", Importance: ImportanceLow, Quotes: []Quote{{Text: "общая цитата в перекрытии", Line: 30}, {Text: "новая цитата второй части", Line: 80}}},
	}}
	got, dropped := MergeSummaries("iv", []Summary{a, b})
	if got.Respondent.Role != "менеджер" || got.Respondent.Summary != "часть 1. часть 2." {
		t.Fatalf("респондент: %+v", got.Respondent)
	}
	if len(got.Findings) != 3 || got.Findings[2].Text != "y" || len(got.Findings[2].Quotes) != 1 || got.Findings[2].Quotes[0].Line != 80 {
		t.Fatalf("находки: %+v", got.Findings)
	}
	// Отброшены: цитата «x повтор» и сама находка, цитата перекрытия у «y».
	if len(dropped) != 3 || dropped[1].Reason != ReasonOverlapDuplicate || dropped[0].Reason != ReasonQuoteDuplicate || dropped[2].Reason != ReasonQuoteDuplicate {
		t.Fatalf("отброшенное: %+v", dropped)
	}
}
