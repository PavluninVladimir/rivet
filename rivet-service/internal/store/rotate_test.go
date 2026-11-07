package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

func rotateBox(t *testing.T) (*secretbox.Box, string) {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(k)
	b, err := secretbox.New(b64)
	if err != nil {
		t.Fatal(err)
	}
	return b, b64
}

// Перешифровка (спека access-policy «Смена опубликованного ключа на
// работающей установке»): после неё токены хостингов, ключи и секретные
// заголовки подключений читаются новым ключом и не читаются старым, в
// event log событие без значений.
func TestRotateSecrets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldBox, oldKey := rotateBox(t)
	newBox, newKey := rotateBox(t)
	admin := mustOwner(t, s)

	const token = "ghp_RotateTestToken000"
	p, err := s.CreateProjectWithRepo(ctx, "rotate", nil, admin.ID, NewRepoConnection{
		Provider: "github", BaseURL: "https://github.com", RepoPath: "acme/rotate", Token: token, TokenOwner: "bot",
	}, oldBox)
	if err != nil {
		t.Fatal(err)
	}
	key, secretHeader, plainHeader := "sk-rotate-test-key-000", "header-secret-000", "plain-value"
	headers := []HeaderInput{{Name: "X-Secret", Value: &secretHeader, Secret: true}, {Name: "X-Plain", Value: &plainHeader}}
	if _, _, err := s.UpsertConnection(ctx, ConnectionInput{
		ID: "rotate-demo", Name: "Rotate", Kind: "vendor", API: "openai", BaseURL: "https://api.example.com/v1",
		Key: &key, Headers: &headers,
	}, oldBox, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	// Локальное подключение без ключа: перешифровывать нечего.
	if _, _, err := s.UpsertConnection(ctx, ConnectionInput{
		ID: "rotate-local", Name: "Local", Kind: "local", API: "openai", BaseURL: "http://localhost:1234/v1",
	}, oldBox, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}

	t.Run("неверный прежний ключ ничего не меняет", func(t *testing.T) {
		wrong, _ := rotateBox(t)
		_, err := s.RotateSecrets(ctx, wrong, newBox)
		var ue *UndecryptableError
		if !errors.As(err, &ue) || len(ue.Items) != 3 {
			t.Fatalf("ожидалась ошибка с тремя нечитаемыми секретами, получено %v", err)
		}
		// Токен назван так, как его видно в консоли: хостинг, владелец, проект.
		if !strings.Contains(err.Error(), "github https://github.com (владелец bot), проекты: rotate") {
			t.Fatalf("подпись токена хостинга: %v", err)
		}
		if got, err := s.ProjectToken(ctx, p.ID, oldBox); err != nil || got != token {
			t.Fatalf("после отказа токен должен читаться прежним ключом: %q, %v", got, err)
		}
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='secret_key.rotated'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("событие при отказе: %d, %v", n, err)
		}
	})

	t.Run("при работающем rivetd перешифровка не идёт", func(t *testing.T) {
		release, err := s.HoldServeLock(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := s.RotateSecrets(ctx, oldBox, newBox); !errors.Is(err, ErrServeRunning) {
			t.Fatalf("ожидался отказ из-за работающего rivetd: %v", err)
		}
		if got, err := s.ProjectToken(ctx, p.ID, oldBox); err != nil || got != token {
			t.Fatalf("после отказа токен должен читаться прежним ключом: %q, %v", got, err)
		}
	})

	waitNoServeLock(t, s)
	res, err := s.RotateSecrets(ctx, oldBox, newBox)
	if err != nil {
		t.Fatal(err)
	}
	if res != (RotateResult{Credentials: 1, ConnectionKeys: 1, ConnectionHeaders: 1}) {
		t.Fatalf("результат: %+v", res)
	}

	if got, err := s.ProjectToken(ctx, p.ID, newBox); err != nil || got != token {
		t.Fatalf("токен новым ключом: %q, %v", got, err)
	}
	if _, err := s.ProjectToken(ctx, p.ID, oldBox); err == nil {
		t.Fatal("токен не должен читаться прежним ключом")
	}
	cl, _, err := s.ConnectionClient(ctx, "rotate-demo", newBox)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Key != key || cl.Headers["X-Secret"] != secretHeader || cl.Headers["X-Plain"] != plainHeader {
		t.Fatalf("подключение новым ключом: key=%q headers=%v", cl.Key, cl.Headers)
	}
	if _, _, err := s.ConnectionClient(ctx, "rotate-demo", oldBox); !errors.Is(err, ErrSecret) {
		t.Fatalf("подключение прежним ключом должно не читаться: %v", err)
	}

	// Событие уровня установки без значений секретов и ключей.
	var text string
	var payload []byte
	if err := s.Pool.QueryRow(ctx, `SELECT text, payload FROM events WHERE type='secret_key.rotated' AND project_id IS NULL`).Scan(&text, &payload); err != nil {
		t.Fatal(err)
	}
	all := text + string(payload)
	for _, secret := range []string{token, key, secretHeader, oldKey, newKey} {
		if strings.Contains(all, secret) {
			t.Fatalf("событие содержит секрет или ключ: %s", all)
		}
	}
	var got RotateResult
	if err := json.Unmarshal(payload, &got); err != nil || got != res {
		t.Fatalf("payload события: %s, %v", payload, err)
	}

	// Повтор с теми же ключами: всё уже под новым ключом, ничего не ломается.
	again, err := s.RotateSecrets(ctx, oldBox, newBox)
	if err != nil {
		t.Fatal(err)
	}
	if again != (RotateResult{AlreadyNew: 3}) {
		t.Fatalf("повтор: %+v", again)
	}
	if got, err := s.ProjectToken(ctx, p.ID, newBox); err != nil || got != token {
		t.Fatalf("токен после повтора: %q, %v", got, err)
	}
}

// Заголовки разбираются как сырой JSON: поля, которых код не знает,
// переносятся без изменений, открытые значения не трогаются.
func TestRecryptHeadersKeepsUnknownFields(t *testing.T) {
	raw := []byte(`[{"name":"X-A","value_enc":"AQID","secret":true,"future":{"x":1}},{"name":"X-B","value":"v","secret":false}]`)
	out, n, err := recryptHeaders(raw, func(enc []byte, name string) ([]byte, bool, error) {
		if name != "X-A" || !bytes.Equal(enc, []byte{1, 2, 3}) {
			t.Fatalf("неожиданный вызов: %q %v", name, enc)
		}
		return []byte{9}, true, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var hs []map[string]any
	if err := json.Unmarshal(out, &hs); err != nil {
		t.Fatal(err)
	}
	if hs[0]["value_enc"] != "CQ==" || hs[0]["future"] == nil || hs[1]["value"] != "v" {
		t.Fatalf("заголовки: %s", out)
	}
}
