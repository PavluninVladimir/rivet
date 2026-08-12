package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Фаззинг webhook до границы БД (спека scm-integration «Аутентичность webhook»):
// store в Server не задан, поэтому упавший на БД путь означал бы нарушение
// инварианта «без валидной подписи обработка не начинается».

func fuzzSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// FuzzWebhookSignature: произвольные тело и подпись → всегда отказ (401,
// либо 422 на нечитаемом теле), никакой паники и обработки.
func FuzzWebhookSignature(f *testing.F) {
	f.Add([]byte(`{"action":"closed"}`), "sha256=deadbeef")
	f.Add([]byte(`{}`), "")
	f.Add([]byte(``), "sha256=")
	f.Fuzz(func(t *testing.T, body []byte, sig string) {
		const secret = "fuzz-secret"
		if sig == fuzzSign(secret, body) {
			t.Skip("валидная подпись — этот путь покрывает FuzzWebhookPayload")
		}
		srv := &Server{WebhookSecret: secret}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		// 422 допустим только для нечитаемого тела (превышен лимит чтения);
		// читаемое тело с плохой подписью обязано давать ровно 401 — иначе
		// разбор JSON начался раньше проверки подписи.
		want := http.StatusUnauthorized
		if len(body) > 1<<20 {
			want = http.StatusUnprocessableEntity
		}
		if rec.Code != want {
			t.Fatalf("невалидная подпись: ожидали %d, got %d (len body %d)", want, rec.Code, len(body))
		}
	})
}

// FuzzWebhookPayload: корректная подпись + ping-событие → разбор произвольного
// JSON без паники и без обращения к БД (ping отсекается после разбора).
func FuzzWebhookPayload(f *testing.F) {
	f.Add([]byte(`{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"merged":true,"head":{"ref":"agent/task-1"}}}`))
	f.Add([]byte(`не json`))
	f.Add([]byte(`{"pull_request":{"head":{"ref":""}}}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		const secret = "fuzz-secret"
		srv := &Server{WebhookSecret: secret}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", fuzzSign(secret, body))
		req.Header.Set("X-GitHub-Event", "ping")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("ping-событие: ожидали 200/422, got %d", rec.Code)
		}
	})
}
