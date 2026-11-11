package localdocker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/redact"
)

const LogLimit = 2 << 20

type Job struct {
	ProjectID             string                              `json:"project_id"`
	EnvironmentID         string                              `json:"environment_id"`
	PublicationID         string                              `json:"publication_id"`
	Version               string                              `json:"version"`
	Generation            int64                               `json:"generation"`
	Config                domain.LocalDockerEnvironmentConfig `json:"config"`
	ContainerID           string                              `json:"container_id,omitempty"`
	Image                 string                              `json:"image,omitempty"`
	Port                  int                                 `json:"port,omitempty"`
	DesiredState          string                              `json:"desired_state,omitempty"`
	Attempt               int                                 `json:"attempt,omitempty"`
	GarbageCollectionOnly bool                                `json:"garbage_collection_only,omitempty"`
	RemoveData            bool                                `json:"remove_data,omitempty"`
	KeepPublications      []string                            `json:"keep_publications,omitempty"`
}
type Result struct {
	ResourcesRemoved bool     `json:"resources_removed,omitempty"`
	Status           string   `json:"status"`
	ContainerID      string   `json:"container_id,omitempty"`
	Image            string   `json:"image,omitempty"`
	Port             int      `json:"port,omitempty"`
	HealthURL        string   `json:"health_url,omitempty"`
	Reason           *Failure `json:"reason,omitempty"`
	Attempt          int      `json:"attempt,omitempty"`
	Logs             string   `json:"logs,omitempty"`
}

