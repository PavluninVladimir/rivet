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

// Фаззинг внешнего ввода webhook (спека scm-integration «Аутентичность
// webhook»): разбор тела события и проверка подлинности. Таргеты работают
// на уровне функций, а не всего обработчика: выбор проекта по репозиторию
// требует БД, а защищают эти таргеты именно разбор и сравнение подписи.

func fuzzSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// FuzzWebhookPayload: произвольное тело разбирается без паники, а
// «сломанный» разбор не выдаёт данных, по которым конвейер что-то делает.
func FuzzWebhookPayload(f *testing.F) {
	f.Add([]byte(`{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"merged":true,"head":{"ref":"agent/task-1"}}}`))
	f.Add([]byte(`{"object_kind":"merge_request","project":{"path_with_namespace":"g/s/p"},"object_attributes":{"action":"merge","source_branch":"b"}}`))
	f.Add([]byte(`не json`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"pull_request":{"head":{"ref":""}}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		gh := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		gh.Header.Set("X-GitHub-Event", "pull_request")
		gl := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
		gl.Header.Set("X-Gitlab-Event", "Merge Request Hook")

		for _, ev := range []hostingEvent{parseGitHubEvent(gh, body), parseGitLabEvent(gl, body)} {
			if ev.Malformed {
				// Нечитаемое тело не должно давать данных, по которым
				// конвейер что-то предпримет.
				if ev.RepoPath != "" || ev.Branch != "" || ev.Kind != "" {
					t.Fatalf("сломанный разбор вернул данные: %+v", ev)
				}
				continue
			}
			// Событие без репозитория разобрать можно (хостинг мог прислать
			// урезанное тело), но обработчик такое игнорирует: выбрать
			// проект и ключ проверки подписи не по чему. Это проверяет
			// TestWebhookIgnoresEventWithoutRepo.
		}
	})
}

// FuzzWebhookSignature: произвольные тело и подпись принимаются только при
// точном совпадении HMAC (GitHub) или токена (GitLab).
func FuzzWebhookSignature(f *testing.F) {
	f.Add([]byte(`{"action":"closed"}`), "sha256=deadbeef")
	f.Add([]byte(`{}`), "")
	f.Add([]byte(``), "sha256=")
	f.Fuzz(func(t *testing.T, body []byte, sig string) {
		const secret = "fuzz-secret"
		gh := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		gh.Header.Set("X-Hub-Signature-256", sig)
		if got, want := verifyGitHubSignature(gh, body, secret), sig == fuzzSign(secret, body); got != want {
			t.Fatalf("HMAC: подпись %q для тела длиной %d дала %v, ожидалось %v", sig, len(body), got, want)
		}

		gl := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
		gl.Header.Set("X-Gitlab-Token", sig)
		if got, want := verifyGitLabToken(gl, body, secret), sig == secret; got != want {
			t.Fatalf("токен GitLab: %q дал %v, ожидалось %v", sig, got, want)
		}
	})
}
