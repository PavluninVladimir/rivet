package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/google/uuid"
)

func loadFixture(t *testing.T, s *Store, host string) (domain.Environment, loadtest.Binding) {
	t.Helper()
	ctx := context.Background()
	env := seedLocal(t, s)

	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET draining=true WHERE capabilities @> ARRAY['deploy']::text[]`); err != nil {
		t.Fatal(err)
	}
	publisher := "docker-" + env.ID
	if err := s.UpsertRunner(ctx, domain.Runner{ID: publisher, Host: host, Secure: true, Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1", LocalDockerSecretsFeature}}); err != nil {
		t.Fatal(err)
	}
	pub, err := s.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("a", 40), "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.StartNextDeployment(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, state := range []string{"building", "starting", "verifying", "running"} {
		res := localdocker.Result{Status: state}
		if state == "running" {
			res.ContainerID = strings.Repeat("b", 64)
			res.Port = 20123
			res.HealthURL = "http://127.0.0.1:20123/"
		}
		if _, ok, err = s.AcceptLocalResult(ctx, pub.ActiveDeploymentID, a.RunnerID, res); err != nil || !ok {
			t.Fatal(state, ok, err)
		}
	}
	if _, err = s.CompleteDeployment(ctx, pub.ActiveDeploymentID, a.RunnerID); err != nil {
		t.Fatal(err)
	}
	v, err := s.LoadTestingView(ctx, env, loadtest.DefaultLimits())
	if err != nil || v.Binding == nil || v.Prompt == nil {
		t.Fatalf("view %+v %v", v, err)
	}
	return env, *v.Binding
}
func loadProfile() loadtest.Profile {
	return loadtest.Profile{Unit: "rps", Target: 2, Ceiling: 2, TargetDurationS: 10, MaxVUs: 2, P95MS: 500, Routes: []loadtest.Route{{Path: "/api/companies", Weight: 1, ExpectedStatus: 200}}}
}
func seedLoadRunner(t *testing.T, s *Store, host string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "k6", Host: host, Capabilities: []string{"load"}, Features: []string{loadtest.Feature}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET boot_id='fixture-boot' WHERE id='k6'`); err != nil {
		t.Fatal(err)
	}
}
func TestLoadPromptBackfillAndRestartDurability(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, binding := loadFixture(t, s, "host-a")
	if _, err := s.Pool.Exec(ctx, `DELETE FROM load_test_plans WHERE env_id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileLoadPlans(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := s.LoadTestingView(ctx, env, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := v.Prompt.ID
	if err = s.DeferLoadPrompt(ctx, env.ID); err != nil {
		t.Fatal(err)
	}
	other, err := New(ctx, s.Pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = other.ReconcileLoadPlans(ctx); err != nil {
		t.Fatal(err)
	}
	v, err = other.LoadTestingView(ctx, env, loadtest.DefaultLimits())
	if err != nil || v.Prompt.ID != id || v.Prompt.State != "deferred" || *v.Binding != binding || v.Latest != nil {
		t.Fatalf("restart rearmed prompt %+v %v", v, err)
	}
	var n int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM load_test_runs`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
func TestLoadQueueFencingAndTransactionalRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, binding := loadFixture(t, s, "host-a")
	seedLoadRunner(t, s, "host-a")
	start := LoadStart{RequestID: uuid.NewString(), Binding: binding, Profile: loadProfile()}
	run, err := s.StartLoadTest(ctx, env.ID, start, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.StartLoadTest(ctx, env.ID, start, loadtest.DefaultLimits())
	if err != nil || same.ID != run.ID {
		t.Fatal("idempotency", same, err)
	}
	claimed, ok, err := s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
	if err != nil || !ok || claimed.ID != run.ID {
		t.Fatal(ok, err)
	}
	if _, ok, err = s.AcceptLoadResult(ctx, "foreign", run.ID, claimed.AssignmentID, "target", loadtest.Result{State: "running"}); err != nil || ok {
		t.Fatal("foreign result accepted", err)
	}
	if _, err = s.QueueLocalOperation(ctx, env.ID, "restart", "fixture"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLoadRun(ctx, run.ID)
	if err != nil || got.State != "interrupted" || got.Reason.Code != "publication_changed" {
		t.Fatalf("fast restart did not interrupt %+v %v", got, err)
	}
	if _, started, e := s.StartNextDeployment(ctx); e != nil || started {
		t.Fatal("restart dispatched before quiescence", started, e)
	}
	if err = s.DeleteEnvironment(ctx, env.ID); err == nil {
		t.Fatal("cascade erased pending cancellation")
	}
	blocked := LoadStart{RequestID: uuid.NewString(), Binding: binding, Profile: loadProfile()}
	if _, e := s.StartLoadTest(ctx, env.ID, blocked, loadtest.DefaultLimits()); e == nil {
		t.Fatal("second load admitted before quiescence")
	}
	var busy bool
	if err = s.Pool.QueryRow(ctx, `SELECT load_test_id IS NOT NULL FROM runners WHERE id='k6'`).Scan(&busy); err != nil || !busy {
		t.Fatal("runner freed before cancellation ack", busy, err)
	}
	if _, _, err = s.AcceptLoadResult(ctx, "k6", run.ID, claimed.AssignmentID, "target", loadtest.Result{State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT load_test_id IS NOT NULL FROM runners WHERE id='k6'`).Scan(&busy); err != nil || busy {
		t.Fatal("terminal ack did not free runner", busy, err)
	}
	if _, started, e := s.StartNextDeployment(ctx); e != nil || !started {
		t.Fatal("restart did not resume after ACK", started, e)
	}
	got, _ = s.GetLoadRun(ctx, run.ID)
	if got.State != "interrupted" {
		t.Fatal("late ack overwrote outcome", got)
	}
}
func TestLoadFairQueueAndRequestIDAcrossEnvironments(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, ab := loadFixture(t, s, "host-a")
	b, bb := loadFixture(t, s, "host-b")
	seedLoadRunner(t, s, "host-b")
	first, err := s.StartLoadTest(ctx, a.ID, LoadStart{RequestID: uuid.NewString(), Binding: ab, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.StartLoadTest(ctx, b.ID, LoadStart{RequestID: uuid.NewString(), Binding: bb, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
	if err != nil || !ok || claimed.ID != second.ID {
		t.Fatalf("head of line blocked ready host first=%s second=%s got=%s ok=%v err=%v", first.ID, second.ID, claimed.ID, ok, err)
	}
	// Cancel both, then race one request ID in different environments.
	_, _ = s.CancelLoadTest(ctx, first.ID)
	_, _ = s.CancelLoadTest(ctx, second.ID)
	_, _, _ = s.AcceptLoadResult(ctx, "k6", second.ID, claimed.AssignmentID, "target", loadtest.Result{State: "interrupted"})
	request := uuid.NewString()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, entry := range []struct {
		env string
		b   loadtest.Binding
	}{{a.ID, ab}, {b.ID, bb}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.StartLoadTest(ctx, entry.env, LoadStart{RequestID: request, Binding: entry.b, Profile: loadProfile()}, loadtest.DefaultLimits())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else {
			var conflict *LoadError
			if errors.As(err, &conflict) && conflict.Code == "idempotency_conflict" {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
}
func TestLoadDeadlineLostRunNotRetriedAndReleaseCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, b := loadFixture(t, s, "host-a")
	seedLoadRunner(t, s, "host-a")
	run, err := s.StartLoadTest(ctx, env.ID, LoadStart{RequestID: uuid.NewString(), Binding: b, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE runners SET boot_id='restarted' WHERE id='k6'`); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.ReconcileLoadRuns(ctx)
	if err != nil || len(stopped) != 1 {
		t.Fatal(stopped, err)
	}
	got, _ := s.GetLoadRun(ctx, run.ID)
	if got.State != "interrupted" {
		t.Fatal(got.State)
	}
	if _, ok, err = s.ClaimLoadTest(ctx, loadtest.DefaultLimits()); err != nil || ok {
		t.Fatal("lost load retried", err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE load_test_runs SET deadline_at=$2 WHERE id=$1`, claimed.ID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PendingLoadCancels(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := s.StartLoadTest(ctx, env.ID, LoadStart{RequestID: uuid.NewString(), Binding: b, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.LoadHistory(ctx, "", b.ReleaseID, next.ID, 10)
	if err != nil || len(page) != 1 || page[0].ID != run.ID {
		t.Fatal("release cursor", page, err)
	}
}

func TestLoadCheckpointRejectedSummaryQuarantinesAndAckEmitsOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, binding := loadFixture(t, s, "host-a")
	seedLoadRunner(t, s, "host-a")
	queued, err := s.StartLoadTest(ctx, env.ID, LoadStart{RequestID: uuid.NewString(), Binding: binding, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
	if err != nil || !ok || run.ID != queued.ID {
		t.Fatal(run, ok, err)
	}
	// Invalid phase cannot be persisted/granted.
	if _, accepted, err := s.AcceptLoadResult(ctx, "k6", run.ID, run.AssignmentID, "capacity_1", loadtest.Result{State: "running"}); err == nil || accepted {
		t.Fatal("out-of-order phase accepted", accepted, err)
	}
	// Malformed evidence changes the report, but does not prove native k6 stopped.
	bad := loadtest.Evidence{Phase: loadProfile().Phases()[0], Summary: []byte(`{}`)}
	got, accepted, err := s.AcceptLoadResult(ctx, "k6", run.ID, run.AssignmentID, "", loadtest.Result{State: "running", Stages: []loadtest.Evidence{bad}})
	if err == nil {
		t.Fatal("completed target incorrectly used as next checkpoint", got, accepted)
	}
	p := loadProfile()
	p.Ceiling = 3
	if _, err = s.Pool.Exec(ctx, `UPDATE load_test_runs SET profile=jsonb_set(profile,'{capacity_ceiling}','3') WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, accepted, err = s.AcceptLoadResult(ctx, "k6", run.ID, run.AssignmentID, "capacity_1", loadtest.Result{State: "running", Stages: []loadtest.Evidence{bad}})
	if err != nil || !accepted || got.State != "inconclusive" || !got.Stopping {
		t.Fatal(got, accepted, err)
	}
	if err = s.DeleteEnvironment(ctx, env.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("quarantined deletion must be expected conflict", err)
	}
	var before, after int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='load_test.status'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptLoadResult(ctx, "k6", run.ID, run.AssignmentID, "", loadtest.Result{State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='load_test.status'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatal("missing quiescence SSE event", before, after)
	}
	if _, _, err = s.AcceptLoadResult(ctx, "k6", run.ID, run.AssignmentID, "", loadtest.Result{State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	var replay int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='load_test.status'`).Scan(&replay); err != nil || replay != after {
		t.Fatal("duplicate quiescence event", replay, after, err)
	}
}
func TestLoadQueuedReasonRefresh(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, binding := loadFixture(t, s, "host-a")
	run, err := s.StartLoadTest(ctx, env.ID, LoadStart{RequestID: uuid.NewString(), Binding: binding, Profile: loadProfile()}, loadtest.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	seedLoadRunner(t, s, "host-b")
	if _, err = s.ReconcileLoadRuns(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLoadRun(ctx, run.ID)
	if err != nil || got.Reason == nil || got.Reason.Code != "wrong_host" {
		t.Fatal(got.Reason, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE runners SET host='host-a',status='running' WHERE id='k6'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileLoadRuns(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetLoadRun(ctx, run.ID)
	if err != nil || got.Reason == nil || got.Reason.Code != "runner_busy" {
		t.Fatal(got.Reason, err)
	}
}

func TestLoadAndDockerClaimsMutuallyExcludeBothOrders(t *testing.T) {
	for _, order := range []string{"docker-first", "load-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			env, binding := loadFixture(t, s, "host-a")
			seedLoadRunner(t, s, "host-a")
			queued, err := s.StartLoadTest(ctx, env.ID, LoadStart{RequestID: uuid.NewString(), Binding: binding, Profile: loadProfile()}, loadtest.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			operation, err := s.QueueLocalOperation(ctx, env.ID, "reconcile", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			var load LoadRun
			var deployment DeployAssignment
			var gotLoad, gotDocker bool
			switch order {
			case "docker-first":
				deployment, gotDocker, err = s.StartNextDeployment(ctx)
				if err != nil || !gotDocker {
					t.Fatal(gotDocker, err)
				}
				load, gotLoad, err = s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
				if err != nil || gotLoad {
					t.Fatal("load overlapped active reconcile", gotLoad, err)
				}
				if _, err = s.ReconcileLoadRuns(ctx); err != nil {
					t.Fatal(err)
				}
				r, err := s.GetLoadRun(ctx, queued.ID)
				if err != nil || r.Reason == nil || r.Reason.Code != "publication_busy" {
					t.Fatal("missing queued operation reason", r.Reason, err)
				}
			case "load-first":
				load, gotLoad, err = s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
				if err != nil || !gotLoad {
					t.Fatal(gotLoad, err)
				}
				deployment, gotDocker, err = s.StartNextDeployment(ctx)
				if err != nil || gotDocker {
					t.Fatal("reconcile overlapped active load", gotDocker, err)
				}
			case "concurrent":
				gate, err := s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err = lockLocalExecutionClaim(ctx, gate, env.ID); err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				wg.Add(2)
				errors := make(chan error, 2)
				go func() {
					defer wg.Done()
					var e error
					load, gotLoad, e = s.ClaimLoadTest(ctx, loadtest.DefaultLimits())
					errors <- e
				}()
				go func() {
					defer wg.Done()
					var e error
					deployment, gotDocker, e = s.StartNextDeployment(ctx)
					errors <- e
				}()
				time.Sleep(30 * time.Millisecond)
				if err = gate.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				wg.Wait()
				close(errors)
				for e := range errors {
					if e != nil {
						t.Fatal(e)
					}
				}
				if gotLoad == gotDocker {
					t.Fatal("claims must have exactly one winner", gotLoad, gotDocker)
				}
			}
			if gotDocker {
				if deployment.Deployment.ID != operation.ActiveDeploymentID {
					t.Fatal("wrong technical operation")
				}
				if err = s.FinishLocalOperation(ctx, deployment.Deployment.ID, deployment.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.ClaimLoadTest(ctx, loadtest.DefaultLimits()); err != nil || !ok {
					t.Fatal("load stayed blocked after Docker quiescence", ok, err)
				}
			} else {
				if _, err = s.CancelLoadTest(ctx, load.ID); err != nil {
					t.Fatal(err)
				}
				if _, _, err = s.AcceptLoadResult(ctx, "k6", load.ID, load.AssignmentID, "", loadtest.Result{State: "interrupted"}); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.StartNextDeployment(ctx); err != nil || !ok {
					t.Fatal("Docker stayed blocked after load ACK", ok, err)
				}
			}
		})
	}
}

func TestDeleteLoadGuardDoesNotBlockDeploymentReleaseForeignKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env, _ := loadFixture(t, s, "host-a")
	pub, err := s.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("c", 40), "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.StartNextDeployment(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// CompleteDeployment locks this row before inserting its release, whose
	// foreign key takes KEY SHARE on environments. Delete must stay compatible.
	completion, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = completion.Rollback(ctx) }()
	if _, err = completion.Exec(ctx, `SELECT id FROM deployments WHERE id=$1 FOR UPDATE`, pub.ActiveDeploymentID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteEnvironment(ctx, env.ID) }()
	waiting := false
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		if err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%SELECT 1 FROM deployments WHERE env_id=%FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("delete did not reach deployment lock after acquiring environment lock")
	}
	if _, err = completion.Exec(ctx, `SET LOCAL statement_timeout='500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err = completion.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR KEY SHARE`, env.ID); err != nil {
		t.Fatal("delete guard deadlocked release FK", err)
	}
	if err = completion.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-deleted:
		if !errors.Is(err, ErrConflict) {
			t.Fatal("active deployment delete must conflict", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delete remained blocked after completion lock released")
	}
}
