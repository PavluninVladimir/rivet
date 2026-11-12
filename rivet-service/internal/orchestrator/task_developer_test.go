package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func pinEva(t *testing.T, f policyFixture, id string) {
	t.Helper()
	if err := f.st.SetTaskDeveloper(context.Background(), id, "eva", "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedEvaCodesAndFixesWithNormalTestAndReview(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"eva", "Ева", "developer", ""},
		[4]string{"lev", "Лев", "tester", ""}, [4]string{"irina", "Ирина", "reviewer", ""})
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "worker2", Agent: store.AgentRivet, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	pinEva(t, f, f.task.ID)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	run := assignAgent(t, f, as.SessionId)
	if as.Stage != pb.StageResult_CODING || run.AgentID != "eva" || as.GetRole().GetName() != "Ева" {
		t.Fatalf("pin was only a preference: %+v %+v", as.GetRole(), run)
	}
	if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		agent string
		stage pb.StageResult_Stage
	}{{"lev", pb.StageResult_TESTING}, {"irina", pb.StageResult_REVIEW}} {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		as = f.out.lastAssign(t)
		run = assignAgent(t, f, as.SessionId)
		if as.Stage != want.stage || run.AgentID != want.agent {
			t.Fatalf("developer pin replaced ordinary test/review: %+v", run)
		}
		if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
			Stage: as.Stage, Ok: want.stage != pb.StageResult_REVIEW, Verdict: "ok", Detail: "review changes"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	run = assignAgent(t, f, as.SessionId)
	if as.Stage != pb.StageResult_FIXING || run.AgentID != "eva" {
		t.Fatalf("review fix lost strict pin: %+v", run)
	}
	if err := f.st.SetTaskDeveloper(ctx, f.task.ID, "maks", "owner"); err == nil {
		t.Fatal("started task could change developer")
	}
}

func TestPinnedDeveloperWaitsWithoutFallbackAndResumes(t *testing.T) {
	for _, which := range []string{"disabled", "model", "explicit-model", "profile", "role"} {
		t.Run(which, func(t *testing.T) {
			ctx := context.Background()
			f := newPolicyFixture(t, "")
			withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"eva", "Ева", "developer", ""})
			pinEva(t, f, f.task.ID)
			if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=false WHERE id='eva'`); err != nil {
				t.Fatal(err)
			}
			if err := f.e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			var sql string
			switch which {
			case "model":
				sql = `UPDATE team_agents SET enabled=true,model='unavailable-model' WHERE id='eva'`
			case "explicit-model":
				sql = `UPDATE task_step_runs SET model='explicit-override' WHERE task_id=$1`
			case "profile":
				sql = `UPDATE task_step_runs SET agent_kind='other-driver' WHERE task_id=$1`
			case "role":
				sql = `UPDATE team_agents SET enabled=true,role_id='reviewer' WHERE id='eva'`
			}
			if sql != "" {
				args := []any{}
				if which == "explicit-model" || which == "profile" {
					args = append(args, f.task.ID)
				}
				if _, err := f.st.Pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			if which == "explicit-model" || which == "profile" {
				if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=true WHERE id='eva'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.GetTask(ctx, f.task.ID)
			if err != nil || got.AgentID != "" || !strings.Contains(got.WaitReason, "Ева") || got.DeveloperAgentID != "eva" {
				t.Fatalf("fell back to another developer or lost named wait: %+v %v", got, err)
			}
			if len(f.out.assignsSince(0)) != 0 {
				t.Fatal("unavailable pin emitted assignment")
			}
			if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=true,role_id='developer',model='' WHERE id='eva'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.Pool.Exec(ctx, `UPDATE task_step_runs SET model='',agent_kind='' WHERE task_id=$1`, f.task.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			got, err = f.st.GetTask(ctx, f.task.ID)
			if err != nil || got.AgentID != "eva" || got.WaitReason != "" {
				t.Fatalf("pin did not resume: %+v %v", got, err)
			}
		})
	}
}

func TestPinnedBusyEvaDoesNotUseFreeMax(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"eva", "Ева", "developer", ""})
	second, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	pinEva(t, f, f.task.ID)
	pinEva(t, f, second.ID)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := f.st.GetTask(ctx, f.task.ID)
	waiting, _ := f.st.GetTask(ctx, second.ID)
	if first.AgentID != "eva" || waiting.AgentID != "" || !strings.Contains(waiting.WaitReason, "Ева") || !strings.Contains(waiting.WaitReason, "занят") {
		t.Fatalf("busy pin fallback: %+v %+v", first, waiting)
	}
	as := f.out.lastAssign(t)
	run := assignAgent(t, f, as.SessionId)
	if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, _ := f.st.GetTask(ctx, second.ID)
	if resumed.AgentID != "eva" {
		t.Fatalf("busy pin did not resume: %+v", resumed)
	}
}

func TestPinnedEvaUsesConfiguredLocalConnectionAndModel(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withLocalModel(t, f)
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""}, [4]string{"eva", "Ева", "developer", "m"})
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET agent='rivet',connection_id='local' WHERE id='eva'`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat", Agent: store.AgentRivet, Model: "m", Models: []string{"m"},
		Capabilities: []string{"coding", "review"}, Features: []string{"native", "roles"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	pinEva(t, f, f.task.ID)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE model_connections SET enabled=false WHERE id='local'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	waiting, _ := f.st.GetTask(ctx, f.task.ID)
	if waiting.AgentID != "" || !strings.Contains(waiting.WaitReason, "Ева") {
		t.Fatalf("disabled local connection fallback: %+v", waiting)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE model_connections SET enabled=true WHERE id='local'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	run := assignAgent(t, f, as.SessionId)
	if run.AgentID != "eva" || run.AssignedConnection != "local" || run.AssignedModel != "m" || as.GetEndpoint().GetConnectionId() != "local" || as.Model != "m" {
		t.Fatalf("pin was a model-only label or used another provider: %+v %+v", run, as.GetEndpoint())
	}
}
