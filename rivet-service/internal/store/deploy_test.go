package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func seedEnv(t *testing.T, s *Store, trigger string) domain.Environment {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "staging", ExecType: "ssh", Trigger: trigger,
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Коалесценция: конкурентные enqueue не создают вторую queued, версия — последняя.
func TestEnqueueDeploymentCoalesces(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedEnv(t, s, "auto")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := s.EnqueueDeployment(ctx, env.ID, "sha", "auto"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := s.EnqueueDeployment(ctx, env.ID, "sha-last", "vova"); err != nil {
		t.Fatal(err)
	}
	deps, err := s.ListDeployments(ctx, env.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Status != "queued" || deps[0].Version != "sha-last" || deps[0].Initiator != "vova" {
		t.Fatalf("ожидалась одна queued с последней версией: %+v", deps)
	}
}

// Дубль имени окружения в проекте — ErrConflict; удаление при активной
// публикации — ErrConflict; после финала — удаляется с историей.
func TestEnvironmentConflictsAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedEnv(t, s, "manual")

	if _, err := s.CreateEnvironment(ctx, domain.Environment{
		ProjectID: env.ProjectID, Name: "staging", ExecType: "ssh", Trigger: "manual",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("дубль имени должен дать ErrConflict, получено %v", err)
	}

	dep, err := s.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE deployments SET status='deploying', started_at=now() WHERE id=$1`, dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnvironment(ctx, env.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("удаление при активной публикации должно дать ErrConflict, получено %v", err)
	}
	if ok, err := s.FinishDeployment(ctx, dep.ID, "", "failed", "стоп"); err != nil || !ok {
		t.Fatalf("финал: %v %v", ok, err)
	}
	if err := s.DeleteEnvironment(ctx, env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDeployment(ctx, dep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("история должна удалиться с окружением, получено %v", err)
	}
}

// Валидация конфигурации окружения (дельта спеки: Verify обязателен,
// verify_url http/https без userinfo, host — безопасный аргумент ssh).
func TestEnvConfigValidate(t *testing.T) {
	bad := []domain.EnvConfig{
		{},                  // нет deploy_cmd
		{DeployCmd: "true"}, // нет Verify
		{DeployCmd: "true", VerifyURL: "ftp://x"},                        // схема
		{DeployCmd: "true", VerifyURL: "http://user:pw@host/health"},     // userinfo
		{DeployCmd: "true", VerifyCmd: "true", Host: "-oProxyCommand=x"}, // опция ssh
		{DeployCmd: "true", VerifyCmd: "true", Host: "host; rm -rf /"},   // мусор в host
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("конфиг #%d должен быть отклонён: %+v", i, c)
		}
	}
	good := []domain.EnvConfig{
		{DeployCmd: "docker compose up -d", VerifyCmd: "curl -f localhost"},
		{DeployCmd: "true", VerifyURL: "https://stage.local:8443/health"},
		{DeployCmd: "true", VerifyCmd: "true", Host: "deploy@stage-01.local:2222"},
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Fatalf("конфиг #%d должен пройти: %v", i, err)
		}
	}
}
