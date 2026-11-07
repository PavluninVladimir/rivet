package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRunnerIDLen: предел длины идентификатора runner'а в символах
// (add-runner-identity). Одно правило для выпуска токена в API и для
// регистрации по протоколу: токен не привязывается к идентификатору,
// который консоль не дала бы указать, а перебор длинных идентификаторов
// не раздувает журнал и event log.
const MaxRunnerIDLen = 128

// RunnerIDProblem: что не так с идентификатором runner'а, или пустая
// строка, если он допустим. Идентификатор обычно имя хоста или выбранное
// администратором слово: непустой, до MaxRunnerIDLen символов, валидный
// UTF-8, без пробелов и управляющих символов. Сам идентификатор в текст не
// попадает: он может быть сколь угодно длинным.
func RunnerIDProblem(id string) string {
	switch {
	case id == "":
		return "пустой идентификатор runner'а"
	case !utf8.ValidString(id):
		return "идентификатор runner'а не в UTF-8"
	case utf8.RuneCountInString(id) > MaxRunnerIDLen:
		return fmt.Sprintf("идентификатор runner'а длиннее %d символов", MaxRunnerIDLen)
	case strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "идентификатор runner'а содержит пробелы или управляющие символы"
	}
	return ""
}
