package analyst

import (
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionFixtureSummarySynthesisPipeline(t *testing.T) {
	paths, err := filepath.Glob("testdata/outcomes/*.json")
	if err != nil || len(paths) != 5 {
		t.Fatal(paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var fixture struct{ Name, Text, Outcome, Rationale string }
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(b, &fixture); err != nil {
				t.Fatal(err)
			}
			st := testStore(t)
			f := seed(t, st)
			if _, err := st.AddInterview(context.Background(), f.project, f.user, store.NewInterview{Title: fixture.Name, Text: fixture.Text}); err != nil {
				t.Fatal(err)
			}
			m := &fakeModel{synth: func(_ int, prompt string) (string, error, bool) {
				if !strings.Contains(prompt, strings.TrimPrefix(fixture.Text, "**Р:** ")) {
					t.Error("summary lost causal material")
				}
				if fixture.Outcome == discovery.OutcomeBacklogReady {
					return synthesisReply(prompt), nil, true
				}
				d := discovery.Decision{Outcome: fixture.Outcome, Rationale: fixture.Rationale, Questions: []discovery.Question{}, Sources: []discovery.DecisionSource{}}
				if fixture.Outcome == discovery.OutcomeNeedsClarification {
					d.Questions = []discovery.Question{{ID: "integration", Text: "В какой системе хранятся договоры и какое API доступно?", Reason: "Нужна граница интеграции", Sources: []discovery.DecisionSource{}}}
				}
				b, _ := json.Marshal(discovery.Synthesis{Decision: d, Personas: []discovery.Persona{}, Needs: []discovery.Need{}, Epics: []discovery.BacklogEpic{}})
				return string(b), nil, true
			}}
			job := f.job(t, m)
			(&Runner{St: st}).Run(context.Background(), job)
			run, err := st.GetRun(context.Background(), f.project, job.Run.ID)
			if err != nil || run.Status != store.RunReady || run.Result == nil || run.Result.Outcome != fixture.Outcome {
				t.Fatalf("%+v %v", run, err)
			}
			var count int
			_ = st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM epics WHERE project_id=$1`, f.project).Scan(&count)
			if count != 0 {
				t.Fatal("analysis auto-created work")
			}
			if fixture.Outcome != discovery.OutcomeBacklogReady && len(run.Backlog) != 0 {
				t.Fatal("fake backlog")
			}
			// Answer is a new material. It does not silently start analysis or edit old result.
			if _, err := st.AddInterview(context.Background(), f.project, f.user, store.NewInterview{Title: "Ответ", Text: "**Р:** Ответ на вопрос integration версии 1: использовать API ECM, доступ предоставлен."}); err != nil {
				t.Fatal(err)
			}
			unchanged, _ := st.GetRun(context.Background(), f.project, job.Run.ID)
			if unchanged.Result.Outcome != fixture.Outcome {
				t.Fatal("answer silently changed outcome")
			}
		})
	}
}
