// Package webui — встроенная прод-сборка консоли (rivet-web).
// Сборка кладётся в dist/ целью `make ui-sync` из коммита rivet-web,
// записанного в REVISION; CI сверяет dist со сборкой этого коммита
// (`make ui-check`). Self-hosted получает UI из одного бинарника rivetd
// (design build-web-console, fix-console-rivetd-tails).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// CSP — политика безопасности содержимого консоли (спека access-policy
// «Заголовки безопасности HTTP»). Всё только со своего адреса: модули
// сборки Vite, запросы к API и поток событий. Встроенных скриптов и eval
// нет. Встроенные стили нужны схемам Mermaid: SVG вставляется разметкой со
// своим <style> и атрибутами style, хэш или nonce для них невозможны.
// data: в картинках — стрелка select в forms.css. Внешних шрифтов нет.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler раздаёт статику консоли; неизвестные пути отдают index.html (SPA).
// У всех ответов заголовки безопасности: CSP, запрет встраивания во фрейм
// (X-Frame-Options для старых браузеров), запрет угадывать тип содержимого
// и Referer без адреса установки для внешних сайтов.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if f, err := sub.Open(path); err == nil {
				_ = f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA-fallback: всё остальное — index.html
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
