// Package loadtest defines the bounded, deterministic load-test contract shared
// by the control plane and the dedicated native k6 runner.
package loadtest

import (
	"fmt"
	"math"
	"mime"
	"net/url"
	"strings"
	"time"
)

const Feature = "k6_load_v1"

type Limits struct {
	MaxVUs             int `json:"max_vus"`
	MaxRPS             int `json:"max_rps"`
	MaxTargetDurationS int `json:"max_target_duration_s"`
	MaxRunDurationS    int `json:"max_run_duration_s"`
}

func DefaultLimits() Limits { return Limits{100, 500, 120, 300} }
func (l Limits) Validate() error {
	for name, value := range map[string]int{"max_vus": l.MaxVUs, "max_rps": l.MaxRPS, "max_target_duration_s": l.MaxTargetDurationS, "max_run_duration_s": l.MaxRunDurationS} {
		if value < 1 || int64(value) > math.MaxInt32 {
			return fmt.Errorf("load limits %s: expected a finite positive int32", name)
		}
	}
	if l.MaxTargetDurationS < 10 {
		return fmt.Errorf("load limits max_target_duration_s: minimum 10")
	}
	return nil
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *FieldError) Error() string       { return e.Field + ": " + e.Message }
func invalid(field, message string) error { return &FieldError{field, message} }

type Route struct {
	Path                string `json:"path"`
	Weight              int    `json:"weight"`
	ExpectedStatus      int    `json:"expected_status"`
	ExpectedContentType string `json:"expected_content_type,omitempty"`
}
type Auth struct {
	Header     string `json:"header"`
	SecretName string `json:"secret_name"`
}
type Profile struct {
	Unit            string  `json:"unit"`
	Target          int     `json:"target"`
	Ceiling         int     `json:"capacity_ceiling"`
	TargetDurationS int     `json:"target_duration_s"`
	MaxVUs          int     `json:"max_vus"`
	ThinkTimeMS     int     `json:"think_time_ms"`
	P95MS           int     `json:"p95_ms"`
	MaxErrorRatePct float64 `json:"max_error_rate_pct"`
	Routes          []Route `json:"routes"`
	Auth            *Auth   `json:"auth,omitempty"`
}
type Phase struct {
	Name      string `json:"name"`
	Load      int    `json:"load"`
	DurationS int    `json:"duration_s"`
}

func (p Profile) Phases() []Phase {
	out := []Phase{{"target", p.Target, p.TargetDurationS}}
	for i := int64(1); i <= 3 && p.Ceiling > p.Target; i++ {
		load := p.Target + int((int64(p.Ceiling-p.Target)*i+2)/3)
		if load > out[len(out)-1].Load {
			out = append(out, Phase{fmt.Sprintf("capacity_%d", len(out)), load, 20})
		}
	}
	return out
}
func (p Profile) BudgetS() int64 {
	total := int64(len(p.Routes)*5 + 10)
	for _, stage := range p.Phases() {
		total += int64(stage.DurationS) + 5
	}
	return total
}
func (p Profile) Validate(l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	bound := l.MaxVUs
	switch p.Unit {
	case "vus":
	case "rps":
		bound = l.MaxRPS
	default:
		return invalid("unit", "ожидается vus или rps")
	}
	if p.Target < 1 || p.Target > bound {
		return invalid("target", "цель вне лимитов установки")
	}
	if p.Ceiling < p.Target || p.Ceiling > bound {
		return invalid("capacity_ceiling", "потолок должен быть не ниже цели и в лимитах установки")
	}
	if p.TargetDurationS < 10 || p.TargetDurationS > l.MaxTargetDurationS {
		return invalid("target_duration_s", "длительность вне лимитов установки")
	}
	if p.MaxVUs < 1 || p.MaxVUs > l.MaxVUs || (p.Unit == "vus" && p.MaxVUs < p.Ceiling) {
		return invalid("max_vus", "недопустимый предел виртуальных пользователей")
	}
	if (p.Unit == "vus" && (p.ThinkTimeMS < 1000 || p.ThinkTimeMS > 10000)) || (p.Unit == "rps" && p.ThinkTimeMS != 0) {
		return invalid("think_time_ms", "VUs: 1000–10000 мс; RPS: 0")
	}
	if p.P95MS < 1 || p.P95MS > 5000 {
		return invalid("p95_ms", "ожидается 1–5000 мс")
	}
	if math.IsNaN(p.MaxErrorRatePct) || math.IsInf(p.MaxErrorRatePct, 0) || p.MaxErrorRatePct < 0 || p.MaxErrorRatePct > 10 {
		return invalid("max_error_rate_pct", "ожидается конечное число 0–10")
	}
	if len(p.Routes) < 1 || len(p.Routes) > 5 {
		return invalid("routes", "нужно от 1 до 5 GET-путей")
	}
	seen := map[string]bool{}
	for i, r := range p.Routes {
		field := fmt.Sprintf("routes.%d", i)
		u, err := url.ParseRequestURI(r.Path)
		decoded, de := url.PathUnescape(r.Path)
		if err != nil || de != nil || len(r.Path) > 200 || !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") || u == nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.ContainsAny(decoded, "\r\n\x00\\#") || seen[r.Path] {
			return invalid(field+".path", "нужен уникальный относительный путь без origin и управляющих символов")
		}
		seen[r.Path] = true
		if r.Weight < 1 || r.Weight > 10 {
			return invalid(field+".weight", "ожидается 1–10")
		}
		if r.ExpectedStatus < 200 || r.ExpectedStatus > 299 {
			return invalid(field+".expected_status", "ожидается успешный статус 200–299")
		}
		if r.ExpectedContentType != "" {
			typ, params, err := mime.ParseMediaType(r.ExpectedContentType)
			if err != nil || len(r.ExpectedContentType) > 100 || len(params) > 0 || !strings.Contains(typ, "/") {
				return invalid(field+".expected_content_type", "ожидается MIME без параметров")
			}
		}
	}
	if p.Auth != nil {
		if p.Auth.Header != "Authorization" && p.Auth.Header != "Cookie" {
			return invalid("auth.header", "ожидается Authorization или Cookie")
		}
		if len(p.Auth.SecretName) < 1 || len(p.Auth.SecretName) > 128 {
			return invalid("auth.secret_name", "нужно имя существующего тестового секрета")
		}
		for _, r := range p.Auth.SecretName {
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return invalid("auth.secret_name", "недопустимое имя секрета")
			}
		}
	}
	if p.BudgetS() > int64(l.MaxRunDurationS) {
		return invalid("target_duration_s", "проверка с preflight и ступенями не помещается в общий дедлайн установки")
	}
	return nil
}

type Binding struct {
	ReleaseID     string `json:"release_id"`
	PublicationID string `json:"publication_id"`
	Version       string `json:"version"`
	Generation    int64  `json:"generation"`
}
type Target struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Binding
	ContainerID   string `json:"container_id"`
	Port          int    `json:"port"`
	ContainerPort int    `json:"container_port"`
	Host          string `json:"host"`
}
type Job struct {
	RunID        string    `json:"run_id"`
	AssignmentID string    `json:"assignment_id"`
	Target       Target    `json:"target"`
	Profile      Profile   `json:"profile"`
	Limits       Limits    `json:"limits"`
	Deadline     time.Time `json:"deadline"`
	// AuthValue is an ephemeral assignment secret. Never marshal Job to a DB,
	// event, error, transcript or report. Stored snapshots contain Profile only.
	AuthValue string `json:"auth_value,omitempty"`
}
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Terminal(state string) bool { return state != "queued" && state != "running" }
