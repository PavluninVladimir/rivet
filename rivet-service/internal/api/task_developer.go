package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) setTaskDeveloper(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	projectID, err := s.St.TaskProjectForViewer(r.Context(), id, currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireOwner(w, r, projectID) {
		return
	}
	var in map[string]json.RawMessage
	if err := decode(r, &in); err != nil || len(in) != 1 || in["agent_id"] == nil {
		fieldInvalid(w, "agent_id", "нужен agent_id разработчика или null для автоматического выбора")
		return
	}
	var selected *string
	if err := json.Unmarshal(in["agent_id"], &selected); err != nil || (selected != nil && strings.TrimSpace(*selected) == "") {
		fieldInvalid(w, "agent_id", "нужен agent_id разработчика или null")
		return
	}
	idAgent := ""
	if selected != nil {
		idAgent = *selected
	}
	if err := s.St.SetTaskDeveloper(r.Context(), id, idAgent, user(r)); err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.St.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
