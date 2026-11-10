package api

import "net/http"

// apiCSP — политика ответов API: API ничего не рисует, поэтому ответ,
// случайно открытый как документ, не грузит ресурсов и не встраивается во
// фрейм (спека access-policy «Заголовки безопасности HTTP»).
const apiCSP = "default-src 'none'; frame-ancestors 'none'"

// BaseHeaders ставит заголовки, общие для всех ответов rivetd: запрет
// угадывать тип содержимого и Referer без адреса установки для внешних
// сайтов. rivetd оборачивает им корневой ServeMux, поэтому заголовки есть и
// у его собственных ответов (редирект на /api/, пути с // и ..). CSP свой у
// консоли и у API.
func BaseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders ставит заголовки безопасности на каждый ответ API, в
// том числе на 401 до аутентификации: общие (BaseHeaders) и политику без
// ресурсов.
func withSecurityHeaders(next http.Handler) http.Handler {
	return BaseHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", apiCSP)
		next.ServeHTTP(w, r)
	}))
}
