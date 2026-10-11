package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Enabled() {
		t.Fatal("с ключом Box должен быть включён")
	}
	const token = "ghp_ExampleToken0123456789"
	sealed, err := b.Seal(token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), token) {
		t.Fatal("шифротекст содержит исходный секрет")
	}
	got, err := b.Open(sealed)
	if err != nil || got != token {
		t.Fatalf("round-trip: %q, %v", got, err)
	}

	// Один и тот же секрет шифруется по-разному: nonce случайный.
	again, err := b.Seal(token)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) == string(sealed) {
		t.Fatal("два шифрования одного секрета совпали — nonce не случайный")
	}
}

func TestOpenRejectsTamperedAndForeign(t *testing.T) {
	b, _ := New(testKey(t))
	sealed, err := b.Seal("secret-value")
	if err != nil {
		t.Fatal(err)
	}

	bad := append([]byte(nil), sealed...)
	bad[len(bad)-1] ^= 0xff
	if _, err := b.Open(bad); err == nil {
		t.Fatal("испорченный шифротекст должен отклоняться")
	}
	if _, err := b.Open(sealed[:3]); err == nil {
		t.Fatal("слишком короткий вход должен отклоняться")
	}

	other, _ := New(testKey(t))
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("чужой ключ не должен расшифровывать")
	}
}

// Без ключа операции с токенами отключены, но конструктор не падает:
// установка работает на глобальном токене (design, решение 4).
func TestNoKeyIsFailClosed(t *testing.T) {
	b, err := New("")
	if err != nil {
		t.Fatalf("пустой ключ не должен быть ошибкой конфигурации: %v", err)
	}
	if b.Enabled() {
		t.Fatal("без ключа Box должен быть выключен")
	}
	if _, err := b.Seal("x"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Seal без ключа: %v", err)
	}
	if _, err := b.Open([]byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Open без ключа: %v", err)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New("не base64!"); err == nil {
		t.Fatal("не-base64 ключ должен отклоняться")
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := New(short); err == nil {
		t.Fatal("ключ неверной длины должен отклоняться")
	}
}
