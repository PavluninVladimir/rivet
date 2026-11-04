// Package ops — чистое ядро релиза и сопровождения (change add-sdlc-cycle,
// спеки backend/operations и backend/release-engineering): отпечаток сбоя,
// доказательства с маскированием и обрезкой, решения по инцидентам и их
// статусам, лимиты, состав релиза, разбор ответов Релиз-инженера и
// Инженера сопровождения, запасные шаблоны и тексты заданий. Ни базы, ни
// HTTP: обнаружение, состав и переходы считает код, модель только пишет
// тексты.
package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Источники инцидента.
const (
	SourceMonitor = "monitor"
	SourceRelease = "release"
)

var (
	// zeroCounter — сводки без ошибок: «0 failed», «Errors: 0», «no errors
	// found». Это не признак.
	zeroCounter = regexp.MustCompile(`(?i)\b0\s+(?:errors?|failed|failures?|exceptions?)\b|\berrors?\s*[:=]\s*0\b|\bno\s+(?:errors?|failures?)\b`)
	// strongSign — сильный признак: error и exception (в том числе как
	// суффикс: TypeError, RuntimeException), panic, fatal и код HTTP 5xx в
	// HTTP-контексте: `HTTP/1.1" 500`, `"status":500`, `statusCode: 502`,
	// `status_code=502`, `code 500`, `→ 500`.
	strongSign = regexp.MustCompile(`(?i)(?:errors?|exception)\b|\b(?:panic|fatal)\b|(?:HTTP/\S+|"?\bstatus(?:_?code)?"?|"?\bcode"?|→|->)[\s:="]*5\d\d\b`)
	// weakSign — слабый признак: fail, failed, failure.
	weakSign = regexp.MustCompile(`(?i)\b(?:fail|failed|failure)\b`)
	// bareHTTP — голый код 5xx без контекста (access-лог morgan
	// `GET /x 500 3.2 ms`, `returned 500`): последний проход перед первой
	// строкой.
	bareHTTP = regexp.MustCompile(`\b5\d\d\b`)
	// bareNotCodeBefore и bareNotCodeAfter — число 5xx не код ответа:
	// идентификатор (`id=523`, `#512`) или величина с единицей (`512 ms`,
	// `503KB`, `500.3ms`).
	bareNotCodeBefore = regexp.MustCompile(`(?i)(?:\bid\s*[=:]\s*|#)$`)
	bareNotCodeAfter  = regexp.MustCompile(`(?i)^(?:\.\d+)?\s*(?:ms|s|sec|secs|seconds?|kb|mb|gb|kib|mib|b|bytes?|%)\b`)
)

// bareCode — в строке есть голый код 5xx, а не идентификатор или величина.
func bareCode(line string) bool {
	for _, loc := range bareHTTP.FindAllStringIndex(line, -1) {
		if bareNotCodeBefore.MatchString(line[:loc[0]]) || bareNotCodeAfter.MatchString(line[loc[1]:]) {
			continue
		}
		return true
	}
	return false
}

// ErrorLine — строка ошибки вывода. Поиск в три прохода: сначала первая
// строка с сильным признаком, затем со слабым, затем последняя строка с
// голым кодом 5xx (числа после `id=` и `#` и величины с единицами не в
// счёт), иначе первая непустая. Сводки с нулевыми счётчиками признаком не
// считаются. Вызывать по полному выводу после redact.String, но до обрезки
// Tail: обрезка может отрезать строку ошибки.
func ErrorLine(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(strings.TrimRight(line, "\r")); line != "" {
			lines = append(lines, line)
		}
	}
	for _, sign := range []*regexp.Regexp{strongSign, weakSign} {
		for _, line := range lines {
			if sign.MatchString(zeroCounter.ReplaceAllString(line, "")) {
				return line
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if bareCode(lines[i]) {
			return lines[i]
		}
	}
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:[Zz]|[+-]\d{2}:?\d{2})?`)
	reSyslogDay = regexp.MustCompile(`\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\b`)
	reClock     = regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`)
	reUUID      = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reHex       = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]{8,}\b`)
	reAlnum     = regexp.MustCompile(`[A-Za-z0-9]{5,}`)
	reLetter    = regexp.MustCompile(`[A-Za-z]`)
	reDigit     = regexp.MustCompile(`[0-9]`)
	reNumber    = regexp.MustCompile(`\d+`)
)

// Normalize убирает из строки ошибки то, что меняется от прогона к
// прогону, одним заполнителем «*»: отметки времени (RFC 3339, syslog,
// HH:MM:SS), UUID, шестнадцатеричные строки от 8 символов, токены от 5
// символов из букв и цифр (временные каталоги, имена подов) и числа.
// Пробелы схлопываются.
func Normalize(line string) string {
	s := reTimestamp.ReplaceAllString(line, "*")
	s = reSyslogDay.ReplaceAllString(s, "*")
	s = reClock.ReplaceAllString(s, "*")
	s = reUUID.ReplaceAllString(s, "*")
	s = reHex.ReplaceAllString(s, "*")
	s = reAlnum.ReplaceAllStringFunc(s, func(m string) string {
		if reLetter.MatchString(m) && reDigit.MatchString(m) {
			return "*"
		}
		return m
	})
	s = reNumber.ReplaceAllString(s, "*")
	return strings.Join(strings.Fields(s), " ")
}

// Fingerprint — отпечаток сбоя: hex sha256 от окружения, источника и
// нормализованной строки ошибки (design, решение 7). Вызывать по полному
// выводу после redact.String, но до обрезки Tail: так отпечаток не зависит
// от длины вывода, а секреты в него не попадают.
func Fingerprint(envID, source, output string) string {
	sum := sha256.Sum256([]byte(envID + "|" + source + "|" + Normalize(ErrorLine(output))))
	return hex.EncodeToString(sum[:])
}
