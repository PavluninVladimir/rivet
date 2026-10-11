// Package redact — маскирование секретов в runner-controlled тексте:
// транскриптах, live-потоке, результатах стадий и вопросах агента
// (спека team-visibility «Видимость по умолчанию и приватность»).
// Маскируется только совпавший фрагмент, не строка целиком; ложные
// срабатывания допустимы — это текст для чтения, не данные.
package redact

import "regexp"

const mask = "***"

// rule маскирует либо совпадение целиком (group=0), либо оставляет
// префиксную группу и маскирует остаток (group=1: `$1***`).
type rule struct {
	re    *regexp.Regexp
	group int
}

var rules = []rule{
	// Известные префиксы токенов: GitHub, Anthropic/OpenAI, Slack, AWS,
	// собственные секреты Rivet (rvt_ PAT, rvs_ cookie-сессии).
	{re: regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}`)},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`)},
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{re: regexp.MustCompile(`\bxox[a-z]-[A-Za-z0-9-]{10,}`)},
	{re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{re: regexp.MustCompile(`\brv[ts]_[A-Za-z0-9_-]{16,}`)},
	// Authorization: Bearer <значение>.
	{re: regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`), group: 1},
	// Пары password/token/secret/api_key = значение (и через двоеточие);
	// ключ может быть суффиксом имени переменной (DB_SECRET, GITHUB_TOKEN).
	{re: regexp.MustCompile(`(?i)\b([\w-]*(?:password|passwd|token|secret|api[_-]?key|apikey|access[_-]?key)"?\s*[=:]\s*)("[^"]{4,}"|'[^']{4,}'|[^\s"',;]{4,})`), group: 1},
}

// pemBlock — PEM-блок приватного ключа целиком (для маскирования цельного
// текста; в потоке тело блока маскирует построчный Stream).
var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|\z)`)

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// String маскирует секреты в цельном тексте (включая многострочные PEM-блоки).
func String(s string) string {
	s = pemBlock.ReplaceAllString(s, mask)
	return applyRules(s)
}

// Bytes — String для []byte (транскрипт перед сохранением в blob).
func Bytes(b []byte) []byte { return []byte(String(string(b))) }

func applyRules(s string) string {
	for _, r := range rules {
		if r.group == 0 {
			s = r.re.ReplaceAllString(s, mask)
		} else {
			s = r.re.ReplaceAllString(s, "${1}"+mask)
		}
	}
	return s
}
