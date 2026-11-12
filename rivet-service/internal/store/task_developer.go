package store

import (
	"context"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
)

const taskDeveloperEditableSQL = `t.status IN ('queued','ready') AND t.attempt_used=0 AND t.started_at IS NULL AND t.runner_id IS NULL
	AND EXISTS(SELECT 1 FROM epics ed WHERE ed.id=t.epic_id AND ed.status IN ('planned','running','paused'))
	AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.task_id=t.id)
	AND NOT EXISTS(SELECT 1 FROM task_step_runs sr WHERE sr.task_id=t.id AND
		(sr.runner_id IS NOT NULL OR sr.session_id IS NOT NULL OR sr.verdict IS NOT NULL))`

func (s *Store) TaskDeveloperEditable(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT `+taskDeveloperEditableSQL+` FROM tasks t WHERE t.id=$1`, id).Scan(&ok)
	return ok, nf(err)
}

// SetTaskDeveloper uses the same task row lock as first assignment. Pending,
// unassigned step runs are permitted; execution history is never reset.
func (s *Store) SetTaskDeveloper(ctx context.Context, id, agentID, actor string) error {
	if agentID != "" && strings.TrimSpace(agentID) != agentID {
		return &FieldError{Field: "agent_id", Msg: "нужен идентификатор разработчика"}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var editable bool
		var before, projectID, epicID string
		if err := tx.QueryRow(ctx, `SELECT `+taskDeveloperEditableSQL+`,COALESCE(t.developer_agent_id,''),e.project_id,t.epic_id
			FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, id).
			Scan(&editable, &before, &projectID, &epicID); err != nil {
			return nf(err)
		}
		if !editable {
			return ErrConflict
		}
		if before == agentID {
			return nil
		}
		name := ""
		if agentID != "" {
			var role string
			if err := tx.QueryRow(ctx, `SELECT name,role_id FROM team_agents WHERE id=$1 FOR SHARE`, agentID).Scan(&name, &role); err != nil {
				return nf(err)
			}
			if role != domain.RoleDeveloper {
				return &FieldError{Field: "agent_id", Msg: "можно закрепить только агента роли developer"}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET developer_agent_id=NULLIF($2,''),updated_at=now(),
			wait_reason=CASE WHEN wait_kind=$3 THEN '' ELSE wait_reason END,
			wait_kind=CASE WHEN wait_kind=$3 THEN '' ELSE wait_kind END WHERE id=$1`, id, agentID, WaitKindTeam); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actor,
			Type: "task.developer_changed", ProjectID: projectID, EpicID: epicID, TaskID: id,
			Text: "изменён выбор разработчика задачи", Payload: map[string]any{"before": before, "agent_id": agentID, "agent_name": name}})
		return err
	})
}

type PinnedDeveloperWait struct {
	TaskID, ProjectID, EpicID, AgentID, AgentName string
	StepID, Role, Model, AgentKind, CurrentReason string
}

func (s *Store) WaitingPinnedDevelopers(ctx context.Context, excludedProjects, excludedEpics []string) ([]PinnedDeveloperWait, error) {
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	if excludedEpics == nil {
		excludedEpics = []string{}
	}
	rows, err := s.Pool.Query(ctx, `SELECT t.id,e.project_id,e.id,t.developer_agent_id,COALESCE(ag.name,t.developer_agent_id),
		r.step_id,r.role,r.model,r.agent_kind,t.wait_reason
		FROM task_step_runs r JOIN tasks t ON t.id=r.task_id JOIN epics e ON e.id=t.epic_id
		LEFT JOIN team_agents ag ON ag.id=t.developer_agent_id
		WHERE e.status='running' AND e.project_id<>ALL($1::uuid[]) AND e.id<>ALL($2::uuid[])
		AND t.status IN ('ready','running','fixing') AND t.developer_agent_id IS NOT NULL
		AND r.step_kind='code' AND r.runner_id IS NULL AND r.verdict IS NULL AND `+agentRunsOnly+`
		AND (t.wait_kind='' OR t.wait_kind=$3) ORDER BY r.id`, excludedProjects, excludedEpics, WaitKindTeam)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PinnedDeveloperWait{}
	for rows.Next() {
		var w PinnedDeveloperWait
		if err := rows.Scan(&w.TaskID, &w.ProjectID, &w.EpicID, &w.AgentID, &w.AgentName, &w.StepID, &w.Role, &w.Model, &w.AgentKind, &w.CurrentReason); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
