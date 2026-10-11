package runner

import "testing"

func TestParseUsageFull(t *testing.T) {
	out := "работаю...\nUSAGE: {\"tokens_in\": 1200, \"tokens_out\": 300, \"cost_usd\": 0.04, \"ctx_pct\": 35}\nготово\n"
	r := parseUsage(out)
	if r.TokensIn == nil || *r.TokensIn != 1200 || r.TokensOut == nil || *r.TokensOut != 300 {
		t.Fatalf("токены разобраны неверно: %+v", r)
	}
	if r.CostUSD == nil || *r.CostUSD != 0.04 {
		t.Fatalf("стоимость разобрана неверно: %+v", r)
	}
	if r.CtxPct == nil || *r.CtxPct != 35 {
		t.Fatalf("ctx_pct разобран неверно: %+v", r)
	}
}

// Частичный отчёт: незаполненные поля остаются «данных нет», не нулями
// (сценарий agent-integration «Частичный отчёт»).
func TestParseUsagePartial(t *testing.T) {
	r := parseUsage(`USAGE: {"tokens_in": 10, "tokens_out": 2}`)
	if r.TokensIn == nil || *r.TokensIn != 10 || r.TokensOut == nil || *r.TokensOut != 2 {
		t.Fatalf("токены разобраны неверно: %+v", r)
	}
	if r.CostUSD != nil || r.CtxPct != nil {
		t.Fatalf("отсутствующие поля должны быть nil: %+v", r)
	}
}

// Нет маркера — пустой отчёт (сценарий «Агент не отчитался о расходе»).
func TestParseUsageAbsent(t *testing.T) {
	r := parseUsage("обычный вывод без отчёта\n")
	if r.TokensIn != nil || r.TokensOut != nil || r.CostUSD != nil || r.CtxPct != nil {
		t.Fatalf("ожидался пустой отчёт, получено %+v", r)
	}
}

func TestParseUsageBrokenJSON(t *testing.T) {
	r := parseUsage("USAGE: {tokens_in: не json}\n")
	if r.TokensIn != nil || r.CtxPct != nil {
		t.Fatalf("битый JSON должен давать пустой отчёт: %+v", r)
	}
}

// Маркер в середине строки не считается (семантика lastSentinelLine),
// разбирается последняя маркерная строка.
func TestParseUsageLastLineWins(t *testing.T) {
	out := "упоминание USAGE: {\"tokens_in\": 1} в тексте\n" +
		"USAGE: {\"tokens_in\": 5}\n" +
		"USAGE: {\"tokens_in\": 7}\n"
	r := parseUsage(out)
	if r.TokensIn == nil || *r.TokensIn != 7 {
		t.Fatalf("ожидался последний отчёт (7), получено %+v", r)
	}
}

func TestParseUsageCtxOutOfRange(t *testing.T) {
	r := parseUsage(`USAGE: {"tokens_in": 1, "ctx_pct": 146}`)
	if r.CtxPct != nil {
		t.Fatalf("ctx_pct вне 0–100 должен отбрасываться: %+v", r)
	}
	if r.TokensIn == nil || *r.TokensIn != 1 {
		t.Fatalf("остальные поля должны сохраниться: %+v", r)
	}
}
