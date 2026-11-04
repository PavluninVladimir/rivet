// Package redact — маскирование секретов в runner-controlled тексте:
// транскриптах, live-потоке, результатах стадий и вопросах агента
// (спека team-visibility «Видимость по умолчанию и приватность»).
// Маскируется только совпавший фрагмент, не строка целиком; ложные
// срабатывания допустимы — это текст для чтения, не данные.
package redact

import (
	"regexp"
	"strings"
)

const mask = "***"

// rule маскирует либо совпадение целиком (group=0), либо оставляет
// префиксную группу и маскирует остаток (group=1: `$1***`). fn, если
// задана, сама решает, чем заменить совпадение (подгруппы читает по re).
type rule struct {
	re    *regexp.Regexp
	group int
	fn    func(re *regexp.Regexp, m string) string
}

// prefixed — замена «префикс + обработанное значение»: g[1] остаётся, g[2]
// проходит через f.
func prefixed(f func(string) string) func(*regexp.Regexp, string) string {
	return func(re *regexp.Regexp, m string) string {
		g := re.FindStringSubmatch(m)
		return g[1] + f(g[2])
	}
}

// ─── пароль в URL ───────────────────────────────────────────────────────

// portLike — начало «пароля», которое на самом деле порт с путём, query
// или фрагментом (`host:8500/api?email=a@b`, `host:8080/users/@me`).
// Цифры прямо перед «@» без пути — это пароль (`postgres:12345@localhost`).
// Неоднозначность: пароль, который начинается с цифр и «/»
// (`app:2024/secret@db`), неотличим от порта с путём, где дальше в пути
// есть «@», и такой пароль не маскируется. Пароли со «/» по RFC должны
// кодироваться (%2F), так что это редкий случай против частого ложного
// срабатывания на URL с «@» в пути и query.
// Кавычка, угловая скобка или запятая после цифр — тоже порт: URL в
// компактном JSON или HTML (`"http://localhost:8500","email":"a@b"`).
var portLike = regexp.MustCompile(`^\d{1,5}[/?#"'<>,]`)

// dsnPassword маскирует пароль в userinfo URL. Основное правило берёт
// пароль жадно до последнего «@» токена: «/», «@», «?», «#» и «+» в нём
// бывают, а «@» встречается и дальше в query. Лучше замаскировать лишнее,
// чем пропустить пароль.
func dsnPassword(re *regexp.Regexp, m string) string {
	g := re.FindStringSubmatch(m)
	if portLike.MatchString(g[2]) {
		return m
	}
	return g[1] + mask + "@"
}

// ─── Authorization ──────────────────────────────────────────────────────

