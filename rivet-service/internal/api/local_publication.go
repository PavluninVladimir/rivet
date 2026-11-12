package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (s *Server) localEnv(w http.ResponseWriter, r *http.Request) (domain.Environment, bool) {
	e, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return e, false
	}
	if e.ExecType != domain.ExecLocalDocker {
		writeErr(w, store.ErrNotFound)
		return e, false
	}
	return e, true
}
func localError(w http.ResponseWriter, err error) {
	for _, v := range []struct {
		err       error
		code, msg string
	}{
		{store.ErrPublicationBusy, "publication_in_progress", "публикация уже идёт"}, {store.ErrEnvironmentPaused, "environment_paused", "автопубликации на паузе, сначала возобновите окружение"}, {store.ErrNoDockerRunner, "no_runner", "нет доступного runner-а с возможностью Docker"}, {store.ErrNoPublication, "no_publication", "ещё нет успешной публикации"}, {store.ErrPublicationStopped, "publication_stopped", "публикация остановлена, опубликуйте её заново"},
	} {
		if errors.Is(err, v.err) {
			writeErrCode(w, http.StatusConflict, v.code, v.msg)
			return
		}
	}
	if errors.Is(err, store.ErrPublicationVersion) {
		writeErrCode(w, 422, "invalid", "version: нет в истории публикаций окружения")
		return
	}
	if writeMonitorErr(w, err) {
		return
	}
	writeErr(w, err)
}
func (s *Server) localPublish(w http.ResponseWriter, r *http.Request) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	s.publishLocal(w, r, env)
}
func (s *Server) publishLocal(w http.ResponseWriter, r *http.Request, env domain.Environment) {
	if env.Paused {
		localError(w, store.ErrEnvironmentPaused)
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
	sha, err := adapter.HeadSHA(r.Context(), p.RepoPath, p.DefaultBranch)
	if err != nil {
		writeErr(w, err)
		return
	}
	pub, err := s.St.EnqueueLocalPublication(r.Context(), env.ID, sha, user(r), false)
	if err != nil {
		localError(w, err)
		return
	}
	writeJSON(w, 202, pub)
}
func (s *Server) localRedeploy(w http.ResponseWriter, r *http.Request) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeErrCode(w, 422, "invalid", "ожидается версия публикации")
		return
	}
	var err error
	if in.Version == "" {
		in.Version, err = s.St.LastLocalVersion(r.Context(), env.ID)
	} else {
		var known bool
		known, err = s.St.LocalVersionInHistory(r.Context(), env.ID, in.Version)
		if err == nil && !known {
			err = store.ErrPublicationVersion
		}
	}
	if err != nil {
		localError(w, err)
		return
	}
	pub, err := s.St.EnqueueLocalPublication(r.Context(), env.ID, in.Version, user(r), false)
	if err != nil {
		localError(w, err)
		return
	}
	writeJSON(w, 202, pub)
}
func (s *Server) localRestart(w http.ResponseWriter, r *http.Request) {
	s.localOperation(w, r, "restart")
}
func (s *Server) localStop(w http.ResponseWriter, r *http.Request) { s.localOperation(w, r, "stop") }
func (s *Server) localOperation(w http.ResponseWriter, r *http.Request, action string) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	pub, err := s.St.QueueLocalOperation(r.Context(), env.ID, action, user(r))
	if err != nil {
		localError(w, err)
		return
	}
	status := 202
	if action == "stop" {
		status = 200
	}
	writeJSON(w, status, pub)
}
func (s *Server) localPublication(w http.ResponseWriter, r *http.Request) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	pub, err := s.St.CurrentLocalPublication(r.Context(), env.ID)
	if err != nil {
		localError(w, err)
		return
	}
	writeJSON(w, 200, pub)
}
func (s *Server) localLogs(w http.ResponseWriter, r *http.Request) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "builder"
	}
	if stream != "builder" && stream != "container" {
		writeErrCode(w, 422, "invalid", "stream: ожидается builder или container")
		return
	}
	n := 200
	if raw := r.URL.Query().Get("tail"); raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil || n < 1 || n > 5000 {
			writeErrCode(w, 422, "invalid", "tail: ожидается 1–5000")
			return
		}
	}
	logs, err := s.St.LocalLogs(r.Context(), env.ID, stream)
	if err != nil {
		localError(w, err)
		return
	}
	lines := strings.Split(strings.TrimSuffix(logs, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
}
