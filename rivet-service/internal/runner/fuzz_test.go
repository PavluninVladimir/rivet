package runner

import (
	"strings"
	"testing"
)

// FuzzParseUsage: USAGE:-отчёт — внешний ввод от произвольного агента.
// Разбор не паникует на любом выводе, ctx_pct за пределами 0–100 не
// просачивается, вывод без маркера в начале строки даёт пустой отчёт.
func FuzzParseUsage(f *testing.F) {
	f.Add(`USAGE: {"tokens_in": 1200, "tokens_out": 300, "cost_usd": 0.04, "ctx_pct": 35}`)
	f.Add("текст\nUSAGE: {\"tokens_in\": 5}\nUSAGE: {битый json")
	f.Add(`USAGE: {"ctx_pct": -1}`)
	f.Add(`USAGE: {"ctx_pct": 101}`)
	f.Add(`USAGE: {"tokens_in": 9223372036854775807}`)
	f.Add("упоминание USAGE: в середине текста\n")
	f.Add("USAGE:")
	f.Add("")
	f.Fuzz(func(t *testing.T, out string) {
		r := parseUsage(out)
		if r.CtxPct != nil && (*r.CtxPct < 0 || *r.CtxPct > 100) {
			t.Fatalf("ctx_pct вне диапазона просочился: %d", *r.CtxPct)
		}
		hasMarker := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(strings.TrimRight(line, "\r")), "USAGE:") {
				hasMarker = true
				break
			}
		}
		if !hasMarker && (r.TokensIn != nil || r.TokensOut != nil || r.CostUSD != nil || r.CtxPct != nil) {
			t.Fatalf("отчёт без маркера должен быть пустым: %+v", r)
		}
	})
}

// FuzzTail: обрезка вывода агента не паникует на любых строке и лимите,
// результат не длиннее лимита (плюс многоточие), короткое возвращается как есть.
func FuzzTail(f *testing.F) {
	f.Add("короткий вывод", 8000)
	f.Add("строка длиннее лимита", 4)
	f.Add("", 0)
	f.Add("x", -1)
	f.Fuzz(func(t *testing.T, s string, n int) {
		res := tail(s, n)
		limit := n
		if limit < 0 {
			limit = 0
		}
		// Сравнение без сложения: limit+len("…") переполнился бы на огромном n.
		if len(res)-len("…") > limit {
			t.Fatalf("tail(len %d, %d) длиннее лимита: %d байт", len(s), n, len(res))
		}
		if n >= 0 && len(s) <= n && res != s {
			t.Fatalf("строка короче лимита должна возвращаться как есть")
		}
	})
}
