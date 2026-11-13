package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/PavluninVladimir/rivet/internal/assistant"
	"github.com/jackc/pgx/v5"
)

// AssistantSnapshot reads a repeatable, explicitly projected view. It never
// serializes domain structs, credentials, environment config or transcripts.
func (s *Store) AssistantSnapshot(ctx context.Context, project, actor string) (assistant.Snapshot, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return assistant.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = assistantActorTx(ctx, tx, project, actor, false, false); err != nil {
		return assistant.Snapshot{}, err
	}
	snap := assistant.Snapshot{ComputedAt: time.Now().UTC(), Facts: []assistant.Fact{}, Refs: []assistant.EvidenceRef{}, Capabilities: []string{"project-assistant", "discovery"}}
	var cursor int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM events WHERE project_id=$1 AND type NOT LIKE 'assistant.%'`, project).Scan(&cursor); err != nil {
		return snap, err
	}
	snap.EventCursor = strconv.FormatInt(cursor, 10)
	queries := []struct{ kind, q string }{
		{"epic", `SELECT id::text,jsonb_build_object('title',left(title,300),'status',status,'kind',kind,'architecture_required',architecture_required,'revision',xmin::text) FROM epics WHERE project_id=$1 ORDER BY id LIMIT 500`},
		{"task", `SELECT t.id::text,jsonb_build_object('num',t.num,'title',left(t.title,300),'status',t.status,'step',t.step_id,'blocked_by',t.blocked_by,'wait_kind',t.wait_kind,'wait_reason',left(COALESCE(t.wait_reason,''),300),'developer_agent_id',t.developer_agent_id,'developer_binding',(SELECT jsonb_build_object('enabled',a.enabled,'connection_id',a.connection_id,'model_id',a.model,'connection_state',c.state) FROM team_agents a LEFT JOIN model_connections c ON c.id=a.connection_id WHERE a.id=t.developer_agent_id),'revision',t.updated_at::text,'epic_id',t.epic_id,'deps',COALESCE((SELECT jsonb_agg(d.dep_id ORDER BY d.dep_id) FROM task_deps d WHERE d.task_id=t.id),'[]')) FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE e.project_id=$1 ORDER BY t.num LIMIT 1000`},
		{"attention", `SELECT id::text,jsonb_build_object('reason',reason,'status',status,'task_id',task_id,'deployment_id',deployment_id) FROM attention WHERE project_id=$1 AND status<>'resolved' ORDER BY id LIMIT 200`},
		{"incident", `SELECT id::text,jsonb_build_object('status',status,'env_id',env_id,'bug_task_id',bug_task_id,'occurrences',occurrences,'title',left(title,300)) FROM incidents WHERE project_id=$1 ORDER BY last_seen DESC,id LIMIT 100`},
		{"environment", `SELECT id::text,jsonb_build_object('name',left(name,200),'paused',paused,'trigger',trigger,'exec_type',exec_type,'revision',xmin::text) FROM environments WHERE project_id=$1 ORDER BY id LIMIT 100`},
		{"publication", `SELECT l.id::text,jsonb_build_object('env_id',l.env_id,'status',l.status,'desired_state',l.desired_state,'generation',l.generation,'version',l.version,'reason_code',l.reason->>'code','checked_at',l.checked_at,'product_acceptance','unknown') FROM local_publications l JOIN environments e ON e.id=l.env_id WHERE e.project_id=$1 AND l.generation=(SELECT max(x.generation) FROM local_publications x WHERE x.env_id=l.env_id) ORDER BY l.id LIMIT 100`},
		{"research", `SELECT id::text,jsonb_build_object('status',status,'outcome',result->>'outcome','version',version) FROM discovery_runs WHERE project_id=$1 ORDER BY started_at DESC,id LIMIT 20`},
		{"session", `SELECT s.id::text,jsonb_build_object('task_id',s.task_id,'epic_id',t.epic_id,'model',s.model,'agent',s.agent,'started_at',s.started_at,'ended_at',s.ended_at,'outcome',CASE WHEN s.outcome IS NOT NULL THEN left(s.outcome,300) ELSE NULL END) FROM sessions s JOIN tasks t ON t.id=s.task_id JOIN epics e ON e.id=t.epic_id WHERE e.project_id=$1 ORDER BY s.started_at DESC,s.id LIMIT 100`},
		{"runner", `SELECT id,jsonb_build_object('status',status,'draining',draining,'capabilities',capabilities) FROM runners WHERE EXISTS(SELECT 1 FROM projects WHERE id=$1) ORDER BY id LIMIT 200`},
		{"model", `SELECT c.id,jsonb_build_object('enabled',c.enabled,'state',c.state,'models',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',x->>'id','missing',x->'missing','hidden',x->'hidden')) FROM jsonb_array_elements(c.models) x),'[]')) FROM model_connections c WHERE EXISTS(SELECT 1 FROM projects WHERE id=$1) ORDER BY c.id LIMIT 100`},
	}
	for _, q := range queries {
		rows, e := tx.Query(ctx, q.q, project)
		if e != nil {
			return snap, fmt.Errorf("assistant snapshot %s: %w", q.kind, e)
		}
		for rows.Next() {
			var id string
			var raw []byte
			if e = rows.Scan(&id, &raw); e != nil {
				rows.Close()
				return snap, e
			}
			v := map[string]any{}
			if e = json.Unmarshal(raw, &v); e != nil {
				rows.Close()
				return snap, e
			}
			for k, x := range v {
				if str, ok := x.(string); ok {
					v[k] = assistant.Redact(str)
				}
			}
			ref := q.kind + ":" + id
			revision := assistant.Hash(v)
			if x, ok := v["revision"].(string); ok {
				revision = x
			}
			url := assistantResourceURL(project, q.kind, id, v)
			snap.Facts = append(snap.Facts, assistant.Fact{ID: ref, Kind: q.kind, ResourceID: id, Value: v, RefIDs: []string{ref}})
			snap.Refs = append(snap.Refs, assistant.EvidenceRef{ID: ref, Kind: assistantRefKind(q.kind), ResourceID: id, Revision: &revision, ObservedAt: snap.ComputedAt, Label: ref, URL: url})
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return snap, e
		}
	}
	snap.Revision = assistant.Hash(struct {
		Facts  []assistant.Fact
		Cursor string
	}{snap.Facts, snap.EventCursor})
	return snap, tx.Commit(ctx)
}
func assistantRefKind(kind string) string {
	if kind == "environment" {
		return "publication"
	}
	return kind
}
func assistantResourceURL(project, kind, id string, v map[string]any) string {
	base := "#/p/" + project
	switch kind {
	case "session":
		if epic, ok := v["epic_id"].(string); ok {
			if task, ok := v["task_id"].(string); ok {
				return base + "/epic/" + epic + "/task/" + task
			}
		}
		return base + "/overview"
	case "task":
		if epic, ok := v["epic_id"].(string); ok && assistant.ValidID(epic) {
			return base + "/epic/" + epic + "/task/" + id
		}
		return base + "/overview"
	case "epic":
		return base + "/epic/" + id
	case "incident":
		return base + "/incidents/" + id
	case "research":
		return base + "/discovery/" + id
	case "publication", "environment":
		return base + "/settings/environments"
	case "runner", "model":
		return base + "/team"
	default:
		return base + "/overview"
	}
}

// lock=true uses the same project advisory lock as membership changes. The
// active user row is locked before the project, matching membership lock order.
func assistantActorTx(ctx context.Context, tx pgx.Tx, project, actor string, owner, lock bool) (string, error) {
	var login string
	var disabled bool
	q := `SELECT login,disabled FROM users WHERE id=$1`
	if lock {
		q += ` FOR SHARE`
	}
	if err := tx.QueryRow(ctx, q, actor).Scan(&login, &disabled); err != nil {
		return "", nf(err)
	}
	if disabled {
		return "", ErrNotFound
	}
	if lock {
		if err := lockProjects(ctx, tx, []string{project}); err != nil {
			return "", err
		}
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2`, project, actor).Scan(&role); err != nil {
		return "", nf(err)
	}
	if owner && role != "owner" {
		return "", assistant.Err(403, "insufficient_role", "нужны права владельца")
	}
	return login, nil
}
