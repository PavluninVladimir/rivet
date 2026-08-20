// Package api — клиентский HTTP API (/api/v1): REST + SSE по api-contract.md.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
)

type Server struct {
	St     *store.Store
	Engine *orchestrator.Engine
	Hub    *stream.Hub
	// Planner — горячо заменяемый планировщик декомпозиции; пересобирается
	// ReloadPlanner из базы либо EnvPlanner (design add-operations-management).
	Planner    *planner.Holder
	EnvPlanner EnvPlanner
	// Version, ProtocolVersion, StartedAt — для состояния установки.
	Version         string
	ProtocolVersion string
	StartedAt       time.Time
	// GRPCAddr и GRPCTLS — адрес протокола runner'ов и включён ли на нём TLS:
	// консоль собирает из них команду запуска runner'а.
	GRPCAddr string
	GRPCTLS  bool
	// Blob — хранилище транскриптов; nil (MinIO отключён) — транскрипты
	// отвечают 404, остальной API работает.
	Blob *blob.Store
	// WebhookSecret — секрет HMAC-подписи входящих webhook'ов;
	// пустой выключает endpoint (fail-closed, спека scm-integration).
	WebhookSecret string
	// Secrets шифрует учётные данные хостингов; выключён без ключа.
	Secrets *secretbox.Box
	// PublicURL — внешний адрес установки для регистрации webhook.
	PublicURL string
	// TrustProxy — доверять X-Forwarded-Proto при выставлении Secure-cookie
	// (rivetd за TLS-терминирующим прокси; design, решение 12).
	TrustProxy bool

	throttle *loginThrottle
}

