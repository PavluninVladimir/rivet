package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Публичная проверка живости: 200 только при ответе базы, деталей нет
// (спека observability «Состояние установки»).
func TestHealth(t *testing.T) {
	t.Run("без базы — 503 без деталей", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("статус = %d, ожидался 503", rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("невалидный JSON: %v", err)
		}
		if body["status"] != "down" || len(body) != 1 {
			t.Fatalf("тело = %v, ожидалось только status=down", body)
		}
	})
	t.Run("с базой — 200", func(t *testing.T) {
		_, srv := testServer(t)
		resp, body := call(t, "GET", srv.URL+"/api/v1/health", "", "", nil)
		mustStatus(t, resp, http.StatusOK, "health")
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		var out map[string]string
		_ = json.Unmarshal(body, &out)
		if out["status"] != "ok" {
			t.Fatalf(`status = %q, ожидался "ok"`, out["status"])
		}
	})
}
