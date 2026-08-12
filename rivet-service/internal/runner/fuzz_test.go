package runner

import "testing"

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
