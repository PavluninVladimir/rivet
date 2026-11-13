package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"sync"
	"testing"
)

func TestLocalPublicationFailureCorrelationAndReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	dockerRunner(t, s)
	var firstID string
	for i, banner := range []string{"Cloning repository...", "Docker version 29.8.2"} {
		if i > 0 {
			if _, err := s.Pool.Exec(ctx, `UPDATE environments SET paused=false WHERE id=$1`, env.ID); err != nil {
				t.Fatal(err)
			}
		}
		pub, err := s.EnqueueLocalPublication(ctx, env.ID, []string{"sha-a", "sha-b"}[i], "admin", false)
		if err != nil {
			t.Fatal(err)
		}
		a, ok, err := s.StartNextDeployment(ctx)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		result := localdocker.Result{Status: "failed", Reason: &localdocker.Failure{Code: "recipe_not_detected", Message: "entrypoint missing"}}
		if _, ok, err := s.AcceptLocalResult(ctx, pub.ActiveDeploymentID, a.RunnerID, result); err != nil || !ok {
			t.Fatal(ok, err)
		}
		pending, err := s.PendingLocalTerminals(ctx)
		if err != nil || len(pending) != 1 || pending[0].Reason.Code != "recipe_not_detected" {
			t.Fatal("durable replay", pending, err)
		}
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := s.FailDeploymentIncident(ctx, pub.ActiveDeploymentID, a.RunnerID, "failed", "entrypoint missing", &ReleaseFailure{Stage: "deploy", Log: banner})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		var id, line string
		var occurrences int
		if err := s.Pool.QueryRow(ctx, `SELECT id::text,error_line,occurrences FROM incidents WHERE env_id=$1 AND status='open'`, env.ID).Scan(&id, &line, &occurrences); err != nil {
			t.Fatal(err)
		}
		if occurrences != i+1 || line != "recipe_not_detected: entrypoint missing" {
			t.Fatal(line, occurrences)
		}
		if i == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatal("split same structured failure")
		}
		pending, err = s.PendingLocalTerminals(ctx)
		if err != nil || len(pending) != 0 {
			t.Fatal("acknowledgement", pending, err)
		}
	}
	var incidents int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE env_id=$1`, env.ID).Scan(&incidents); err != nil || incidents != 1 {
		t.Fatal(incidents, err)
	}
}
