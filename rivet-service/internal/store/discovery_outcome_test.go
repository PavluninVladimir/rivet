package store

import (
	"context"
	"errors"
	"github.com/PavluninVladimir/rivet/internal/discovery"
	"testing"
)

func TestDiscoveryNonExecutingOutcomeStoreGuards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "outcomes", "o/outcomes", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{discovery.OutcomeNeedsClarification, discovery.OutcomeNoDevelopment} {
		r, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: []RunInterview{{ID: "material", Title: "Запрос", SHA256: "sha"}}})
		if err != nil {
			t.Fatal(err)
		}
		d := discovery.Decision{Outcome: outcome, Rationale: "Нужен контекст или нет необходимости разработки", Questions: []discovery.Question{}, Sources: []discovery.DecisionSource{}}
		if outcome == discovery.OutcomeNeedsClarification {
			d.Questions = []discovery.Question{{ID: "system", Text: "Какую систему доработать?", Reason: "Нужна граница интеграции", Sources: []discovery.DecisionSource{}}}
		}
		if _, err = s.FinishRunReady(ctx, r.ID, discovery.Synthesis{Decision: d, Personas: []discovery.Persona{}, Needs: []discovery.Need{}, Epics: []discovery.BacklogEpic{}}, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = s.SaveBacklog(ctx, p.ID, r.ID, 1, syntheticResult().Epics); !errors.Is(err, ErrDiscoveryOutcomeNotActionable) {
			t.Fatal("backlog bypass", err)
		}
		// A corrupted edited backlog cannot change the immutable result decision.
		if _, err = s.Pool.Exec(ctx, `UPDATE discovery_runs SET backlog='[{"key":"h1","title":"injected"}]' WHERE id=$1`, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ApproveRun(ctx, p.ID, r.ID, owner.ID, 1, true); !errors.Is(err, ErrDiscoveryOutcomeNotActionable) {
			t.Fatal("approve bypass", err)
		}
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("unwanted work", count, err)
	}
	var events int
	err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND type='discovery.approved'`, p.ID).Scan(&events)
	if err != nil || events != 0 {
		t.Fatal("unwanted approval", events, err)
	}
}
