package llm

import (
	"errors"
	"testing"
)

// Локальный адрес сервера модели определяется по хосту целиком, без
// подстрок и DNS (fix-agent-without-connection).
func TestIsLocalURL(t *testing.T) {
	for u, want := range map[string]bool{
		"http://localhost:11434/v1":                true,
		"http://LOCALHOST:1234/v1":                 true,
		"http://api.localhost/v1":                  true,
		"http://127.0.0.1:8080/v1":                 true,
		"http://127.0.0.2/v1":                      true,
		"http://[::1]:11434/v1":                    true,
		"http://[0:0:0:0:0:0:0:1]/v1":              true,
		"http://[::ffff:127.0.0.1]/v1":             true,
		"http://localhost.evil.com/v1":             false,
		"http://127.0.0.1.nip.io/v1":               false,
		"https://api.example.com/v1//localhost":    false,
		"https://api.example.com/v1?x=//127.0.0.1": false,
		"https://api.deepseek.com":                 false,
		"http://192.168.1.10:11434/v1":             false,
		"":                                         false,
		"not a url":                                false,
	} {
		if got := IsLocalURL(u); got != want {
			t.Errorf("IsLocalURL(%q) = %v, ждали %v", u, got, want)
		}
	}
}

// Отказ по ключу несёт код ответа: 401 — ключ отклонён, 403 — у ключа нет
// прав. Текст прежний, по нему control plane читает вердикт runner'а.
func TestAuthErrorStatus(t *testing.T) {
	e401, e403 := authError(401), authError(403)
	if !errors.Is(e401, ErrUnauthorized) || !errors.Is(e403, ErrUnauthorized) {
		t.Fatal("AuthError — это ErrUnauthorized")
	}
	if !KeyRejected(e401) || KeyRejected(e403) || KeyRejected(errors.New("HTTP 401")) {
		t.Fatal("KeyRejected только для 401")
	}
	if e401.Error() != "провайдер отклонил ключ: HTTP 401" {
		t.Fatalf("текст отказа: %q", e401.Error())
	}
	for text, want := range map[string]int{
		"агент завершился с ошибкой: ход 2: " + e401.Error() + "\nвывод":         401,
		"ревьюер завершился с ошибкой: " + e403.Error():                          403,
		"агент завершился с ошибкой: нет ключа подключения":                      0,
		"агент завершился с ошибкой: провайдер отклонил ключ":                    0,
		"агент завершился с ошибкой: инструмент упал\nв выводе: " + e401.Error(): 0,
	} {
		if got := RejectionStatus(text); got != want {
			t.Fatalf("%q: %d, ожидался %d", text, got, want)
		}
	}
}
