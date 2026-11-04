package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Роли и команда агентов (add-agent-roles, спека backend/agent-roles): роль
// участника разрешается при входе на шаг по шаблону и политике проекта,
// стадию ведёт свободный агент команды роли (его выбирает планировщик
// вместе с runner'ом). Отключённый или пропавший шаблон — участник без
// роли; роль без агентов в команде — участник по шаблону через capability.

// roleBindings — привязки ролей участников шага по их идентификаторам.
func (e *Engine) roleBindings(ctx context.Context, task domain.Task, step policy.ResolvedStep) map[string]store.RoleBinding {
	out := map[string]store.RoleBinding{}
	var overrides map[string]policy.RoleModel
	loaded := false
	for _, p := range step.Participants {
		if p.IsUser() || p.Agent.Role == "" {
			continue
		}
		if !loaded {
			overrides = e.projectRoleModels(ctx, task)
			loaded = true
		}
		r, err := e.St.GetRole(ctx, p.Agent.Role)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("шаблон роли участника", "task", task.ID, "role", p.Agent.Role, "err", err)
		}
		team, terr := e.St.RoleHasTeam(ctx, p.Agent.Role)
		if terr != nil {
			slog.Error("команда роли", "role", p.Agent.Role, "err", terr)
		}
		var ov *policy.RoleModel
		if m, ok := overrides[p.Agent.Role]; ok {
			ov = &m
		}
		out[p.ID] = store.ResolveRole(r, err == nil, ov, team)
	}
	return out
}

// projectRoleModels — переопределения моделей ролей в политике проекта.
func (e *Engine) projectRoleModels(ctx context.Context, task domain.Task) map[string]policy.RoleModel {
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return nil
	}
	eff, err := e.St.EffectivePolicy(ctx, p.ID)
	if err != nil {
		slog.Error("политика проекта для ролей", "project", p.ID, "err", err)
		return nil
	}
	return eff.Presets.RoleModels
}

// performer — исполнитель запуска: шаблон роли (включённый) и агент
// команды, если запуск его получил.
type performer struct {
	role  *domain.AgentRole
	agent *domain.TeamAgent
}

// runPerformer — шаблон и агент запуска на момент назначения.
func (e *Engine) runPerformer(ctx context.Context, run store.StepRun) performer {
	var p performer
	if run.Role == "" {
		return p
	}
	r, err := e.St.GetRole(ctx, run.Role)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("шаблон роли запуска", "run", run.ID, "role", run.Role, "err", err)
		}
		return p
	}
	if !r.Enabled {
		return p
	}
	p.role = &r
	if run.AgentID != "" {
		a, err := e.St.GetTeamAgent(ctx, run.AgentID)
		if err == nil {
			p.agent = &a
		} else if !errors.Is(err, store.ErrNotFound) {
			slog.Error("агент запуска", "run", run.ID, "agent", run.AgentID, "err", err)
		}
	}
	return p
}

// instruction — инструкция шаблона и дополнение агента.
func (p performer) instruction() string {
	inst := strings.TrimSpace(p.role.Instruction)
	if p.agent != nil && strings.TrimSpace(p.agent.InstructionExtra) != "" {
		inst += "\n" + strings.TrimSpace(p.agent.InstructionExtra)
	}
	return inst
}

// assignmentRole — роль в назначении: шаблон, имя агента, инструкция.
func assignmentRole(p performer) *pb.AgentRole {
	if p.role == nil {
		return nil
	}
	ar := &pb.AgentRole{Id: p.role.ID, Title: p.role.Title, Instruction: p.instruction()}
	if p.agent != nil {
		ar.Name = p.agent.Name
	}
	return ar
}

// sessionPerformer — исполнитель сессии для событий её шагов.
type sessionPerformer struct {
	role       map[string]any
	agent      map[string]any
	runnerName string
}

// rememberPerformer запоминает исполнителя открытой сессии.
func (e *Engine) rememberPerformer(sessionID string, p performer, runner domain.Runner) {
	sp := sessionPerformer{runnerName: runner.Label()}
	if p.role != nil {
		sp.role = map[string]any{"id": p.role.ID, "title": p.role.Title}
	}
	if p.agent != nil {
		sp.agent = map[string]any{"id": p.agent.ID, "name": p.agent.Name}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.performers == nil {
		e.performers = map[string]sessionPerformer{}
	}
	e.performers[sessionID] = sp
}

// PerformerPayload дополняет payload события шага сессии агентом, ролью и
// именем runner'а (спека agent-roles «Агент-исполнитель в событиях и API»).
// После рестарта rivetd исполнитель берётся из запуска сессии.
func (e *Engine) PerformerPayload(ctx context.Context, sessionID string, payload map[string]any) map[string]any {
	e.mu.Lock()
	sp, ok := e.performers[sessionID]
	e.mu.Unlock()
	if !ok {
		run, err := e.St.RunBySession(ctx, sessionID)
		if err != nil {
			return payload
		}
		var runner domain.Runner
		if run.RunnerID != "" {
			if rr, err := e.St.GetRunner(ctx, run.RunnerID); err == nil {
				runner = rr
			}
		}
		e.rememberPerformer(sessionID, e.runPerformer(ctx, run), runner)
		e.mu.Lock()
		sp = e.performers[sessionID]
		e.mu.Unlock()
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if sp.runnerName != "" {
		payload["runner_name"] = sp.runnerName
	}
	if sp.role != nil {
		payload["role"] = sp.role
	}
	if sp.agent != nil {
		payload["agent"] = sp.agent
	}
	return payload
}

// roleFits — runner подходит роли участника: capability шаблона и профиль
// шаблона, если у участника тип не указан.
func roleFits(r domain.Runner, p policy.ResolvedParticipant, rb store.RoleBinding) bool {
	if rb.Role == "" {
		return true
	}
	if rb.Capability != "" && !contains(r.Capabilities, rb.Capability) {
		return false
	}
	if p.Agent.Kind == "" && rb.Agent != "" && r.Agent != rb.Agent {
		return false
	}
	return true
}

// roleTitles — названия шаблонов для подписей в событиях.
func (e *Engine) roleTitles(ctx context.Context) map[string]string {
	roles, err := e.St.ListRoles(ctx)
	if err != nil {
		slog.Error("шаблоны ролей", "err", err)
		return map[string]string{}
	}
	out := make(map[string]string, len(roles))
	for _, r := range roles {
		out[r.ID] = r.Title
	}
	return out
}

// runnerLabels — отображаемые имена runner'ов по идентификаторам.
func (e *Engine) runnerLabels(ctx context.Context) map[string]string {
	list, err := e.St.ListRunners(ctx)
	if err != nil {
		slog.Error("runner'ы для подписей", "err", err)
		return map[string]string{}
	}
	out := make(map[string]string, len(list))
	for _, r := range list {
		out[r.ID] = r.Label()
	}
	return out
}

// ReleaseRunnerRuns — runner перезапущен: его открытые запуски
// отвязываются от runner'а и агента команды, сессии закрыты, память о них
// забывается. Запуски назначит планировщик заново.
func (e *Engine) ReleaseRunnerRuns(ctx context.Context, runnerID string, keepSessions []string) error {
	runs, err := e.St.ReleaseRunnerRuns(ctx, runnerID, keepSessions)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.SessionID != "" {
			e.dropSessionID(r.SessionID)
		}
	}
	return nil
}
