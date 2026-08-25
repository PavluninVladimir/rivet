// Окружения публикации и деплой (api-contract implement-deployment).
package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// environmentView — DTO Environment контракта.
type environmentView struct {
	ID             string           `json:"id"`
	ProjectID      string           `json:"project_id"`
	Name           string           `json:"name"`
	ExecType       string           `json:"exec_type"`
	Trigger        string           `json:"trigger"`
	Config         domain.EnvConfig `json:"config"`
	Paused         bool             `json:"paused"`
	LastDeployment *deploymentView  `json:"last_deployment"`
	CreatedAt      time.Time        `json:"created_at"`
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
	v := environmentView{
		ID: e.ID, ProjectID: e.ProjectID, Name: e.Name, ExecType: e.ExecType,
		Trigger: e.Trigger, Config: e.Config, Paused: e.Paused, CreatedAt: e.Created,
	}
	if d, err := s.St.LastDeployment(r.Context(), e.ID); err == nil {
		dto := deploymentDTO(d)
		v.LastDeployment = &dto
	}
	return v
}

type environmentInput struct {
	Name     string           `json:"name"`
	ExecType string           `json:"exec_type"`
	Trigger  string           `json:"trigger"`
	Config   domain.EnvConfig `json:"config"`
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
	if in.ExecType != "" && in.ExecType != domain.ExecSSH && in.ExecType != domain.ExecPipeline {
		return "exec_type: ожидается ssh или pipeline"
	}
	if in.Trigger != "auto" && in.Trigger != "manual" {
		return "trigger: ожидается auto или manual"
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
	for _, e := range envs {
		out = append(out, s.environmentDTO(r, e))
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
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Trigger == "" {
		in.Trigger = "manual"
	}
	if msg := in.validate(); msg != "" {
		unprocessable(w, msg)
		return
	}
	env, err := s.St.CreateEnvironment(r.Context(), domain.Environment{
		ProjectID: r.PathValue("id"), Name: in.Name, ExecType: in.execType(),
		Trigger: in.Trigger, Config: in.Config,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.envEvent(r, env, "окружение создано")
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
		Name    *string           `json:"name"`
		Trigger *string           `json:"trigger"`
		Config  *domain.EnvConfig `json:"config"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Name != nil {
		env.Name = *in.Name
	}
	if in.Trigger != nil {
		env.Trigger = *in.Trigger
	}
	if in.Config != nil {
		// Конфигурацию нельзя менять под идущей публикацией: она читает её
		// на ходу, и правка адреса проверки или пайплайна относилась бы уже
		// к другой публикации. Проверку делает сам UPDATE — окно между
		// проверкой и записью закрыто (ответ 409).
		env.Config = *in.Config
	}
	full := environmentInput{Name: env.Name, ExecType: env.ExecType, Trigger: env.Trigger, Config: env.Config}
	if msg := full.validate(); msg != "" {
		unprocessable(w, msg)
		return
	}
	env, err = s.St.UpdateEnvironment(r.Context(), env)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.envEvent(r, env, "конфигурация окружения изменена")
	writeJSON(w, http.StatusOK, s.environmentDTO(r, env))
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
