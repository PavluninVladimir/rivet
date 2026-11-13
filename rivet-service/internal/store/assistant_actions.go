package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/assistant"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/jackc/pgx/v5"
)

const assistantActionCols = `id::text,run_id::text,type,target_id::text,expected_revision,status,guard,failure_episode_id,evidence_ids,operation_id::text,result,created_at,finished_at`

func scanAssistantAction(row pgx.Row) (assistant.Action, error) {
	a := assistant.Action{Args: map[string]any{}}
	var g, refs, result []byte
	err := row.Scan(&a.ID, &a.RunID, &a.Type, &a.TargetID, &a.ExpectedRevision, &a.Status, &g, &a.FailureEpisodeID, &refs, &a.OperationID, &result, &a.CreatedAt, &a.FinishedAt)
	if err != nil {
		return a, nf(err)
	}
	if err = json.Unmarshal(g, &a.Guard); err != nil {
		return a, err
	}
	if err = json.Unmarshal(refs, &a.EvidenceIDs); err != nil {
		return a, err
	}
	if len(result) > 0 {
		err = json.Unmarshal(result, &a.Result)
	}
	return a, err
}
func (s *Store) assistantActions(ctx context.Context, run string) ([]assistant.Action, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+assistantActionCols+` FROM assistant_actions WHERE run_id=$1 ORDER BY created_at,id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []assistant.Action{}
	for rows.Next() {
		a, e := scanAssistantAction(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) ProposeAssistantAction(ctx context.Context, j AssistantJob, i assistant.Intent) (assistant.Action, error) {
	if !assistant.Registered(i.Type) || !assistant.ValidID(i.TargetID) {
		return assistant.Action{}, ErrInvalid
	}
	kind := map[string]string{"task.retry": "task", "epic.resume": "epic", "environment.resume": "environment"}[i.Type]
	revision := ""
	for _, r := range j.Snapshot.Refs {
		if r.Kind == kind && r.ResourceID == i.TargetID && r.Revision != nil {
			revision = *r.Revision
		}
		if kind == "environment" && r.ID == "environment:"+i.TargetID && r.Revision != nil {
			revision = *r.Revision
		}
	}
	if revision == "" {
		return assistant.Action{}, ErrNotFound
	}
	refs, _ := json.Marshal(i.EvidenceIDs)
	a, err := scanAssistantAction(s.Pool.QueryRow(ctx, `INSERT INTO assistant_actions(run_id,project_id,type,target_id,expected_revision,evidence_ids) SELECT id,project_id,$2,$3,$4,$5 FROM assistant_runs WHERE id=$1 AND status='running' AND trigger IN('recover','event') AND lease_epoch=$6 ON CONFLICT(run_id,type,target_id) DO UPDATE SET run_id=EXCLUDED.run_id RETURNING `+assistantActionCols, j.Run.ID, i.Type, i.TargetID, revision, refs, j.Epoch))
	return a, err
}

// AssistantPolicy is evaluated inside the SAME transaction as the domain
// mutation. No callback is supplied by the model or by an HTTP request body.
type AssistantPolicy func(context.Context, string, string, domain.User) (assistant.Guard, error)
type assistantMutationKey struct{}
type assistantMutation struct {
	Job    AssistantJob
	Action assistant.Action
	Policy AssistantPolicy
}

func WithAssistantMutation(ctx context.Context, j AssistantJob, a assistant.Action, p AssistantPolicy) context.Context {
	return context.WithValue(ctx, assistantMutationKey{}, assistantMutation{j, a, p})
}

// Assistant mutation entry runs before a domain row lock, using the same user /
// project lock order as membership revocation. Duplicate committed effects are
// recognized by their durable operation_id before checking resource revisions.
func assistantMutationBeginTx(ctx context.Context, tx pgx.Tx, kind, target string) (bool, error) {
	m, ok := ctx.Value(assistantMutationKey{}).(assistantMutation)
	if !ok {
		return false, nil
	}
	if m.Action.Type != kind || m.Action.TargetID != target {
		return false, assistant.Err(403, "operation_mismatch", "действие не соответствует операции")
	}
	j := m.Job
	if _, err := assistantActorTx(ctx, tx, j.Project, j.Run.InitiatorID, false, true); err != nil {
		return false, err
	}
	c, err := assistantConfigTx(ctx, tx, j.Project, true)
	if err != nil {
		return false, err
	}
	if c.Revision != j.Config.Revision {
		return false, assistant.Err(409, "stale_revision", "настройка ассистента изменилась")
	}
	if j.Run.Trigger == "event" {
		if !c.Enabled || c.SponsorID == nil || *c.SponsorID != j.Run.InitiatorID {
			return false, assistant.Err(403, "sponsor_revoked", "автоматический режим выключен")
		}
		if _, err = assistantActorTx(ctx, tx, j.Project, j.Run.InitiatorID, true, false); err != nil {
			return false, err
		}
	}
	var state string
	var fence int64
	if err = tx.QueryRow(ctx, `SELECT r.status,l.epoch FROM assistant_runs r JOIN assistant_leases l ON l.project_id=r.project_id AND l.run_id=r.id WHERE r.id=$1 AND l.expires_at>now() AND l.expires_at-interval '150 seconds'+make_interval(secs=>$2)>now() FOR UPDATE OF r,l`, j.Run.ID, j.Config.Limits.MaxRunSeconds).Scan(&state, &fence); err != nil {
		return false, assistant.Err(409, "executor_expired", "срок исполнения истёк")
	}
	if state != "running" || fence != j.Epoch {
		return false, assistant.Err(409, "executor_expired", "исполнитель остановлен или заменён")
	}
	var actionStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM assistant_actions WHERE id=$1 AND run_id=$2 AND operation_id=$3 FOR UPDATE`, m.Action.ID, j.Run.ID, m.Action.OperationID).Scan(&actionStatus); err != nil {
		return false, nf(err)
	}
	if actionStatus == "succeeded" {
		return true, nil
	}
	if actionStatus != "proposed" && actionStatus != "executing" {
		return false, assistant.Err(409, "action_terminal", "действие уже завершено")
	}
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM assistant_actions WHERE run_id=$1 AND status='succeeded'`, j.Run.ID).Scan(&n); err != nil {
		return false, err
	}
	if n >= j.Config.Limits.MaxRunActions {
		return false, assistant.Err(429, "budget_exhausted", "лимит действий исчерпан")
	}
	if m.Policy == nil {
		return false, assistant.Err(503, "policy_unavailable", "нет решения политики")
	}
	var freshActor domain.User
	if err = tx.QueryRow(ctx, `SELECT id::text,login,is_admin FROM users WHERE id=$1 AND NOT disabled`, j.Run.InitiatorID).Scan(&freshActor.ID, &freshActor.Login, &freshActor.Admin); err != nil {
		return false, nf(err)
	}
	g, err := m.Policy(ctx, kind, j.Project, freshActor)
	if err != nil {
		return false, err
	}
	if g.Decision != "allow" {
		status := 403
		if g.Decision == "unavailable" {
			status = 503
		}
		return false, assistant.Err(status, g.Code, g.Message)
	}
	current, episode, err := assistantTargetGuardTx(ctx, tx, j.Project, kind, target)
	if err != nil {
		return false, err
	}
	if current != m.Action.ExpectedRevision {
		return false, assistant.Err(409, "stale_revision", "состояние ресурса изменилось")
	}
	if episode != "" {
		var used bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assistant_retry_episodes WHERE project_id=$1 AND task_id=$2 AND episode_id=$3)`, j.Project, target, episode).Scan(&used); err != nil {
			return false, err
		}
		if used {
			return false, assistant.Err(409, "failure_episode_repeated", "эта причина уже получила автоматический retry; нужно подтверждённое исправление причины")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO assistant_retry_episodes(project_id,task_id,episode_id,action_id) VALUES($1,$2,$3,$4)`, j.Project, target, episode, m.Action.ID); err != nil {
			return false, err
		}
	}
	guard, _ := json.Marshal(g)
	_, err = tx.Exec(ctx, `UPDATE assistant_actions SET status='executing',guard=$2,failure_episode_id=NULLIF($3,'') WHERE id=$1`, m.Action.ID, guard, episode)
	return false, err
}
func assistantTargetGuardTx(ctx context.Context, tx pgx.Tx, project, kind, id string) (string, string, error) {
	var revision string
	switch kind {
	case "task.retry":
		var status, stage, reason, blockedBy, pr, description, process, developer, connection, model, connectionState string
		err := tx.QueryRow(ctx, `SELECT t.status,t.updated_at::text,COALESCE(t.step_id,''),COALESCE(t.block_reason,''),COALESCE(t.blocked_by::text,''),COALESCE(t.pr_url,''),t.description,t.process_hash,COALESCE(t.developer_agent_id,''),COALESCE(a.connection_id,''),COALESCE(a.model,''),COALESCE(c.state,'') FROM tasks t JOIN epics e ON e.id=t.epic_id LEFT JOIN team_agents a ON a.id=t.developer_agent_id LEFT JOIN model_connections c ON c.id=a.connection_id WHERE t.id=$1 AND e.project_id=$2 FOR UPDATE OF t`, id, project).Scan(&status, &revision, &stage, &reason, &blockedBy, &pr, &description, &process, &developer, &connection, &model, &connectionState)
		if err != nil {
			return "", "", nf(err)
		}
		if status != "failed" && status != "blocked" {
			return "", "", assistant.Err(409, "requires_human", "автоматический retry доступен только failed/blocked")
		}
		if blockedBy != "" {
			return "", "", assistant.Err(409, "requires_human", "сначала восстановите задачу-первопричину")
		}
		// PrepareRetry may create a PR before the domain transaction. Its remote
		// effect is not safely deduplicated yet, so this branch stays human-owned.
		var proc []byte
		if err = tx.QueryRow(ctx, `SELECT process FROM tasks WHERE id=$1`, id).Scan(&proc); err != nil {
			return "", "", err
		}
		parsed := TaskProcess(domain.Task{Process: proc})
		needsPR := false
		if parsed != nil {
			if step, ok := parsed.Step(stage); ok && step.Kind == policy.StepReview {
				needsPR = true
			}
		}
		if pr == "" && needsPR {
			return "", "", assistant.Err(409, "requires_human", "сначала подготовьте PR штатным retry")
		}
		if developer != "" {
			var enabled bool
			if err = tx.QueryRow(ctx, `SELECT enabled FROM team_agents WHERE id=$1`, developer).Scan(&enabled); err != nil || !enabled || connectionState == "invalid" {
				return "", "", assistant.Err(409, "requires_human", "закреплённый агент или его подключение недоступны")
			}
			if connection != "" {
				if _, err = assistantModelTx(ctx, tx, connection, model); err != nil {
					return "", "", assistant.Err(409, "requires_human", "модель закреплённого агента недоступна")
				}
			}
		}
		// Free-form diagnostics can change wording/family between sessions.
		// Until a trustworthy causal correction is established, treat all
		// failures of this stage/basis as one unresolved episode. This is
		// intentionally stricter than guessing a new episode from log text.
		family := "unresolved_failure"
		basis := assistant.Hash([]string{assistant.Hash(description), process, developer, connection, model})
		return revision, assistant.FailureEpisode(id, stage, family, basis), nil
	case "epic.resume":
		var status string
		if err := tx.QueryRow(ctx, `SELECT status,xmin::text FROM epics WHERE id=$1 AND project_id=$2 FOR UPDATE`, id, project).Scan(&status, &revision); err != nil {
			return "", "", nf(err)
		}
		if status != "paused" {
			return "", "", assistant.Err(409, "requires_human", "возобновляется только приостановленный Epic")
		}
		return revision, "", nil
	case "environment.resume":
		var paused bool
		if err := tx.QueryRow(ctx, `SELECT paused,xmin::text FROM environments WHERE id=$1 AND project_id=$2 FOR UPDATE`, id, project).Scan(&paused, &revision); err != nil {
			return "", "", nf(err)
		}
		if !paused {
			return "", "", assistant.Err(409, "requires_human", "окружение уже не приостановлено")
		}
		return revision, "", nil
	default:
		return "", "", assistant.Err(503, "policy_unavailable", "неизвестная операция")
	}
}
func assistantMutationDoneTx(ctx context.Context, tx pgx.Tx, status string) error {
	m, ok := ctx.Value(assistantMutationKey{}).(assistantMutation)
	if !ok {
		return nil
	}
	result, _ := json.Marshal(map[string]any{"resource_status": status, "effect_ref": m.Action.TargetID})
	_, err := tx.Exec(ctx, `UPDATE assistant_actions SET status='succeeded',finished_at=now(),result=$2 WHERE id=$1 AND status='executing'`, m.Action.ID, result)
	if err != nil {
		return err
	}
	return assistantEventTx(ctx, tx, m.Job.Project, m.Job.Run.ID, "assistant.action.updated", "действие ассистента принято", map[string]any{"action_id": m.Action.ID, "operation_id": m.Action.OperationID})
}
func (s *Store) RecordAssistantActionError(ctx context.Context, j AssistantJob, a assistant.Action, actionErr error) error {
	status := "unknown"
	g := assistant.Guard{Decision: "unavailable", Code: "effect_unknown", Message: "результат неизвестен, повтор автоматически не отправляется"}
	var e *assistant.Error
	if errors.As(actionErr, &e) {
		g.Code = e.Code
		g.Message = e.Message
		switch e.Status {
		case 403:
			g.Decision = "deny"
			status = "denied"
		case 409:
			g.Decision = "requires_human"
			status = "requires_human"
		case 429:
			status = "failed"
		default:
			status = "failed"
		}
	}
	var bad domain.ErrBadTransition
	if errors.As(actionErr, &bad) || errors.Is(actionErr, ErrConflict) || errors.Is(actionErr, ErrArchitectureRequired) || errors.Is(actionErr, ErrAssessmentStale) {
		status = "requires_human"
		g = assistant.Guard{Decision: "requires_human", Code: "domain_guard", Message: "доменная проверка не разрешила переход"}
	}
	raw, _ := json.Marshal(g)
	decisionURL := assistantResourceURL(j.Project, strings.Split(a.Type, ".")[0], a.TargetID, nil)
	for _, ref := range j.Snapshot.Refs {
		if ref.ResourceID == a.TargetID {
			decisionURL = ref.URL
			break
		}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE assistant_actions SET status=$2,guard=$3,finished_at=CASE WHEN $2='unknown' THEN NULL ELSE now() END,result=jsonb_build_object('decision_url',$4::text) WHERE id=$1 AND status<>'succeeded'`, a.ID, status, raw, decisionURL)
		if err != nil {
			return err
		}
		return assistantEventTx(ctx, tx, j.Project, j.Run.ID, "assistant.action.updated", g.Message, map[string]any{"action_id": a.ID})
	})
}
