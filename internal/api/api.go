// Package api — клиентский HTTP API (/api/v1): REST + SSE по api-contract.md.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
)

type Server struct {
	St      *store.Store
	Engine  *orchestrator.Engine
	Hub     *stream.Hub
	Planner *planner.Planner
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("GET /api/v1/projects/{id}/epics", s.listEpics)
	mux.HandleFunc("POST /api/v1/projects/{id}/epics", s.createEpic)
	mux.HandleFunc("GET /api/v1/epics/{id}", s.getEpic)
	mux.HandleFunc("POST /api/v1/epics/{id}/tasks", s.addTask)
	mux.HandleFunc("POST /api/v1/epics/{id}/decompose", s.decompose)
	mux.HandleFunc("POST /api/v1/epics/{id}/start", s.epicAction(domain.EpicRunning, "Epic запущен"))
	mux.HandleFunc("POST /api/v1/epics/{id}/pause", s.epicAction(domain.EpicPaused, "Epic приостановлен"))
	mux.HandleFunc("POST /api/v1/epics/{id}/resume", s.epicAction(domain.EpicRunning, "Epic возобновлён"))
	mux.HandleFunc("POST /api/v1/epics/{id}/archive", s.epicAction(domain.EpicArchived, "Epic архивирован"))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
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
	return mux
}

// user — идентичность пользователя; до появления аутентификации — dev-заголовок.
func user(r *http.Request) string {
	if u := r.Header.Get("X-Rivet-User"); u != "" {
		return u
	}
	return "dev"
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
	out, err := s.St.ListProjects(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string         `json:"name"`
		Repo   string         `json:"repo"`
		Checks []domain.Check `json:"checks"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" || in.Repo == "" {
		unprocessable(w, "нужны name и repo (owner/name)")
		return
	}
	p, err := s.St.CreateProject(r.Context(), in.Name, in.Repo, in.Checks)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ─── epics ───────────────────────────────────────────────────────────────

func (s *Server) listEpics(w http.ResponseWriter, r *http.Request) {
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
	e, err := s.St.CreateEpic(r.Context(), r.PathValue("id"), in.Title, in.Goal)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

type epicView struct {
	domain.Epic
	Tasks    []domain.Task    `json:"tasks"`
	Progress map[string]any   `json:"progress"`
	Usage    []store.UsageRow `json:"usage,omitempty"`
}

func (s *Server) getEpic(w http.ResponseWriter, r *http.Request) {
	e, err := s.St.GetEpic(r.Context(), r.PathValue("id"))
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
	writeJSON(w, http.StatusOK, epicView{
		Epic: e, Tasks: tasks,
		Progress: map[string]any{"pct": pct, "weighted": true},
	})
}

func (s *Server) addTask(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("id")
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) taskRetry(w http.ResponseWriter, r *http.Request) {
	if err := s.St.ResolveTask(r.Context(), r.PathValue("id"), "", user(r), false); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) taskCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.St.ResolveTask(r.Context(), r.PathValue("id"), "", user(r), true); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) taskMerge(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.MergeTask(r.Context(), r.PathValue("id"), user(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}

// ─── attention / runners / events / usage ────────────────────────────────

func (s *Server) listAttention(w http.ResponseWriter, r *http.Request) {
	out, err := s.St.ListAttention(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) claimAttention(w http.ResponseWriter, r *http.Request) {
	if err := s.St.ClaimAttention(r.Context(), r.PathValue("id"), user(r)); err != nil {
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
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runnerDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	out, err := s.St.Events(r.Context(), store.EventFilter{
		ProjectID: q.Get("project"), EpicID: q.Get("epic"),
		TaskID: q.Get("task"), Type: q.Get("type"), AfterID: after,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	out, err := s.St.UsageSummary(r.Context(), r.URL.Query().Get("group_by"))
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
			payload, _ := json.Marshal(map[string]string{"task_id": c.TaskID, "data": string(c.Data)})
			fmt.Fprintf(w, "event: session.log\ndata: %s\n\n", payload)
			fl.Flush()
		case <-poll.C:
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
