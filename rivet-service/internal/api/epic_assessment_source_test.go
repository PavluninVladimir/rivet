package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func linkAssessmentSource(t *testing.T, f discFixture, epicID, sourceID, quote string) {
	t.Helper()
	ctx := context.Background()
	run, err := f.st.CreateRun(ctx, f.project, store.NewRun{Interviews: []store.RunInterview{{ID: sourceID, Title: "Issue 7"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET discovery_run_id=$2 WHERE id=$1`, epicID, run.ID); err != nil {
		t.Fatal(err)
	}
	need, _ := json.Marshal(map[string]any{"id": "n1", "title": "Company count", "evidence": []any{
		map[string]any{"interview": sourceID, "line": 1, "text": quote},
		map[string]any{"interview": sourceID, "line": 1, "text": quote},
	}})
	if _, err = f.st.Pool.Exec(ctx, `INSERT INTO epic_needs(epic_id,run_id,need_id,need) VALUES($1,$2,'n1',$3)`, epicID, run.ID, need); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerReassessmentIncludesFullSourceScopeWithStaleDocs(t *testing.T) {
	llm := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llm.URL)
	enableAssessmentAPI(t, f)
	ctx := context.Background()
	const quote = "Нужен счётчик компаний в списке и поиске."
	const scope = "Компании, поиск и CRUD уже работают. Небольшое изменение существующего интерфейса без изменения API, авторизации и структуры данных."
	const source = "https://github.com/o/crm/issues/7"
	iv, err := f.st.AddFeedback(ctx, f.project, f.uid.ID, store.NewFeedback{Source: source, Text: quote + "\n" + scope})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.AddFeedback(ctx, f.project, f.uid.ID, store.NewFeedback{Source: "unrelated", Text: "UNRELATED_MATERIAL_MUST_NOT_BE_INCLUDED"}); err != nil {
		t.Fatal(err)
	}
	epic, err := f.st.CreateEpic(ctx, f.project, "Company count", "Show total and filtered company counts")
	if err != nil {
		t.Fatal(err)
	}
	linkAssessmentSource(t, f, epic.ID, iv.ID, quote)
	if _, _, err = f.st.SaveProjectArchitecture(ctx, f.project, store.ArchitectureSave{Status: "ok", SHA: "old-auth-only", Data: json.RawMessage(`{"components":[{"id":"auth"}]}`)}); err != nil {
		t.Fatal(err)
	}
	// Existing passed result and queued plan reproduce the live upgrade case.
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET status='passed',result=$2 WHERE epic_id=$1`, epic.ID, assessmentReply); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 5 {
		task, err := f.st.CreateTask(ctx, epic.ID, store.NewTask{Title: "Preserved task", Description: "Keep original plan"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	base := f.srv.URL + "/api/v1/epics/" + epic.ID
	resp, raw := call(t, "GET", base+"/architecture-assessment", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "owner assessment")
	var view epicAssessmentView
	_ = json.Unmarshal(raw, &view)
	if !view.CanReevaluate {
		t.Fatal("owner cannot request reassessment")
	}
	oldRevision := view.Revision
	body := map[string]any{"revision": oldRevision}
	resp, raw = call(t, "GET", base+"/architecture-assessment", f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "member can read")
	_ = json.Unmarshal(raw, &view)
	if view.CanReevaluate {
		t.Fatal("member shown owner action")
	}
	resp, _ = call(t, "POST", base+"/architecture-assessment/reevaluate", f.member, "", body)
	mustStatus(t, resp, http.StatusForbidden, "member cannot invalidate passed result")
	resp, _ = call(t, "POST", base+"/architecture-assessment/reevaluate", f.admin, "", body)
	mustStatus(t, resp, http.StatusNotFound, "nonmember cannot discover Epic")
	resp, raw = call(t, "POST", base+"/architecture-assessment/reevaluate", f.user, "", body)
	mustStatus(t, resp, http.StatusAccepted, "explicit owner reassessment")
	view = epicAssessmentView{}
	_ = json.Unmarshal(raw, &view)
	if view.Revision != oldRevision+1 || view.Assessment.Status != "pending" || view.Assessment.Result != nil {
		t.Fatalf("old result not invalidated: %s", raw)
	}
	resp, _ = call(t, "POST", base+"/architecture-assessment/reevaluate", f.user, "", body)
	mustStatus(t, resp, http.StatusConflict, "stale duplicate request")
	resp, _ = call(t, "POST", base+"/start", f.user, "", nil)
	mustStatus(t, resp, http.StatusConflict, "reassessment closes first Start")
	var calls atomic.Int32
	f.api.Engine.OpsComplete = func(_ context.Context, _, _, prompt string) (string, error) {
		calls.Add(1)
		_, input, ok := strings.Cut(prompt, "ВХОДНЫЕ ДАННЫЕ:\n")
		var data struct {
			Sources        []struct{ ID, Text, Source, SHA256 string } `json:"source_materials"`
			Implementation string                                      `json:"implementation_evidence"`
			Plan           []json.RawMessage                           `json:"existing_plan"`
		}
		if !ok || json.Unmarshal([]byte(input), &data) != nil || len(data.Sources) != 1 || len(data.Plan) != 5 {
			t.Errorf("missing or unscoped full-source context: %s", input)
		} else if s := data.Sources[0]; s.ID != iv.ID || s.Text != quote+"\n"+scope || s.Source != source || s.SHA256 != iv.SHA256 {
			t.Error("source scope was reduced to selected quotes")
		}
		if data.Implementation != "not_inspected" || !strings.Contains(prompt, "НЕ доказывает отсутствие реализации") || !strings.Contains(prompt, "Не расширяй небольшое изменение интерфейса") || strings.Contains(prompt, "UNRELATED_MATERIAL") {
			t.Error("documentation-vs-code boundary or source scope missing")
		}
		return `{"decision":"no_change","summary":"Заявленная существующая реализация достаточна для UI-счётчика; код независимо не проверен.","impact":[],"constraints":["Без изменения API, авторизации и структуры данных"],"documentation":["Уточнить неполное описание существующих компонентов"],"risks":["Перед изменением проверить существующий код списка и поиска"]}`, nil
	}
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	f.api.Engine.WaitOps()
	view.AssessmentView = waitAssessment(t, f, epic.ID, "passed")
	if calls.Load() != 1 || view.Assessment.Result.Decision != "no_change" {
		t.Fatal("reassessment did not complete normally")
	}
	got, _ := f.st.GetEpic(ctx, epic.ID)
	if got.Goal != epic.Goal || got.Title != epic.Title || got.Status != domain.EpicPlanned || got.StartedAt != nil {
		t.Fatal("reassessment edited goal or auto-started work")
	}
	for _, id := range ids {
		task, err := f.st.GetTask(ctx, id)
		if err != nil || task.Status != domain.TaskQueued || task.Description != "Keep original plan" {
			t.Fatalf("preserved task changed: %+v %v", task, err)
		}
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, epic.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", base+"/architecture-assessment/reevaluate", f.user, "", map[string]any{"revision": view.Revision})
	mustStatus(t, resp, http.StatusConflict, "cannot reevaluate after first Start")
}

func TestAssessmentFullSourcesWaitWithoutTruncationOrCrossProjectRead(t *testing.T) {
	for _, which := range []string{"oversize", "foreign"} {
		t.Run(which, func(t *testing.T) {
			llm := fakeAnalystLLM(t, nil)
			f := seedDiscovery(t, llm.URL)
			enableAssessmentAPI(t, f)
			ctx := context.Background()
			e, err := f.st.CreateEpic(ctx, f.project, "Company count", "UI only")
			if err != nil {
				t.Fatal(err)
			}
			project := f.project
			if which == "foreign" {
				p, err := f.st.CreateProject(ctx, "Private", "o/private", nil, f.uid.ID)
				if err != nil {
					t.Fatal(err)
				}
				project = p.ID
			}
			text := "PRIVATE_SOURCE_MUST_NOT_BE_READ"
			if which == "oversize" {
				text = strings.Repeat("x", (128<<10)+1)
			}
			iv, err := f.st.AddInterview(ctx, project, f.uid.ID, store.NewInterview{Title: "Source", Text: text})
			if err != nil {
				t.Fatal(err)
			}
			linkAssessmentSource(t, f, e.ID, iv.ID, "short quote")
			var calls atomic.Int32
			f.api.Engine.OpsComplete = func(context.Context, string, string, string) (string, error) {
				calls.Add(1)
				return assessmentReply, nil
			}
			if err := f.api.ReconcileEpicAssessments(ctx); err != nil {
				t.Fatal(err)
			}
			f.api.Engine.WaitOps()
			v := waitAssessment(t, f, e.ID, "waiting")
			if calls.Load() != 0 || v.Assessment.Attempts != 0 || v.Assessment.Error == "" || strings.Contains(v.Assessment.Error, text) {
				t.Fatalf("source was truncated, leaked, or spent model budget: %+v", v)
			}
		})
	}
}
