package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Итог стадии с ошибкой подключения сохраняет первую строку (ошибку агента
// с кодом отказа провайдера), даже если вывод агента длиннее предела.
func TestHeadTailKeepsErrorLine(t *testing.T) {
	errLine := "агент завершился с ошибкой: ход 7: провайдер отклонил ключ: HTTP 401"
	out := strings.Repeat("длинный ответ агента ", 2000) + "КОНЕЦ"
	got := headTail(errLine+"\n"+out, 8000)
	if !strings.HasPrefix(got, errLine+"\n") || !strings.HasSuffix(got, "КОНЕЦ") || len(got) > 8000 || !utf8.ValidString(got) {
		t.Fatalf("первая строка и конец вывода в пределе: длина %d, начало %q", len(got), got[:min(len(got), 120)])
	}
	if short := headTail("коротко\nвывод", 8000); short != "коротко\nвывод" {
		t.Fatalf("короткий итог не меняется: %q", short)
	}
	one := headTail(strings.Repeat("ошибка ", 3000), 8000)
	if !strings.HasPrefix(one, "ошибка ") || len(one) > 8000 || !utf8.ValidString(one) {
		t.Fatalf("однострочный итог режется с конца: длина %d", len(one))
	}
}
