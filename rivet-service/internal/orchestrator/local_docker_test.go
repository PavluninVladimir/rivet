package orchestrator

import (
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"strings"
	"testing"
	"time"
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
