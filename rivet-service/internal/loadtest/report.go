package loadtest

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

const SummaryLimit = 128 << 10

type Evidence struct {
	Phase    Phase           `json:"phase"`
	Summary  json.RawMessage `json:"summary"`
	ExitCode int             `json:"exit_code"`
}
type Result struct {
	State     string     `json:"state"`
	Reason    *Failure   `json:"reason,omitempty"`
	K6Version string     `json:"k6_version"`
	Stages    []Evidence `json:"stages"`
}
type RouteMetrics struct {
	Path      string   `json:"path"`
	Requests  int64    `json:"requests"`
	P95       *float64 `json:"p95_ms"`
	P99       *float64 `json:"p99_ms"`
	ErrorRate *float64 `json:"error_rate"`
}
type Stage struct {
	Stage      string          `json:"stage"`
	Load       int             `json:"load"`
	Unit       string          `json:"unit"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason"`
	DurationS  float64         `json:"duration_s"`
	Requests   int64           `json:"requests"`
	RPS        *float64        `json:"achieved_rps"`
	VUs        *float64        `json:"active_vus"`
	MaxVUs     *float64        `json:"max_vus"`
	P95        *float64        `json:"p95_ms"`
	P99        *float64        `json:"p99_ms"`
	ErrorRate  *float64        `json:"error_rate"`
	Dropped    int64           `json:"dropped_iterations"`
	Thresholds map[string]bool `json:"thresholds"`
	Routes     []RouteMetrics  `json:"per_route"`
}
type Report struct {
	ExecutionState string   `json:"execution_state"`
	Scope          string   `json:"scope"`
	Binding        Binding  `json:"binding"`
	Profile        Profile  `json:"profile"`
	K6Version      string   `json:"k6_version"`
	TargetResult   string   `json:"target_result"`
	CapacityResult string   `json:"capacity_result"`
	HighestPassing *int     `json:"highest_passing"`
	FirstFailing   *int     `json:"first_failing"`
	StopReason     string   `json:"stop_reason"`
	Warnings       []string `json:"warnings"`
	Stages         []Stage  `json:"stages"`
}
type metric struct {
	Values     map[string]*float64 `json:"values"`
	Thresholds map[string]struct {
		OK *bool `json:"ok"`
	} `json:"thresholds"`
}
type summary struct {
	State struct {
		DurationMS *float64 `json:"testRunDurationMs"`
	} `json:"state"`
	Metrics map[string]metric `json:"metrics"`
}

