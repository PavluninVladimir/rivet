package store

import (
	"context"
	"testing"
	"time"
)

func TestStaleAttentionConcurrentPauseAndHealthInvalidation(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := seedLocal(t, s)
	_, card := attentionFailure(t, s, env, 1)
	pub := attentionWitness(t, s, env, 2)
	pause, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pause.Rollback(context.Background()) }()
	if _, err = pause.Exec(ctx, `UPDATE environments SET paused=true WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	// Repair sees the old healthy snapshot but must not resolve a locked env.
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("concurrent pause bypass", n, err)
	}
	if err = pause.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ResolveStaleDeployAttention(ctx); err != nil || n != 0 {
		t.Fatal("committed pause bypass", n, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE environments SET paused=false WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	health, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = health.Rollback(context.Background()) }()
	if _, err = health.Exec(ctx, `UPDATE local_publications SET status='unhealthy',health_failures=3 WHERE id=$1`, pub); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := s.ResolveStaleDeployAttention(ctx); done <- result{n, err} }()
	// Wait for the actual PostgreSQL publication lock, not scheduler timing.
	blocked := false
	for i := 0; i < 100; i++ {
		if err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM local_publications%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = health.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if !blocked || r.err != nil || r.n != 0 {
		t.Fatal("stale health witness accepted", blocked, r.n, r.err)
	}
	var status string
	if err = s.Pool.QueryRow(ctx, `SELECT status FROM attention WHERE id=$1`, card).Scan(&status); err != nil || status != "claimed" {
		t.Fatal(status, err)
	}
}
