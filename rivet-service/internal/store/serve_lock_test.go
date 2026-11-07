package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Блокировка «работает rivetd» переживает обрыв своего соединения
// (рестарт Postgres, failover): rivetd переподключается и берёт её заново,
// и перешифровка по-прежнему отказывает. Соединение видно по
// application_name.
func TestServeLockSurvivesReconnect(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prev := serveLockPing
	serveLockPing = 50 * time.Millisecond
	defer func() { serveLockPing = prev }()

	release, err := s.HoldServeLock(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	holder := func() (int, bool) {
		t.Helper()
		var pid int
		err := s.Pool.QueryRow(ctx, `
			SELECT a.pid FROM pg_locks l JOIN pg_stat_activity a USING (pid)
			WHERE l.locktype = 'advisory' AND l.granted AND a.application_name = $1 AND a.datname = current_database()`,
			ServeLockApp).Scan(&pid)
		return pid, err == nil
	}
	first, ok := holder()
	if !ok {
		t.Fatal("блокировку не видно в pg_locks по application_name")
	}
	if _, err := s.Pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, first); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pid, ok := holder(); ok && pid != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("после обрыва соединения блокировка не взята заново")
		}
		time.Sleep(20 * time.Millisecond)
	}

	oldBox, _ := rotateBox(t)
	newBox, _ := rotateBox(t)
	if _, err := s.RotateSecrets(ctx, oldBox, newBox); !errors.Is(err, ErrServeRunning) {
		t.Fatalf("после переподключения перешифровка должна отказывать: %v", err)
	}
	release()
	waitNoServeLock(t, s)
	if _, err := s.RotateSecrets(ctx, oldBox, newBox); err != nil {
		t.Fatalf("после остановки rivetd перешифровка должна идти: %v", err)
	}
}

// Перешифровка, прошедшая в окне без блокировки, выдаёт себя: после
// повторного захвата rivetd видит новое событие secret_key.rotated и
// вызывает onRotated (rivetd завершается).
func TestServeLockDetectsRotationInWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prev := serveLockPing
	serveLockPing = 50 * time.Millisecond
	defer func() { serveLockPing = prev }()

	rotated := make(chan struct{}, 1)
	release, err := s.HoldServeLock(ctx, func() { rotated <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var pid int
	if err := s.Pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE application_name = $1 AND datname = current_database()`,
		ServeLockApp).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// Перешифровка «в окне»: событие появилось, пока блокировки не было.
	if _, err := s.AppendEvent(ctx, EventInput{ActorKind: "system", ActorID: "rotate-key", Type: "secret_key.rotated", Text: "тест"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rotated:
	case <-time.After(10 * time.Second):
		t.Fatal("перешифровка в окне без блокировки не замечена")
	}
}

// Пока идёт перешифровка, rivetd ждёт её конца и видит, кто держит
// блокировку. Без перешифровки старт не ждёт.
func TestServeLockWaitsForRotation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prev := serveLockPoll
	serveLockPoll = 20 * time.Millisecond
	defer func() { serveLockPoll = prev }()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, serveLockKey); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		release, err := s.HoldServeLock(ctx, nil)
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("блокировка взята при идущей перешифровке: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	c, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	if h := lockHolders(ctx, c); !strings.Contains(h, "ExclusiveLock") {
		t.Fatalf("держатель блокировки: %q", h)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("после конца перешифровки блокировка не взята")
	}
}

// Перешифровка не ждёт таблицы секретов дольше lock_timeout: за чужой
// транзакцией (rivetd прежней версии) она падает с понятной ошибкой.
func TestRotateSecretsBusyTables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prev := rotateLockTimeout
	rotateLockTimeout = "200ms"
	defer func() { rotateLockTimeout = prev }()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE model_connections IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	oldBox, _ := rotateBox(t)
	newBox, _ := rotateBox(t)
	if _, err := s.RotateSecrets(ctx, oldBox, newBox); !errors.Is(err, ErrSecretsBusy) {
		t.Fatalf("ожидалась ErrSecretsBusy: %v", err)
	}
}

// waitNoServeLock ждёт, пока сервер снимет блокировку закрытого соединения:
// Close не ждёт, пока backend завершится.
func waitNoServeLock(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM (`+holdersSQL+`) h`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("блокировка «работает rivetd» не снята после release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
