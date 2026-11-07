package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// setHash ставит пароль в обход проверок hashPassword: так в базе
// оказываются пароли установок, поднятых до open-source.
func setHash(t *testing.T, s *Store, id, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE users SET password_hash=$2 WHERE id=$1`, id, string(hash)); err != nil {
		t.Fatal(err)
	}
}

// Администратор с паролем из документации (стенд по старому compose с
// admin-local) при следующем входе обязан сменить пароль, событие без
// пароля пишется один раз (спека access-policy, сценарий «Опубликованный
// пароль у существующего администратора»).
func TestFlagPublishedPasswords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "admin", "", "pw-temporary-1", true)
	if err != nil {
		t.Fatal(err)
	}
	setHash(t, s, admin.ID, "admin-local")
	own, err := s.CreateUser(ctx, "alice", "", "alice-own-password", true)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, "bob", "", "pw-temporary-2", false)
	if err != nil {
		t.Fatal(err)
	}
	setHash(t, s, member.ID, "change-me")
	disabled, err := s.CreateUser(ctx, "carol", "", "pw-temporary-3", true)
	if err != nil {
		t.Fatal(err)
	}
	setHash(t, s, disabled.ID, "e2e-password")
	if _, err := s.Pool.Exec(ctx, `UPDATE users SET disabled=true WHERE id=$1`, disabled.ID); err != nil {
		t.Fatal(err)
	}

	flagged, err := s.FlagPublishedPasswords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(flagged, []string{"admin"}) {
		t.Fatalf("отмечены %v, ожидался только admin", flagged)
	}
	for id, want := range map[string]bool{admin.ID: true, own.ID: false, member.ID: false} {
		u, err := s.GetUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if u.MustChangePassword != want {
			t.Fatalf("%s: must_change_password=%v, ожидалось %v", u.Login, u.MustChangePassword, want)
		}
	}
	var n int
	var text string
	if err := s.Pool.QueryRow(ctx, `SELECT count(*), max(text) FROM events WHERE type='user.password_published'`).Scan(&n, &text); err != nil {
		t.Fatal(err)
	}
	if n != 1 || strings.Contains(text, "admin-local") {
		t.Fatalf("событие: %d, %q", n, text)
	}

	// Повторный старт: уже отмеченного не трогает и событие не дублирует.
	again, err := s.FlagPublishedPasswords(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("повтор: %v, %v", again, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='user.password_published'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("событий после повтора: %d, %v", n, err)
	}

	// Сменить опубликованный пароль на опубликованный же нельзя.
	if err := s.ChangePassword(ctx, admin.ID, "admin-local", "change-me", ""); !errors.Is(err, ErrPublishedPassword) {
		t.Fatalf("смена на опубликованный пароль: %v", err)
	}
	if err := s.ChangePassword(ctx, admin.ID, "admin-local", "свой-новый-пароль", ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetUser(ctx, admin.ID); u.MustChangePassword {
		t.Fatal("после смены пароля признак должен сняться")
	}
}

// Пароль из документации не задаётся ни одним путём: создание
// пользователя и bootstrap идут через hashPassword.
func TestPublishedPasswordRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, pw := range []string{"admin-local", "change-me", "e2e-password"} {
		if _, err := s.CreateUser(ctx, "dave", "", pw, false); !errors.Is(err, ErrPublishedPassword) {
			t.Fatalf("%q: %v", pw, err)
		}
	}
	if err := s.Bootstrap(ctx, "admin", "admin-local"); !errors.Is(err, ErrPublishedPassword) {
		t.Fatalf("bootstrap с опубликованным паролем: %v", err)
	}
}
