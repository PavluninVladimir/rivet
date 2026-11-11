package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Подготовка стенда начинается с первым рабочим Epic, независимо от DAG.
// Она не выдаёт готовую публикацию, пока нет проверенного merge-артефакта.
func prepareLocalStandTx(ctx context.Context, tx pgx.Tx, projectID string) error {
	// Лок проекта сериализует одновременный старт нескольких Epic.
	if _, e := tx.Exec(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, projectID); e != nil {
		return e
	}
	var envID, execType string
	e := tx.QueryRow(ctx, `SELECT id::text,exec_type FROM environments WHERE project_id=$1 ORDER BY created_at LIMIT 1`, projectID).Scan(&envID, &execType)
	if errors.Is(e, pgx.ErrNoRows) {
		var dockerConfigured bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runners WHERE capabilities @> ARRAY['deploy','docker'] AND features @> ARRAY['local_docker_v1'])`).Scan(&dockerConfigured); e != nil {
			return e
		}
		if !dockerConfigured {
			return nil
		}
		cfg := domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}
		cfg.Defaults()
		data, _ := json.Marshal(domain.EnvConfig{LocalDocker: &cfg})
		e = tx.QueryRow(ctx, `INSERT INTO environments(project_id,name,exec_type,trigger,config,runner_caps) VALUES($1,'Локальный стенд','local_docker','auto',$2,'{}') RETURNING id::text`, projectID, data).Scan(&envID)
		execType = domain.ExecLocalDocker
	}
	if e != nil {
		return e
	}
	if execType != domain.ExecLocalDocker {
		return nil
	}
	var existing bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_environment_preparations WHERE env_id=$1) OR EXISTS(SELECT 1 FROM local_publications WHERE env_id=$1)`, envID).Scan(&existing); e != nil || existing {
		return e
	}
	busy, e := localBusy(ctx, tx, envID)
	if e != nil || busy {
		return e
	}
	var depID string
	if e = tx.QueryRow(ctx, `INSERT INTO deployments(env_id,version,initiator) VALUES($1,'','release-engineer') RETURNING id::text`, envID).Scan(&depID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO local_environment_preparations(env_id,deployment_id) VALUES($1,$2)`, envID, depID); e != nil {
		return e
	}
	_, e = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "environment.preparing", ProjectID: projectID, Text: "Инженер по выпуску готовит локальный Docker-стенд параллельно с разработкой; публикация начнётся после первого проверенного merge", Payload: map[string]any{"environment_id": envID, "status": "queued"}})
	return e
}
func (s *Store) LocalPreparation(ctx context.Context, envID string) (string, error) {
	var status string
	e := s.Pool.QueryRow(ctx, `SELECT status FROM local_environment_preparations WHERE env_id=$1`, envID).Scan(&status)
	return status, nf(e)
}
func (s *Store) IsLocalPreparation(ctx context.Context, depID string) (bool, error) {
	var ok bool
	e := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_environment_preparations WHERE deployment_id=$1)`, depID).Scan(&ok)
	return ok, e
}
func (s *Store) SetLocalPreparation(ctx context.Context, depID, runnerID, status, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID, envID string
		e := tx.QueryRow(ctx, `UPDATE local_environment_preparations p SET status=$3,reason=$4,updated_at=now() FROM deployments d,environments e WHERE p.deployment_id=$1 AND d.id=p.deployment_id AND d.runner_id=$2 AND d.ended_at IS NULL AND e.id=p.env_id RETURNING e.project_id::text,e.id::text`, depID, runnerID, status, reason).Scan(&projectID, &envID)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		_, e = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: "environment.preparing", ProjectID: projectID, Text: map[string]string{"waiting_artifact": "Docker готов. Инженер по выпуску ожидает первый проверенный артефакт", "failed": "Подготовка Docker-стенда не выполнена: " + reason}[status], Payload: map[string]any{"environment_id": envID, "status": status}})
		return e
	})
}
