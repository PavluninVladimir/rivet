package ops

import (
	"strings"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/redact"
)

// Пределы доказательств (спека operations «Инцидент и отпечаток сбоя»,
// «Безопасность данных сопровождения»).
const (
	MaxEvidenceLines = 200
	MaxEvidenceBytes = 64 << 10
)

const truncMark = "… (начало обрезано)\n"

// Evidence готовит вывод проверки, логов или публикации к сохранению и к
// передаче модели: маскирует секреты редактором и оставляет хвост не
// больше 200 строк и 64 КиБ. Маскирование идёт до обрезки, чтобы обрезка
// не разрезала секрет на неузнаваемый кусок.
func Evidence(text string) string {
	return Tail(redact.String(text), MaxEvidenceLines, MaxEvidenceBytes)
}

// Tail — последние maxLines строк, не больше maxBytes байт с пометкой об
// обрезке в начале. Обрезка по байтам идёт по границе строки, а если
// последняя строка сама длиннее предела, по границе символа UTF-8. Пометка
// не ставится, если не помещается в предел. Пределы меньше единицы дают
// пустую строку.
func Tail(s string, maxLines, maxBytes int) string {
	if maxLines < 1 || maxBytes < 1 {
		return ""
	}
	s = strings.TrimRight(s, "\n")
	cut := false
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s = strings.Join(lines[len(lines)-maxLines:], "\n")
		cut = true
	}
	mark := truncMark
	if len(mark) >= maxBytes {
		mark = ""
	}
	budget := maxBytes
	if cut || len(s) > maxBytes {
		budget = maxBytes - len(mark)
	}
	if len(s) > budget {
		cut = true
		s = s[len(s)-budget:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		} else {
			for len(s) > 0 && !utf8.RuneStart(s[0]) {
				s = s[1:]
			}
		}
	}
	if cut {
		return mark + s
	}
	return s
}
