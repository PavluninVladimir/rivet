package orchestrator

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func TestPinnedEvaKeylessDockerHostModel(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withLocalModel(t, f)
	const base = "http://host.docker.internal:8001/v1"
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "local", Name: "MTPLX local", Kind: "local", API: "openai", BaseURL: base}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"eva", "Ева", "developer", "m"})
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET agent='rivet',connection_id='local' WHERE id='eva'`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat", Agent: store.AgentRivet, Model: "m", Models: []string{"m"},
		Capabilities: []string{"coding", "review"}, Features: []string{"native", "roles"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	pinEva(t, f, f.task.ID)
	checked := false
	f.e.CheckKey = func(context.Context, llm.Client) error { checked = true; return &llm.AuthError{Status: 401} }
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	run := assignAgent(t, f, as.SessionId)
	if run.AgentID != "eva" || run.AssignedConnection != "local" || run.AssignedModel != "m" ||
		as.GetEndpoint().GetBaseUrl() != base || as.GetEndpoint().GetKey() != "" {
		t.Fatalf("Docker-host selection did not preserve keyless Eva connection: %+v %+v", run, as.GetEndpoint())
	}
	// The Docker-host alias refers to the worker's host. A 401 must not make
	// control plane probe its own host or invalidate the worker's connection.
	codingResult(t, f, run.RunnerID, as, reject401)
	if checked || connState(t, f, "local") == domain.LLMStateInvalid {
		t.Fatal("control plane probed/rejected its own Docker-host endpoint")
	}
}
