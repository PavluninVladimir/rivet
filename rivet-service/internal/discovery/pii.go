package discovery

import (
	"regexp"
	"strings"
)

// Признаки персональных данных в тексте задачи (add-product-discovery):
// email, телефон, имена и описания респондентов из метаданных интервью.
// Дословный пересказ требования — не персональные данные, его проверяет
// QuoteLeak мягче.

var (
	emailRe = regexp.MustCompile(`[\p{L}\p{N}._%+-]+@[\p{L}\p{N}-]+(\.[\p{L}\p{N}-]+)+`)
	// Телефон: +7 или 8, затем 10 цифр с пробелами, скобками и дефисами,
	// либо международный номер от 10 цифр с плюсом.
	phoneRe = regexp.MustCompile(`(?:\+7|\b8)[\s(-]*\d{3}[\s)-]*\d{3}[\s-]*\d{2}[\s-]*\d{2}\b|\+\d(?:[\s()-]*\d){9,}`)
	// Имя с инициалом («Кирилл Л.») и имя с фамилией («Анна Смирнова»).
	nameInitialRe = regexp.MustCompile(`\p{Lu}\p{Ll}{2,}\s+\p{Lu}\.`)
	fullNameRe    = regexp.MustCompile(`\p{Lu}\p{Ll}{2,}\s+\p{Lu}\p{Ll}{2,}`)
	// Название в кавычках («ТехноПривод», "Вектор") — обычно компания.
	quotedNameRe = regexp.MustCompile(`[«"]([^«»"]{3,60})[»"]`)
)

// PIIMarkers — признаки респондентов из метаданных интервью (название и
// роль): имена, названия в кавычках и полные описания роли от 4 слов.
// Короткая роль («Менеджер по продажам») признаком не считается: так
// пишут и о пользователях продукта.
func PIIMarkers(meta []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		n := strings.TrimSpace(NormalizeTitle(strings.Trim(s, ".,;: ")))
		if len([]rune(n)) < 3 || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, m := range meta {
		for _, s := range nameInitialRe.FindAllString(m, -1) {
			add(strings.TrimSuffix(s, "."))
		}
		for _, s := range fullNameRe.FindAllString(m, -1) {
			add(s)
		}
		for _, g := range quotedNameRe.FindAllStringSubmatch(m, -1) {
			add(g[1])
		}
		if len(leakWords(m)) >= 4 {
			add(m)
		}
	}
	return out
}

// PIISign — признак персональных данных в тексте: email, телефон или
// признак респондента из markers (см. PIIMarkers). Возвращает вид
// признака без самого значения: его нельзя писать в логи.
func PIISign(text string, markers []string) (string, bool) {
	if emailRe.MatchString(text) {
		return "email", true
	}
	if phoneRe.MatchString(text) {
		return "телефон", true
	}
	norm := " " + strings.Join(leakWords(text), " ") + " "
	for _, m := range markers {
		if w := strings.Join(leakWords(m), " "); w != "" && strings.Contains(norm, " "+w+" ") {
			return "имя или описание респондента", true
		}
	}
	return "", false
}
