package orchestrator

import (
	"context"
	"errors"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (e *Engine) markPinnedDeveloperWaiting(ctx context.Context, seen map[string]bool, excluded, excludedEpics []string, bad map[string]string) error {
	waits, err := e.St.WaitingPinnedDevelopers(ctx, excluded, excludedEpics)
	if err != nil {
		return err
	}
	for _, w := range waits {
		if seen[w.TaskID] {
			continue
		}
		seen[w.TaskID] = true
		why := "нужен подходящий свободный runner"
		ag, err := e.St.GetTeamAgent(ctx, w.AgentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		role, roleErr := e.St.GetRole(ctx, domain.RoleDeveloper)
		if roleErr != nil && !errors.Is(roleErr, store.ErrNotFound) {
			return roleErr
		}
		profile := ag.Agent
		if profile == "" {
			profile = role.Agent
		}
		switch {
		case err != nil:
			why = "агент недоступен"
		case !ag.Enabled:
			why = "агент выключен"
		case ag.RoleID != domain.RoleDeveloper:
			why = "агент больше не имеет роли разработчика"
		case roleErr != nil || !role.Enabled:
			why = "роль разработчика выключена"
		case w.Role != domain.RoleDeveloper:
			why = "code-шаг не использует роль developer"
		case w.Model != "":
			why = "явная модель code-шага несовместима с закреплением конфигурации агента"
		case w.AgentKind != "" && profile != "" && w.AgentKind != profile:
			why = "профиль code-шага несовместим с агентом"
		case bad[w.AgentID] != "":
			why = bad[w.AgentID]
		case ag.TaskID != "":
			why = "агент занят другой работой"
		}
		reason := "ожидает закреплённого разработчика «" + w.AgentName + "»: " + why
		if reason == w.CurrentReason {
			continue
		}
		if err := e.St.SetTaskTeamWait(ctx, w.TaskID, reason); err != nil {
			return err
		}
		if _, err := e.St.AppendEvent(ctx, store.EventInput{ActorKind: domain.ActorScheduler,
			Type: "task.wait", ProjectID: w.ProjectID, EpicID: w.EpicID, TaskID: w.TaskID,
			Text: "ожидание: " + reason, Payload: map[string]any{"step": w.StepID, "agent_id": w.AgentID, "agent_name": w.AgentName, "reason": reason}}); err != nil {
			return err
		}
	}
	return nil
}
