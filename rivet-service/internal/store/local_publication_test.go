package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
)

func seedLocal(t *testing.T, s *Store) domain.Environment {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, e := s.CreateProject(ctx, "local", "o/r", nil, owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	env, e := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "local", ExecType: domain.ExecLocalDocker, Trigger: "auto", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}})
	if e != nil {
		t.Fatal(e)
	}
	return env
}
func dockerRunner(t *testing.T, s *Store) {
	t.Helper()
	if e := s.UpsertRunner(context.Background(), domain.Runner{ID: "docker", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); e != nil {
		t.Fatal(e)
	}
}
func TestLocalQueueConcurrencyAndRunnerCompatibility(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	if _, e := s.EnqueueLocalPublication(ctx, env.ID, "a", "admin", false); !errors.Is(e, ErrNoDockerRunner) {
		t.Fatal(e)
	}
	if e := s.UpsertRunner(ctx, domain.Runner{ID: "old", Capabilities: []string{"deploy", "docker"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.EnqueueLocalPublication(ctx, env.ID, "a", "admin", false); !errors.Is(e, ErrNoDockerRunner) {
		t.Fatal("old protocol accepted", e)
	}
	dockerRunner(t, s)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, e := s.EnqueueLocalPublication(ctx, env.ID, "a", "admin", false)
			if e == nil {
				wins.Add(1)
			} else if !errors.Is(e, ErrPublicationBusy) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
	a, ok, e := s.StartNextDeployment(ctx)
	if e != nil || !ok || a.RunnerID != "docker" {
		t.Fatalf("assignment %+v %v %v", a, ok, e)
	}
}
func TestLocalCASAndConfigurationSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	dockerRunner(t, s)
	p, e := s.EnqueueLocalPublication(ctx, env.ID, "a", "admin", false)
	if e != nil {
		t.Fatal(e)
	}
	a, ok, e := s.StartNextDeployment(ctx)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if _, ok, e := s.AcceptLocalResult(ctx, p.ActiveDeploymentID, "stranger", localdocker.Result{Status: "building"}); e != nil || ok {
		t.Fatal("foreign result", e)
	}
	if _, ok, e := s.AcceptLocalResult(ctx, p.ActiveDeploymentID, a.RunnerID, localdocker.Result{Status: "verifying"}); e != nil || ok {
		t.Fatal("skipped transition", e)
	}
	for _, status := range []string{"building", "starting", "verifying", "running"} {
		res := localdocker.Result{Status: status}
		if status == "running" {
			res.ContainerID = "container"
			res.Image = "image"
			res.Port = 20123
			res.HealthURL = "http://127.0.0.1:20123/"
		}
		if _, ok, e := s.AcceptLocalResult(ctx, p.ActiveDeploymentID, a.RunnerID, res); e != nil || !ok {
			t.Fatal(status, e)
		}
	}
	if _, e = s.CompleteDeployment(ctx, p.ActiveDeploymentID, a.RunnerID); e != nil {
		t.Fatal(e)
	}
	if _, ok, e = s.AcceptLocalResult(ctx, p.ActiveDeploymentID, a.RunnerID, localdocker.Result{Status: "failed"}); e != nil || ok {
		t.Fatal("accepted stale result", e)
	}
	env.Config.LocalDocker.ContainerPort = 9000
	if _, e = s.UpdateEnvironment(ctx, env); e != nil {
		t.Fatal(e)
	}
	got, e := s.LocalPublication(ctx, env.ID)
	if e != nil || got.Config.ContainerPort != 8080 {
		t.Fatalf("snapshot %+v %v", got, e)
	}
	q, e := s.QueueLocalOperation(ctx, env.ID, "stop", "admin")
	if e != nil || q.DesiredState != "stopped" {
		t.Fatalf("stop %+v %v", q, e)
	}
	if _, _, e = s.StartNextDeployment(ctx); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.AcceptLocalResult(ctx, q.ActiveDeploymentID, a.RunnerID, localdocker.Result{Status: "stopped"}); e != nil {
		t.Fatal(e)
	}
	if e = s.RequestLocalDeletion(ctx, env.ID); e != nil {
		t.Fatal("confirmed stop must allow queued deletion before lease finalization", e)
	}
	if e = s.FinishLocalOperation(ctx, q.ActiveDeploymentID, a.RunnerID); e != nil {
		t.Fatal(e)
	}
	again, e := s.QueueLocalOperation(ctx, env.ID, "stop", "admin")
	if e != nil || again.ActiveDeploymentID != q.ActiveDeploymentID {
		t.Fatal("not idempotent", e)
	}
	if _, e = s.QueueLocalOperation(ctx, env.ID, "restart", "admin"); !errors.Is(e, ErrPublicationStopped) {
		t.Fatal(e)
	}
}

func TestLocalHealthRequiresThreeDistinctChecks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	dockerRunner(t, s)
	p, e := s.EnqueueLocalPublication(ctx, env.ID, "v1", "admin", false)
	if e != nil {
		t.Fatal(e)
	}
	a, ok, e := s.StartNextDeployment(ctx)
	if e != nil || !ok {
		t.Fatal(e)
	}
	for _, status := range []string{"building", "starting", "verifying", "running"} {
		res := localdocker.Result{Status: status}
		if status == "running" {
			res.ContainerID = "container"
			res.Image = "image"
			res.Port = 20123
		}
		if _, _, e = s.AcceptLocalResult(ctx, p.ActiveDeploymentID, a.RunnerID, res); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.CompleteDeployment(ctx, p.ActiveDeploymentID, a.RunnerID); e != nil {
		t.Fatal(e)
	}
	for n := 1; n <= 3; n++ {
		op, e := s.QueueLocalOperation(ctx, env.ID, "reconcile", "platform")
		if e != nil {
			t.Fatal(e)
		}
		a, ok, e = s.StartNextDeployment(ctx)
		if e != nil || !ok {
			t.Fatal(e)
		}
		res := localdocker.Result{Status: "unhealthy", Reason: &localdocker.Failure{Code: "healthcheck_failed", Message: "not ready"}}
		// Повтор одного результата до Ack не считается ещё одной проверкой.
		for range 2 {
			if _, _, e = s.AcceptLocalResult(ctx, op.ActiveDeploymentID, a.RunnerID, res); e != nil {
				t.Fatal(e)
			}
		}
		if e = s.FinishLocalOperation(ctx, op.ActiveDeploymentID, a.RunnerID); e != nil {
			t.Fatal(e)
		}
		got, e := s.LocalPublication(ctx, env.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := "running"
		if n == 3 {
			want = "unhealthy"
		}
		if got.Status != want {
			t.Fatalf("check %d: %s want %s", n, got.Status, want)
		}
	}
	view, e := s.GetMonitor(ctx, env)
	if e != nil || view.State != MonitorFail || view.ConsecutiveFail != 3 || len(view.Runs) != 3 {
		t.Fatalf("monitor %+v %v", view, e)
	}
}
