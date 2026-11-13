package store

import (
	"context"
	"fmt"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"sync"
	"testing"
)

func attentionFailure(t *testing.T, s *Store, env domain.Environment, generation int) (string, string) {
	t.Helper()
	ctx := context.Background()
	var dep, card string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO deployments(env_id,version,status,started_at,ended_at,created_at) VALUES($1,'broken','failed',now()-interval '5 minutes',now()-interval '4 minutes',now()-interval '6 minutes') RETURNING id::text`, env.ID).Scan(&dep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO local_publications(env_id,deployment_id,active_deployment_id,completed_operation_id,version,generation,status,reason) VALUES($1,$2,$2,$2,'broken',$3,'failed','{"code":"recipe_not_detected","message":"missing entrypoint"}')`, env.ID, dep, generation); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO attention(project_id,deployment_id,reason,message,status,claimed_by) VALUES($1,$2,'DEPLOY_FAILED','failure','claimed','admin') RETURNING id::text`, env.ProjectID, dep).Scan(&card); err != nil {
		t.Fatal(err)
	}
	return dep, card
}
func attentionWitness(t *testing.T, s *Store, env domain.Environment, generation int) string {
	t.Helper()
	ctx := context.Background()
	var dep, pub string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO deployments(env_id,version,status,started_at,ended_at) VALUES($1,'healthy','done',now()-interval '2 minutes',now()-interval '1 minute') RETURNING id::text`, env.ID).Scan(&dep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO releases(project_id,env_id,deployment_id,version) VALUES($1,$2,$3,'healthy')`, env.ProjectID, env.ID, dep); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO local_publications(env_id,deployment_id,active_deployment_id,completed_operation_id,version,generation,status,container_id,image_digest,host_port,health_url,checked_at) VALUES($1,$2,$2,$2,'healthy',$3,'running','container','sha256:healthy',25000,'http://127.0.0.1:25000/health',now()) RETURNING id::text`, env.ID, dep, generation).Scan(&pub); err != nil {
		t.Fatal(err)
	}
	return pub
}
func TestStaleDeployAttentionWitnessAndConcurrency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	_, a := attentionFailure(t, s, env, 1)
	_, b := attentionFailure(t, s, env, 2)
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("incident claim is not evidence", n, err)
	}
	pub := attentionWitness(t, s, env, 3)
	// Latest publication must have a completely healthy witness.
	for _, bad := range []string{"health_failures=1", "reason='{\"code\":\"healthcheck_failed\"}'", "health_url=''", "completed_operation_id=NULL", "operation='cleanup'", "desired_state='stopped'", "status='unhealthy'"} {
		if _, err := s.Pool.Exec(ctx, "UPDATE local_publications SET "+bad+" WHERE id=$1", pub); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
			t.Fatal(bad, n, err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE local_publications SET health_failures=0,reason=NULL,health_url='http://127.0.0.1:25000/health',completed_operation_id=active_deployment_id,operation='publish',desired_state='running',status='running' WHERE id=$1`, pub); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE environments SET paused=true WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("pause bypass", n, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE environments SET paused=false WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ResolveStaleDeployAttention(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{a, b} {
		var status, claimed string
		var resolved bool
		if err := s.Pool.QueryRow(ctx, `SELECT status,claimed_by,resolved_at IS NOT NULL FROM attention WHERE id=$1`, id).Scan(&status, &claimed, &resolved); err != nil || status != "resolved" || claimed != "admin" || !resolved {
			t.Fatal(status, claimed, resolved, err)
		}
	}
	var events int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND payload->>'action'='resolved_stale_attention'`, env.ProjectID).Scan(&events); err != nil || events != 1 {
		t.Fatal("audit not once", events, err)
	}
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("repeat", n, err)
	}
	_, newer := attentionFailure(t, s, env, 4)
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("old container closed new failure", newer, n, err)
	}
}
func TestStaleDeployAttentionBoundedPositiveCandidates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	blocked := seedLocal(t, s)
	dep, _ := attentionFailure(t, s, blocked, 1)
	// An old unresolved environment cannot starve a later proven recovery.
	for i := 0; i < 101; i++ {
		_, err := s.Pool.Exec(ctx, `INSERT INTO attention(project_id,deployment_id,reason,message) VALUES($1,$2,'DEPLOY_FAILED',$3)`, blocked.ProjectID, dep, fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	healthy := seedLocal(t, s)
	_, card := attentionFailure(t, s, healthy, 1)
	attentionWitness(t, s, healthy, 2)
	n, err := s.ResolveStaleDeployAttention(ctx)
	if err != nil || n != 1 {
		t.Fatal(card, n, err)
	}
}
