package loadtest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed script.js
var program []byte

// Runtime has only an inspection seam for isolated HTTP fixtures. Production
// uses Docker inspect, then native k6. No shell or project code is executed.
type Runtime struct {
	K6Bin    string
	Inspect  func(context.Context, Target) error
	Progress func(string, Result) error
}

var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var versionRE = regexp.MustCompile(`^k6 v(2\.(?:3|4)\.(?:0|[1-9]\d*))(?:[ \t\r\n]|$)`)

func checkedK6Version(output []byte) string {
	if len(output) > 4096 {
		return ""
	}
	match := versionRE.FindSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func cleanEnv(dir string) []string {
	return []string{"PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "XDG_CONFIG_HOME=" + dir, "LANG=C", "LC_ALL=C"}
}
func privateFile(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0600)
}
func (r Runtime) inspect(ctx context.Context, t Target) error {
	if r.Inspect != nil {
		return r.Inspect(ctx, t)
	}
	return InspectTarget(ctx, t)
}
func InspectTarget(ctx context.Context, t Target) error {
	if !containerID.MatchString(t.ContainerID) || t.Port < 1 || t.Port > 65535 || t.ContainerPort < 1 || t.ContainerPort > 65535 {
		return fmt.Errorf("недопустимая привязка контейнера")
	}
	// Inspect only the identity fields: never collect Config.Env application secrets.
	const inspectFormat = `[{"Id":{{json .Id}},"State":{"Running":{{json .State.Running}}},"Config":{"Labels":{{json .Config.Labels}}},"HostConfig":{"PortBindings":{{json .HostConfig.PortBindings}}}}]`
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", inspectFormat, t.ContainerID)
	cmd.WaitDelay = time.Second
	var output boundedOutput
	output.limit = SummaryLimit + 1
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	data := output.data
	if err != nil || len(data) > SummaryLimit {
		return fmt.Errorf("контейнер публикации недоступен на этом Docker-хосте")
	}
	return validateInspection(t, data)
}
func validateInspection(t Target, data []byte) error {
	var out []struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		HostConfig struct {
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if json.Unmarshal(data, &out) != nil || len(out) != 1 || out[0].ID != t.ContainerID || !out[0].State.Running {
		return fmt.Errorf("публикация не соответствует заданию")
	}
	labels := map[string]string{"rivet.managed": "true", "rivet.project": t.ProjectID, "rivet.environment": t.EnvironmentID, "rivet.publication": t.PublicationID, "rivet.version": t.Version, "rivet.generation": strconv.FormatInt(t.Generation, 10)}
	for key, value := range labels {
		if value == "" || out[0].Config.Labels[key] != value {
			return fmt.Errorf("метки публикации изменились")
		}
	}
	bindings := out[0].HostConfig.PortBindings[strconv.Itoa(t.ContainerPort)+"/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != strconv.Itoa(t.Port) {
		return fmt.Errorf("порт публикации изменился")
	}
	return nil
}
func preflight(ctx context.Context, j Job) error {
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, route := range j.Profile.Routes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", j.Target.Port, route.Path), nil)
		if err != nil {
			return fmt.Errorf("недопустимый GET-путь")
		}
		if j.Profile.Auth != nil {
			req.Header.Set(j.Profile.Auth.Header, j.AuthValue)
		}
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("GET-сценарий недоступен")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()
		typ, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if res.StatusCode != route.ExpectedStatus || (route.ExpectedContentType != "" && !strings.EqualFold(typ, route.ExpectedContentType)) {
			return fmt.Errorf("GET-сценарий не прошёл проверку статуса/Content-Type; настройте путь и тестовую авторизацию")
		}
	}
	return nil
}

// discardOutput never retains k6 stdout/stderr, which might echo request data
// on tool errors. The summary is read only from the fixed private file.
func runNative(ctx context.Context, bin, dir string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(dir)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	protectProcess(cmd)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), err
	}
	return -1, err
}
func (r Runtime) Execute(ctx context.Context, j Job) (result Result) {
	result = Result{State: "config_needed", Stages: []Evidence{}}
	fail := func(state, code, message string) Result {
		result.State = state
		result.Reason = &Failure{code, message}
		return result
	}
	if err := j.Profile.Validate(j.Limits); err != nil {
		return fail("config_needed", "invalid_profile", "профиль не соответствует конечным лимитам")
	}
	if j.Target.Port < 1 || j.Target.Port > 65535 || j.RunID == "" || j.AssignmentID == "" {
		return fail("config_needed", "invalid_binding", "нет привязки публикации")
	}
	if j.Profile.Auth != nil && (j.AuthValue == "" || len(j.AuthValue) > 4096 || strings.ContainsAny(j.AuthValue, "\x00\r\n")) {
		return fail("config_needed", "auth_required", "нужен корректный явно выбранный тестовый секрет")
	}
	deadline := j.Deadline
	local := time.Now().Add(time.Duration(j.Limits.MaxRunDurationS) * time.Second)
	if deadline.IsZero() || local.Before(deadline) {
		deadline = local
	}
	if !deadline.After(time.Now()) {
		return fail("interrupted", "deadline", "дедлайн попытки истёк")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	dir, err := os.MkdirTemp("", "rivet-k6-")
	if err != nil {
		return fail("config_needed", "workspace", "не удалось создать закрытый каталог k6")
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			result.State = "config_needed"
			result.Reason = &Failure{Code: "workspace_cleanup", Message: "не удалось удалить закрытый каталог теста"}
		}
	}()
	bin := r.K6Bin
	if bin == "" {
		bin = "k6"
	}
	bin, err = exec.LookPath(bin)
	if err != nil {
		return fail("config_needed", "k6_unavailable", "k6 не найден на выделенном runner")
	}
	versionCmd := exec.CommandContext(ctx, bin, "version")
	versionCmd.Env = cleanEnv(dir)
	versionCmd.Dir = dir
	versionCmd.WaitDelay = time.Second
	protectProcess(versionCmd)
	var versionOutput boundedOutput
	versionOutput.limit = 4097
	versionCmd.Stdout = &versionOutput
	versionCmd.Stderr = io.Discard
	err = versionCmd.Run()
	version := checkedK6Version(versionOutput.data)
	if err != nil || version == "" {
		return fail("config_needed", "k6_version", "нужен проверенный k6 2.3.x или 2.4.x")
	}
	result.K6Version = version
	if privateFile(dir, "script.js", program) != nil || privateFile(dir, "config.json", []byte(`{}`)) != nil {
		return fail("config_needed", "workspace", "не удалось подготовить private k6 config")
	}
	if err = r.inspect(ctx, j.Target); err != nil {
		return fail("config_needed", "publication_changed", "не удалось подтвердить контейнер, метки и порт публикации")
	}
	if err = preflight(ctx, j); err != nil {
		return fail("config_needed", "scenario_check_failed", err.Error())
	}
	for _, phase := range j.Profile.Phases() {
		if time.Until(deadline) < time.Duration(phase.DurationS+5)*time.Second {
			return fail("interrupted", "deadline", "ступень не помещается в оставшийся дедлайн")
		}
		if ctx.Err() != nil {
			return fail("interrupted", "interrupted", "нагрузочный тест прерван")
		}
		if err = r.inspect(ctx, j.Target); err != nil {
			return fail("interrupted", "publication_changed", "публикация изменилась до ступени")
		}
		result.State = "running"
		if r.Progress != nil {
			if err = r.Progress(phase.Name, result); err != nil {
				return fail("interrupted", "interrupted", "назначение больше не активно")
			}
		}
		if ctx.Err() != nil || time.Until(deadline) < time.Duration(phase.DurationS+5)*time.Second {
			return fail("interrupted", "deadline", "после сохранения checkpoint ступень не помещается в дедлайн")
		}
		stage := struct {
			Base       string  `json:"base"`
			Profile    Profile `json:"profile"`
			Phase      Phase   `json:"phase"`
			AuthHeader string  `json:"auth_header,omitempty"`
			AuthValue  string  `json:"auth_value,omitempty"`
			DeadlineMS int64   `json:"deadline_ms"`
		}{Base: fmt.Sprintf("http://127.0.0.1:%d", j.Target.Port), Profile: j.Profile, Phase: phase, AuthValue: j.AuthValue, DeadlineMS: deadline.UnixMilli()}
		if j.Profile.Auth != nil {
			stage.AuthHeader = j.Profile.Auth.Header
		}
		data, _ := json.Marshal(stage)
		if privateFile(dir, "stage.json", data) != nil {
			return fail("config_needed", "workspace", "не удалось подготовить параметры k6")
		}
		_ = os.Remove(filepath.Join(dir, "summary.json"))
		code, runErr := runNative(ctx, bin, dir, "run", "--quiet", "--no-color", "--no-usage-report", "--config", filepath.Join(dir, "config.json"), filepath.Join(dir, "script.js"))
		if ctx.Err() != nil {
			return fail("interrupted", "interrupted", "нагрузочный тест прерван по отмене или дедлайну")
		}
		file, err := os.Open(filepath.Join(dir, "summary.json"))
		if err != nil {
			return fail("inconclusive", "summary_missing", "k6 не вернул измерения")
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, SummaryLimit+1))
		_ = file.Close()
		if readErr != nil || len(raw) > SummaryLimit {
			return fail("inconclusive", "invalid_summary", "неполные измерения k6")
		}
		evidence := Evidence{Phase: phase, Summary: raw, ExitCode: code}
		parsed, err := ParseStage(j.Profile, evidence)
		if err != nil {
			return fail("inconclusive", "invalid_summary", "невалидные измерения k6")
		}
		result.Stages = append(result.Stages, evidence)
		if parsed.Status != "passed" {
			state := parsed.Status
			if state == "failed" && len(result.Stages) > 1 {
				state = "passed"
			}
			return fail(state, parsed.Reason, "ступень не прошла; дальнейшее повышение нагрузки остановлено")
		}
		if runErr != nil {
			return fail("inconclusive", "k6_execution_failed", "k6 не завершил ступень")
		}
	}
	result.State = "passed"
	return result
}

// CheckBinary validates the dedicated runner dependency without making requests.
func CheckBinary(ctx context.Context, bin string) error {
	if bin == "" {
		bin = "k6"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("k6 не найден: укажите -k6-bin")
	}
	dir, err := os.MkdirTemp("", "rivet-k6-check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = cleanEnv(dir)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	protectProcess(cmd)
	var output boundedOutput
	output.limit = 4097
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if cmd.Run() != nil || checkedK6Version(output.data) == "" {
		return fmt.Errorf("нужен проверенный native k6 2.3.x или 2.4.x")
	}
	return nil
}

// boundedOutput bounds memory even if an unexpected binary produces huge output.
type boundedOutput struct {
	data  []byte
	limit int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(w.data) < w.limit {
		w.data = append(w.data, p[:min(len(p), w.limit-len(w.data))]...)
	}
	return n, nil
}
