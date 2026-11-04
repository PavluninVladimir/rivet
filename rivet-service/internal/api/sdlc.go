// Цикл разработки, релизы, инциденты и наблюдение (api-contract
// add-sdlc-cycle).
package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (s *Server) routesSDLC(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{id}/cycle", s.getCycle)
	mux.HandleFunc("GET /api/v1/projects/{id}/releases", s.listReleases)
	mux.HandleFunc("GET /api/v1/releases/{id}", s.getRelease)
	mux.HandleFunc("POST /api/v1/releases/{id}/notes", s.regenerateNotes)
	mux.HandleFunc("GET /api/v1/projects/{id}/incidents", s.listIncidents)
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.getIncident)
	mux.HandleFunc("POST /api/v1/incidents/{id}/resolve", s.resolveIncident)
	mux.HandleFunc("POST /api/v1/incidents/{id}/bug", s.rebugIncident)
	mux.HandleFunc("GET /api/v1/environments/{id}/monitor", s.getMonitor)
}

// writeErrCode — ошибка API с машиночитаемым кодом.
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// listLimit — limit из запроса в пределах 1–100.
func listLimit(r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 {
		return 0, false
	}
	return n, true
}

// GET /projects/{id}/cycle?period=today|7d|30d&tz= — участники проекта.
func (s *Server) getCycle(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = cycle.DefaultPeriod
	}
	if !cycle.ValidPeriod(period) {
		writeErrCode(w, http.StatusBadRequest, "invalid", "period: ожидается today, 7d или 30d")
		return
	}
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			writeErrCode(w, http.StatusBadRequest, "invalid", "tz: ожидается IANA-зона")
			return
		}
		loc = l
	}
	var busy store.CycleBusy
	if s.Engine != nil {
		busy = s.Engine.OpsBusy()
	}
	v, err := s.St.LoadCycle(r.Context(), projectID, period, loc, busy)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ─── релизы ─────────────────────────────────────────────────────────────

func nextCursor(next string) any {
	if next == "" {
		return nil
	}
	return next
}

// GET /projects/{id}/releases?env&limit&before.
func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	limit, ok := listLimit(r, 20)
	if !ok {
		writeErrCode(w, http.StatusBadRequest, "invalid", "limit: от 1 до 100")
		return
	}
	q := r.URL.Query()
	rels, next, err := s.St.ListReleases(r.Context(), projectID, q.Get("env"), q.Get("before"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": rels, "next": nextCursor(next)})
}

// releaseForViewer — релиз, если зритель участник его проекта (иначе 404).
func (s *Server) releaseForViewer(w http.ResponseWriter, r *http.Request) (store.Release, bool) {
	rel, err := s.St.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return rel, false
	}
	if !s.requireMember(w, r, rel.ProjectID) {
		return rel, false
	}
	return rel, true
}

// GET /releases/{id} — с notes_md.
func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.releaseForViewer(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// POST /releases/{id}/notes — перегенерация, только владелец проекта.
func (s *Server) regenerateNotes(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.releaseForViewer(w, r)
	if !ok || !s.requireOwner(w, r, rel.ProjectID) {
		return
	}
	rel, err := s.St.RequestReleaseNotes(r.Context(), rel.ID)
	switch {
	case errors.Is(err, store.ErrNotesPending):
		writeErrCode(w, http.StatusConflict, "notes_pending", err.Error())
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	if s.Engine != nil {
		s.Engine.EnqueueOps(r.Context(), orchestrator.OpsJob{Kind: orchestrator.JobReleaseNotes, RefID: rel.ID, ProjectID: rel.ProjectID})
	}
	rel.NotesMD = ""
	writeJSON(w, http.StatusAccepted, rel)
}

// ─── инциденты ──────────────────────────────────────────────────────────

// GET /projects/{id}/incidents?status&env&limit&before.
func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", "active", "open", "mitigated", "resolved":
	default:
		writeErrCode(w, http.StatusBadRequest, "invalid", "status: open, mitigated, resolved или active")
		return
	}
	limit, ok := listLimit(r, 50)
	if !ok {
		writeErrCode(w, http.StatusBadRequest, "invalid", "limit: от 1 до 100")
		return
	}
	incs, next, err := s.St.ListIncidents(r.Context(), projectID, status, q.Get("env"), q.Get("before"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": incs, "next": nextCursor(next)})
}

// incidentProject — проект инцидента, если зритель его участник.
func (s *Server) incidentProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	projectID, err := s.St.IncidentProject(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return projectID, s.requireMember(w, r, projectID)
}

// GET /incidents/{id} — целиком, с доказательствами и историей.
func (s *Server) getIncident(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.incidentProject(w, r); !ok {
		return
	}
	in, err := s.St.GetIncident(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// POST /incidents/{id}/resolve {"comment"} — ручное закрытие владельцем.
func (s *Server) resolveIncident(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.incidentProject(w, r)
	if !ok || !s.requireOwner(w, r, projectID) {
		return
	}
	var in struct {
		Comment string `json:"comment"`
	}
	if err := decode(r, &in); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid", "невалидный JSON")
		return
	}
	err := s.St.ResolveIncident(r.Context(), r.PathValue("id"), in.Comment, user(r))
	var fe *store.FieldError
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "invalid", "message": fe.Msg, "field": fe.Field}, "field": fe.Field})
		return
	case errors.Is(err, store.ErrIncidentResolved):
		writeErrCode(w, http.StatusConflict, "incident_resolved", err.Error())
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	s.writeIncident(w, r, http.StatusOK)
}

// POST /incidents/{id}/bug — «создать баг заново» владельцем: снимает
// отметку отмены человеком и создаёт задачу-баг с готовым описанием.
func (s *Server) rebugIncident(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.incidentProject(w, r)
	if !ok || !s.requireOwner(w, r, projectID) {
		return
	}
	_, err := s.St.RebugIncident(r.Context(), r.PathValue("id"), user(r))
	switch {
	case errors.Is(err, store.ErrIncidentResolved):
		writeErrCode(w, http.StatusConflict, "incident_resolved", err.Error())
		return
	case errors.Is(err, store.ErrBugUnfinished):
		writeErrCode(w, http.StatusConflict, "bug_exists", err.Error())
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	s.writeIncident(w, r, http.StatusCreated)
}

func (s *Server) writeIncident(w http.ResponseWriter, r *http.Request, status int) {
	in, err := s.St.GetIncident(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, status, in)
}

// ─── наблюдение ─────────────────────────────────────────────────────────

// GET /environments/{id}/monitor — участники проекта окружения.
func (s *Server) getMonitor(w http.ResponseWriter, r *http.Request) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.St.GetMonitor(r.Context(), env)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// writeMonitorErr — ошибка настроек наблюдения с полем формы.
func writeMonitorErr(w http.ResponseWriter, err error) bool {
	var fe *domain.EnvFieldError
	if !errors.As(err, &fe) {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": map[string]any{"code": "invalid", "message": fe.Msg, "field": fe.Field}, "field": fe.Field})
	return true
}

// maintenanceLocked — Epic «Сопровождение» не декомпозируется и не
// архивируется, пока у проекта есть открытые инциденты.
func (s *Server) maintenanceLocked(w http.ResponseWriter, r *http.Request, epic domain.Epic) bool {
	if epic.Kind != domain.EpicMaintenance {
		return false
	}
	active, err := s.St.ActiveIncidents(r.Context(), epic.ProjectID)
	if err != nil {
		writeErr(w, err)
		return true
	}
	if active {
		writeErrCode(w, http.StatusConflict, "maintenance_locked", store.ErrMaintenanceLocked.Error())
		return true
	}
	return false
}
