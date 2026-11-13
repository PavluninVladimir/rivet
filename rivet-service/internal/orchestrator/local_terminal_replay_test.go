package orchestrator

import (
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"testing"
	"time"
)

func TestLocalTerminalReplayTrustsAcceptedState(t *testing.T) {
	for _, stored := range []string{"failed", "running"} {
		t.Run(stored, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			engine := New(st, &fakeSCM{}, nil, &capture{}, 90*time.Second)
			_, base := seedDeploy(t, st, engine, "manual")
			base.ID = ""
			base.Name = "local-replay"
			base.ExecType = domain.ExecLocalDocker
			base.Config = domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}
			env, err := st.CreateEnvironment(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.UpsertRunner(ctx, domain.Runner{ID: "docker", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); err != nil {
				t.Fatal(err)
			}
			pub, err := st.EnqueueLocalPublication(ctx, env.ID, "version", "admin", false)
			if err != nil {
				t.Fatal(err)
			}
			a, ok, err := st.StartNextDeployment(ctx)
			if err != nil || !ok {
				t.Fatal(ok, err)
			}
			result := localdocker.Result{Status: stored, ContainerID: "container", Image: "sha256:image", Port: 25000, HealthURL: "http://127.0.0.1:25000/"}
			if stored == "failed" {
				result.Reason = &localdocker.Failure{Code: "recipe_not_detected", Message: "original reason"}
			}
			if _, ok, err = st.AcceptLocalResult(ctx, pub.ActiveDeploymentID, a.RunnerID, result); err != nil || !ok {
				t.Fatal(ok, err)
			}
			// Crash after acceptance; repeated delivery changes its payload.
			opposite := "failed"
			if stored == "failed" {
				opposite = "running"
			}
			raw, _ := json.Marshal(localdocker.Result{Status: opposite, Reason: &localdocker.Failure{Code: "forged", Message: "changed on redelivery"}})
			if err = engine.onLocalDockerResult(ctx, a.RunnerID, &pb.DeployResult{DeploymentId: pub.ActiveDeploymentID, LocalDockerResult: raw}); err != nil {
				t.Fatal(err)
			}
			want := "done"
			if stored == "failed" {
				want = "failed"
			}
			if got := deploymentStatus(t, st, pub.ActiveDeploymentID); got != want {
				t.Fatalf("stored %s finalized %s", stored, got)
			}
			if err = engine.replayLocalTerminals(ctx); err != nil {
				t.Fatal(err)
			}
			var n int
			if err = st.Pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE env_id=$1`, env.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (stored == "failed" && n != 1) || (stored == "running" && n != 0) {
				t.Fatal("duplicate or fabricated incident", n)
			}
		})
	}
}
