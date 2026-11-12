// Окружения публикации и деплой (api-contract implement-deployment).
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// environmentView — DTO Environment контракта.
// capRe — имя capability runner'а.
var capRe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,40}$`)

type environmentView struct {
	ID             string           `json:"id"`
	ProjectID      string           `json:"project_id"`
	Name           string           `json:"name"`
	ExecType       string           `json:"exec_type"`
	Trigger        string           `json:"trigger"`
	Config         domain.EnvConfig `json:"config"`
	Paused         bool             `json:"paused"`
	RunnerCaps     []string         `json:"runner_caps"`
	LastDeployment *deploymentView  `json:"last_deployment"`
	// MonitorState — состояние наблюдения: ok, fail, unknown или disabled.
	MonitorState string `json:"monitor_state"`
	// MonitorReason и MonitorLastAt — причина состояния и время последней
	// проверки: панелям хватает их без истории проверок.
	MonitorReason string     `json:"monitor_reason"`
	MonitorLastAt *time.Time `json:"monitor_last_at"`
	// Host: хост от провайдера: живой или последний; null без привязки
	// (add-environment-provisioning).
	Host        *envHostView `json:"host"`
	CreatedAt   time.Time    `json:"created_at"`
	Preparation string       `json:"preparation,omitempty"`
}

// deploymentView — DTO Deployment контракта: created_at — очередь,
// started_at — исполнение (длительность клиент считает сам).
type deploymentView struct {
	ID        string `json:"id"`
	EnvID     string `json:"env_id"`
	Version   string `json:"version"`
	Status    string `json:"status"`
	Initiator string `json:"initiator"`
	RunnerID  string `json:"runner_id"`
	Detail    string `json:"detail"`
	HasLog    bool   `json:"has_log"`
	// ExternalRunID и ExternalURL — прогон внешнего пайплайна доставки;
	// пустые у собственной доставки (api-contract add-external-delivery).
	ExternalRunID string     `json:"external_run_id"`
	ExternalURL   string     `json:"external_url"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
}

func deploymentDTO(d domain.Deployment) deploymentView {
	return deploymentView{
		ID: d.ID, EnvID: d.EnvID, Version: d.Version, Status: d.Status,
		Initiator: d.Initiator, RunnerID: d.RunnerID, Detail: d.Detail,
		HasLog: d.LogRef != "", ExternalRunID: d.ExternalRunID, ExternalURL: d.ExternalURL,
		CreatedAt: d.Created, StartedAt: d.Started, EndedAt: d.Ended,
	}
}

func (s *Server) environmentDTO(r *http.Request, e domain.Environment) environmentView {
	states, err := s.St.MonitorStates(r.Context(), e.ProjectID)
	if err != nil {
		states = nil
	}
	hosts := map[string]domain.EnvHost{}
	if h, err := s.St.CurrentHost(r.Context(), e.ID); err == nil {
		hosts[e.ID] = h
	}
	return s.environmentDTOWith(r, e, states, hosts)
}

// environmentDTOWith: DTO окружения с состояниями наблюдения и хостами
// проекта, прочитанными один раз на список.
func (s *Server) environmentDTOWith(r *http.Request, e domain.Environment, states map[string]store.MonitorBrief,
	hosts map[string]domain.EnvHost) environmentView {
	v := environmentView{
		ID: e.ID, ProjectID: e.ProjectID, Name: e.Name, ExecType: e.ExecType,
		Trigger: e.Trigger, Config: e.Config, Paused: e.Paused,
		RunnerCaps: e.RunnerCaps, CreatedAt: e.Created,
	}
	if h, ok := hosts[e.ID]; ok {
		v.Host = hostDTO(h)
	}
	v.Preparation, _ = s.St.LocalPreparation(r.Context(), e.ID)
	if d, err := s.St.LastDeployment(r.Context(), e.ID); err == nil {
		dto := deploymentDTO(d)
		v.LastDeployment = &dto
	}
	v.MonitorState = store.MonitorDisabled
	if e.Config.Monitor != nil && e.Config.Monitor.Enabled {
		v.MonitorState = store.MonitorUnknown
		if b, ok := states[e.ID]; ok && b.State != "" && b.State != store.MonitorDisabled {
			v.MonitorState, v.MonitorReason, v.MonitorLastAt = b.State, b.Reason, b.LastAt
		}
	}
	if e.ExecType == domain.ExecLocalDocker {
		if p, err := s.St.CurrentLocalPublication(r.Context(), e.ID); err == nil {
			v.Config.Monitor = &domain.MonitorConfig{Enabled: p.DesiredState == "running", CheckURL: p.HealthURL, IntervalMin: 1}
		}
		if m, err := s.St.LocalMonitorView(r.Context(), e.ID); err == nil {
			v.MonitorState = m.State
			v.MonitorReason = m.Reason
			v.MonitorLastAt = m.LastAt
		}
	}

	return v
}

