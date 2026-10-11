package api

import (
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Каталог агентов (add-agent-profiles, api-contract): /system/agents/*
// администратору, /agents участникам без шаблона окружения.

type agentView struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Adapter      string                 `json:"adapter"`
	Command      string                 `json:"command"`
	Capabilities []string               `json:"capabilities"`
	Models       []domain.AgentModelRef `json:"models"`
	DefaultModel *domain.AgentModelRef  `json:"default_model"`
	Env          []domain.EnvVar        `json:"env,omitempty"`
	Args         []string               `json:"args,omitempty"`
	Secrets      string                 `json:"secrets"`
	Enabled      bool                   `json:"enabled"`
	Runners      int                    `json:"runners"`
	Preset       bool                   `json:"preset"`
	UpdatedAt    time.Time              `json:"updated_at"`
	UpdatedBy    string                 `json:"updated_by"`
}

func agentDTO(a domain.AgentProfile, full bool) agentView {
	v := agentView{
		ID: a.ID, Name: a.Name, Adapter: a.Adapter, Command: a.Command, Capabilities: a.Capabilities,
		Models: a.Models, DefaultModel: a.DefaultModel, Secrets: a.Secrets, Enabled: a.Enabled,
		Runners: a.Runners, Preset: a.Preset, UpdatedAt: a.UpdatedAt, UpdatedBy: a.UpdatedBy,
	}
	if full {
		v.Env, v.Args = a.Env, a.Args
		if v.Env == nil {
			v.Env = []domain.EnvVar{}
		}
		if v.Args == nil {
			v.Args = []string{}
		}
	}
	return v
}

func (s *Server) agentsPayload(r *http.Request, full bool) (map[string]any, error) {
	list, err := s.St.ListAgents(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]agentView, 0, len(list))
	for _, a := range list {
		out = append(out, agentDTO(a, full))
	}
	ext, err := s.St.ExternalAgents(r.Context())
	if err != nil {
		return nil, err
	}
	return map[string]any{"agents": out, "external": ext}, nil
}

// listAgents — каталог администратору со всеми полями.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out, err := s.agentsPayload(r, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listAgentsForMembers — каталог без шаблона окружения (окно шага процесса).
func (s *Server) listAgentsForMembers(w http.ResponseWriter, r *http.Request) {
	out, err := s.agentsPayload(r, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name         string                 `json:"name"`
		Adapter      string                 `json:"adapter"`
		Command      string                 `json:"command"`
		Capabilities []string               `json:"capabilities"`
		Models       []domain.AgentModelRef `json:"models"`
		DefaultModel *domain.AgentModelRef  `json:"default_model"`
		Env          []domain.EnvVar        `json:"env"`
		Args         []string               `json:"args"`
		Secrets      string                 `json:"secrets"`
		Enabled      *bool                  `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	u := currentUser(r)
	a, created, err := s.St.UpsertAgent(r.Context(), store.AgentInput{
		ID: r.PathValue("id"), Name: in.Name, Adapter: in.Adapter, Command: in.Command, Capabilities: in.Capabilities,
		Models: in.Models, DefaultModel: in.DefaultModel, Env: in.Env, Args: in.Args, Secrets: in.Secrets, Enabled: in.Enabled,
	}, u.ID, u.Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, agentDTO(a, true))
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.DeleteAgent(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		s.writeConnErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
