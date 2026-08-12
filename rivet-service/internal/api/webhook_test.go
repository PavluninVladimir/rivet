package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Аутентичность webhook (спека scm-integration): fail-closed без секрета,
// отклонение невалидной подписи, приём валидной. Проверки подписи идут до
// обращения к БД, поэтому store здесь не нужен.
func postWebhook(t *testing.T, srv *Server, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "ping")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookFailClosedWithoutSecret(t *testing.T) {
	srv := &Server{} // секрет не настроен
	if rec := postWebhook(t, srv, `{}`, sign("любой", `{}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("без секрета ожидали 403, got %d", rec.Code)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	srv := &Server{WebhookSecret: "s3cret"}
	if rec := postWebhook(t, srv, `{}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без подписи ожидали 401, got %d", rec.Code)
	}
	if rec := postWebhook(t, srv, `{}`, sign("другой-секрет", `{}`)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("с чужой подписью ожидали 401, got %d", rec.Code)
	}
	// Подпись от другого тела тоже отклоняется.
	if rec := postWebhook(t, srv, `{"a":1}`, sign("s3cret", `{}`)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("с подписью другого тела ожидали 401, got %d", rec.Code)
	}
}

func TestWebhookAcceptsValidSignature(t *testing.T) {
	srv := &Server{WebhookSecret: "s3cret"}
	body := `{"action":"opened"}`
	rec := postWebhook(t, srv, body, sign("s3cret", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("с валидной подписью ожидали 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("ping-событие должно игнорироваться: %s", rec.Body.String())
	}
}
