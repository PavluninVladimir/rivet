package api

import (
	"strings"
	"testing"
)

// FuzzBearerSecret: заголовок Authorization — внешний ввод. Разбор не
// паникует, отдаёт секрет только из корректной формы «Bearer <секрет>»
// и никогда не возвращает строку с пробельной обвязкой.
func FuzzBearerSecret(f *testing.F) {
	f.Add("Bearer rvt_abc123")
	f.Add("bearer rvt_abc123")
	f.Add("Bearer   rvt_abc123  ")
	f.Add("Basic dXNlcjpwYXNz")
	f.Add("Bearer")
	f.Add("Bearer ")
	f.Add("")
	f.Add("BearerX rvt_abc")
	f.Fuzz(func(t *testing.T, header string) {
		got := bearerSecret(header)
		if got == "" {
			return
		}
		if strings.TrimSpace(got) != got {
			t.Fatalf("секрет с пробельной обвязкой: %q из %q", got, header)
		}
		if !strings.EqualFold(header[:7], "Bearer ") {
			t.Fatalf("секрет из заголовка без Bearer-префикса: %q", header)
		}
	})
}
