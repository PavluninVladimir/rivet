package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"github.com/google/uuid"
)

type loadSender struct {
	messages  []*pb.PlaneMsg
	delivered bool
	secure    bool
	grants    atomic.Int64
}

func (s *loadSender) Send(_ string, m *pb.PlaneMsg) bool {
	if s.delivered {
		s.messages = append(s.messages, m)
		if m.GetLoadTestContinue() != nil {
			s.grants.Add(1)
		}
	}
	return s.delivered
}
func (s *loadSender) SendSecure(id string, m *pb.PlaneMsg) bool {
	if !s.secure {
		return false
	}
	return s.Send(id, m)
}
func TestLoadAPIQueueNativeK6PersistedReport(t *testing.T) {
	bin := os.Getenv("RIVET_K6_TEST_BIN")
	if bin == "" {
		bin = "k6"
	}
	bin, err := exec.LookPath(bin)
	if err != nil {
		if os.Getenv("RIVET_REQUIRE_K6") == "1" {
			t.Fatal(err)
		}
		t.Skip("native k6 fixture requires pinned CI dependency")
	}
	f := seedEnvAPI(t)
	ctx := context.Background()
	out := &loadSender{delivered: true, secure: true}
	engine := orchestrator.New(f.st, scm.NewFake(), nil, out, 90*time.Second)
	box, err := secretbox.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	engine.Box = box
	srv := httptest.NewServer((&Server{St: f.st, Engine: engine, Secrets: box}).Handler())
	defer srv.Close()
	owner := loginSession(t, srv, f.admin, "pw-testpass")
	member := loginSession(t, srv, f.member, "pw-testpass")
	other := loginSession(t, srv, f.mallory, "pw-testpass")
	// Own a free loopback fixture in the managed local-publication port range.
	var listener net.Listener
	port := 0
	for p := 25000; p < 29999; p++ {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			port = p
			break
		}
	}
	if port == 0 {
		t.Fatal("no isolated fixture port")
	}
	auth := "Bearer explicit-test-credential-canary"
	var requests atomic.Int64
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	target.Listener = listener
	target.Start()
	defer target.Close()
	env, err := f.st.CreateEnvironment(ctx, domain.Environment{ProjectID: f.projectID, Name: "load-fixture", ExecType: domain.ExecLocalDocker, Trigger: "manual", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}})
	if err != nil {
		t.Fatal(err)
	}
	publisher := "fixture-publisher"
	if err = f.st.UpsertRunner(ctx, domain.Runner{ID: publisher, Host: "fixture-host", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1", store.LocalDockerSecretsFeature}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	publication, err := f.st.EnqueueLocalPublication(ctx, env.ID, strings.Repeat("a", 40), "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok, err := f.st.StartNextDeployment(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, state := range []string{"building", "starting", "verifying", "running"} {
		result := localdocker.Result{Status: state}
		if state == "running" {
			result.ContainerID = strings.Repeat("b", 64)
			result.Port = port
			result.HealthURL = target.URL + "/"
		}
		if _, ok, err = f.st.AcceptLocalResult(ctx, publication.ActiveDeploymentID, assignment.RunnerID, result); err != nil || !ok {
			t.Fatal(state, ok, err)
		}
	}
	if _, err = f.st.CompleteDeployment(ctx, publication.ActiveDeploymentID, publisher); err != nil {
		t.Fatal(err)
	}
	if err = f.st.PatchApplicationSecrets(ctx, env.ID, store.ApplicationSecretPatch{Set: map[string]string{"LOAD_AUTH": auth}}, box, ""); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/environments/" + env.ID
	resp, raw := call(t, "GET", base+"/load-testing", owner, "", nil)
	mustStatus(t, resp, 200, "durable first-release question")
	var view store.LoadView
	if err = json.Unmarshal(raw, &view); err != nil || view.Prompt == nil || view.Binding == nil || !view.CanManage {
		t.Fatal(string(raw), err)
	}
	if requests.Load() != 0 {
		t.Fatal("question generated traffic")
	}
	profile := loadtest.Profile{Unit: "rps", Target: 2, Ceiling: 2, TargetDurationS: 10, MaxVUs: 2, P95MS: 500, Routes: []loadtest.Route{{Path: "/api/companies", Weight: 1, ExpectedStatus: 200, ExpectedContentType: "application/json"}}, Auth: &loadtest.Auth{Header: "Authorization", SecretName: "LOAD_AUTH"}}
	input := store.LoadStart{RequestID: uuid.NewString(), Binding: *view.Binding, Profile: profile}
	resp, _ = call(t, "POST", base+"/load-tests", member, "", input)
	mustStatus(t, resp, 403, "member cannot start load")
	resp, _ = call(t, "GET", base+"/load-testing", other, "", nil)
	mustStatus(t, resp, 404, "foreign project hidden")
	resp, raw = call(t, "POST", base+"/load-tests", owner, "", input)
	mustStatus(t, resp, 202, "owner queued explicit profile")
	var run store.LoadRun
	if err = json.Unmarshal(raw, &run); err != nil || run.State != "queued" {
		t.Fatal(string(raw), err)
	}
	// No dedicated runner: the queue must persist without sending any traffic.
	if err = engine.TickLoadTests(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || len(out.messages) != 0 {
		t.Fatal("load before capable runner")
	}
	if err = f.st.UpsertRunner(ctx, domain.Runner{ID: "fixture-k6", Host: "fixture-host", Capabilities: []string{"load"}, Features: []string{loadtest.Feature}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE runners SET boot_id='fixture-boot' WHERE id='fixture-k6'`); err != nil {
		t.Fatal(err)
	}
	if err = engine.TickLoadTests(ctx); err != nil {
		t.Fatal(err)
	}
	if len(out.messages) != 1 || out.messages[0].GetLoadTest() == nil {
		t.Fatal("missing additive load assignment", out.messages)
	}
	wire := out.messages[0].GetLoadTest()
	var job loadtest.Job
	if err = json.Unmarshal(wire.Payload, &job); err != nil || job.AuthValue != auth {
		t.Fatal("missing explicitly referenced auth", err)
	}
	runtime := loadtest.Runtime{K6Bin: bin, Inspect: func(_ context.Context, got loadtest.Target) error {
		if got.Binding != *view.Binding || got.Port != port {
			return fmt.Errorf("wrong isolated target")
		}
		return nil
	}, Progress: func(stage string, result loadtest.Result) error {
		raw, _ := json.Marshal(result)
		before := len(out.messages)
		if err := engine.OnLoadTestResult(ctx, "fixture-k6", &pb.LoadTestResult{RunId: run.ID, AssignmentId: wire.AssignmentId, Stage: stage, Payload: raw}); err != nil {
			return err
		}
		if len(out.messages) != before+1 || out.messages[before].GetLoadTestContinue() == nil {
			return fmt.Errorf("no committed checkpoint grant")
		}
		return nil
	}}
	result := runtime.Execute(ctx, job)
	rawResult, _ := json.Marshal(result)
	if err = engine.OnLoadTestResult(ctx, "fixture-k6", &pb.LoadTestResult{RunId: run.ID, AssignmentId: wire.AssignmentId, Payload: rawResult}); err != nil {
		t.Fatal(err)
	}
	resp, raw = call(t, "GET", srv.URL+"/api/v1/load-tests/"+run.ID, member, "", nil)
	mustStatus(t, resp, 200, "member persisted report")
	if err = json.Unmarshal(raw, &run); err != nil || run.State != "passed" || run.Report == nil || len(run.Report.Stages) != 1 || run.Report.Stages[0].Requests < 20 || run.Report.TargetResult != "passed" {
		t.Fatal(string(raw), err)
	}
	if strings.Contains(string(raw), auth) || strings.Contains(string(raw), "auth_value") || strings.Contains(string(raw), "raw_summary") {
		t.Fatal("secret/private summary leaked")
	}
	resp, raw = call(t, "GET", base+"/load-tests", owner, "", nil)
	mustStatus(t, resp, 200, "history")
	if !strings.Contains(string(raw), run.ID) {
		t.Fatal("report absent from history")
	}
	var busy bool
	if err = f.st.Pool.QueryRow(ctx, `SELECT load_test_id IS NOT NULL FROM runners WHERE id='fixture-k6'`).Scan(&busy); err != nil || busy {
		t.Fatal("runner ownership not released", err)
	}
	t.Logf("API -> durable queue -> native k6 %s -> DB -> member GET: %d requests; target=%s", run.Report.K6Version, run.Report.Stages[0].Requests, run.Report.TargetResult)
	// Explicit proof: a transport ACK/stale message or failed/pending transaction
	// cannot authorize the next native phase.
	input.RequestID = uuid.NewString()
	resp, raw = call(t, "POST", base+"/load-tests", owner, "", input)
	mustStatus(t, resp, 202, "checkpoint fixture")
	var checkpointRun store.LoadRun
	_ = json.Unmarshal(raw, &checkpointRun)
	if err = engine.TickLoadTests(ctx); err != nil {
		t.Fatal(err)
	}
	checkpointWire := out.messages[len(out.messages)-1].GetLoadTest()
	if checkpointWire == nil {
		t.Fatal("missing checkpoint assignment")
	}
	progressRaw, _ := json.Marshal(loadtest.Result{State: "running", Stages: []loadtest.Evidence{}})
	progress := &pb.LoadTestResult{RunId: checkpointRun.ID, AssignmentId: checkpointWire.AssignmentId, Stage: "target", Payload: progressRaw}
	baseline := out.grants.Load()
	stale := &pb.LoadTestResult{RunId: checkpointRun.ID, AssignmentId: "stale", Stage: "target", Payload: progressRaw}
	if err = engine.OnLoadTestResult(ctx, "fixture-k6", stale); err != nil || out.grants.Load() != baseline {
		t.Fatal("stale result granted", err)
	}
	if _, err = f.st.Pool.Exec(ctx, `CREATE FUNCTION reject_load_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'isolated checkpoint write failure'; END $$; CREATE TRIGGER reject_load_progress BEFORE UPDATE ON load_test_runs FOR EACH ROW EXECUTE FUNCTION reject_load_progress()`); err != nil {
		t.Fatal(err)
	}
	if err = engine.OnLoadTestResult(ctx, "fixture-k6", progress); err == nil || out.grants.Load() != baseline {
		t.Fatal("DB rejection granted phase", err)
	}
	if _, err = f.st.Pool.Exec(ctx, `DROP TRIGGER reject_load_progress ON load_test_runs; DROP FUNCTION reject_load_progress()`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM load_test_runs WHERE id=$1 FOR UPDATE`, checkpointRun.ID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- engine.OnLoadTestResult(ctx, "fixture-k6", progress) }()
	select {
	case err := <-finished:
		t.Fatal("checkpoint ignored uncommitted DB lock", err)
	case <-time.After(60 * time.Millisecond):
	}
	if out.grants.Load() != baseline {
		t.Fatal("phase granted before persistence")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("committed checkpoint did not finish")
	}
	if out.grants.Load() != baseline+1 {
		t.Fatal("missing post-commit grant")
	}
	resp, raw = call(t, "POST", srv.URL+"/api/v1/load-tests/"+checkpointRun.ID+"/cancel", owner, "", nil)
	mustStatus(t, resp, 200, "cancel waits for actual stop")
	var cancelling store.LoadRun
	_ = json.Unmarshal(raw, &cancelling)
	if !cancelling.Stopping {
		t.Fatal("cancel falsely proved quiescence", string(raw))
	}
	if err = engine.OnLoadTestResult(ctx, "fixture-k6", progress); err != nil || out.grants.Load() != baseline+1 {
		t.Fatal("cancelled checkpoint granted phase", err)
	}
	stopRaw, _ := json.Marshal(loadtest.Result{State: "interrupted", Stages: []loadtest.Evidence{}})
	if err = engine.OnLoadTestResult(ctx, "fixture-k6", &pb.LoadTestResult{RunId: checkpointRun.ID, AssignmentId: checkpointWire.AssignmentId, Payload: stopRaw}); err != nil {
		t.Fatal(err)
	}
	t.Log("checkpoint fencing: stale/DB reject/delayed commit/cancel all prevented premature grant")
	input.RequestID = uuid.NewString()
	input.Profile.Ceiling = 5
	resp, raw = call(t, "POST", base+"/load-tests", owner, "", input)
	mustStatus(t, resp, 202, "partial report fixture")
	var partialRun store.LoadRun
	_ = json.Unmarshal(raw, &partialRun)
	if err = engine.TickLoadTests(ctx); err != nil {
		t.Fatal(err)
	}
	partialWire := out.messages[len(out.messages)-1].GetLoadTest()
	if partialWire == nil {
		t.Fatal("missing partial assignment")
	}
	known := loadtest.Result{State: "running", K6Version: result.K6Version, Stages: result.Stages}
	if _, accepted, err := f.st.AcceptLoadResult(ctx, "fixture-k6", partialRun.ID, partialWire.AssignmentId, "capacity_1", known); err != nil || !accepted {
		t.Fatal("known target evidence rejected", accepted, err)
	}
	bad := loadtest.Evidence{Phase: input.Profile.Phases()[1], Summary: json.RawMessage(`{}`)}
	broken := loadtest.Result{State: "running", K6Version: "untrusted", Stages: append(append([]loadtest.Evidence{}, known.Stages...), bad)}
	partial, accepted, err := f.st.AcceptLoadResult(ctx, "fixture-k6", partialRun.ID, partialWire.AssignmentId, "capacity_2", broken)
	if err != nil || !accepted || partial.State != "inconclusive" || !partial.Stopping || len(partial.Evidence) != 1 || partial.Report == nil || partial.Report.K6Version != result.K6Version || partial.Report.TargetResult != "passed" || partial.Report.CapacityResult != "inconclusive" {
		t.Fatal("committed evidence erased after malformed new summary", partial, accepted, err)
	}
	t.Log("partial report retains committed real target metrics and k6 version after invalid capacity summary")

}
