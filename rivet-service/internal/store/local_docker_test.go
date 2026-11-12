package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"testing"
)

func TestLocalDockerPersistence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "local", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "local", ExecType: domain.ExecLocalDocker, Trigger: "manual", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(ctx, s.Pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.GetEnvironment(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.LocalDocker == nil || got.Config.LocalDocker.StartupTimeoutS != 120 || got.Config.LocalDocker.ContainerPort != 8080 {
		t.Fatalf("config not durable: %+v", got.Config)
	}
	var id string
	err = s.Pool.QueryRow(ctx, `INSERT INTO local_publications(env_id,version,generation) VALUES($1,'abc',1) RETURNING id`, e.ID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO local_publications(env_id,version,generation) VALUES($1,'def',2)`, e.ID); err == nil {
		t.Fatal("two in-flight publications accepted")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE local_publications SET status='running' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO local_publications(env_id,version,generation) VALUES($1,'def',2)`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO local_publications(env_id,version,generation,status) VALUES($1,'def',2,'stopped')`, e.ID); err == nil {
		t.Fatal("duplicate generation accepted")
	}
}
