package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"testing"
)

func TestStartEpicPreparesLocalStandOnce(t *testing.T) {
	s := testStore(t)
	dockerRunner(t, s)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, e := s.CreateProject(ctx, "new product", "o/product", nil, owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		ep, e := s.CreateEpic(ctx, p.ID, "work", "goal")
		if e != nil {
			t.Fatal(e)
		}
		mustTask(t, s, ep.ID, NewTask{Title: "Build app"})
		if e = s.TransitionEpic(ctx, ep.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: owner.Login, Type: "epic.status"}); e != nil {
			t.Fatal(e)
		}
	}
	envs, e := s.ListEnvironments(ctx, p.ID)
	if e != nil || len(envs) != 1 || envs[0].ExecType != domain.ExecLocalDocker || envs[0].Trigger != "auto" {
		t.Fatalf("environments %+v %v", envs, e)
	}
	status, e := s.LocalPreparation(ctx, envs[0].ID)
	if e != nil || status != "queued" {
		t.Fatal(status, e)
	}
	var count int
	if e = s.Pool.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE env_id=$1`, envs[0].ID).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}

func TestFirstArtifactWaitsForPreparation(t *testing.T) {
	s := testStore(t)
	dockerRunner(t, s)
	ctx := context.Background()
	owner := mustOwner(t, s)
	project, e := s.CreateProject(ctx, "parallel", "o/p", nil, owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	ep, e := s.CreateEpic(ctx, project.ID, "work", "goal")
	if e != nil {
		t.Fatal(e)
	}
	mustTask(t, s, ep.ID, NewTask{Title: "app"})
	if e = s.TransitionEpic(ctx, ep.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); e != nil {
		t.Fatal(e)
	}
	a, ok, e := s.StartNextDeployment(ctx)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if e = s.EnqueueAutoDeployments(ctx, project.ID, "first-verified"); e != nil {
		t.Fatal("lost first artifact while preparing", e)
	}
	if e = s.SetLocalPreparation(ctx, a.Deployment.ID, a.RunnerID, "waiting_artifact", ""); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishLocalOperation(ctx, a.Deployment.ID, a.RunnerID); e != nil {
		t.Fatal(e)
	}
	if e = s.FlushPendingLocalPublications(ctx); e != nil {
		t.Fatal(e)
	}
	p, e := s.LocalPublication(ctx, a.Env.ID)
	if e != nil || p.Version != "first-verified" || p.Status != "queued" {
		t.Fatalf("first publication %+v %v", p, e)
	}
}