func (s *Server) Handler() http.Handler {
	s.throttle = newLoginThrottle()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.logout)
	mux.HandleFunc("GET /api/v1/auth/me", s.me)
	mux.HandleFunc("POST /api/v1/auth/password", s.changePassword)
	mux.HandleFunc("GET /api/v1/users", s.listUsers)
	mux.HandleFunc("POST /api/v1/users", s.createUser)
	mux.HandleFunc("PATCH /api/v1/users/{id}", s.patchUser)
	mux.HandleFunc("POST /api/v1/users/{id}/password/reset", s.resetUserPassword)
	mux.HandleFunc("GET /api/v1/projects/{id}/members", s.listMembers)
	mux.HandleFunc("POST /api/v1/projects/{id}/members", s.addMember)
	mux.HandleFunc("PATCH /api/v1/projects/{id}/members/{login}", s.setMemberRole)
	mux.HandleFunc("DELETE /api/v1/projects/{id}/members/{login}", s.removeMember)
	mux.HandleFunc("GET /api/v1/tokens", s.listTokens)
	mux.HandleFunc("POST /api/v1/tokens", s.createToken)
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.deleteToken)
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/system/status", s.systemStatus)
	mux.HandleFunc("GET /api/v1/system/models", s.listModels)
	mux.HandleFunc("PUT /api/v1/system/models/{provider}", s.putModel)
	mux.HandleFunc("POST /api/v1/system/models/{provider}/check", s.checkModel)
	mux.HandleFunc("DELETE /api/v1/system/models/{provider}", s.deleteModel)
	mux.HandleFunc("GET /api/v1/runner-tokens", s.listRunnerTokens)
	mux.HandleFunc("POST /api/v1/runner-tokens", s.createRunnerToken)
	mux.HandleFunc("DELETE /api/v1/runner-tokens/{id}", s.revokeRunnerToken)
	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("PATCH /api/v1/projects/{id}", s.patchProject)
	mux.HandleFunc("POST /api/v1/scm/probe", s.scmProbe)
	mux.HandleFunc("GET /api/v1/projects/{id}/repository", s.getRepository)
	mux.HandleFunc("PUT /api/v1/projects/{id}/credentials", s.putCredentials)
	mux.HandleFunc("GET /api/v1/projects/{id}/epics", s.listEpics)
	mux.HandleFunc("POST /api/v1/projects/{id}/epics", s.createEpic)
	mux.HandleFunc("GET /api/v1/epics/{id}", s.getEpic)
	mux.HandleFunc("POST /api/v1/epics/{id}/tasks", s.addTask)
	mux.HandleFunc("POST /api/v1/epics/{id}/decompose", s.decompose)
	mux.HandleFunc("POST /api/v1/epics/{id}/start", s.epicAction(domain.EpicRunning, "Epic запущен"))
	mux.HandleFunc("POST /api/v1/epics/{id}/pause", s.epicAction(domain.EpicPaused, "Epic приостановлен"))
	mux.HandleFunc("POST /api/v1/epics/{id}/resume", s.epicAction(domain.EpicRunning, "Epic возобновлён"))
	mux.HandleFunc("POST /api/v1/epics/{id}/archive", s.epicAction(domain.EpicArchived, "Epic архивирован"))
	mux.HandleFunc("GET /api/v1/projects/{id}/environments", s.listEnvironments)
	mux.HandleFunc("POST /api/v1/projects/{id}/environments", s.createEnvironment)
	mux.HandleFunc("PATCH /api/v1/environments/{id}", s.patchEnvironment)
	mux.HandleFunc("DELETE /api/v1/environments/{id}", s.deleteEnvironment)
	mux.HandleFunc("POST /api/v1/environments/{id}/deploy", s.envDeploy)
	mux.HandleFunc("POST /api/v1/environments/{id}/resume", s.envResume)
	mux.HandleFunc("GET /api/v1/environments/{id}/deployments", s.envDeployments)
	mux.HandleFunc("GET /api/v1/deployments/{id}/log", s.deploymentLog)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}/sessions", s.listTaskSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}/transcript", s.sessionTranscript)
	mux.HandleFunc("POST /api/v1/tasks/{id}/answer", s.taskAnswer)
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.taskRetry)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.taskCancel)
	mux.HandleFunc("POST /api/v1/tasks/{id}/merge", s.taskMerge)
	mux.HandleFunc("GET /api/v1/attention", s.listAttention)
	mux.HandleFunc("POST /api/v1/attention/{id}/claim", s.claimAttention)
	mux.HandleFunc("GET /api/v1/runners", s.listRunners)
	mux.HandleFunc("POST /api/v1/runners/{id}/drain", s.runnerDrain(true))
	mux.HandleFunc("POST /api/v1/runners/{id}/undrain", s.runnerDrain(false))
	mux.HandleFunc("GET /api/v1/events", s.listEvents)
	mux.HandleFunc("GET /api/v1/usage", s.usage)
	mux.HandleFunc("GET /api/v1/stream", s.sse)
	mux.HandleFunc("POST /api/v1/webhooks/github", s.githubWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/gitlab", s.gitlabWebhook)
	return s.withAuth(mux)
}

// ─── формат ошибок и ответов ─────────────────────────────────────────────

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal"
	var bad domain.ErrBadTransition
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.As(err, &bad):
		status, code = http.StatusConflict, "bad_transition"
	case errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrLastAdmin),
		errors.Is(err, store.ErrLastMember),
		errors.Is(err, store.ErrLastOwner),
		errors.Is(err, store.ErrRevoked):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, store.ErrInvalid):
		status, code = http.StatusUnprocessableEntity, "invalid"
	case errors.Is(err, store.ErrWeakPassword), errors.Is(err, store.ErrSamePassword):
		status, code = http.StatusUnprocessableEntity, "invalid"
	case errors.Is(err, store.ErrBadPassword):
		status, code = http.StatusUnauthorized, "unauthorized"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]apiError{"error": {Code: code, Message: err.Error()}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func unprocessable(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity,
		map[string]apiError{"error": {Code: "invalid", Message: msg}})
}

