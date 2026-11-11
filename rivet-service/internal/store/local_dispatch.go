package store

import (
	"context"
	"errors"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
)

// RequeueUndeliveredLocal возвращает только не начатую Docker-операцию.
// Lease снимается атомарно. Новый Hello/heartbeat не затирается отметкой offline.
func (s *Store) RequeueUndeliveredLocal(ctx context.Context, depID, runnerID string, seen time.Time) (bool, error) {
	requeued := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var envID, projectID string
		err := tx.QueryRow(ctx, `SELECT d.env_id::text,e.project_id::text FROM deployments d
   JOIN environments e ON e.id=d.env_id AND e.exec_type='local_docker'
   WHERE d.id=$1 AND d.runner_id=$2 AND d.status='deploying' AND d.ended_at IS NULL
   AND (EXISTS(SELECT 1 FROM local_publications p WHERE p.active_deployment_id=d.id AND (p.operation<>'publish' OR p.status='queued'))
    OR EXISTS(SELECT 1 FROM local_environment_preparations p WHERE p.deployment_id=d.id AND p.status IN ('queued','preparing')))
   FOR UPDATE OF d`, depID, runnerID).Scan(&envID, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE deployments SET status='queued',runner_id='',started_at=NULL WHERE id=$1`, depID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE runners SET deployment_id=NULL,
   status=CASE WHEN last_seen<=$3 THEN 'offline' ELSE 'idle' END,
   offline_reason=CASE WHEN last_seen<=$3 THEN 'undelivered' ELSE '' END
   WHERE id=$1 AND deployment_id=$2`, runnerID, depID, seen); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE local_environment_preparations SET status='queued',updated_at=now() WHERE deployment_id=$1`, depID); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "deploy.status", ProjectID: projectID,
			Text: "Задание Docker-runner-у не доставлено, публикация ожидает подключения", Payload: map[string]any{"environment_id": envID, "deployment_id": depID, "status": "queued"}})
		requeued = err == nil
		return err
	})
	return requeued, err
}
