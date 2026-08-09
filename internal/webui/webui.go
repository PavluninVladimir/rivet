// Package webui — встроенная прод-сборка консоли (rivet-web).
// Сборка кладётся в dist/ целью `make ui`; self-hosted получает UI
// из одного бинарника rivetd (design build-web-console).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler раздаёт статику консоли; неизвестные пути отдают index.html (SPA).
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
