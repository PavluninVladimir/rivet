package store

import (
	"context"
	"testing"
)

func TestStaleAttentionNonlocalRequiresLaterRelease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedEnv(t, s, "manual")
	var failed, card string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO deployments(env_id,version,status,created_at,started_at,ended_at) VALUES($1,'bad','failed',now()-interval '6 minutes',now()-interval '5 minutes',now()-interval '4 minutes') RETURNING id::text`, env.ID).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO attention(project_id,deployment_id,reason,message) VALUES($1,$2,'DEPLOY_FAILED','failed') RETURNING id::text`, env.ProjectID, failed).Scan(&card); err != nil {
		t.Fatal(err)
	}
	good, err := s.EnqueueDeployment(ctx, env.ID, "healthy", "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Generic done is not release-backed and cannot clear the card.
	if _, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='done',started_at=now()-interval '1 minute',ended_at=now() WHERE id=$1`, good.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("unverified done", n, err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO releases(project_id,env_id,deployment_id,version) VALUES($1,$2,$3,'healthy')`, env.ProjectID, env.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	// Ambiguous timestamps and rollback are deliberately not proof.
	for _, update := range []string{"created_at=(SELECT created_at FROM deployments WHERE id=$2)", "rollback=true"} {
		query := "UPDATE deployments SET " + update + " WHERE id=$1"
		var err error
		if update == "rollback=true" {
			_, err = s.Pool.Exec(ctx, query, good.ID)
		} else {
			_, err = s.Pool.Exec(ctx, query, good.ID, failed)
		}
		if err != nil {
			t.Fatal(err)
		}
		if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
			t.Fatal("ambiguous witness", n, err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE deployments SET created_at=now()-interval '2 minutes',rollback=false WHERE id=$1`, good.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 1 {
		t.Fatal("valid release not recognized", card, n, err)
	}
}
