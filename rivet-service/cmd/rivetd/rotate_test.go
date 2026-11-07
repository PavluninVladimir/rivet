package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/config"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// Пара ключей проверяется до обращения к базе: оба заданы, новый не
// опубликован и не совпадает с прежним. Прежним может быть опубликованный.
func TestRotateBoxes(t *testing.T) {
	published := "cml2ZXQtbG9jYWwtZGV2LWtleS0zMi1ieXRlcyEhISE="
	k1, k2 := testKey(t), testKey(t)
	cases := []struct {
		name     string
		old, new string
		want     string
	}{
		{"без прежнего ключа", "", k1, "RIVET_SECRET_KEY_OLD"},
		{"без нового ключа", k1, "", "openssl rand -base64 32"},
		{"новый опубликован", k1, published, "опубликован"},
		{"прежний не base64", "не ключ", k1, "RIVET_SECRET_KEY_OLD"},
		{"ключи совпадают", k1, k1, "совпадают"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := rotateBoxes(config.Config{SecretKeyOld: c.old, SecretKey: c.new})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ошибка %v, ожидалось упоминание %q", err, c.want)
			}
		})
	}
	if _, _, err := rotateBoxes(config.Config{SecretKeyOld: published, SecretKey: k2}); err != nil {
		t.Fatalf("уход с опубликованного ключа должен разрешаться: %v", err)
	}
	_, _, err := rotateBoxes(config.Config{SecretKeyOld: k1, SecretKey: published})
	if !errors.Is(err, config.ErrPublished) {
		t.Fatalf("опубликованный новый ключ: %v", err)
	}
}
