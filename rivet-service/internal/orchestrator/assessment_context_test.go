package orchestrator

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/planner"
)

func TestAssessmentProjectsValidResearchWithoutLosingReferences(t *testing.T) {
	var needs []discovery.Need
	for i := 0; i < planner.MaxContextNeeds+1; i++ {
		n := discovery.Need{ID: fmt.Sprintf("n%d", i), Title: "need", Importance: discovery.ImportanceMedium}
		for q := 0; q < discovery.MaxQuotesPerNeed; q++ {
			n.Evidence = append(n.Evidence, discovery.QuoteRef{Interview: fmt.Sprintf("source-%d-%d", i, q), Text: fmt.Sprintf("quote-%d", q), Line: q + 1})
		}
		needs = append(needs, n)
	}
	needs[20].Importance = discovery.ImportanceHigh
	original := make([]discovery.Need, len(needs))
	for i, n := range needs {
		original[i] = n
		original[i].Evidence = append([]discovery.QuoteRef(nil), n.Evidence...)
	}
	p := projectAssessmentNeeds(needs)
	if len(p.Needs) != 20 || p.TotalNeeds != 21 || p.OmittedNeeds != 1 || p.Needs[0].ID != "n20" || p.Needs[1].ID != "n0" {
		t.Fatalf("projection %+v", p)
	}
	for _, n := range p.Needs {
		if len(n.Evidence) != 3 {
			t.Fatal("unbounded prompt quotes")
		}
	}
	if len(p.Evidence) != 21 || p.Evidence[0].TotalQuotes != 5 || p.Evidence[0].OmittedQuotes != 2 || len(p.Evidence[0].SourceIDs) != 5 || p.Evidence[20].ShownQuotes != 0 || len(p.Evidence[20].SourceIDs) != 5 {
		t.Fatal("lost omitted source references", p.Evidence)
	}
	if !reflect.DeepEqual(needs, original) {
		t.Fatal("original evidence mutated")
	}
	p.Needs[0].Evidence[0].Text = "projection only"
	if !reflect.DeepEqual(needs, original) {
		t.Fatal("projection aliases original evidence")
	}
}