// ─── projects ────────────────────────────────────────────────────────────

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.St.ListProjects(r.Context(), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// DTO, а не доменная структура: у неё нет json-тегов, и подключение
	// репозитория не доехало бы до консоли.
	out := make([]projectView, 0, len(list))
	for _, p := range list {
		out = append(out, projectDTO(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// createProject — подключение репозитория при создании проекта
// (api-contract add-repo-onboarding): режимы «подключить существующий» и
// «создать новый», плюс устаревшая форма {name, repo} для необновлённой
// консоли. Проект не создаётся, пока проверка доступа не прошла.
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string         `json:"name"`
		Repo   string         `json:"repo"` // устаревшая форма
		Checks []domain.Check `json:"checks"`

		Provider string `json:"provider"`
		RepoURL  string `json:"repo_url"`
		BaseURL  string `json:"base_url"`
		Token    string `json:"token"`
		Create   *struct {
			Owner      string `json:"owner"`
			RepoName   string `json:"repo_name"`
			Visibility string `json:"visibility"`
		} `json:"create"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		unprocessable(w, "нужно название проекта")
		return
	}
	// Устаревшая форма: GitHub на глобальном токене установки.
	if in.Token == "" && in.RepoURL == "" && in.Create == nil {
		if in.Repo == "" {
			unprocessable(w, "нужен repo_url и токен (или устаревшее поле repo)")
			return
		}
		p, err := s.St.CreateProject(r.Context(), in.Name, in.Repo, in.Checks, currentUser(r).ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, projectDTO(p))
		return
	}
	if in.Token == "" {
		unprocessable(w, "нужен токен доступа к хостингу")
		return
	}
	if !s.Secrets.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "no_secret_key", Message: "ключ шифрования не настроен: учётные данные не сохранить"}})
		return
	}
	provider, baseURL, repoPath, err := probeInput{
		Provider: in.Provider, RepoURL: in.RepoURL, BaseURL: in.BaseURL,
	}.resolve()
	if err != nil {
		unprocessable(w, err.Error())
		return
	}
	provider = s.effectiveProvider(provider)

	// Режим «создать новый»: сначала проверяем токен, затем создаём
	// репозиторий — он инициализируется с базовой веткой.
	if in.Create != nil {
		res := s.probe(r, provider, baseURL, "", in.Token)
		if !res.OK {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": apiError{Code: res.Reason, Message: res.Message}, "probe": res})
			return
		}
		adapter, aerr := s.adapters().For(provider, baseURL, in.Token)
		if aerr != nil {
			writeErr(w, aerr)
			return
		}
		info, cerr := adapter.CreateRepo(r.Context(), scm.NewRepo{
			Owner: in.Create.Owner, Name: in.Create.RepoName,
			Private: in.Create.Visibility != "public",
		})
		if errors.Is(cerr, scm.ErrRepoExists) {
			writeErr(w, store.ErrConflict)
			return
		}
		if cerr != nil {
			writeErr(w, cerr)
			return
		}
		s.finishProject(w, r, in.Name, in.Checks, store.NewRepoConnection{
			Provider: string(provider), BaseURL: baseURL, RepoPath: info.Path,
			DefaultBranch: info.DefaultBranch, Token: in.Token, TokenOwner: res.TokenOwner,
		})
		return
	}

	if repoPath == "" {
		unprocessable(w, "нужен URL репозитория")
		return
	}
	res := s.probe(r, provider, baseURL, repoPath, in.Token)
	if !res.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": apiError{Code: res.Reason, Message: res.Message}, "probe": res})
		return
	}
	s.finishProject(w, r, in.Name, in.Checks, store.NewRepoConnection{
		Provider: string(provider), BaseURL: baseURL, RepoPath: res.RepoPath,
		DefaultBranch: res.DefaultBranch, Token: in.Token, TokenOwner: res.TokenOwner,
	})
}

// finishProject создаёт проект с подключением и подписывает хостинг на события.
func (s *Server) finishProject(w http.ResponseWriter, r *http.Request, name string,
	checks []domain.Check, conn store.NewRepoConnection) {

	p, err := s.St.CreateProjectWithRepo(r.Context(), name, checks, currentUser(r).ID, conn, s.Secrets)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.registerWebhook(r, p, conn.Token)
	s.repoEvent(r, p, "репозиторий подключён: "+p.RepoPath)
	writeJSON(w, http.StatusCreated, projectDTO(p))
}

// patchProject — правка названия и проверок (api-contract: checks
// заменяются целиком).
func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r, r.PathValue("id")) {
		return
	}
	p, err := s.St.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Name   *string         `json:"name"`
		Checks *[]domain.Check `json:"checks"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	name, checks := p.Name, p.Checks
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if in.Checks != nil {
		checks = *in.Checks
	}
	if name == "" {
		unprocessable(w, "название проекта не может быть пустым")
		return
	}
	for _, c := range checks {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Cmd) == "" {
			unprocessable(w, "у проверки должны быть имя и команда")
			return
		}
	}
	p, err = s.St.UpdateProjectSettings(r.Context(), p.ID, name, checks)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "project.settings",
		ProjectID: p.ID, Text: "настройки проекта изменены",
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectDTO(p))
}

// projectView — проект в API. Поля перечислены явно: доменная структура
// несёт секрет webhook и ссылку на учётные данные, и встраивание вынесло
// бы их наружу. Repo дублирует repo_path для клиентов старой формы.
type projectView struct {
	ID            string         `json:"ID"`
	Name          string         `json:"Name"`
	Checks        []domain.Check `json:"Checks"`
	Created       time.Time      `json:"Created"`
	Repo          string         `json:"Repo"`
	Provider      string         `json:"provider"`
	BaseURL       string         `json:"base_url"`
	RepoPath      string         `json:"repo_path"`
	DefaultBranch string         `json:"default_branch"`
	WebURL        string         `json:"web_url"`
}

func projectDTO(p domain.Project) projectView {
	return projectView{
		ID: p.ID, Name: p.Name, Checks: p.Checks, Created: p.Created,
		Repo: p.RepoPath, Provider: p.Provider, BaseURL: p.BaseURL,
		RepoPath: p.RepoPath, DefaultBranch: p.DefaultBranch, WebURL: p.WebURL(),
	}
}

// ─── epics ───────────────────────────────────────────────────────────────

func (s *Server) listEpics(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	out, err := s.St.ListEpics(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createEpic(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
		Goal  string `json:"goal"`
	}
	if err := decode(r, &in); err != nil || in.Title == "" {
		unprocessable(w, "нужен title")
		return
	}
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	e, err := s.St.CreateEpic(r.Context(), r.PathValue("id"), in.Title, in.Goal)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

type epicView struct {
	domain.Epic
	Tasks      []domain.Task    `json:"tasks"`
	Progress   map[string]any   `json:"progress"`
	Usage      []store.UsageRow `json:"usage,omitempty"`
	UsageTotal *store.UsageRow  `json:"usage_total,omitempty"`
}

func (s *Server) getEpic(w http.ResponseWriter, r *http.Request) {
	e, err := s.St.EpicForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.St.ListEpicTasks(r.Context(), e.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var total, done int
	for _, t := range tasks {
		total += t.Estimate
		if t.Status == domain.TaskDone {
			done += t.Estimate
		}
	}
	pct := 0
	if total > 0 {
		pct = done * 100 / total
	}
	usage, usageTotal, err := s.St.EpicUsage(r.Context(), e.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, epicView{
		Epic: e, Tasks: tasks,
		Progress:   map[string]any{"pct": pct, "weighted": true},
		Usage:      usage,
		UsageTotal: usageTotal,
	})
}

func (s *Server) addTask(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("id")
	if !s.requireEpicMember(w, r, epicID) {
		return
	}
	var in struct {
		Title        string   `json:"title"`
		Description  string   `json:"description"`
		Criteria     []string `json:"criteria"`
		Deps         []string `json:"deps"`
		Capabilities []string `json:"capabilities"`
		Estimate     int      `json:"estimate"`
	}
	if err := decode(r, &in); err != nil || in.Title == "" {
		unprocessable(w, "нужен title")
		return
	}
	// Ацикличность: строим DAG существующих задач + новая (спека domain-model).
	existing, err := s.St.ListEpicTasks(r.Context(), epicID)
	if err != nil {
		writeErr(w, err)
		return
	}
	deps := map[string][]string{"__new__": in.Deps}
	for _, t := range existing {
		deps[t.ID] = t.Deps
	}
	if err := store.ValidateDAG(deps); err != nil {
		unprocessable(w, err.Error())
		return
	}
	crit := make([]domain.Criterion, 0, len(in.Criteria))
	for _, c := range in.Criteria {
		crit = append(crit, domain.Criterion{Text: c})
	}
	t, err := s.St.CreateTask(r.Context(), epicID, store.NewTask{
		Title: in.Title, Description: in.Description, Criteria: crit,
		Deps: in.Deps, Capabilities: in.Capabilities, Estimate: in.Estimate,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) epicAction(to domain.EpicStatus, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireEpicMember(w, r, r.PathValue("id")) {
			return
		}
		err := s.St.TransitionEpic(r.Context(), r.PathValue("id"), to, store.EventInput{
			ActorKind: domain.ActorUser, ActorID: user(r), Type: "epic.status",
			Text: text, Payload: map[string]any{"status": string(to)},
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(to)})
	}
}

// ─── tasks ───────────────────────────────────────────────────────────────

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	t, err := s.St.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	timeline, err := s.St.Events(r.Context(), store.EventFilter{TaskID: t.ID, Limit: 200})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "timeline": timeline})
}

func (s *Server) taskAnswer(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil || in.Text == "" {
		unprocessable(w, "нужен text ответа")
		return
	}
	if err := s.St.ResolveTask(r.Context(), r.PathValue("id"), in.Text, user(r), false); err != nil {
		writeErr(w, err)
		return
	}
	s.dropSession(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) taskRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	if err := s.St.ResolveTask(r.Context(), r.PathValue("id"), "", user(r), false); err != nil {
		writeErr(w, err)
		return
	}
	s.dropSession(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) taskCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	if err := s.St.ResolveTask(r.Context(), r.PathValue("id"), "", user(r), true); err != nil {
		writeErr(w, err)
		return
	}
	s.dropSession(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// dropSession — ResolveTask закрыл сессии задачи в БД, кеш Engine обязан
// их забыть (Engine nil в части тестов).
func (s *Server) dropSession(taskID string) {
	if s.Engine != nil {
		s.Engine.DropSession(taskID)
	}
}

func (s *Server) taskMerge(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	if err := s.Engine.MergeTask(r.Context(), r.PathValue("id"), user(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}

// ─── attention / runners / events / usage ────────────────────────────────

func (s *Server) listAttention(w http.ResponseWriter, r *http.Request) {
	out, err := s.St.ListAttention(r.Context(), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) claimAttention(w http.ResponseWriter, r *http.Request) {
	if err := s.St.ClaimAttention(r.Context(), r.PathValue("id"), user(r), currentUser(r).ID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "claimed"})
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	out, err := s.St.ListRunners(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// Метаданные чужой работы не раскрываются: TaskID виден только участникам
	// проекта задачи (api-contract, design решение 5).
	for i, rn := range out {
		if rn.TaskID == "" {
			continue
		}
		projectID, _, err := s.St.TaskRefs(r.Context(), rn.TaskID)
		if err != nil {
			out[i].TaskID = ""
			continue
		}
		member, err := s.St.IsMember(r.Context(), projectID, currentUser(r).ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !member {
			out[i].TaskID = ""
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runnerDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		if err := s.St.SetRunnerDraining(r.Context(), r.PathValue("id"), drain); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"draining": drain})
	}
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after int64
	_, _ = fmt.Sscanf(q.Get("cursor"), "%d", &after)
	var limit int
	_, _ = fmt.Sscanf(q.Get("limit"), "%d", &limit)
	// Лента аудита установки — только администратору (спека observability).
	installation := q.Get("scope") == "installation"
	if installation && !s.requireAdmin(w, r) {
		return
	}
	out, err := s.St.Events(r.Context(), store.EventFilter{
		ProjectID: q.Get("project"), EpicID: q.Get("epic"),
		TaskID: q.Get("task"), Type: q.Get("type"), AfterID: after, Limit: limit,
		ViewerID: currentUser(r).ID, Installation: installation,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var from, to time.Time
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				unprocessable(w, name+": ожидается время в RFC3339")
				return
			}
			*dst = t
		}
	}
	groupBy := q.Get("group_by")
	switch groupBy {
	case "", "epic", "task", "runner", "model", "project":
	default:
		unprocessable(w, "group_by: ожидается epic, task, runner, model или project")
		return
	}
	// Установочный срез — только администратору (спека observability
	// «Установочный срез по проектам»).
	scope := store.UsageScope{ViewerID: currentUser(r).ID}
	if q.Get("scope") == "installation" {
		if !s.requireAdmin(w, r) {
			return
		}
		scope.Installation = true
	}
	out, err := s.St.UsageSummary(r.Context(), scope, groupBy, from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── SSE ─────────────────────────────────────────────────────────────────

// sse: события проекта из event log (реплей по Last-Event-ID, далее — поллинг)
// плюс live-чанки session.log из hub (без реплея — по контракту).
func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		unprocessable(w, "нужен параметр project")
		return
	}
	// SSE аутентифицируется только cookie (api-contract): поток долгоживущий,
	// его отзыв привязан к сессии консоли. Bearer сюда не годится, поэтому
	// cookie проверяется по-настоящему, а не полагаемся на middleware
	// (иначе битая cookie + валидный PAT дали бы поток мимо сессии).
	sessionUser, err := s.sseSessionUser(r)
	if err != nil {
		unauthorized(w)
		return
	}
	if ok, err := s.St.IsMember(r.Context(), projectID, sessionUser.ID); err != nil || !ok {
		writeErr(w, store.ErrNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errors.New("streaming не поддерживается"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	var after int64
	_, _ = fmt.Sscanf(r.Header.Get("Last-Event-ID"), "%d", &after)

	logs, unsub := s.Hub.Subscribe(projectID)
	defer unsub()

	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	// Отзыв доступа закрывает открытый поток: сессия и членство
	// перепроверяются раз в recheckEvery тиков (деактивация, logout,
	// удаление из проекта — данные перестают течь, api-contract «SSE»).
	const recheckEvery = 10
	ticks := 0
	accessAlive := func() bool {
		u, err := s.sseSessionUser(r)
		if err != nil {
			return false
		}
		ok, err := s.St.IsMember(r.Context(), projectID, u.ID)
		return err == nil && ok
	}

	flushEvents := func() bool {
		evs, err := s.St.Events(r.Context(), store.EventFilter{ProjectID: projectID, AfterID: after, Limit: 200})
		if err != nil {
			return false
		}
		for _, e := range evs {
			after = e.ID
			raw, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, sseType(e.Type), raw)
		}
		if len(evs) > 0 {
			fl.Flush()
		}
		return true
	}
	if !flushEvents() {
		return
	}
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-logs:
			if c.DeployID != "" {
				payload, _ := json.Marshal(map[string]string{"deploy_id": c.DeployID, "data": string(c.Data)})
				fmt.Fprintf(w, "event: deploy.log\ndata: %s\n\n", payload)
			} else {
				payload, _ := json.Marshal(map[string]string{"task_id": c.TaskID, "data": string(c.Data)})
				fmt.Fprintf(w, "event: session.log\ndata: %s\n\n", payload)
			}
			fl.Flush()
		case <-poll.C:
			if ticks++; ticks%recheckEvery == 0 && !accessAlive() {
				return
			}
			if !flushEvents() {
				return
			}
		}
	}
}

// sseType отображает типы event log в типы контракта SSE.
func sseType(t string) string {
	switch t {
	case "task.status", "task.assign", "task.transition_denied", "task.review_passed":
		return "task.status"
	case "epic.status":
		return "epic.progress"
	case "session.step":
		return "session.step"
	default:
		return t
	}
}
