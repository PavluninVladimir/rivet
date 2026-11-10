package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Заголовки безопасности у ответов API (спека access-policy «Заголовки
// безопасности HTTP», fix-console-rivetd-tails): и у успешного ответа, и у
// 401 без аутентификации, и у неизвестного пути.
func TestAPISecurityHeaders(t *testing.T) {
	_, srv := testServer(t)
	for path, code := range map[string]int{
		"/api/v1/health":   http.StatusOK,
		"/api/v1/projects": http.StatusUnauthorized,
		"/api/v1/nowhere":  http.StatusUnauthorized,
	} {
		resp, _ := call(t, "GET", srv.URL+path, "", "", nil)
		mustStatus(t, resp, code, path)
		for k, v := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "same-origin",
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		} {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, ждали %q", path, k, got, v)
			}
		}
	}
}

// Корневой ServeMux rivetd сам отвечает редиректами (подкаталог без слеша,
// пути с // и ..): обёрнутый BaseHeaders, он ставит общие заголовки и на
// них (fix-console-rivetd-tails, ревью P3-2).
func TestBaseHeadersOnRootMuxRedirects(t *testing.T) {
	root := http.NewServeMux()
	root.Handle("/api/", http.NotFoundHandler())
	root.Handle("/", http.NotFoundHandler())
	srv := httptest.NewServer(BaseHeaders(root))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/api", "/api//v1/health", "/x/../api/v1"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.URL.Opaque = path // без нормализации пути клиентом
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			t.Fatalf("%s: ждали редирект корневого mux, статус %d", path, resp.StatusCode)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: заголовки редиректа %v", path, resp.Header)
		}
	}
}
