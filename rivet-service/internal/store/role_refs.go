package store

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Ссылки шаблонов ролей, команды и переопределений моделей ролей на
// профили агентов и подключения (add-agent-roles): удалить, выключить
// профиль или подключение, отвязать или скрыть модель, на которую они
// ссылаются, нельзя — иначе агент команды или роль молча перестанут
// назначаться.

type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// modelUse — что в команде и шаблонах использует модель или профиль.
type modelUse struct {
	Kind         string // team_agent | role | role_model
	ID           string // агент, шаблон или «проект:роль»
	Agent        string
	ConnectionID string
	Model        string
}

// modelUses — все использования троек модели и профилей: агенты команды,
// модели и профили шаблонов, переопределения ролей в действующих политиках
// проектов.
func modelUses(ctx context.Context, q rowQuerier) ([]modelUse, error) {
	var out []modelUse
	// Агент без своего профиля работает на профиле шаблона.
	rows, err := q.Query(ctx, `SELECT a.id, CASE WHEN a.agent = '' THEN r.agent ELSE a.agent END, a.connection_id, a.model
		FROM team_agents a JOIN agent_roles r ON r.id = a.role_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u modelUse
		if err := rows.Scan(&u.ID, &u.Agent, &u.ConnectionID, &u.Model); err != nil {
			rows.Close()
			return nil, err
		}
		u.Kind = "team_agent"
		out = append(out, u)
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT id, agent, COALESCE(default_model->>'connection_id', ''), COALESCE(default_model->>'model', '') FROM agent_roles`)
	if err != nil {
		return nil, err
	}
	roleAgent := map[string]string{}
	for rows.Next() {
		var u modelUse
		if err := rows.Scan(&u.ID, &u.Agent, &u.ConnectionID, &u.Model); err != nil {
			rows.Close()
			return nil, err
		}
		u.Kind = "role"
		roleAgent[u.ID] = u.Agent
		out = append(out, u)
	}
	rows.Close()
	rows, err = q.Query(ctx, `
		SELECT COALESCE(project_id::text, ''), content FROM (
			SELECT DISTINCT ON (scope, project_id) scope, project_id, content FROM policy_versions
			ORDER BY scope, project_id, version DESC) v`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pid string
		var raw []byte
		if err := rows.Scan(&pid, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var doc struct {
			RoleModels map[string]policy.RoleModel `json:"role_models"`
		}
		_ = json.Unmarshal(raw, &doc)
		for role, m := range doc.RoleModels {
			// Переопределение без профиля работает на профиле шаблона.
			agent := m.Agent
			if agent == "" {
				agent = roleAgent[role]
			}
			out = append(out, modelUse{Kind: "role_model", ID: pid + ":" + role, Agent: agent, ConnectionID: m.ConnectionID, Model: m.Model})
		}
	}
	rows.Close()
	return out, rows.Err()
}

// usesRefs — ссылки по отобранным использованиям, в стабильном порядке.
func usesRefs(uses []modelUse, match func(modelUse) bool) []Ref {
	var refs []Ref
	for _, u := range uses {
		if match(u) {
			refs = append(refs, Ref{Kind: u.Kind, ID: u.ID})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].ID < refs[j].ID
	})
	return refs
}

// connectionUseRefs — использования подключения; models — только эти модели
// (nil — любая).
func connectionUseRefs(ctx context.Context, q rowQuerier, connID string, models map[string]bool) ([]Ref, error) {
	uses, err := modelUses(ctx, q)
	if err != nil {
		return nil, err
	}
	return usesRefs(uses, func(u modelUse) bool {
		return u.ConnectionID == connID && u.Model != "" && (models == nil || models[u.Model])
	}), nil
}

// agentUseRefs — использования профиля агента; keep — привязки, которые
// остаются у профиля (nil — профиль удаляется или выключается целиком:
// важна любая ссылка на него).
func agentUseRefs(ctx context.Context, q rowQuerier, agentID string, keep map[string]bool) ([]Ref, error) {
	uses, err := modelUses(ctx, q)
	if err != nil {
		return nil, err
	}
	return usesRefs(uses, func(u modelUse) bool {
		if u.Agent != agentID {
			return false
		}
		if keep == nil {
			return true
		}
		return u.Model != "" && !keep[u.ConnectionID+"/"+u.Model]
	}), nil
}
