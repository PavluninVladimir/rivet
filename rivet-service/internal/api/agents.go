package api

import (
	"encoding/json"
	"net/http"
	"strings"
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
	Limits       domain.AgentLimits     `json:"limits"`
}

func agentDTO(a domain.AgentProfile, full bool) agentView {
	v := agentView{
		ID: a.ID, Name: a.Name, Adapter: a.Adapter, Command: a.Command, Capabilities: a.Capabilities,
		Models: a.Models, DefaultModel: a.DefaultModel, Secrets: a.Secrets, Enabled: a.Enabled,
		Runners: a.Runners, Preset: a.Preset, UpdatedAt: a.UpdatedAt, UpdatedBy: a.UpdatedBy, Limits: a.Limits,
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
	// Агентов вне каталога нет (add-native-agent): runner'ы со сторонним
	// агентом не регистрируются, а runner выкатки агентом не является.
	// Поле оставлено пустым для совместимости со старыми клиентами.
	return map[string]any{"agents": out, "external": []struct{}{}}, nil
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
	// Каталог — один профиль агента Rivet (add-native-agent, design решение
	// 10): правятся лимиты, режим секретов, модель по умолчанию и признак
	// «включено». Других агентов нет, создать их нельзя.
	if r.PathValue("id") != store.AgentRivet {
		fieldErr(w, "agent_unsupported", "id", "агентом может быть только агент Rivet")
		return
	}
	// Непереданное поле сохраняет прежнее значение, переданное заменяет
	// его: "default_model": null — автоматический выбор, "limits": {} или
	// null — умолчания цикла, "name": null или "" — «Агент Rivet». Режим
	// секретов и enabled без значения (null, "") тоже сохраняются: поле
	// безопасности не сбрасывается молча (api-contract add-native-agent).
	var in struct {
		Name         json.RawMessage `json:"name"`
		DefaultModel json.RawMessage `json:"default_model"`
		Secrets      *string         `json:"secrets"`
		Enabled      *bool           `json:"enabled"`
		Limits       json.RawMessage `json:"limits"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	ain := store.AgentInput{ID: store.AgentRivet, Name: "Агент Rivet", Adapter: store.AdapterNative, Enabled: in.Enabled,
		KeepName: len(in.Name) == 0, KeepDefaultModel: len(in.DefaultModel) == 0, KeepLimits: len(in.Limits) == 0}
	if in.Secrets != nil {
		ain.Secrets = *in.Secrets
	}
	var name *string
	if err := json.Unmarshal(orNull(in.Name), &name); err != nil {
		fieldInvalid(w, "name", "name: ожидается строка или null")
		return
	}
	if name != nil && strings.TrimSpace(*name) != "" {
		ain.Name = *name
	}
	if err := json.Unmarshal(orNull(in.DefaultModel), &ain.DefaultModel); err != nil {
		fieldInvalid(w, "default_model", "default_model: ожидается {connection_id, model} или null")
		return
	}
	if err := json.Unmarshal(orNull(in.Limits), &ain.Limits); err != nil {
		fieldInvalid(w, "limits", "limits: ожидается объект {max_turns, max_tokens, max_minutes} или null")
		return
	}
	u := currentUser(r)
	a, created, err := s.St.UpsertAgent(r.Context(), ain, u.ID, u.Login)
	if err != nil {
		s.writeConnErr(w, err, "профиль агента")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, agentDTO(a, true))
}

// orNull — непереданное поле как JSON null (для разбора значения).
func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	// Агент Rivet — предустановленный и единственный: удалить нельзя.
	if r.PathValue("id") == store.AgentRivet {
		fieldErr(w, "agent_preset", "id", "агент Rivet предустановлен, его можно только выключить")
		return
	}
	if err := s.St.DeleteAgent(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		s.writeConnErr(w, err, "профиль агента")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
