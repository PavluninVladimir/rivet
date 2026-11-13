package orchestrator

import (
	"sort"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/planner"
)

type assessmentEvidenceProjection struct {
	NeedID        string   `json:"need_id"`
	TotalQuotes   int      `json:"total_quotes"`
	ShownQuotes   int      `json:"shown_quotes"`
	OmittedQuotes int      `json:"omitted_quotes"`
	SourceIDs     []string `json:"source_ids"`
}
type assessmentNeedsProjection struct {
	Needs        []discovery.Need               `json:"-"`
	TotalNeeds   int                            `json:"total_needs"`
	ShownNeeds   int                            `json:"shown_needs"`
	OmittedNeeds int                            `json:"omitted_needs"`
	Evidence     []assessmentEvidenceProjection `json:"evidence"`
}

// Project only the prompt. Source collection and persisted evidence must use
// the complete original needs, including references outside this projection.
func projectAssessmentNeeds(needs []discovery.Need) assessmentNeedsProjection {
	sorted := append([]discovery.Need(nil), needs...)
	rank := map[string]int{discovery.ImportanceHigh: 0, discovery.ImportanceMedium: 1, discovery.ImportanceLow: 2}
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Importance] < rank[sorted[j].Importance] })
	shown := min(len(sorted), planner.MaxContextNeeds)
	p := assessmentNeedsProjection{Needs: make([]discovery.Need, 0, shown), TotalNeeds: len(needs), ShownNeeds: shown, OmittedNeeds: len(needs) - shown, Evidence: make([]assessmentEvidenceProjection, 0, len(needs))}
	for i, n := range sorted {
		quotes := 0
		if i < shown {
			quotes = min(len(n.Evidence), planner.MaxContextQuotes)
			projected := n
			projected.Evidence = append([]discovery.QuoteRef(nil), n.Evidence[:quotes]...)
			p.Needs = append(p.Needs, projected)
		}
		refs := make([]string, 0, len(n.Evidence))
		seen := map[string]bool{}
		for _, q := range n.Evidence {
			if !seen[q.Interview] {
				refs = append(refs, q.Interview)
				seen[q.Interview] = true
			}
		}
		p.Evidence = append(p.Evidence, assessmentEvidenceProjection{NeedID: n.ID, TotalQuotes: len(n.Evidence), ShownQuotes: quotes, OmittedQuotes: len(n.Evidence) - quotes, SourceIDs: refs})
	}
	return p
}
