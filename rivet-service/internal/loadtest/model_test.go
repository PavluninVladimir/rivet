package loadtest

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func testProfile() Profile {
	return Profile{Unit: "rps", Target: 2, Ceiling: 2, TargetDurationS: 10, MaxVUs: 2, P95MS: 500, MaxErrorRatePct: 0, Routes: []Route{{Path: "/api/companies", Weight: 1, ExpectedStatus: 200, ExpectedContentType: "application/json"}}}
}
func TestProfileBoundsAndOrigin(t *testing.T) {
	good := testProfile()
	if err := good.Validate(DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"https://evil.test/", "//evil.test/", "/api#fragment", "/%0d%0aHost:evil", "/api\\evil"} {
		p := good
		p.Routes = append([]Route{}, good.Routes...)
		p.Routes[0].Path = path
		if p.Validate(DefaultLimits()) == nil {
			t.Fatal(path)
		}
	}
	for _, mutate := range []func(*Profile){func(p *Profile) { p.MaxErrorRatePct = math.NaN() }, func(p *Profile) { p.Ceiling = 1 }, func(p *Profile) { p.Target = 501; p.Ceiling = 501 }, func(p *Profile) { p.P95MS = 0 }, func(p *Profile) { p.Auth = &Auth{Header: "Host", SecretName: "TEST_TOKEN"} }} {
		p := good
		mutate(&p)
		if p.Validate(DefaultLimits()) == nil {
			t.Fatalf("accepted invalid %+v", p)
		}
	}
	good.P95MS = 1
	if err := good.Validate(DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	good.Target, good.Ceiling = 700, 700
	if err := good.Validate(Limits{1000, 1000, 120, 300}); err != nil {
		t.Fatal(err)
	}
	if err := good.Validate(DefaultLimits()); err == nil {
		t.Fatal("default limit ignored")
	}
	p := testProfile()
	p.Ceiling = 10
	if len(p.Phases()) != 4 || p.Phases()[3].Load != 10 {
		t.Fatal(p.Phases())
	}
	l := DefaultLimits()
	l.MaxRunDurationS = 20
	if p.Validate(l) == nil {
		t.Fatal("time budget ignored")
	}
}
func TestZeroErrorInclusiveAndIncompleteMetrics(t *testing.T) {
	p := testProfile()
	raw := `{"state":{"testRunDurationMs":10000},"metrics":{"vus":{"values":{"max":2}},"vus_max":{"values":{"max":2}},"dropped_iterations":{"values":{"count":0},"thresholds":{"count<=0":{"ok":true}}},"http_reqs":{"values":{"count":20,"rate":2}},"http_req_duration":{"values":{"p(95)":1,"p(99)":2},"thresholds":{"p(95)<=500":{"ok":true}}},"request_errors":{"values":{"rate":0},"thresholds":{"rate<=0":{"ok":true}}},"route_0_requests":{"values":{"count":20}},"route_0_latency":{"values":{"p(95)":1,"p(99)":2}},"route_0_errors":{"values":{"rate":0}}}}`
	e := Evidence{Phase: p.Phases()[0], Summary: json.RawMessage(raw)}
	st, err := ParseStage(p, e)
	if err != nil || st.Status != "passed" {
		t.Fatalf("%+v %v", st, err)
	}
	e.Summary = json.RawMessage(strings.Replace(raw, `"rate":2`, `"rate":1`, 1))
	st, _ = ParseStage(p, e)
	if st.Status != "inconclusive" {
		t.Fatal(st)
	}
	e.Summary = json.RawMessage(strings.ReplaceAll(raw, `"rate":0`, `"rate":0.1`))
	st, _ = ParseStage(p, e)
	if st.Status != "failed" {
		t.Fatal(st)
	}
	for _, bad := range []string{
		strings.Replace(raw, `"p(95)":1`, `"p(95)":null`, 1),
		strings.Replace(raw, `"rate":0`, `"rate":null`, 1),
		strings.Replace(raw, `"count":0`, `"count":null`, 1),
		strings.Replace(raw, `"count":0`, `"count":-1`, 1),
		strings.Replace(raw, `"count":0`, `"count":0.5`, 1),
		strings.Replace(raw, `"ok":true`, `"ok":null`, 1),
		strings.Replace(raw, `"count<=0"`, `"unknown"`, 1),
	} {
		st, err := ParseStage(p, Evidence{Phase: e.Phase, Summary: json.RawMessage(bad)})
		if err == nil && st.Status == "passed" {
			t.Fatal("invalid metric passed", bad)
		}
	}
	for _, state := range []string{"failed", "config_needed", "interrupted", "cancelled"} {
		report, err := BuildReport(p, Binding{}, Result{State: state, Stages: []Evidence{{Phase: e.Phase, Summary: json.RawMessage(raw)}}})
		if err != nil || ReportState(report) != state {
			t.Fatalf("terminal %s masked: %+v %v", state, report, err)
		}
	}

	e.Summary = json.RawMessage(`{"metrics":{}}`)
	st, _ = ParseStage(p, e)
	if st.Status != "inconclusive" || st.P95 != nil {
		t.Fatal(st)
	}
}

func TestGeneratorShortageTakesPrecedenceOverSLOFailure(t *testing.T) {
	p := testProfile()
	p.Ceiling = 5
	good := `{"state":{"testRunDurationMs":10000},"metrics":{"vus":{"values":{"max":2}},"vus_max":{"values":{"max":2}},"dropped_iterations":{"values":{"count":0},"thresholds":{"count<=0":{"ok":true}}},"http_reqs":{"values":{"count":20,"rate":2}},"http_req_duration":{"values":{"p(95)":1,"p(99)":2},"thresholds":{"p(95)<=500":{"ok":true}}},"request_errors":{"values":{"rate":0},"thresholds":{"rate<=0":{"ok":true}}},"route_0_requests":{"values":{"count":20}},"route_0_latency":{"values":{"p(95)":1,"p(99)":2}},"route_0_errors":{"values":{"rate":0}}}}`
	bad := strings.ReplaceAll(good, `"p(95)":1`, `"p(95)":9999`)
	bad = strings.Replace(bad, `"count":0`, `"count":7`, 1)
	bad = strings.ReplaceAll(bad, `"ok":true`, `"ok":false`)
	first := Evidence{Phase: p.Phases()[0], Summary: json.RawMessage(good)}
	shortage := Evidence{Phase: p.Phases()[1], Summary: json.RawMessage(bad), ExitCode: 99}
	stage, err := ParseStage(p, shortage)
	if err != nil || stage.Status != "inconclusive" || stage.Dropped != 7 || stage.P95 == nil || *stage.P95 != 9999 {
		t.Fatal(stage, err)
	}
	report, err := BuildReport(p, Binding{}, Result{State: "inconclusive", Stages: []Evidence{first, shortage}})
	if err != nil || report.TargetResult != "passed" || report.CapacityResult != "inconclusive" || report.FirstFailing != nil {
		t.Fatal(report, err)
	}
}