// Command получает только argv, без shell и интерполяции команд.
type Command func(context.Context, []string, io.Writer) (string, error)
type Runtime struct {
	Command  Command
	Client   *http.Client
	Progress func(string) error
	Log      io.Writer
}
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
	sink io.Writer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.sink != nil {
		_, _ = b.sink.Write(redact.Bytes(p))
	}
	if n >= LogLimit {
		b.data = append(b.data[:0], p[n-LogLimit:]...)
	} else {
		if n+len(b.data) > LogLimit {
			b.data = append([]byte(nil), b.data[n+len(b.data)-LogLimit:]...)
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func dockerCommand(ctx context.Context, args []string, sink io.Writer) (string, error) {
	b := &tailBuffer{sink: sink}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = b
	cmd.Stderr = b
	cmd.WaitDelay = 3 * time.Second
	e := cmd.Run()
	return string(redact.Bytes(b.data)), e
}
func (r *Runtime) call(ctx context.Context, args ...string) (string, error) {
	f := r.Command
	if f == nil {
		f = dockerCommand
	}
	return f(ctx, args, r.Log)
}
func (r *Runtime) phase(s string) error {
	if r.Progress != nil {
		return r.Progress(s)
	}
	return nil
}

var objectID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)
var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (j Job) validate() error {
	for _, v := range []string{j.ProjectID, j.EnvironmentID, j.PublicationID} {
		if !objectID.MatchString(v) {
			return fail("config_invalid", "некорректный идентификатор публикации")
		}
	}
	if j.Generation < 1 {
		return fail("config_invalid", "некорректное поколение публикации")
	}
	return (domain.EnvConfig{LocalDocker: &j.Config}).Validate(domain.ExecLocalDocker)
}
func (j Job) name() string { return fmt.Sprintf("rivet-%s-g%d", j.EnvironmentID, j.Generation) }
func (j Job) labels() map[string]string {
	return map[string]string{"rivet.managed": "true", "rivet.project": j.ProjectID, "rivet.environment": j.EnvironmentID, "rivet.publication": j.PublicationID, "rivet.version": j.Version, "rivet.generation": strconv.FormatInt(j.Generation, 10)}
}
func (j Job) labelArgs() []string {
	m := j.labels()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var a []string
	for _, k := range keys {
		a = append(a, "--label", k+"="+m[k])
	}
	return a
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

type Inspection struct {
	ID     string
	Image  string
	Config struct {
		Labels map[string]string
		User   string
	}
	HostConfig struct {
		NetworkMode   string
		Privileged    bool
		PortBindings  map[string][]PortBinding
		RestartPolicy struct{ Name string }
	}
	NetworkSettings struct {
		Ports map[string][]PortBinding
	}
	State struct {
		Running  bool
		Status   string
		ExitCode int
	}
	Created time.Time
}

func owned(c Inspection, j Job) bool {
	for k, v := range j.labels() {
		if c.Config.Labels[k] != v {
			return false
		}
	}
	return true
}
func ParseInspection(raw string, j Job) (Inspection, error) {
	var a []Inspection
	if e := json.Unmarshal([]byte(raw), &a); e != nil || len(a) != 1 {
		return Inspection{}, fail("docker_unavailable", "Docker вернул некорректное описание контейнера")
	}
	c := a[0]
	if !containerID.MatchString(c.ID) {
		return c, fail("docker_unavailable", "Docker вернул некорректный container_id")
	}
	if !owned(c, j) {
		return c, fail("config_invalid", "контейнер не принадлежит этой публикации")
	}
	if c.HostConfig.NetworkMode != "bridge" || c.HostConfig.Privileged {
		return c, fail("loopback_not_bound", "небезопасный сетевой режим контейнера")
	}
	n := 0
	ports := c.NetworkSettings.Ports
	if !c.State.Running {
		ports = c.HostConfig.PortBindings
	}
	for p, bs := range ports {
		for _, b := range bs {
			n++
			if (b.HostIP != "127.0.0.1" && b.HostIP != "::1") || b.HostPort != strconv.Itoa(j.Port) || p != strconv.Itoa(j.Config.ContainerPort)+"/tcp" {
				return c, fail("loopback_not_bound", "привязка контейнера не совпадает с loopback-адресом публикации")
			}
		}
	}
	if n == 0 {
		return c, fail("loopback_not_bound", "Docker не подтвердил loopback-привязку порта")
	}
	return c, nil
}
func (r *Runtime) inspect(ctx context.Context, j Job) (Inspection, error) {
	name := j.name()
	if j.ContainerID != "" {
		if !containerID.MatchString(j.ContainerID) {
			return Inspection{}, fail("config_invalid", "некорректный container_id")
		}
		name = j.ContainerID
	}
	raw, e := r.call(ctx, "container", "inspect", name)
	if e != nil {
		return Inspection{}, fail("container_exited", "контейнер публикации не найден")
	}
	return ParseInspection(raw, j)
}
func freePort() (int, error) {
	var b [4]byte
	_, _ = rand.Read(b[:])
	start := int(binary.LittleEndian.Uint32(b[:]) % 10000)
	for i := 0; i < 10000; i++ {
		p := 20000 + (start+i)%10000
		l, e := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
		if e == nil {
			_ = l.Close()
			return p, nil
		}
	}
	return 0, fail("port_conflict", "нет свободного порта в диапазоне 20000–29999")
}
func (r *Runtime) probe(ctx context.Context, url string) error {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return e
	}
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
func (r *Runtime) health(ctx context.Context, j Job) error {
	c, cancel := context.WithTimeout(ctx, time.Duration(j.Config.StartupTimeoutS)*time.Second)
	defer cancel()
	for {
		if e := r.probe(c, fmt.Sprintf("http://127.0.0.1:%d%s", j.Port, j.Config.HealthPath)); e == nil {
			return nil
		}
		select {
		case <-c.Done():
			return fail("healthcheck_failed", "приложение не ответило за время запуска")
		case <-time.After(time.Second):
		}
	}
}
func result(j Job, status string, e error) Result {
	out := Result{Status: status, ContainerID: j.ContainerID, Image: j.Image, Port: j.Port, Attempt: j.Attempt}
	if j.Port > 0 {
		out.HealthURL = fmt.Sprintf("http://127.0.0.1:%d%s", j.Port, j.Config.HealthPath)
	}
	if e != nil {
		var f *Failure
		if errors.As(e, &f) {
			out.Reason = f
		} else {
			out.Reason = &Failure{"docker_unavailable", "операция Docker не выполнена"}
		}
	}
	return out
}
func (r *Runtime) remove(ctx context.Context, j Job) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = r.call(c, "container", "rm", "--force", j.name())
}
func (r *Runtime) volume(ctx context.Context, j Job) (string, error) {
	name := "rivet-data-" + j.EnvironmentID
	raw, e := r.call(ctx, "volume", "inspect", name)
	if e == nil {
		var a []struct{ Labels map[string]string }
		if json.Unmarshal([]byte(raw), &a) != nil || len(a) != 1 || a[0].Labels["rivet.managed"] != "true" || a[0].Labels["rivet.environment"] != j.EnvironmentID || a[0].Labels["rivet.project"] != j.ProjectID {
			return "", fail("config_invalid", "том данных не принадлежит окружению")
		}
		return name, nil
	}
	_, e = r.call(ctx, "volume", "create", "--label", "rivet.managed=true", "--label", "rivet.project="+j.ProjectID, "--label", "rivet.environment="+j.EnvironmentID, name)
	return name, e
}

// Старое поколение выключается control plane после durable принятия running.
func (r *Runtime) Publish(ctx context.Context, root string, j Job) Result {
	if e := j.validate(); e != nil {
		return result(j, "failed", e)
	}
	j.Config.Defaults()
	if _, e := r.call(ctx, "info", "--format", "{{.ServerVersion}}"); e != nil {
		return result(j, "unknown", fail("docker_unavailable", "Docker недоступен"))
	}
	if e := r.phase("building"); e != nil {
		return result(j, "failed", e)
	}
	b, e := Prepare(root, j.Config)
	if e != nil {
		return result(j, "failed", e)
	}
	defer b.Close()
	tag := fmt.Sprintf("rivet/%s:g%d", strings.ToLower(j.EnvironmentID), j.Generation)
	args := append([]string{"build", "--network", "default", "--tag", tag, "--file", b.Dockerfile}, j.labelArgs()...)
	args = append(args, b.Dir)
	if _, e = r.call(ctx, args...); e != nil {
		return result(j, "failed", fail("build_failed", "сборка образа не выполнена, см. лог сборки"))
	}
	raw, e := r.call(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	j.Image = strings.TrimSpace(raw)
	if e != nil || !imageID.MatchString(j.Image) {
		return result(j, "failed", fail("image_missing", "Docker не подтвердил собранный образ"))
	}
	volume, e := r.volume(ctx, j)
	if e != nil {
		return result(j, "failed", e)
	}
	if e = r.phase("starting"); e != nil {
		return result(j, "failed", e)
	}
	for attempt := 0; attempt < 3; attempt++ {
		j.Port, e = freePort()
		if e != nil {
			return result(j, "failed", e)
		}
		args = []string{"run", "--detach", "--name", j.name(), "--network", "bridge", "--restart", "unless-stopped", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256", "--memory", "512m", "--cpus", "1", "--tmpfs", "/tmp:rw,nosuid,noexec,size=64m", "--log-opt", "max-size=2m", "--log-opt", "max-file=1", "--mount", "type=volume,src=" + volume + ",dst=/data", "--publish", fmt.Sprintf("127.0.0.1:%d:%d", j.Port, j.Config.ContainerPort)}
		args = append(args, j.labelArgs()...)
		keys := make([]string, 0, len(j.Config.Env))
		for k := range j.Config.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--env", k+"="+j.Config.Env[k])
		}
		args = append(args, j.Image)
		raw, e = r.call(ctx, args...)
		if e == nil {
			j.ContainerID = strings.TrimSpace(raw)
			break
		}
		c, _ := r.inspect(ctx, j)
		if owned(c, j) {
			r.remove(ctx, j)
		}
		if !strings.Contains(raw, "port is already allocated") && !strings.Contains(raw, "address already in use") {
			return result(j, "failed", fail("container_exited", "контейнер не запущен, см. лог"))
		}
	}
	if e != nil {
		return result(j, "failed", fail("port_conflict", "не удалось занять свободный loopback-порт"))
	}
	c, e := r.inspect(ctx, j)
	if e != nil {
		if owned(c, j) {
			r.remove(ctx, j)
		}
		return result(j, "failed", e)
	}
	if !c.State.Running {
		r.remove(ctx, j)
		return result(j, "failed", fail("container_exited", "контейнер завершился до проверки"))
	}
	if e = r.phase("verifying"); e != nil {
		r.remove(ctx, j)
		return result(j, "failed", e)
	}
	if e = r.health(ctx, j); e != nil {
		r.remove(ctx, j)
		return result(j, "failed", e)
	}
	j.Attempt = 0
	return result(j, "running", nil)
}
func (r *Runtime) Operate(ctx context.Context, action string, j Job) Result {
	if e := j.validate(); e != nil {
		return result(j, "failed", e)
	}
	j.Config.Defaults()
	if _, e := r.call(ctx, "info", "--format", "{{.ServerVersion}}"); e != nil {
		return result(j, "unknown", fail("docker_unavailable", "Docker недоступен"))
	}
	if action == "remove" {
		return r.removePublication(ctx, j)
	}
	c, e := r.inspect(ctx, j)
	if e != nil {
		if j.DesiredState == "stopped" && c.ID == "" {
			return result(j, "stopped", nil)
		}
		var f *Failure
		if owned(c, j) && errors.As(e, &f) && f.Code == "loopback_not_bound" {
			r.remove(ctx, j)
		}
		return result(j, "failed", e)
	}
	j.ContainerID = c.ID
	j.Image = c.Image
	switch action {
	case "stop":
		if _, e = r.call(ctx, "container", "update", "--restart", "no", c.ID); e == nil {
			_, e = r.call(ctx, "container", "stop", "--time", strconv.Itoa(j.Config.StopTimeoutS), c.ID)
		}
		if e != nil {
			return result(j, "unknown", e)
		}
		return result(j, "stopped", nil)
	case "restart":
		if j.DesiredState == "stopped" {
			return result(j, "stopped", nil)
		}
		if _, e = r.call(ctx, "container", "restart", "--time", strconv.Itoa(j.Config.StopTimeoutS), c.ID); e != nil {
			return result(j, "unknown", e)
		}
		if e = r.health(ctx, j); e != nil {
			return result(j, "unhealthy", e)
		}
		return result(j, "running", nil)
	case "logs":
		raw, e := r.call(ctx, "container", "logs", "--tail", "5000", c.ID)
		res := result(j, "running", e)
		if !c.State.Running {
			res.Status = "stopped"
		}
		res.Logs = raw
		return res
	case "remove":
		r.remove(ctx, j)
		return result(j, "stopped", nil)
	case "reconcile":
		if j.DesiredState == "stopped" {
			if c.State.Running || c.HostConfig.RestartPolicy.Name != "no" {
				return r.Operate(ctx, "stop", j)
			}
			return result(j, "stopped", nil)
		}
		if !c.State.Running {
			if j.Attempt >= 3 {
				return result(j, "failed", fail("container_exited", "контейнер не восстановился после трёх попыток"))
			}
			pause := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}[j.Attempt]
			select {
			case <-ctx.Done():
				return result(j, "unknown", ctx.Err())
			case <-time.After(pause):
			}
			j.Attempt++
			if _, e = r.call(ctx, "container", "start", c.ID); e != nil {
				return result(j, "unknown", fail("container_exited", "не удалось запустить контейнер"))
			}
		}
		if e = r.probe(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", j.Port, j.Config.HealthPath)); e != nil {
			return result(j, "unhealthy", fail("healthcheck_failed", "приложение не отвечает на проверку"))
		}
		j.Attempt = 0
		return result(j, "running", nil)
	default:
		return result(j, "failed", fail("config_invalid", "неизвестная операция публикации"))
	}
}

// PrepareHost не создаёт контейнер до появления артефакта приложения.
func (r *Runtime) PrepareHost(ctx context.Context, j Job) Result {
	if e := j.validate(); e != nil {
		return result(j, "failed", e)
	}
	if _, e := r.call(ctx, "info", "--format", "{{.ServerVersion}}"); e != nil {
		return result(j, "failed", fail("docker_unavailable", "Docker недоступен для подготовки стенда"))
	}
	for _, image := range []string{GoImage, AlpineImage, NodeImage, NginxImage} {
		if _, e := r.call(ctx, "image", "inspect", "--format", "{{.Id}}", image); e != nil {
			if _, e = r.call(ctx, "pull", image); e != nil {
				return result(j, "failed", fail("docker_unavailable", "не удалось подготовить базовый образ сборки"))
			}
		}
	}
	return result(j, "waiting_artifact", nil)
}
