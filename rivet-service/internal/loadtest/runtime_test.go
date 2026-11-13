package loadtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func realK6(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RIVET_K6_TEST_BIN")
	if bin == "" {
		bin = "k6"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		if os.Getenv("RIVET_REQUIRE_K6") == "1" {
			t.Fatal("real k6 required", err)
		}
		t.Skip("k6 not installed; CI installs pinned k6")
	}
	return path
}
func fixtureJob(t *testing.T, url string, p Profile) Job {
	t.Helper()
	port, err := strconv.Atoi(strings.Split(strings.TrimPrefix(url, "http://"), ":")[1])
	if err != nil {
		t.Fatal(err)
	}
	return Job{RunID: "fixture", AssignmentID: "lease", Target: Target{Port: port}, Profile: p, Limits: DefaultLimits(), Deadline: time.Now().Add(60 * time.Second)}
}
func TestNativeK6RealSummaryPassAndCleanSettings(t *testing.T) {
	bin := realK6(t)
	var proxyCalls, requests atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	auth := "Bearer isolated-credential-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/api/companies" && r.URL.Path != "/api/deals" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	tmp := t.TempDir()
	export := filepath.Join(tmp, "unexpected-export.json")
	config := filepath.Join(tmp, "user-config.json")
	if err := os.WriteFile(config, []byte(`{"out":["json=`+export+`"],"vus":9999}`), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"TMPDIR": tmp, "K6_OUT": "json=" + export, "K6_CONFIG": config, "K6_WEB_DASHBOARD": "true", "HTTP_PROXY": trap.URL, "HTTPS_PROXY": trap.URL, "ALL_PROXY": trap.URL} {
		t.Setenv(key, value)
	}
	p := testProfile()
	p.Auth = &Auth{Header: "Authorization", SecretName: "TEST_AUTH"}
	p.Routes = append(p.Routes, Route{Path: "/api/deals", Weight: 2, ExpectedStatus: 200, ExpectedContentType: "application/json"})
	j := fixtureJob(t, server.URL, p)
	j.AuthValue = auth
	result := (Runtime{K6Bin: bin, Inspect: func(_ context.Context, target Target) error {
		if target.Port != j.Target.Port {
			t.Fatal("wrong target")
		}
		return nil
	}}).Execute(context.Background(), j)
	report, err := BuildReport(p, Binding{}, result)
	if err != nil || ReportState(report) != "passed" || result.State != "passed" {
		b, _ := json.Marshal(report)
		t.Fatalf("real k6: state=%s reason=%+v report=%s err=%v", result.State, result.Reason, b, err)
	}
	if result.K6Version != "2.3.0" && !strings.HasPrefix(result.K6Version, "2.3.") {
		t.Fatal(result.K6Version)
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("k6/preflight inherited proxy")
	}
	if _, err := os.Stat(export); !os.IsNotExist(err) {
		t.Fatal("k6 inherited output config", err)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), auth) {
		t.Fatal("credential in result")
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rivet-k6-") {
			t.Fatal("secret workspace retained")
		}
	}
	if len(report.Stages) != 1 || len(report.Stages[0].Routes) != 2 || requests.Load() < 20 {
		t.Fatal("missing actual route measurements", requests.Load())
	}
	t.Logf("actual k6 %s: requests=%d p95=%.3f errors=%.3f dropped=%d", result.K6Version, report.Stages[0].Requests, *report.Stages[0].P95, *report.Stages[0].ErrorRate, report.Stages[0].Dropped)
}
func TestNativeK6ThresholdAbortAndPreflight(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n > 1 {
			w.WriteHeader(500)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	p := testProfile()
	p.Ceiling = 5
	p.TargetDurationS = 30
	start := time.Now()
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }}).Execute(context.Background(), fixtureJob(t, server.URL, p))
	report, err := BuildReport(p, Binding{}, result)
	if err != nil || result.State != "failed" || report.TargetResult != "failed" || len(result.Stages) != 1 || time.Since(start) > 25*time.Second {
		t.Fatalf("SLO failure did not stop capacity: %+v %+v %v", result.Reason, report, err)
	}
	if *report.Stages[0].ErrorRate <= 0 || report.Stages[0].Requests == 0 {
		t.Fatal("missing real failure metrics")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, http.StatusFound) }))
	defer redirect.Close()
	before := requests.Load()
	result = (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }}).Execute(context.Background(), fixtureJob(t, redirect.URL, testProfile()))
	if result.State != "config_needed" || len(result.Stages) != 0 || requests.Load() != before {
		t.Fatal("redirect followed or load started", result.State)
	}
}
func TestNativeK6CancellationStopsProcess(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }, Progress: func(_ string, _ Result) error { time.AfterFunc(700*time.Millisecond, cancel); return nil }}
	start := time.Now()
	result := runtime.Execute(ctx, fixtureJob(t, server.URL, testProfile()))
	if result.State != "interrupted" || time.Since(start) > 5*time.Second {
		t.Fatal("cancel failed", result.State, time.Since(start))
	}
	n := requests.Load()
	time.Sleep(150 * time.Millisecond)
	if requests.Load() != n {
		t.Fatal("orphan continued sending requests")
	}
}

