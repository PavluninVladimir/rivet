package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func TestLocalDockerPipeline(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	engine := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, engine, "manual")
	env.ExecType = domain.ExecLocalDocker
	env.Config = domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}
	var err error
	env.ID = ""
	env.Name = "local"
	env.Trigger = "auto"
	env, err = st.CreateEnvironment(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertRunner(ctx, domain.Runner{ID: "docker", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); err != nil {
		t.Fatal(err)
	}
	p, err := st.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("a", 40), "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	job := out.lastDeploy(t)
	var plan localdocker.Job
	if err = json.Unmarshal(job.LocalDockerJob, &plan); err != nil {
		t.Fatal(err)
	}
	if job.LocalDockerAction != "publish" || plan.PublicationID != p.ID || !job.Checkout || job.DeployCmd != "" {
		t.Fatalf("job %+v", job)
	}
	send := func(status string, reason *localdocker.Failure) {
		t.Helper()
		r := localdocker.Result{Status: status, Reason: reason}
		if status == "running" {
			r.ContainerID = strings.Repeat("b", 64)
			r.Image = "sha256:" + strings.Repeat("c", 64)
			r.Port = 20123
			r.HealthURL = "http://127.0.0.1:20123/"
		}
		data, _ := json.Marshal(r)
		if err := engine.OnDeployResult(ctx, "docker", &pb.DeployResult{DeploymentId: job.DeploymentId, LocalDockerResult: data}); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"building", "starting", "verifying", "running"} {
		send(status, nil)
	}
	if got := deploymentStatus(t, st, p.ActiveDeploymentID); got != "done" {
		t.Fatal(got)
	}
	// Новый merge во время публикации не теряется: отложенная версия в БД.
	next, err := st.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("d", 40), "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.EnqueueAutoDeployments(ctx, env.ProjectID, strings.Repeat("e", 40)); err != nil {
		t.Fatal(err)
	}
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	job = out.lastDeploy(t)
	send("building", nil)
	send("failed", &localdocker.Failure{Code: "build_failed", Message: "test failure"})
	old, err := st.CurrentLocalPublication(ctx, env.ID)
	if err != nil || old.Status != "running" || old.Port != 20123 || old.Reason == nil || old.Reason.Code != "build_failed" {
		t.Fatalf("old publication %+v %v", old, err)
	}
	failed, err := st.LocalPublication(ctx, env.ID)
	if err != nil || failed.ID != next.ID || failed.Status != "failed" {
		t.Fatalf("new publication %+v %v", failed, err)
	}
	got, err := st.GetEnvironment(ctx, env.ID)
	if err != nil || !got.Paused {
		t.Fatal("failure did not pause", err)
	}
	// Старый результат не может испортить новое поколение.
	send("running", nil)
	failed, _ = st.LocalPublication(ctx, env.ID)
	if failed.Status != "failed" {
		t.Fatal("stale result accepted")
	}
}

type reconnectDockerSender struct {
	capture
	connected     bool
	beforeFailure func()
}

func (s *reconnectDockerSender) Send(id string, msg *pb.PlaneMsg) bool {
	if !s.connected {
		if s.beforeFailure != nil {
			s.beforeFailure()
		}
		return false
	}
	return s.capture.Send(id, msg)
}

func TestLocalDockerUndeliveredJobRetriesAfterHello(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		for _, helloDuringSend := range []bool{false, true} {
			t.Run(fmt.Sprintf("preparing=%v/helloDuringSend=%v", preparing, helloDuringSend), func(t *testing.T) {
				ctx := context.Background()
				st := testStore(t)
				out := &reconnectDockerSender{}
				engine := New(st, &fakeSCM{}, nil, out, 90*time.Second)
				_, env := seedDeploy(t, st, engine, "manual")
				env.ID = ""
				env.Name = "docker"
				env.ExecType = domain.ExecLocalDocker
				env.Config = domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}
				var err error
				env, err = st.CreateEnvironment(ctx, env)
				if err != nil {
					t.Fatal(err)
				}
				if err = st.UpsertRunner(ctx, domain.Runner{ID: "docker", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); err != nil {
					t.Fatal(err)
				}
				depID := ""
				var initial store.LocalPublication
				if preparing {
					if err = st.Pool.QueryRow(ctx, `INSERT INTO deployments(env_id,version,initiator) VALUES($1,'','release-engineer') RETURNING id::text`, env.ID).Scan(&depID); err != nil {
						t.Fatal(err)
					}
					if _, err = st.Pool.Exec(ctx, `INSERT INTO local_environment_preparations(env_id,deployment_id) VALUES($1,$2)`, env.ID, depID); err != nil {
						t.Fatal(err)
					}
				} else {
					initial, err = st.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("a", 40), "user", false)
					if err != nil {
						t.Fatal(err)
					}
					depID = initial.ActiveDeploymentID
				}
				if helloDuringSend {
					out.beforeFailure = func() {
						if err := st.RunnerAttached(ctx, "docker"); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err = engine.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if got := deploymentStatus(t, st, depID); got != "queued" {
					t.Fatal(got)
				}
				runner, err := st.GetRunner(ctx, "docker")
				if err != nil {
					t.Fatal(err)
				}
				want := "offline"
				if helloDuringSend {
					want = "idle"
				}
				if string(runner.Status) != want {
					t.Fatalf("runner %s, want %s", runner.Status, want)
				}
				if active, err := st.RunnerActiveDeployment(ctx, "docker"); err != nil || active != "" {
					t.Fatal(active, err)
				}
				if preparing {
					if status, err := st.LocalPreparation(ctx, env.ID); err != nil || status != "queued" {
						t.Fatal(status, err)
					}
				}
				if err = st.RunnerAttached(ctx, "docker"); err != nil {
					t.Fatal(err)
				}
				out.connected = true
				if err = engine.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				job := out.lastDeploy(t)
				if job.DeploymentId != depID {
					t.Fatal("new deployment instead of redelivery")
				}
				if got := deploymentStatus(t, st, depID); got != "deploying" {
					t.Fatal(got)
				}
				if !preparing {
					got, err := st.LocalPublication(ctx, env.ID)
					if err != nil || got.ID != initial.ID || got.Generation != initial.Generation || got.Attempt != initial.Attempt {
						t.Fatalf("publication changed: %+v %v", got, err)
					}
					data, _ := json.Marshal(localdocker.Result{Status: "building"})
					if err = engine.OnDeployResult(ctx, "docker", &pb.DeployResult{DeploymentId: depID, LocalDockerResult: data}); err != nil {
						t.Fatal(err)
					}
					requeued, err := st.RequeueUndeliveredLocal(ctx, depID, "docker", time.Now())
					if err != nil || requeued {
						t.Fatal("already building publication requeued", err)
					}
				}
			})
		}
	}
}