var (
	// knownScheme — известные схемы: после них маскируется токен от 8
	// символов или с цифрой («Bearer token берётся из заголовка» — проза).
	knownScheme = regexp.MustCompile(`(?i)^(bearer|basic|token|digest|negotiate|apikey|ntlm|aws4-hmac-sha256)(\s+)`)
	// anyScheme — слово перед пробелом, похожее на схему (Bot, SharedKey,
	// OAuth, Splunk): схемой считается, только если само не похоже на
	// учётные данные.
	anyScheme = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]{1,30})(\s+)`)
	// authToken — один токен учётных данных.
	authToken = regexp.MustCompile(`^[^\s'",;\]]+`)
	// authParam — параметр Digest и AWS4: k=v и k="v".
	authParam = regexp.MustCompile(`(\b[\w-]+=)("[^"]*"|[^\s,"\]]+)`)
	// quotedParam и startsQuotedParam — параметры вида k="v" (OAuth 1.0)
	// после схемы вне списка. Пары без кавычек (`denied user=ivan`) — это
	// logfmt, а не параметры схемы.
	quotedParam       = regexp.MustCompile(`(\b[\w-]+=)("[^"]*")`)
	startsQuotedParam = regexp.MustCompile(`^[\w-]+="`)
	// leadQuote — открывающие кавычки и скобки значения: YAML, конфигурации,
	// `map[Authorization:[Bearer …]]` из Go.
	leadQuote = regexp.MustCompile(`^[\['"]*`)
	// headerAuth — заголовок и YAML через «:», logfmt через «=» без
	// пробелов: код `const authorization = …` не трогается.
	headerAuth = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization(?::[ \t]*|=))([^\r\n]*)`)
)

// credentialLike — токен без схемы похож на учётные данные: только
// печатный ASCII и либо от 16 символов, либо буквы вместе с цифрами.
// Слова прозы («the», «проверка», «denied», «**») не похожи.
func credentialLike(tok string) bool {
	letters, digits := false, false
	for _, r := range tok {
		switch {
		case r < 0x21 || r > 0x7e:
			return false
		case r >= '0' && r <= '9':
			digits = true
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters = true
		}
	}
	return len(tok) >= 16 || (letters && digits)
}

// afterKnownScheme — токен после известной схемы маскируется, если он от 8
// символов или с цифрой.
func afterKnownScheme(tok string) bool {
	return len(tok) >= 8 || strings.ContainsAny(tok, "0123456789")
}

// maskAuth маскирует значение Authorization:
//   - после известной схемы один токен от 8 символов или с цифрой, у Digest
//     и AWS4 значения параметров;
//   - после другой схемы значения параметров k="v" (OAuth) или следующий
//     токен, если он похож на учётные данные;
//   - без схемы первый токен, если он похож на учётные данные.
//
// После замаскированного токена разбор продолжается по остатку строки:
// второй заголовок в той же строке лога тоже маскируется. Остальное
// (logfmt, проза ревью, код) остаётся как есть.
func maskAuth(v string) string {
	lead := leadQuote.FindString(v)
	rest := v[len(lead):]
	if m := knownScheme.FindStringSubmatch(rest); m != nil {
		after := rest[len(m[0]):]
		switch strings.ToLower(m[1]) {
		case "digest", "aws4-hmac-sha256":
			return lead + m[0] + authParam.ReplaceAllString(after, "${1}"+mask)
		}
		if tok := authToken.FindString(after); tok != "" && afterKnownScheme(tok) {
			return lead + m[0] + mask + continueAuth(after[len(tok):])
		}
		return v
	}
	if m := anyScheme.FindStringSubmatch(rest); m != nil && !credentialLike(m[1]) {
		after := rest[len(m[0]):]
		if startsQuotedParam.MatchString(after) {
			return lead + m[0] + quotedParam.ReplaceAllString(after, "${1}"+mask)
		}
		if tok := authToken.FindString(after); tok != "" && credentialLike(tok) {
			return lead + m[0] + mask + continueAuth(after[len(tok):])
		}
		return v
	}
	if tok := authToken.FindString(rest); tok != "" && credentialLike(tok) {
		return lead + mask + continueAuth(rest[len(tok):])
	}
	return v
}

// continueAuth ищет следующий заголовок Authorization в остатке строки.
func continueAuth(s string) string {
	return headerAuth.ReplaceAllStringFunc(s, func(m string) string {
		g := headerAuth.FindStringSubmatch(m)
		return g[1] + maskAuth(g[2])
	})
}

// ─── Cookie ─────────────────────────────────────────────────────────────

var (
	// Пары name=value: в заголовке значение в кавычках или до «;» (запятая
	// бывает внутри значения), в JSON с экранированными кавычками или до
	// кавычки.
	cookiePairHeader = regexp.MustCompile(`([^\s=;"\[]+=)("[^"]*"|[^\s;\]]+)`)
	cookiePairJSON   = regexp.MustCompile(`([^\s=;"\\]+=)(\\"(?:[^"\\]|\\.)*?\\"|[^\s;"\\]+)`)
	// cookieStartsPair — значение начинается с пары name=value.
	cookieStartsPair = regexp.MustCompile(`^[^\s=;"\\\[]+=`)
)

// maskCookie маскирует значение Cookie и Set-Cookie: если оно начинается
// с пары name=value, значения всех пар. Иначе первый токен, только если он
// похож на учётные данные. Проза («Set-Cookie: sid задаётся без флага
// Secure, добавь SameSite=Lax», «Cookie: используем HttpOnly») остаётся
// читаемой.
func maskCookie(pair *regexp.Regexp) func(string) string {
	return func(v string) string {
		lead := leadQuote.FindString(v)
		rest := v[len(lead):]
		if cookieStartsPair.MatchString(rest) {
			return lead + pair.ReplaceAllString(rest, "${1}"+mask)
		}
		if tok := authToken.FindString(rest); tok != "" && credentialLike(tok) {
			return lead + mask + rest[len(tok):]
		}
		return v
	}
}

// cookieLogfmt маскирует в logfmt только первую пару после `cookie=`:
// остальные поля строки (`path=/api status=500`) не трогаются.
func cookieLogfmt(re *regexp.Regexp, m string) string {
	g := re.FindStringSubmatch(m)
	return g[1] + g[2] + mask
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
	// Authorization, Proxy-Authorization, X-…-Authorization:
	//   - ключ JSON-лога (pino-http, winston, ECS и OpenTelemetry с
	//     префиксом `http.request.header.`), значение строкой или массивом,
	//     до закрывающей неэкранированной кавычки;
	//   - то же в экранированном JSON внутри строки сообщения, значение до \";
	//   - заголовок и YAML через «:», logfmt через «=» без пробелов. Код
	//     `const authorization = …` с пробелами вокруг «=» не трогается.
	{re: regexp.MustCompile(`(?i)("(?:[\w.-]*[.-])?(?:proxy-|x-)?authorization"\s*:\s*\[?\s*")((?:[^"\\]|\\.)*)`), fn: prefixed(maskAuth)},
	{re: regexp.MustCompile(`(?i)(\\"(?:[\w.-]*[.-])?(?:proxy-|x-)?authorization\\"\s*:\s*\[?\s*\\")((?:[^"\\]|\\[^"])*)`), fn: prefixed(maskAuth)},
	{re: headerAuth, fn: prefixed(maskAuth)},
	// Пары password/token/secret/api_key = значение (и через двоеточие);
	// ключ может быть суффиксом имени переменной (DB_SECRET, GITHUB_TOKEN).
	{re: regexp.MustCompile(`(?i)\b([\w-]*(?:password|passwd|token|secret|api[_-]?key|apikey|access[_-]?key)"?\s*[=:]\s*)("[^"]{4,}"|'[^']{4,}'|[^\s"',;]{4,})`), group: 1},
	// Пароль в URL подключения: postgres://app:pass@db, redis://:pass@cache.
	// Пустой пароль (`https://user:@host`) не маскируется.
	{re: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]*:)([^\s"'<>]+)@`), fn: dsnPassword},
	// Повтор для пароля с кавычками и угловыми скобками, которые основное
	// правило не пропускает: группа до пробела без «@», где такой символ есть.
	{re: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@"'<>]*:)([^\s@]*['"<>][^\s@]*)@`), fn: dsnPassword},
	// Cookie и Set-Cookie: ключ JSON-лога (с префиксом, строкой или
	// массивом), то же в экранированном JSON, заголовок через «:» до конца
	// строки, logfmt `cookie=` только первая пара.
	{re: regexp.MustCompile(`(?i)("(?:[\w.-]*[.-])?(?:set-)?cookie"\s*:\s*\[?\s*")((?:[^"\\]|\\.)*)`), fn: prefixed(maskCookie(cookiePairJSON))},
	{re: regexp.MustCompile(`(?i)(\\"(?:[\w.-]*[.-])?(?:set-)?cookie\\"\s*:\s*\[?\s*\\")((?:[^"\\]|\\[^"])*)`), fn: prefixed(maskCookie(cookiePairJSON))},
	{re: regexp.MustCompile(`(?i)(\b(?:set-)?cookie:[ \t]*)([^\r\n]*)`), fn: prefixed(maskCookie(cookiePairHeader))},
	{re: regexp.MustCompile(`(?i)(\bcookie=)((?:[^\s=;"]+=)?)("[^"]*"|[^\s;]+)`), fn: cookieLogfmt},
	// Голый JWT: base64url-части через точку, заголовок начинается с eyJ,
	// заголовок и тело не короче 5 символов, подпись может быть пустой
	// (alg none).
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)},
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
		if r.fn != nil {
			re, fn := r.re, r.fn
			s = re.ReplaceAllStringFunc(s, func(m string) string { return fn(re, m) })
			continue
		}
		if r.group == 0 {
			s = r.re.ReplaceAllString(s, mask)
		} else {
			s = r.re.ReplaceAllString(s, "${1}"+mask)
		}
	}
	return s
}