func TestNativeK6VUsTargetPassCapacityBoundary(t *testing.T) {
	bin := realK6(t)
	var inflight atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if n > 1 {
			w.WriteHeader(500)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	p := testProfile()
	p.Unit = "vus"
	p.Target = 1
	p.Ceiling = 2
	p.MaxVUs = 2
	p.ThinkTimeMS = 1000
	job := fixtureJob(t, server.URL, p)
	var phases []string
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }, Progress: func(stage string, result Result) error {
		phases = append(phases, stage)
		if len(result.Stages) != len(phases)-1 {
			t.Fatal("checkpoint before completed evidence")
		}
		return nil
	}}).Execute(context.Background(), job)
	report, err := BuildReport(p, Binding{}, result)
	if err != nil || result.State != "passed" || report.TargetResult != "passed" || report.CapacityResult != "slo_boundary" || len(report.Stages) != 2 || report.HighestPassing == nil || *report.HighestPassing != 1 || report.FirstFailing == nil || *report.FirstFailing != 2 {
		b, _ := json.Marshal(report)
		t.Fatalf("VUs boundary result=%+v report=%s err=%v", result.Reason, b, err)
	}
	if len(phases) != 2 || report.Stages[0].Status != "passed" || report.Stages[1].Status != "failed" {
		t.Fatal(phases, report.Stages)
	}
	t.Logf("actual k6 %s VUs: target1 PASS requests=%d; capacity2 boundary errors=%.3f", result.K6Version, report.Stages[0].Requests, *report.Stages[1].ErrorRate)
}
func TestNativeK6DroppedRateIsInconclusive(t *testing.T) {
	bin := realK6(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	p := testProfile()
	p.Target = 50
	p.Ceiling = 50
	p.MaxVUs = 1
	p.P95MS = 1000
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }}).Execute(context.Background(), fixtureJob(t, server.URL, p))
	report, err := BuildReport(p, Binding{}, result)
	if err != nil || result.State != "inconclusive" || len(report.Stages) != 1 || report.Stages[0].Dropped == 0 {
		t.Fatalf("dropped result %+v report %+v err %v", result.Reason, report, err)
	}
	t.Logf("actual generator shortage: achieved %.2f of50RPS; dropped=%d", *report.Stages[0].RPS, report.Stages[0].Dropped)
}
func TestNativeK6CheckpointRejectStopsTraffic(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
	}))
	defer server.Close()
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }, Progress: func(string, Result) error { return context.Canceled }}).Execute(context.Background(), fixtureJob(t, server.URL, testProfile()))
	if result.State != "interrupted" || requests.Load() != 1 || len(result.Stages) != 0 {
		t.Fatal(result, requests.Load())
	}
}
func TestNativeK6DeadlineCannotStartOversizedPhase(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
	}))
	defer server.Close()
	j := fixtureJob(t, server.URL, testProfile())
	j.Deadline = time.Now().Add(time.Second)
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }}).Execute(context.Background(), j)
	if result.State != "interrupted" || len(result.Stages) != 0 || requests.Load() != 1 {
		t.Fatal(result, requests.Load())
	}
}

func TestNativeK6DelayedCheckpointCannotConsumePhaseBudget(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
	}))
	defer server.Close()
	job := fixtureJob(t, server.URL, testProfile())
	job.Deadline = time.Now().Add(15200 * time.Millisecond)
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }, Progress: func(string, Result) error { time.Sleep(300 * time.Millisecond); return nil }}).Execute(context.Background(), job)
	if result.State != "interrupted" || requests.Load() != 1 || len(result.Stages) != 0 {
		t.Fatal("late grant started native load", result.Reason, requests.Load())
	}
}
func TestNativeK6ContextDeadlineKillsActualProcess(t *testing.T) {
	bin := realK6(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error { return nil }}).Execute(ctx, fixtureJob(t, server.URL, testProfile()))
	if result.State != "interrupted" || requests.Load() < 2 {
		t.Fatal("deadline did not stop actual native k6", result.Reason, requests.Load())
	}
	before := requests.Load()
	time.Sleep(200 * time.Millisecond)
	if requests.Load() != before {
		t.Fatal("native requests after deadline result")
	}
}