type environmentInput struct {
	Name     string           `json:"name"`
	ExecType string           `json:"exec_type"`
	Trigger  string           `json:"trigger"`
	Config   domain.EnvConfig `json:"config"`
	// RunnerCaps — требуемые capability runner'а публикации помимо deploy.
	RunnerCaps []string `json:"runner_caps"`
}

// execType — тип исполнения окружения; пустой считается ssh (поведение до
// появления внешней доставки).
func (in environmentInput) execType() string {
	if in.ExecType == "" {
		return domain.ExecSSH
	}
	return in.ExecType
}

// validate — 422-валидация по контракту; ошибки конфигурации — domain.
func (in environmentInput) validate() string {
	if in.Name == "" {
		return "нужно имя окружения"
	}
	switch in.execType() {
	case domain.ExecSSH, domain.ExecPipeline, domain.ExecK8s, domain.ExecGitOps, domain.ExecLocalDocker:
	default:
		return "exec_type: ожидается ssh, k8s, pipeline, gitops или local_docker"
	}
	if in.Trigger != "auto" && in.Trigger != "manual" {
		return "trigger: ожидается auto или manual"
	}
	for _, c := range in.RunnerCaps {
		if !capRe.MatchString(c) {
			return "runner_caps: ожидаются имена capability (латиница, цифры, дефис)"
		}
	}
	if err := in.Config.Validate(in.execType()); err != nil {
		return err.Error()
	}
	return ""
}

