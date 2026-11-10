package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Заголовки безопасности консоли (спека access-policy «Заголовки
// безопасности HTTP», fix-console-rivetd-tails): у index.html, ассета
// сборки и SPA-fallback одни и те же.
func TestHandlerSecurityHeaders(t *testing.T) {
	assets, err := fs.ReadDir(distFS, "dist/assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("во встроенной сборке нет ассетов: %v", err)
	}
	h := Handler()
	for _, path := range []string{"/", "/assets/" + assets[0].Name(), "/p/x/tasks"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: статус %d", path, rec.Code)
		}
		want := map[string]string{
			"Content-Security-Policy": CSP,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "same-origin",
			"X-Frame-Options":         "DENY",
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, ждали %q", path, k, got, v)
			}
		}
	}
}

// Политика строгая там, где это возможно: скрипты только свои, без
// встроенных и eval, фреймы и плагины запрещены, источников вне своего
// адреса нет.
func TestCSPStrict(t *testing.T) {
	directives := map[string]string{}
	for _, d := range strings.Split(CSP, ";") {
		f := strings.Fields(d)
		if len(f) > 0 {
			directives[f[0]] = strings.Join(f[1:], " ")
		}
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"connect-src":     "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
	} {
		if directives[name] != want {
			t.Errorf("%s = %q, ждали %q", name, directives[name], want)
		}
	}
	for _, bad := range []string{"unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(CSP, bad) {
			t.Errorf("в политике %q: %s", bad, CSP)
		}
	}
	if strings.Contains(directives["script-src"], "unsafe-inline") {
		t.Errorf("встроенные скрипты разрешены: %s", CSP)
	}
}

// Сборка консоли совместима с политикой: в index.html нет встроенных
// скриптов и обработчиков, скрипты и стили — файлы сборки.
func TestIndexWithoutInlineScripts(t *testing.T) {
	raw, err := fs.ReadFile(distFS, "dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for i := 0; ; {
		j := strings.Index(html[i:], "<script")
		if j < 0 {
			break
		}
		tag := html[i+j : i+j+strings.Index(html[i+j:], ">")+1]
		if !strings.Contains(tag, ` src="/assets/`) {
			t.Errorf("встроенный скрипт в index.html: %s", tag)
		}
		i += j + len(tag)
	}
	for _, attr := range []string{" onload=", " onclick=", " onerror=", "javascript:"} {
		if strings.Contains(html, attr) {
			t.Errorf("в index.html %q", attr)
		}
	}
}