func number(m map[string]metric, name, key string) *float64 {
	v := m[name].Values[key]
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return nil
	}
	return v
}
func count(m map[string]metric, name string) *int64 {
	v := number(m, name, "count")
	if v == nil || *v >= float64(math.MaxInt64) || math.Trunc(*v) != *v {
		return nil
	}
	n := int64(*v)
	return &n
}
func ParseStage(p Profile, e Evidence) (Stage, error) {
	st := Stage{Stage: e.Phase.Name, Load: e.Phase.Load, Unit: p.Unit, Status: "inconclusive", Reason: "incomplete_metrics", Thresholds: map[string]bool{}, Routes: []RouteMetrics{}}
	if len(e.Summary) > SummaryLimit {
		return st, fmt.Errorf("k6 summary too large")
	}
	var data summary
	if err := json.Unmarshal(e.Summary, &data); err != nil {
		return st, fmt.Errorf("invalid k6 summary")
	}
	if data.State.DurationMS == nil || math.IsNaN(*data.State.DurationMS) || math.IsInf(*data.State.DurationMS, 0) || *data.State.DurationMS < 0 {
		return st, fmt.Errorf("invalid k6 duration")
	}
	m := data.Metrics
	st.DurationS = *data.State.DurationMS / 1000
	requests := count(m, "http_reqs")
	if requests != nil {
		st.Requests = *requests
	}
	st.RPS = number(m, "http_reqs", "rate")
	st.P95 = number(m, "http_req_duration", "p(95)")
	st.P99 = number(m, "http_req_duration", "p(99)")
	st.ErrorRate = number(m, "request_errors", "rate")
	dropped := count(m, "dropped_iterations")
	if dropped != nil {
		st.Dropped = *dropped
	} else if _, exists := m["dropped_iterations"]; exists {
		return st, fmt.Errorf("invalid dropped iterations")
	}
	st.VUs = number(m, "vus", "max")
	st.MaxVUs = number(m, "vus_max", "max")
	complete := (p.Unit != "rps" || dropped != nil) && st.VUs != nil && st.MaxVUs != nil && st.Requests > 0 && st.RPS != nil && st.P95 != nil && st.P99 != nil && st.ErrorRate != nil && *st.ErrorRate <= 1
	routeTotal := int64(0)
	weightedErrors := float64(0)
	for i, r := range p.Routes {
		prefix := fmt.Sprintf("route_%d_", i)
		rm := RouteMetrics{Path: r.Path, Requests: 0, P95: number(m, prefix+"latency", "p(95)"), P99: number(m, prefix+"latency", "p(99)"), ErrorRate: number(m, prefix+"errors", "rate")}
		if c := count(m, prefix+"requests"); c != nil {
			rm.Requests = *c
		}
		routeTotal += rm.Requests
		if rm.ErrorRate != nil {
			weightedErrors += *rm.ErrorRate * float64(rm.Requests)
		}
		complete = complete && rm.Requests > 0 && rm.P95 != nil && rm.P99 != nil && rm.ErrorRate != nil && *rm.ErrorRate <= 1
		st.Routes = append(st.Routes, rm)
	}
	for name, expr := range map[string]string{"request_errors": "rate<=" + strconv.FormatFloat(p.MaxErrorRatePct/100, 'f', -1, 64), "http_req_duration": "p(95)<=" + strconv.Itoa(p.P95MS)} {
		t, ok := m[name].Thresholds[expr]
		if !ok || t.OK == nil {
			complete = false
		} else {
			st.Thresholds[name+":"+expr] = *t.OK
		}
	}
	if p.Unit == "rps" {
		t, ok := m["dropped_iterations"].Thresholds["count<=0"]
		if !ok || t.OK == nil {
			complete = false
		}
	}
	complete = complete && routeTotal == st.Requests
	if st.ErrorRate != nil && st.Requests > 0 && math.Abs(weightedErrors/float64(st.Requests)-*st.ErrorRate) > 0.000001 {
		complete = false
	}
	if !complete {
		return st, nil
	}
	// Inclusive boundaries are intentional: an error budget of zero must pass
	// when every request succeeded. A threshold exit is never ignored.
	if *st.P95 > float64(p.P95MS) || *st.ErrorRate > p.MaxErrorRatePct/100 {
		st.Status, st.Reason = "failed", "slo_failed"
		return st, nil
	}
	for _, ok := range st.Thresholds {
		if !ok {
			st.Status, st.Reason = "failed", "slo_failed"
			return st, nil
		}
	}
	if st.Dropped > 0 || (p.Unit == "rps" && *st.RPS < float64(e.Phase.Load)*0.95) {
		st.Reason = "generator_or_sut_limited"
		return st, nil
	}
	if p.Unit == "vus" && (st.VUs == nil || *st.VUs < float64(e.Phase.Load)) {
		st.Reason = "vus_not_reached"
		return st, nil
	}
	if st.Requests < 10 || st.DurationS < float64(e.Phase.DurationS)*0.95 {
		st.Reason = "insufficient_samples"
		return st, nil
	}
	if e.ExitCode != 0 {
		st.Reason = "k6_execution_failed"
		return st, nil
	}
	st.Status, st.Reason = "passed", ""
	return st, nil
}
func BuildReport(p Profile, b Binding, r Result) (Report, error) {
	report := Report{ExecutionState: r.State, Scope: "Только заданная read-only GET-смесь и указанная версия; не мощность всей системы.", Binding: b, Profile: p, K6Version: r.K6Version, TargetResult: "not_run", CapacityResult: "not_run", Stages: []Stage{}, Warnings: []string{"Генератор работает на хосте публикации; жёсткая OS CPU/memory-изоляция не заявляется."}}
	phases := p.Phases()
	if len(r.Stages) > len(phases) {
		return report, fmt.Errorf("unexpected stage count")
	}
	for i, e := range r.Stages {
		if e.Phase != phases[i] {
			return report, fmt.Errorf("unexpected load stage")
		}
		if i > 0 && report.Stages[i-1].Status != "passed" {
			return report, fmt.Errorf("load increased after failed stage")
		}
		st, err := ParseStage(p, e)
		if err != nil {
			return report, err
		}
		report.Stages = append(report.Stages, st)
		if i == 0 {
			report.TargetResult = st.Status
		}
		switch st.Status {
		case "passed":
			v := st.Load
			report.HighestPassing = &v
		case "failed":
			v := st.Load
			report.FirstFailing = &v
		}
	}
	if r.Reason != nil {
		report.StopReason = r.Reason.Code
	}
	if report.TargetResult == "passed" {
		switch {
		case len(report.Stages) > 0 && report.Stages[len(report.Stages)-1].Status == "failed":
			report.CapacityResult = "slo_boundary"
		case len(report.Stages) > 0 && report.Stages[len(report.Stages)-1].Status == "inconclusive":
			report.CapacityResult = "inconclusive"
		case len(report.Stages) == len(phases) && report.Stages[len(report.Stages)-1].Load == p.Ceiling:
			report.CapacityResult = "ceiling_reached"
		case r.State != "running":
			report.CapacityResult = "inconclusive"
		}
	}
	if r.State == "cancelled" || r.State == "interrupted" {
		report.Warnings = append(report.Warnings, "Тест прерван; полная проверка профиля не завершена.")
		if report.TargetResult == "passed" {
			report.CapacityResult = "inconclusive"
		}
	}
	return report, nil
}
func ReportState(r Report) string {
	switch r.ExecutionState {
	case "config_needed", "interrupted", "cancelled", "failed", "inconclusive":
		return r.ExecutionState
	}
	if r.TargetResult == "failed" {
		return "failed"
	}
	if r.TargetResult != "passed" || r.CapacityResult == "inconclusive" || r.CapacityResult == "not_run" {
		return "inconclusive"
	}
	return "passed"
}
