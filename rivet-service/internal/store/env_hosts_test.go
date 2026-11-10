package store

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Миграция 0046 расширяет CHECK предмета эскалации, а не переписывает его:
// причина, которую добавил параллельный change (например, SCM_CREDENTIALS
// из fix-credential-state в 0038), после 0046 по-прежнему принимается, а
// эскалация хоста: тоже. Два окружения проекта ждут решения одновременно.
func TestMigration0046KeepsParallelReasons(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	files := migrationsBefore(t, "9999")
	files["0038_parallel_reason.sql"] = []byte(`
ALTER TABLE attention DROP CONSTRAINT attention_subject;
ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
    (task_id IS NOT NULL AND deployment_id IS NULL) OR
    (task_id IS NULL AND deployment_id IS NOT NULL) OR
    (task_id IS NULL AND deployment_id IS NULL
        AND reason IN ('POLICY_ENGINE', 'POLICY_SOURCE', 'OPS_LIMIT', 'SCM_CREDENTIALS')));`)
	if err := migrateFiles(ctx, url, files); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attention (project_id, reason, message) VALUES ($1, 'SCM_CREDENTIALS', 'x')`, p.ID); err != nil {
		t.Fatalf("причина параллельного change'а потеряна: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attention (project_id, reason, message) VALUES ($1, 'UNKNOWN', 'x')`, p.ID); err == nil {
		t.Fatal("эскалация без предмета с неизвестной причиной принята")
	}
	var hosts []string
	for _, name := range []string{"staging", "prod"} {
		env, err := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: name, ExecType: "ssh", Trigger: "manual",
			Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true", Provision: &domain.ProvisionConfig{Provider: domain.ProvisionLocal}}})
		if err != nil {
			t.Fatal(err)
		}
		h, err := s.RequestHost(ctx, env, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RequestHost(ctx, env, "admin"); err == nil {
			t.Fatal("второй живой хост окружения создан")
		}
		hosts = append(hosts, h.ID)
	}
	for _, id := range hosts {
		if _, ok, err := s.ChangeHost(ctx, id, HostChange{
			Open: []HostAttention{{Reason: domain.AttHostDelete, Message: "удалить тома"}}}); err != nil || !ok {
			t.Fatalf("решение хоста %s: %v", id, err)
		}
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE reason='HOST_DELETE' AND status <> 'resolved'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("два окружения проекта ждут решения: %d %v", n, err)
	}
	// Эскалация хоста не привязывается к публикации или задаче.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attention (project_id, host_id, deployment_id, reason, message)
		VALUES ($1, $2, gen_random_uuid(), 'HOST_FAILED', 'x')`, p.ID, hosts[0]); err == nil {
		t.Fatal("эскалация хоста с публикацией принята")
	}
	list, err := s.ListAttention(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, a := range list {
		if a.Reason == domain.AttHostDelete {
			if a.HostID == "" || a.EnvironmentID == "" {
				t.Fatalf("у эскалации хоста нет предмета: %+v", a)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("эскалаций хоста в очереди %d", seen)
	}
}

// Колонки перехода: только из списка: опечатка в коде не станет SQL.
func TestChangeHostRejectsUnknownColumn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.ChangeHost(ctx, "00000000-0000-0000-0000-000000000000", HostChange{
		Set: map[string]any{"state; DROP TABLE env_hosts": 1}}); err == nil {
		t.Fatal("неизвестная колонка принята")
	}
}

// Токен подключения инфраструктуры перешифровывается вместе с остальными
// секретами установки (add-environment-provisioning): после смены ключа он
// читается новым и не читается прежним.
func TestRotateInfraToken(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldBox, _ := rotateBox(t)
	newBox, _ := rotateBox(t)
	admin := mustOwner(t, s)
	token := "fake-ruvds-rotate-token"
	if _, _, err := s.UpsertInfraConnection(ctx, InfraConnInput{ID: "ruvds", Provider: "ruvds", Name: "RUVDS",
		Token: &token}, oldBox, admin.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	waitNoServeLock(t, s)
	res, err := s.RotateSecrets(ctx, oldBox, newBox)
	if err != nil {
		t.Fatal(err)
	}
	if res.InfraTokens != 1 {
		t.Fatalf("перешифровано токенов инфраструктуры: %+v", res)
	}
	if _, got, err := s.InfraConnectionToken(ctx, "ruvds", newBox); err != nil || got != token {
		t.Fatalf("токен новым ключом: %q %v", got, err)
	}
	if _, _, err := s.InfraConnectionToken(ctx, "ruvds", oldBox); err == nil {
		t.Fatal("токен читается прежним ключом")
	}
}
