package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/google/uuid"
)

func (s *Server) registerLoadTesting(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{id}/load-testing", s.projectLoadTesting)
	mux.HandleFunc("GET /api/v1/environments/{id}/load-testing", s.environmentLoadTesting)
	mux.HandleFunc("POST /api/v1/environments/{id}/load-testing/defer", s.deferLoadTesting)
	mux.HandleFunc("POST /api/v1/environments/{id}/load-tests", s.startLoadTesting)
	mux.HandleFunc("GET /api/v1/environments/{id}/load-tests", s.loadHistory)
	mux.HandleFunc("GET /api/v1/releases/{id}/load-tests", s.releaseLoadHistory)
	mux.HandleFunc("GET /api/v1/load-tests/{id}", s.getLoadTesting)
	mux.HandleFunc("POST /api/v1/load-tests/{id}/cancel", s.cancelLoadTesting)
}
func (s *Server) loadLimits() loadtest.Limits {
	if s.Engine != nil && s.Engine.LoadLimits.MaxVUs > 0 {
		return s.Engine.LoadLimits
	}
	return loadtest.DefaultLimits()
}
func loadAPIError(w http.ResponseWriter, err error) {
	var field *loadtest.FieldError
	var conflict *store.LoadError
	switch {
	case errors.As(err, &field):
		writeJSON(w, 422, map[string]any{"error": map[string]string{"code": "invalid", "message": field.Message, "field": field.Field}, "field": field.Field})
	case errors.As(err, &conflict):
		writeErrCode(w, 409, conflict.Code, conflict.Message)
	case errors.Is(err, secretbox.ErrNoKey):
		writeErrCode(w, 503, "no_secret_key", "ключ шифрования тестового секрета не настроен")
	default:
		writeErr(w, err)
	}
}
func (s *Server) loadView(r *http.Request, env domain.Environment) (store.LoadView, error) {
	v, err := s.St.LoadTestingView(r.Context(), env, s.loadLimits())
	if err != nil {
		return v, err
	}
	role, err := s.St.MemberRole(r.Context(), env.ProjectID, currentUser(r).ID)
	v.CanManage = role == domain.RoleOwner
	return v, err
}
func (s *Server) projectLoadTesting(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	if !s.requireMember(w, r, project) {
		return
	}
	envs, err := s.St.ListEnvironments(r.Context(), project)
	if err != nil {
		writeErr(w, err)
		return
	}
	views := []store.LoadView{}
	for _, env := range envs {
		v, err := s.loadView(r, env)
		if err != nil {
			writeErr(w, err)
			return
		}
		views = append(views, v)
	}
	writeJSON(w, 200, map[string]any{"environments": views})
}
func (s *Server) loadEnv(w http.ResponseWriter, r *http.Request, manage bool) (domain.Environment, bool) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return env, false
	}
	if manage && !s.requireOwner(w, r, env.ProjectID) {
		return env, false
	}
	return env, true
}
func (s *Server) environmentLoadTesting(w http.ResponseWriter, r *http.Request) {
	env, ok := s.loadEnv(w, r, false)
	if !ok {
		return
	}
	v, err := s.loadView(r, env)
	if err != nil {
		loadAPIError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) deferLoadTesting(w http.ResponseWriter, r *http.Request) {
	env, ok := s.loadEnv(w, r, true)
	if !ok {
		return
	}
	if err := s.St.DeferLoadPrompt(r.Context(), env.ID); err != nil {
		loadAPIError(w, err)
		return
	}
	s.environmentLoadTesting(w, r)
}
func (s *Server) startLoadTesting(w http.ResponseWriter, r *http.Request) {
	env, ok := s.loadEnv(w, r, true)
	if !ok {
		return
	}
	var in store.LoadStart
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErrCode(w, 422, "invalid", "невалидный профиль JSON")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErrCode(w, 422, "invalid", "ожидается один JSON-объект")
		return
	}
	if in.Profile.Auth != nil && !s.Secrets.Enabled() {
		loadAPIError(w, secretbox.ErrNoKey)
		return
	}
	run, err := s.St.StartLoadTest(r.Context(), env.ID, in, s.loadLimits())
	if err != nil {
		loadAPIError(w, err)
		return
	}
	writeJSON(w, 202, run)
}
func (s *Server) getLoadTesting(w http.ResponseWriter, r *http.Request) {
	run, err := s.St.LoadRunForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, run)
}
func (s *Server) cancelLoadTesting(w http.ResponseWriter, r *http.Request) {
	run, err := s.St.LoadRunForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireOwner(w, r, run.ProjectID) {
		return
	}
	run, err = s.St.CancelLoadTest(r.Context(), run.ID)
	if err != nil {
		loadAPIError(w, err)
		return
	}
	if s.Engine != nil {
		_ = s.Engine.FlushLoadCancels(r.Context())
	}
	writeJSON(w, 200, run)
}
func (s *Server) loadHistory(w http.ResponseWriter, r *http.Request) {
	env, ok := s.loadEnv(w, r, false)
	if !ok {
		return
	}
	limit, ok := listLimit(r, 20)
	if !ok {
		writeErrCode(w, 400, "invalid", "limit: 1–100")
		return
	}
	before := r.URL.Query().Get("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			writeErrCode(w, 400, "invalid", "before: ожидается UUID курсора")
			return
		}
	}
	runs, err := s.St.LoadHistory(r.Context(), env.ID, "", before, limit+1)
	if err != nil {
		writeErr(w, err)
		return
	}
	var next *string
	if len(runs) > limit {
		runs = runs[:limit]
		id := runs[len(runs)-1].ID
		next = &id
	}
	writeJSON(w, 200, map[string]any{"runs": runs, "next": next})
}
func (s *Server) releaseLoadHistory(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.releaseForViewer(w, r)
	if !ok {
		return
	}
	runs, err := s.St.LoadHistory(r.Context(), "", rel.ID, "", 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
}
