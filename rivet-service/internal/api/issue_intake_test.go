package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

type intakeSCM struct {
	*scm.Fake
	mu         sync.Mutex
	issues     []scm.Issue
	calls      []int
	validators []string
	err        error
	read       func(int, string) (scm.IssuePage, error)
}

func (a *intakeSCM) ListIssues(_ context.Context, _, _ string, page int, etag string) (scm.IssuePage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, page)
	a.validators = append(a.validators, etag)
	if a.read != nil {
		return a.read(page, etag)
	}
	return scm.IssuePage{Issues: append([]scm.Issue(nil), a.issues...), ETag: `"test"`}, a.err
}

func TestIssueIntakePaginationAndDisableDuringFetch(t *testing.T) {
	f := seedDiscovery(t, "")
	ctx := context.Background()
	a := &intakeSCM{Fake: scm.NewFake()}
	a.read = func(page int, _ string) (scm.IssuePage, error) {
		next := page + 1
		if page == 7 {
			next = 0
		}
		return scm.IssuePage{Issues: []scm.Issue{{Number: page, Title: "request", Body: "need search"}}, NextPage: next, ETag: "never cache paginated"}, nil
	}
	f.api.Engine = orchestrator.New(f.st, a, nil, nil, time.Minute)
	c := store.IssueIntakeConfig{Enabled: true, Label: "rivet"}
	if err := f.st.SetIssueIntake(ctx, f.project, f.uid.ID, c); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		forceIssueDue(t, f)
		if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
			t.Fatal(err)
		}
		var page int
		var etag string
		if err := f.st.Pool.QueryRow(ctx, `SELECT next_page,etag FROM project_issue_intake WHERE project_id=$1`, f.project).Scan(&page, &etag); err != nil {
			t.Fatal(err)
		}
		want := 4
		if i == 1 {
			want = 7
		}
		if i == 2 {
			want = 1
		}
		if page != want || etag != "" {
			t.Fatalf("durable cursor at poll %d: %d %s", i, page, etag)
		}
	}
	v := intakeView(t, f)
	if len(v.Sources) != 7 {
		t.Fatalf("bounded scan lost sources %+v", v)
	}
	for _, etag := range a.validators {
		if etag != "" {
			t.Fatalf("unsafe pagination ETag %q", etag)
		}
	}
	started, release := make(chan struct{}), make(chan struct{})
	a.read = func(_ int, _ string) (scm.IssuePage, error) {
		close(started)
		<-release
		return scm.IssuePage{Issues: []scm.Issue{{Number: 99, Title: "stale", Body: "not allowed"}}}, nil
	}
	forceIssueDue(t, f)
	done := make(chan error, 1)
	go func() { done <- f.api.PollProjectIssues(ctx, f.project, "gleb") }()
	<-started
	c.Enabled = false
	if err := f.st.SetIssueIntake(ctx, f.project, f.uid.ID, c); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	if len(v.Sources) != 7 || v.Enabled {
		t.Fatalf("disable failed to cancel in-flight intake: %+v", v)
	}
}
func forceIssueDue(t *testing.T, f discFixture) {
	t.Helper()
	if _, err := f.st.Pool.Exec(context.Background(), `UPDATE project_issue_intake SET next_poll_at=now()-interval '1 second' WHERE project_id=$1`, f.project); err != nil {
		t.Fatal(err)
	}
}
func intakeView(t *testing.T, f discFixture) store.IssueIntakeView {
	t.Helper()
	v, err := f.st.IssueIntake(context.Background(), f.project)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIssueIntakeOperationsToRealResearch(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	llm := fakeAnalystLLM(t, gate)
	f := seedDiscovery(t, llm.URL)
	ctx := context.Background()
	a := &intakeSCM{Fake: scm.NewFake(), issues: []scm.Issue{{Number: 1, Title: "Enhancement", Body: "Нужен поиск клиентов.\nСейчас теряю контакты."}}}
	e := orchestrator.New(f.st, a, nil, nil, time.Minute)
	f.api.Engine = e
	e.IssueWatch = f.api.PollProjectIssues
	url := f.srv.URL + "/api/v1/projects/" + f.project + "/operations/github-issues"
	config := store.IssueIntakeConfig{Enabled: true, AutoResearch: true, Label: "rivet"}
	resp, _ := call(t, "PATCH", url, f.member, "", config)
	mustStatus(t, resp, http.StatusForbidden, "member cannot enable automatic work")
	resp, _ = call(t, "PATCH", url, f.user, "", store.IssueIntakeConfig{Enabled: true, Label: "a,b"})
	mustStatus(t, resp, http.StatusBadRequest, "one explicit label")
	resp, _ = call(t, "PATCH", url, f.user, "", config)
	mustStatus(t, resp, http.StatusOK, "owner opts in")
	resp, _ = call(t, "GET", url, f.admin, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "nonmember cannot read sources")
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=false WHERE id='analyst'`); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual scheduler dispatch → SRE worker, not a manual sync API.
	bg, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.api.RunIssueIntake(bg) }()
	deadline := time.Now().Add(5 * time.Second)
	for intakeView(t, f).LastCheckAt == nil {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("SRE did not check issues")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	e.WaitOps()
	v := intakeView(t, f)
	if len(v.Sources) != 1 || v.Sources[0].RunID != "" || !strings.Contains(v.Error, "включения роли Аналитика") {
		t.Fatalf("disabled gate: %+v", v)
	}
	var events int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND type='ops.job' AND payload->>'agent_id'='gleb' AND payload->>'state'='started'`, f.project).Scan(&events); err != nil || events != 1 {
		t.Fatalf("real Gleb worker: %d %v", events, err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=true WHERE id='analyst'`); err != nil {
		t.Fatal(err)
	}
	forceIssueDue(t, f)
	if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	rid := v.Sources[0].RunID
	if rid == "" || v.Sources[0].RunStatus != "running" {
		t.Fatalf("Vera not started: %+v", v)
	}
	run, err := f.st.GetRun(ctx, f.project, rid)
	if err != nil || run.AgentID != "vera" || run.Model != "fake-model" {
		t.Fatalf("configured research agent: %+v %v", run, err)
	}
	// A second source received while Vera is busy stays pending, without a task.
	a.mu.Lock()
	a.issues = append(a.issues, scm.Issue{Number: 2, Title: "Bug", Body: "Не сохраняется клиент."})
	a.mu.Unlock()
	forceIssueDue(t, f)
	if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	if len(v.Sources) != 2 || !strings.Contains(v.Error, "завершения текущего исследования") {
		t.Fatalf("busy gate: %+v", v)
	}
	release()
	ready := f.waitRun(t, rid, func(r apiRun) bool { return r.Status != "running" })
	if ready.Status != "ready" {
		t.Fatalf("real synthesis failed: %+v", ready)
	}
	forceIssueDue(t, f)
	if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	rid2 := v.Sources[0].RunID
	if rid2 == "" || rid2 == rid {
		t.Fatalf("pending source not resumed: %+v", v)
	}
	ready = f.waitRun(t, rid2, func(r apiRun) bool { return r.Status != "running" })
	if ready.Status != "ready" {
		t.Fatalf("second synthesis: %+v", ready)
	}
	var n int
	if err = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM epics WHERE project_id=$1`, f.project).Scan(&n); err != nil || n != 0 {
		t.Fatalf("approval bypass: %d %v", n, err)
	}
	// Only explicit, ordinary Discovery approval creates work. Its evidence
	// links both sources even when the first source's original run is older.
	if _, err = f.st.ApproveRun(ctx, f.project, rid2, f.uid.ID, ready.BacklogRev, false); err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	for _, source := range v.Sources {
		if len(source.Epics) == 0 {
			t.Fatalf("approved evidence lost for issue %d: %+v", source.Number, v)
		}
	}
	resp, raw := call(t, "GET", url, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "member sees handoff")
	if !json.Valid(raw) || !strings.Contains(string(raw), "research_run_id") {
		t.Fatalf("source trace missing: %s", raw)
	}
}

func TestIssueIntakeWaitsForModelBudgetAndRedactsFailures(t *testing.T) {
	f := seedDiscovery(t, "")
	ctx := context.Background()
	a := &intakeSCM{Fake: scm.NewFake(), issues: []scm.Issue{{Number: 1, Title: "request", Body: "Need a search field."}}}
	f.api.Engine = orchestrator.New(f.st, a, nil, nil, time.Minute)
	if err := f.st.SetIssueIntake(ctx, f.project, f.uid.ID, store.IssueIntakeConfig{Enabled: true, AutoResearch: true, Label: "rivet"}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
		t.Fatal(err)
	}
	v := intakeView(t, f)
	if len(v.Sources) != 1 || v.Sources[0].RunID != "" || !strings.Contains(v.Error, "модели Аналитика") {
		t.Fatalf("model gate: %+v", v)
	}
	budget := int64(1)
	if _, err := f.st.SaveProjectPolicy(ctx, f.project, policy.Overrides{DailyTokenBudget: &budget}, "owner"); err != nil {
		t.Fatal(err)
	}
	in := int64(2)
	if err := f.st.RecordUsage(ctx, store.UsageInput{ProjectID: f.project, Role: "analyst", Model: "test", TokensIn: &in}); err != nil {
		t.Fatal(err)
	}
	forceIssueDue(t, f)
	if err := f.api.PollProjectIssues(ctx, f.project, "gleb"); err != nil {
		t.Fatal(err)
	}
	v = intakeView(t, f)
	if !strings.Contains(v.Error, "бюджета") {
		t.Fatalf("budget gate: %+v", v)
	}
	a.err = errors.New("SECRET token + issue body + remote error")
	forceIssueDue(t, f)
	err := f.api.PollProjectIssues(ctx, f.project, "gleb")
	v = intakeView(t, f)
	if err == nil || strings.Contains(err.Error()+v.Error, "SECRET") || v.Sources[0].Attempts != 0 {
		t.Fatalf("unsafe error/attempt spent: %+v %v", v, err)
	}
}
