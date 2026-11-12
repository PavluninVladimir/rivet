package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

type secretSender struct {
	capture
	secureSends int
}

func (s *secretSender) SendSecure(id string, msg *pb.PlaneMsg) bool {
	s.secureSends++
	return s.Send(id, msg)
}

func TestApplicationSecretAssignedPipelineAndTranscript(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &secretSender{}
	engine := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, engine, "manual")
	env.ID = ""
	env.Name = "secret-app"
	env.ExecType = domain.ExecLocalDocker
	env.Config = domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}
	var err error
	env, err = st.CreateEnvironment(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	engine.Box = box
	const marker = "pipeline-test-application-secret"
	if err = st.PatchApplicationSecrets(ctx, env.ID, store.ApplicationSecretPatch{Set: map[string]string{"ADMIN_PASSWORD": marker}}, box, "test"); err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertRunner(ctx, domain.Runner{ID: "docker", Secure: true, Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1", store.LocalDockerSecretsFeature}}); err != nil {
		t.Fatal(err)
	}
	p, err := st.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("a", 40), "test", false)
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
	if out.secureSends != 1 || !plan.Sensitive || plan.Secrets["ADMIN_PASSWORD"] != marker {
		t.Fatal("secure dispatch missing")
	}
	if plan.Config.Env["ADMIN_PASSWORD"] != "" {
		t.Fatal("secret merged into public env")
	}
	engine.OnDeployTranscript(p.ActiveDeploymentID, []byte(marker))
	if len(engine.deployLogs[p.ActiveDeploymentID]) != 0 {
		t.Fatal("plaintext transcript retained")
	}
	data, _ := json.Marshal(localdocker.Result{Status: "failed", Logs: marker, Reason: &localdocker.Failure{Code: marker, Message: marker}})
	if err = engine.OnDeployResult(ctx, "docker", &pb.DeployResult{DeploymentId: job.DeploymentId, LocalDockerResult: data}); err != nil {
		t.Fatal(err)
	}
	dep, err := st.GetDeployment(ctx, p.ActiveDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dep.Detail, marker) {
		t.Fatal("plaintext deployment detail")
	}
	engine.WaitArchitecture()
}
