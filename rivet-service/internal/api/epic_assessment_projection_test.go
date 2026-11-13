package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func TestFiveQuoteResearchReachesArchitectThenRequestedPlanning(t *testing.T) {
	llm := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llm.URL)
	enableAssessmentAPI(t, f)
	ctx := context.Background()
	var sources []store.DiscoveryInterview
	for i := 0; i < 6; i++ {
		text := fmt.Sprintf("Source %d evidence supporting the requested existing workflow.", i+1)
		if i == 4 {
			text += "\nLATE_SOURCE_ONLY_BOUNDARY: preserve existing API and authentication."
		}
		if i == 5 {
			text += "\nOMITTED_NEED_ONLY_BOUNDARY: preserve existing storage."
		}
		iv, err := f.st.AddFeedback(ctx, f.project, f.uid.ID, store.NewFeedback{Source: fmt.Sprintf("https://example.invalid/issue/%d", i+1), Text: text})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, iv)
	}
	snapshot := make([]store.RunInterview, 0, len(sources))
	for _, iv := range sources {
		snapshot = append(snapshot, store.RunInterview{ID: iv.ID, Title: iv.Title})
	}
	run, err := f.st.CreateRun(ctx, f.project, store.NewRun{Interviews: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	epic, err := f.st.CreateEpic(ctx, f.project, "Contract workflow request", "Extend the existing contract launch and control workflow within current boundaries.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET discovery_run_id=$2 WHERE id=$1`, epic.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	// The producer permits 60 needs and 5 quotes. Use 21 valid needs and put a distinct
	// source on n9, the last ID in the store's stable lexical order.
	for i := 1; i <= 21; i++ {
		need := discovery.Need{ID: fmt.Sprintf("n%d", i), Title: fmt.Sprintf("Workflow requirement %d", i), Kind: discovery.KindNeed, Importance: discovery.ImportanceMedium}
		refs := sources[:1]
		if i == 1 {
			need.Importance = discovery.ImportanceHigh
			refs = sources[:discovery.MaxQuotesPerNeed]
		}
		if i == 9 {
			refs = sources[5:]
		}
		for _, iv := range refs {
			need.Evidence = append(need.Evidence, discovery.QuoteRef{Interview: iv.ID, InterviewTitle: iv.Title, RespondentRole: iv.RespondentRole, Text: strings.Split(iv.Text, "\n")[0], Line: 1, Kind: discovery.MaterialFeedback})
		}
		raw, _ := json.Marshal(need)
		if _, err = f.st.Pool.Exec(ctx, `INSERT INTO epic_needs(epic_id,run_id,need_id,need) VALUES($1,$2,$3,$4)`, epic.ID, run.ID, need.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	original, err := f.st.EpicNeeds(ctx, epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var architectCalls, plannerCalls atomic.Int32
	f.api.Engine.OpsComplete = func(ctx context.Context, _, _, prompt string) (string, error) {
		architectCalls.Add(1)
		input := strings.SplitN(prompt, "ВХОДНЫЕ ДАННЫЕ:\n", 2)
		if len(input) != 2 {
			t.Error("missing structured input")
			return "", fmt.Errorf("missing input")
		}
		var data struct {
			Needs      []discovery.Need `json:"needs"`
			Projection struct {
				TotalNeeds   int `json:"total_needs"`
				ShownNeeds   int `json:"shown_needs"`
				OmittedNeeds int `json:"omitted_needs"`
				Evidence     []struct {
					NeedID        string   `json:"need_id"`
					TotalQuotes   int      `json:"total_quotes"`
					ShownQuotes   int      `json:"shown_quotes"`
					OmittedQuotes int      `json:"omitted_quotes"`
					SourceIDs     []string `json:"source_ids"`
				} `json:"evidence"`
			} `json:"needs_projection"`
			Sources []struct {
				ID, Text, Source, SHA256, Kind string
				Deleted                        *bool
			} `json:"source_materials"`
		}
		if err := json.Unmarshal([]byte(input[1]), &data); err != nil {
			t.Error(err)
		}
		if len(data.Needs) != planner.MaxContextNeeds || data.Needs[0].ID != "n1" || len(data.Needs[0].Evidence) != planner.MaxContextQuotes || data.Projection.TotalNeeds != 21 || data.Projection.OmittedNeeds != 1 {
			t.Errorf("wrong bounded projection %+v", data.Projection)
		}
		if len(data.Sources) != len(sources) {
			t.Errorf("late source omitted: %d", len(data.Sources))
		}
		for _, iv := range sources {
			found := false
			for _, src := range data.Sources {
				if src.ID == iv.ID {
					found = true
					if src.Text != iv.Text || src.SHA256 != iv.SHA256 || src.Source != iv.RespondentRole || src.Kind != discovery.MaterialFeedback || src.Deleted == nil || *src.Deleted {
						t.Error("source text/hash/deleted/scope changed")
					}
				}
			}
			if !found {
				t.Error("lost original material", iv.ID)
			}
		}
		for _, meta := range data.Projection.Evidence {
			if meta.NeedID == "n1" && (meta.TotalQuotes != 5 || meta.OmittedQuotes != 2 || len(meta.SourceIDs) != 5) {
				t.Error("lost fourth/fifth source references")
			}
			if meta.NeedID == "n9" && (meta.ShownQuotes != 0 || len(meta.SourceIDs) != 1 || meta.SourceIDs[0] != sources[5].ID) {
				t.Error("lost omitted need reference")
			}
		}
		if !strings.Contains(input[1], "LATE_SOURCE_ONLY_BOUNDARY") || !strings.Contains(input[1], "OMITTED_NEED_ONLY_BOUNDARY") {
			t.Error("lost full source restrictions")
		}
		close(started)
		select {
		case <-release:
			return `{"decision":"no_change","summary":"Use the existing workflow boundary","impact":[],"constraints":["LATE_SOURCE_ONLY_BOUNDARY","OMITTED_NEED_ONLY_BOUNDARY"],"documentation":[],"risks":[]}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(_ context.Context, prompt string) (string, error) {
		plannerCalls.Add(1)
		if !strings.Contains(prompt, "LATE_SOURCE_ONLY_BOUNDARY") || !strings.Contains(prompt, "OMITTED_NEED_ONLY_BOUNDARY") {
			t.Error("assessment constraints missing from planner")
		}
		return `[{"title":"Extend current workflow","description":"Extend the existing workflow within current boundaries.","criteria":["Existing access controls remain effective"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	endpoint := f.srv.URL + "/api/v1/epics/" + epic.ID
	resp, _ := call(t, "POST", endpoint+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "requested plan waits for real assessment")
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		v, _ := f.st.EpicAssessment(ctx, epic.ID)
		t.Fatalf("valid 5-quote / 21-need input blocked architect: %+v", v.Assessment)
	}
	if plannerCalls.Load() != 0 {
		t.Fatal("planner bypassed assessment")
	}
	resp, _ = call(t, "POST", endpoint+"/start", f.user, "", nil)
	mustStatus(t, resp, http.StatusConflict, "gate remains closed while architect runs")
	releaseOnce.Do(func() { close(release) })
	f.api.Engine.WaitOps()
	waitAssessment(t, f, epic.ID, "passed")
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		tasks, err := f.st.ListEpicTasks(ctx, epic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) == 1 {
			if tasks[0].Status != domain.TaskQueued {
				t.Fatal("product work auto-started")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ordinary requested planning did not resume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := f.st.EpicNeeds(ctx, epic.ID)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("stored full needs/evidence/references changed", err)
	}
	if architectCalls.Load() != 1 || plannerCalls.Load() != 1 {
		t.Fatal("duplicate/missing gate work", architectCalls.Load(), plannerCalls.Load())
	}
	t.Log("valid 5 quotes / 21 needs -> architect 20/3 and all 6 full sources -> passed -> planner; originals intact")
}