// GET /projects/{id}/environments — участники проекта.
func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	envs, err := s.St.ListEnvironments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]environmentView, 0, len(envs)) // пустой список — []
	states, err := s.St.MonitorStates(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	hosts, err := s.St.CurrentHosts(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, e := range envs {
		out = append(out, s.environmentDTOWith(r, e, states, hosts))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /projects/{id}/environments — только администратор: deploy_cmd —
// произвольный shell от имени deploy-runner'а (design, решение 6).
func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in environmentInput
	if err := decode(r, &in); err != nil {
		if writeMonitorErr(w, err) {
			return
		}
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Trigger == "" {
		in.Trigger = "manual"
	}
	if in.Config.Provision != nil {
		// Адрес и ключ хоста от провайдера задаёт Rivet.
		in.Config.Host, in.Config.HostKey = "", ""
		if writeMonitorErr(w, in.Config.Provision.Validate()) || !s.assignLocalPort(w, r, &in.Config, "") ||
			!s.checkInfraConn(w, r, in.Config.Provision) {
			return
		}
	}
	if writeMonitorErr(w, in.Config.Validate(in.execType())) {
		return
	}
	if msg := in.validate(); msg != "" {
		unprocessable(w, msg)
		return
	}
	if writeMonitorErr(w, in.Config.ValidateMonitor(in.execType())) {
		return
	}
	env, err := s.St.CreateEnvironment(r.Context(), domain.Environment{
		ProjectID: r.PathValue("id"), Name: in.Name, ExecType: in.execType(),
		Trigger: in.Trigger, Config: in.Config, RunnerCaps: in.RunnerCaps,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.envEvent(r, env, "окружение создано")
	if err := s.Engine.RequestHostIfNeeded(r.Context(), env, user(r)); err != nil {
		slog.Error("хост окружения: запрос", "env", env.ID, "err", err)
	}
	writeJSON(w, http.StatusCreated, s.environmentDTO(r, env))
}

// PATCH /environments/{id} — админ; config заменяется целиком (replace).
func (s *Server) patchEnvironment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	env, err := s.St.GetEnvironment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// config заменяется целиком (контракт: replace, не merge) — указатели
	// отличают «поле не прислали» от «прислали новое значение».
	var in struct {
		Name       *string           `json:"name"`
		Trigger    *string           `json:"trigger"`
		Config     *domain.EnvConfig `json:"config"`
		RunnerCaps *[]string         `json:"runner_caps"`
	}
	if err := decode(r, &in); err != nil {
		if writeMonitorErr(w, err) {
			return
		}
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Name != nil {
		env.Name = *in.Name
	}
	if in.Trigger != nil {
		env.Trigger = *in.Trigger
	}
	prevMonitor := env.Config.Monitor
	prevProvision := env.Config.Provision
	if in.Config != nil {
		// Конфигурацию нельзя менять под идущей публикацией: она читает её
		// на ходу, и правка адреса проверки или пайплайна относилась бы уже
		// к другой публикации. Проверку делает сам UPDATE — окно между
		// проверкой и записью закрыто (ответ 409).
		cur := env.Config
		env.Config = *in.Config
		if !s.applyProvision(w, r, env.ID, cur, &env.Config) {
			return
		}
	}
	if in.RunnerCaps != nil {
		env.RunnerCaps = *in.RunnerCaps
	}
	full := environmentInput{Name: env.Name, ExecType: env.ExecType, Trigger: env.Trigger,
		Config: env.Config, RunnerCaps: env.RunnerCaps}
	if writeMonitorErr(w, full.Config.Validate(full.execType())) {
		return
	}
	if msg := full.validate(); msg != "" {
		unprocessable(w, msg)
		return
	}
	if writeMonitorErr(w, full.Config.ValidateMonitor(full.execType())) {
		return
	}
	monitorChanged := in.Config != nil && !reflect.DeepEqual(prevMonitor, env.Config.Monitor)
	env, err = s.St.UpdateEnvironment(r.Context(), env)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Новые настройки наблюдения действуют сразу: идущая проверка
	// отменяется, следующая — на ближайшем тике.
	if monitorChanged {
		if err := s.St.ResetMonitor(r.Context(), env.ID); err != nil {
			slog.Warn("сброс наблюдения окружения", "env", env.ID, "err", err)
		}
	}
	s.envEvent(r, env, "конфигурация окружения изменена")
	if p := env.Config.Provision; p != nil && (prevProvision == nil || !prevProvision.SameServer(*p)) {
		if err := s.Engine.RequestHostIfNeeded(r.Context(), env, user(r)); err != nil {
			slog.Error("хост окружения: запрос", "env", env.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, s.environmentDTO(r, env))
}

// applyProvision: привязка к провайдеру в правке окружения: при живом
// хосте сервер не меняется (только команды и скрипт), адрес и ключ хоста
// остаются за Rivet.
func (s *Server) applyProvision(w http.ResponseWriter, r *http.Request, envID string, cur domain.EnvConfig, next *domain.EnvConfig) bool {
	if cur.Provision == nil && next.Provision == nil {
		return true
	}
	h, live, err := s.St.LiveHost(r.Context(), envID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if live {
		if next.Provision == nil || cur.Provision == nil || !cur.Provision.SameServer(*next.Provision) {
			writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "host_live",
				Message: "у окружения есть хост (" + h.State + "): сначала удалите его, потом меняйте провайдера и ресурсы"}})
			return false
		}
		next.Host, next.HostKey = cur.Host, cur.HostKey
	} else if next.Provision != nil {
		next.Host, next.HostKey = "", ""
	}
	if next.Provision == nil {
		return true
	}
	return !writeMonitorErr(w, next.Provision.Validate()) && s.assignLocalPort(w, r, next, envID) &&
		s.checkInfraConn(w, r, next.Provision)
}

// assignLocalPort: порт локального хоста: свободный из диапазона
// установки, а заданный вручную не должен быть занят другим окружением.
func (s *Server) assignLocalPort(w http.ResponseWriter, r *http.Request, cfg *domain.EnvConfig, envID string) bool {
	p := cfg.Provision
	if p == nil || p.Provider != domain.ProvisionLocal {
		return true
	}
	used, err := s.St.UsedLocalPorts(r.Context(), envID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if p.Port != 0 {
		if other, ok := used[p.Port]; ok {
			fieldErr(w, "invalid", "config.provision.port", "порт "+strconv.Itoa(p.Port)+" занят окружением "+other)
			return false
		}
		return true
	}
	// Автоматический порт выбирается при сохранении в транзакции Store.
	return true
}

// checkInfraConn: подключение привязки существует, включено и того же
// провайдера.
func (s *Server) checkInfraConn(w http.ResponseWriter, r *http.Request, p *domain.ProvisionConfig) bool {
	if p.Provider == domain.ProvisionLocal {
		return true
	}
	c, err := s.St.GetInfraConnection(r.Context(), p.Connection)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (c.Provider != p.Provider || !c.Enabled)) {
		fieldErr(w, "invalid", "config.provision.connection", "подключение "+p.Connection+" не найдено или выключено")
		return false
	}
	if err != nil {
		writeErr(w, err)
		return false
	}
	return true
}

// DELETE /environments/{id} — админ; 409 при выполняющейся публикации.
func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	env, err := s.St.GetEnvironment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if env.ExecType == domain.ExecLocalDocker {
		if err := s.St.RequestLocalDeletion(r.Context(), env.ID); err != nil {
			localError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
		return
	}
	if err := s.St.DeleteEnvironment(r.Context(), env.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.envEvent(r, env, "окружение удалено")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /environments/{id}/deploy — участник; публикует текущий HEAD базовой
// ветки (HeadSHA у SCM в момент запуска), коалесценция с queued.
func (s *Server) envDeploy(w http.ResponseWriter, r *http.Request) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if env.ExecType == domain.ExecLocalDocker {
		s.publishLocal(w, r, env)
		return
	}

	if env.Paused {
		writeErr(w, store.ErrConflict) // сначала resume (контракт)
		return
	}
	p, err := s.St.GetProject(r.Context(), env.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	adapter, err := s.Engine.SCMFor(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	version, err := adapter.HeadSHA(r.Context(), p.RepoPath, p.DefaultBranch)
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.St.EnqueueDeployment(r.Context(), env.ID, version, user(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, deploymentDTO(d))
}

// POST /environments/{id}/resume — участник снимает паузу после провала.
func (s *Server) envResume(w http.ResponseWriter, r *http.Request) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.St.SetEnvPaused(r.Context(), env.ID, false); err != nil {
		writeErr(w, err)
		return
	}
	env.Paused = false
	s.envEvent(r, env, "автопубликации возобновлены")
	writeJSON(w, http.StatusOK, s.environmentDTO(r, env))
}

// GET /environments/{id}/deployments — история публикаций.
func (s *Server) envDeployments(w http.ResponseWriter, r *http.Request) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 200)
	}
	deps, err := s.St.ListDeployments(r.Context(), env.ID, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]deploymentView, 0, len(deps)) // пустая история — []
	for _, d := range deps {
		out = append(out, deploymentDTO(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /deployments/{id}/log — сохранённый лог из blob; 404 не различает
// причины (паттерн транскриптов сессий).
func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	ref, err := s.St.DeploymentLogForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ref == "" || s.Blob == nil {
		writeErr(w, store.ErrNotFound)
		return
	}
	data, err := s.Blob.Get(r.Context(), ref)
	if err != nil {
		slog.Error("deploy log get", "deployment", r.PathValue("id"), "ref", ref, "err", err)
		writeErr(w, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

// envEvent — изменение конфигурации окружений видно в event log
// (кто и что менял; design, решение 6).
func (s *Server) envEvent(r *http.Request, env domain.Environment, text string) {
	_, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "environment.config",
		ProjectID: env.ProjectID,
		Text:      text + ": " + env.Name,
		Payload:   map[string]any{"environment_id": env.ID},
	})
	if err != nil {
		slog.Error("environment event", "env", env.ID, "err", err)
	}
}
