package runner

import "testing"

// Разбор маркера вердикта шага prompt (спека agent-integration «Стадия PROMPT»).
func TestParsePromptVerdict(t *testing.T) {
	cases := []struct{ out, verdict, detail string }{
		{"работа\nVERDICT: OK", "ok", ""},
		{"VERDICT: APPROVED", "ok", ""},
		{"VERDICT: APPROVED: всё хорошо", "ok", "всё хорошо"},
		{"VERDICT: CHANGES: миграция необратима", "changes", "миграция необратима"},
		{"VERDICT: FAIL: нет доступа", "fail", "нет доступа"},
		{"VERDICT: что-то странное", "changes", "что-то странное"},
		{"VERDICT: OKAY", "changes", "OKAY"},
		{"без маркера", "ok", "задание выполнено (маркера вердикта нет)"},
	}
	for _, c := range cases {
		v, d := parsePromptVerdict(c.out)
		if v != c.verdict || d != c.detail {
			t.Errorf("%q: got %s %q, want %s %q", c.out, v, d, c.verdict, c.detail)
		}
	}
}
