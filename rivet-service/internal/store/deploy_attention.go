package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ResolveStaleDeployAttention repairs historical cards using the same witness
// as successful completion. It never resolves incidents or resumes an env.
func (s *Store) ResolveStaleDeployAttention(ctx context.Context) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { var err error; n, err = resolveStaleDeployAttentionTx(ctx, tx, ""); return err })
	return n, err
}
func resolveStaleDeployAttentionTx(ctx context.Context, tx pgx.Tx, envID string) (int, error) {
	// Lock the positively identified environments before taking a fresh witness
	// snapshot. Enqueue/resume/pause serialize on these rows. Local health
	// acceptance also serializes on the latest publication row. This prevents
	// a concurrent pause/new failure from invalidating proof before commit.
	locked, err := tx.Query(ctx, `SELECT e.id::text FROM environments e WHERE e.id IN (SELECT candidate.env_id FROM (`+staleDeployCandidateSQL+` ORDER BY a.created_at,a.id LIMIT 100) candidate) ORDER BY e.id FOR UPDATE OF e SKIP LOCKED`, envID)
	if err != nil {
		return 0, err
	}
	var envs []string
	for locked.Next() {
		var id string
		if err := locked.Scan(&id); err != nil {
			locked.Close()
			return 0, err
		}
		envs = append(envs, id)
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return 0, err
	}
	if len(envs) == 0 {
		return 0, nil
	}
	for _, id := range envs {
		latest, err := tx.Query(ctx, `SELECT id FROM local_publications WHERE env_id=$1 ORDER BY generation DESC LIMIT 1 FOR UPDATE`, id)
		if err != nil {
			return 0, err
		}
		for latest.Next() {
		}
		err = latest.Err()
		latest.Close()
		if err != nil {
			return 0, err
		}
	}
	rows, err := tx.Query(ctx, `WITH candidates AS (
 `+staleDeployCandidateSQL+` AND e.id=ANY($2::uuid[])
 ORDER BY a.created_at,a.id LIMIT 100 FOR UPDATE OF a SKIP LOCKED
), resolved AS (
 UPDATE attention a SET status='resolved',resolved_at=now() FROM candidates c
 WHERE a.id=c.id AND a.status IN('open','claimed')
 RETURNING a.id,c.env_id,c.project_id,c.deployment_id,c.publication_id,c.generation
) SELECT id::text,env_id::text,project_id::text,deployment_id::text,COALESCE(publication_id::text,''),COALESCE(generation,0) FROM resolved`, envID, envs)
	if err != nil {
		return 0, err
	}
	type proof struct {
		ID, Env, Project, Deployment, Publication string
		Generation                                int64
	}
	var proofs []proof
	for rows.Next() {
		var p proof
		if err := rows.Scan(&p.ID, &p.Env, &p.Project, &p.Deployment, &p.Publication, &p.Generation); err != nil {
			rows.Close()
			return 0, err
		}
		proofs = append(proofs, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	groups := map[string][]proof{}
	for _, p := range proofs {
		groups[p.Env] = append(groups[p.Env], p)
	}
	for env, ps := range groups {
		ids := make([]string, 0, len(ps))
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		p := ps[0]
		_, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "deploy.status", ProjectID: p.Project, Text: "устаревшие ошибки доставки сняты после проверенного восстановления", Payload: map[string]any{"action": "resolved_stale_attention", "environment_id": env, "deployment_id": p.Deployment, "publication_id": p.Publication, "generation": p.Generation, "attention_ids": ids}})
		if err != nil {
			return 0, err
		}
	}
	return len(proofs), nil
}

const staleDeployCandidateSQL = `SELECT a.id,e.id AS env_id,e.project_id,w.deployment_id,w.publication_id,w.generation
 FROM attention a
 JOIN deployments failed ON failed.id=a.deployment_id AND failed.status='failed' AND failed.ended_at IS NOT NULL
 JOIN environments e ON e.id=failed.env_id AND e.project_id=a.project_id AND NOT e.paused
 JOIN LATERAL (
  SELECT good.id AS deployment_id,p.id AS publication_id,p.generation
  FROM local_publications p
  JOIN deployments good ON good.id=p.deployment_id AND good.env_id=e.id AND good.status='done' AND good.ended_at IS NOT NULL
  JOIN releases release ON release.deployment_id=good.id AND release.env_id=e.id AND release.project_id=e.project_id
  JOIN deployments operation ON operation.id=p.active_deployment_id AND operation.env_id=e.id AND operation.status='done' AND operation.ended_at IS NOT NULL
  JOIN local_publications origin ON origin.deployment_id=failed.id AND origin.env_id=e.id AND origin.generation<p.generation
  WHERE e.exec_type='local_docker' AND p.env_id=e.id
    AND p.generation=(SELECT max(generation) FROM local_publications WHERE env_id=e.id)
    AND p.status='running' AND p.desired_state='running' AND p.health_failures=0
    AND (p.reason IS NULL OR p.reason='null'::jsonb OR p.reason='{}'::jsonb)
    AND p.container_id<>'' AND p.image_digest<>'' AND p.host_port>0 AND p.health_url<>'' AND p.checked_at IS NOT NULL
    AND p.operation IN('publish','reconcile','restart') AND p.completed_operation_id=p.active_deployment_id
  UNION ALL
  SELECT good.id,NULL::uuid,NULL::bigint
  FROM deployments good
  JOIN releases release ON release.deployment_id=good.id AND release.env_id=e.id AND release.project_id=e.project_id
  WHERE e.exec_type<>'local_docker' AND good.env_id=e.id AND good.status='done' AND NOT good.rollback
    AND good.ended_at IS NOT NULL AND good.started_at IS NOT NULL
    AND failed.created_at<good.created_at AND failed.ended_at<=good.started_at AND good.created_at>failed.ended_at
    AND (COALESCE(e.config->>'verify_cmd','')<>'' OR COALESCE(e.config->>'verify_url','')<>'' OR (e.exec_type='k8s' AND COALESCE(e.config->>'workload','')<>''))
    AND NOT EXISTS(SELECT 1 FROM deployments newer WHERE newer.env_id=e.id AND newer.created_at>=good.created_at AND newer.id<>good.id)
 ) w ON true
 WHERE a.reason='DEPLOY_FAILED' AND a.status IN('open','claimed') AND ($1::text='' OR e.id::text=$1)
`
