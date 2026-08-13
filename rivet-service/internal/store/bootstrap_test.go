package store

import (
	"context"
	"strings"
	"testing"
)

// Сценарий «Bootstrap первого администратора» + fail-fast и идемпотентность.
func TestBootstrap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Пустая установка без конфигурации — отказ (без владельца не стартуем).
	if err := s.Bootstrap(ctx, "", ""); err == nil || !strings.Contains(err.Error(), "RIVET_ADMIN_LOGIN") {
		t.Fatalf("ожидался fail-fast с подсказкой, получено %v", err)
	}

	if err := s.Bootstrap(ctx, "root", "secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "root", "secret"); err != nil {
		t.Fatalf("вход bootstrap-админа: %v", err)
	}

	// Повторный старт идемпотентен: env игнорируется, второй админ не создаётся.
	if err := s.Bootstrap(ctx, "other", "pw"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("ожидался 1 пользователь, есть %d", n)
	}
}

// Backfill: проект без участников (создан до change'а) передаётся админу.
func TestBootstrapBackfillOrphans(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Bootstrap(ctx, "root", "secret"); err != nil {
		t.Fatal(err)
	}
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "legacy", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Симулируем проект старой схемы: участников нет вовсе.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.Bootstrap(ctx, "", ""); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListMembers(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Login != "root" {
		t.Fatalf("осиротевший проект должен уйти админу root, получено %+v", members)
	}
}
