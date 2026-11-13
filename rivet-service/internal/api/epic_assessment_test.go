package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

const assessmentReply = `{"decision":"changes_required","summary":"Preserve current boundary","impact":["counter API"],"constraints":["Keep tenant isolation"],"documentation":["Document counter API"],"risks":[]}`

func enableAssessmentAPI(t *testing.T, f discFixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.Pool.Exec(ctx, `ALTER TABLE epics ENABLE TRIGGER prepare_epic_architecture`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='fake',model='fake-model' WHERE id='ada'`); err != nil {
		t.Fatal(err)
	}
	f.api.Engine = orchestrator.New(f.st, scm.NewFake(), nil, nil, time.Minute)
}

func waitAssessment(t *testing.T, f discFixture, id, status string) store.AssessmentView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := f.st.EpicAssessment(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Assessment != nil && v.Assessment.Status == status {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("assessment not %s: %+v", status, v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResearchApprovalRunsAdaBeforeRequestedPlanner(t *testing.T) {
	llm := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llm.URL)
	enableAssessmentAPI(t, f)
	ctx := context.Background()
	var adaCalls, plannerCalls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f.api.Engine.OpsComplete = func(ctx context.Context, conn, model, prompt string) (string, error) {
		if conn != "fake" || model != "fake-model" || !strings.Contains(prompt, "[rivet:epic_architecture_assessment]") {
			t.Error("wrong architect/model/job")
		}
		if !strings.Contains(prompt, "discovery_run_id") || !strings.Contains(prompt, "https://github.com/o/crm/issues/7") {
			t.Error("research source missing from architect input")
		}
		adaCalls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
			return assessmentReply, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(_ context.Context, prompt string) (string, error) {
		plannerCalls.Add(1)
		if !strings.Contains(prompt, "Keep tenant isolation") || !strings.Contains(prompt, "Document counter API") {
			t.Error("architect constraints not delivered to planner")
		}
		return `[{"title":"Implement company counter","description":"Return total and filtered counts within the tenant boundary.","criteria":["Tenant isolation is preserved"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	if _, err := f.st.AddFeedback(ctx, f.project, f.uid.ID, store.NewFeedback{Source: "https://github.com/o/crm/issues/7", Text: "Не вижу количество компаний.\nНужен результат поиска с числом компаний."}); err != nil {
		t.Fatal(err)
	}
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	resp, raw := call(t, "POST", base+"/analyses", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "start normal Vera research")
	var startedRun apiRun
	_ = json.Unmarshal(raw, &startedRun)
	ready := f.waitRun(t, startedRun.ID, func(r apiRun) bool { return r.Status != "running" })
	if ready.Status != "ready" {
		t.Fatalf("research failed: %+v", ready)
	}
	resp, raw = call(t, "POST", base+"/analyses/"+ready.ID+"/approve", f.user, "", map[string]any{"backlog_rev": ready.BacklogRev, "decompose": true})
	mustStatus(t, resp, http.StatusOK, "normal user backlog approval")
	var approved struct {
		Epics []store.ApprovedEpic `json:"epics"`
	}
	if err := json.Unmarshal(raw, &approved); err != nil || len(approved.Epics) != 2 {
		t.Fatalf("approval %s %v", raw, err)
	}
	if err := f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("Ada did not start")
	}
	id := approved.Epics[0].EpicID
	view, err := f.st.LoadCycle(ctx, f.project, "7d", time.UTC, f.api.Engine.OpsBusy())
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range view.Phases {
		if phase.ID == cycle.Planning && (phase.Counters.Active+phase.Counters.Waiting+phase.Counters.NeedsHuman) != 0 {
			t.Fatalf("premature Planning: %+v", phase)
		}
		if phase.ID == cycle.Architecture {
			if phase.NoData || phase.Counters.Active != 1 {
				t.Fatalf("architecture work missing %+v", phase)
			}
			if len(phase.Agents) == 0 || phase.Agents[0].Name != "Ада" || phase.Agents[0].State != cycle.AgentWorking {
				t.Fatalf("Ada not shown working: %+v", phase.Agents)
			}
		}
	}
	if plannerCalls.Load() != 0 {
		t.Fatal("planner ran before architect")
	}
	resp, teamRaw := call(t, "GET", f.srv.URL+"/api/v1/team", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "actual architect busy metadata")
	var team struct {
		Agents []struct {
			ID   string `json:"id"`
			Busy bool   `json:"busy"`
			Step string `json:"step"`
		} `json:"agents"`
	}
	_ = json.Unmarshal(teamRaw, &team)
	found := false
	for _, a := range team.Agents {
		if a.ID == "ada" {
			found = a.Busy && a.Step == "architecture_assessment"
		}
	}
	if !found {
		t.Fatalf("Ada not busy in team API: %s", teamRaw)
	}
	resp, teamRaw = call(t, "GET", f.srv.URL+"/api/v1/team", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "nonmember team metadata")
	for _, a := range approved.Epics {
		if strings.Contains(string(teamRaw), a.EpicID) {
			t.Fatal("assessment disclosed Epic to nonmember")
		}
	}
	resp, raw = call(t, "POST", f.srv.URL+"/api/v1/epics/"+id+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "manual request waits same gate")
	if !strings.Contains(string(raw), "architecture_pending") {
		t.Fatalf("202 without waiting state %s", raw)
	}
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/epics/"+id+"/start", f.user, "", nil)
	mustStatus(t, resp, http.StatusConflict, "cannot start before architecture")
	releaseOnce.Do(func() { close(release) })
	f.api.Engine.WaitOps()
	for _, a := range approved.Epics {
		waitAssessment(t, f, a.EpicID, "passed")
	}
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		if err = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE e.project_id=$1`, f.project).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("requested planning did not resume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, a := range approved.Epics {
		ep, err := f.st.GetEpic(ctx, a.EpicID)
		if err != nil || ep.Status != domain.EpicPlanned {
			t.Fatalf("coding auto-started: %+v %v", ep, err)
		}
		tasks, _ := f.st.ListEpicTasks(ctx, ep.ID)
		if len(tasks) != 1 || tasks[0].Status != domain.TaskQueued {
			t.Fatalf("unexpected plan %+v", tasks)
		}
	}
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	f.api.Engine.WaitOps()
	if adaCalls.Load() != 2 || plannerCalls.Load() != 2 {
		t.Fatalf("duplicate/missing work: Ada%d planner%d", adaCalls.Load(), plannerCalls.Load())
	}
	var badPhase int
	if err = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND type='ops.job' AND payload->>'kind'='epic_architecture_assessment' AND payload->>'phase_to'<>'architecture'`, f.project).Scan(&badPhase); err != nil || badPhase != 0 {
		t.Fatalf("architect labeled Release: %d %v", badPhase, err)
	}
}

func TestEpicAssessmentWorkerWaitsForRoleModelAndBudget(t *testing.T) {
	for _, which := range []string{"role", "model", "budget"} {
		t.Run(which, func(t *testing.T) {
			llm := fakeAnalystLLM(t, nil)
			f := seedDiscovery(t, llm.URL)
			enableAssessmentAPI(t, f)
			ctx := context.Background()
			var calls atomic.Int32
			f.api.Engine.OpsComplete = func(context.Context, string, string, string) (string, error) {
				calls.Add(1)
				return assessmentReply, nil
			}
			e, err := f.st.CreateEpic(ctx, f.project, "Manual request", "Add counter")
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "role":
				_, err = f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=false WHERE id='architect'`)
			case "model":
				_, err = f.st.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='',model='' WHERE id='ada'`)
			case "budget":
				limit := int64(1)
				_, err = f.st.SaveProjectPolicy(ctx, f.project, policy.Overrides{DailyTokenBudget: &limit}, "owner")
				if err == nil {
					tokens := int64(2)
					err = f.st.RecordUsage(ctx, store.UsageInput{ProjectID: f.project, Role: "analyst", Model: "test", TokensIn: &tokens})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
				t.Fatal(err)
			}
			f.api.Engine.WaitOps()
			v := waitAssessment(t, f, e.ID, "waiting")
			if calls.Load() != 0 || v.Assessment.Attempts != 0 || v.Assessment.Error == "" {
				t.Fatalf("missing visible gate: %+v calls%d", v, calls.Load())
			}
			switch which {
			case "role":
				_, err = f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=true WHERE id='architect'`)
			case "model":
				_, err = f.st.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='fake',model='fake-model' WHERE id='ada'`)
			case "budget":
				limit := int64(10000)
				_, err = f.st.SaveProjectPolicy(ctx, f.project, policy.Overrides{DailyTokenBudget: &limit}, "owner")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.st.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET next_attempt_at=now() WHERE epic_id=$1`, e.ID); err != nil {
				t.Fatal(err)
			}
			if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
				t.Fatal(err)
			}
			f.api.Engine.WaitOps()
			waitAssessment(t, f, e.ID, "passed")
			if calls.Load() != 1 {
				t.Fatalf("retry calls%d", calls.Load())
			}
		})
	}
}

func TestRecoveredQueuedPlanIsArchitectInputWithoutReplanning(t *testing.T) {
	llm := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llm.URL)
	enableAssessmentAPI(t, f)
	ctx := context.Background()
	e, err := f.st.CreateEpic(ctx, f.project, "Recovered plan", "counter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `WITH legacy AS (UPDATE epics SET architecture_required=false WHERE id=$1 RETURNING id)
		DELETE FROM epic_architecture_assessments WHERE epic_id IN (SELECT id FROM legacy)`, e.ID); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for range 5 {
		task, err := f.st.CreateTask(ctx, e.ID, store.NewTask{Title: "preserved task", Description: "existing implementation plan", Criteria: []domain.Criterion{{Text: "existing criterion"}}})
		if err != nil {
			t.Fatal(err)
		}
		ids[task.ID] = true
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET architecture_required=true WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `INSERT INTO epic_architecture_assessments(epic_id,project_id,revision) SELECT id,project_id,architecture_revision FROM epics WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	f.api.Engine.OpsComplete = func(_ context.Context, _, _, prompt string) (string, error) {
		for id := range ids {
			if !strings.Contains(prompt, id) {
				t.Errorf("existing task %s omitted from assessment", id)
			}
		}
		if !strings.Contains(prompt, "existing implementation plan") || !strings.Contains(prompt, "existing criterion") {
			t.Error("existing plan context incomplete")
		}
		return assessmentReply, nil
	}
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	f.api.Engine.WaitOps()
	v := waitAssessment(t, f, e.ID, "passed")
	if !v.Assessment.ExistingPlan {
		t.Fatal("legacy plan not identified in API")
	}
	tasks, err := f.st.ListEpicTasks(ctx, e.ID)
	if err != nil || len(tasks) != 5 {
		t.Fatalf("plan changed %d %v", len(tasks), err)
	}
	for _, task := range tasks {
		if !ids[task.ID] || task.Status != domain.TaskQueued || task.Description != "existing implementation plan" {
			t.Fatalf("legacy task rewritten %+v", task)
		}
	}
	if ids, err := f.st.DueAssessmentPlans(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("legacy plan scheduled again %+v %v", ids, err)
	}
}

func TestAssessmentRejectsNeedsBeyondDiscoveryLimit(t *testing.T) {
	llm := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llm.URL)
	enableAssessmentAPI(t, f)
	ctx := context.Background()
	e, err := f.st.CreateEpic(ctx, f.project, "bounded research", "counter")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.st.CreateRun(ctx, f.project, store.NewRun{Interviews: []store.RunInterview{{ID: f.project, Title: "test source"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET discovery_run_id=$2 WHERE id=$1`, e.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `INSERT INTO epic_needs(epic_id,run_id,need_id,need)
		SELECT $1,$2,'n'||g,jsonb_build_object('id','n'||g,'title','requirement','evidence','[]'::jsonb) FROM generate_series(1,$3::int) g`, e.ID, run.ID, discovery.MaxNeeds+1); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f.api.Engine.OpsComplete = func(context.Context, string, string, string) (string, error) {
		calls.Add(1)
		return assessmentReply, nil
	}
	if err = f.api.ReconcileEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	f.api.Engine.WaitOps()
	v := waitAssessment(t, f, e.ID, "waiting")
	if calls.Load() != 0 || v.Assessment.Attempts != 0 || !strings.Contains(v.Assessment.Error, "лимит результата исследования") {
		t.Fatalf("unseen requirements silently accepted: %+v", v)
	}
}
