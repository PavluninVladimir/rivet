package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/jackc/pgx/v5"
)

var ErrPublicationBusy = errors.New("publication_in_progress")
var ErrEnvironmentPaused = errors.New("environment_paused")
var ErrNoDockerRunner = errors.New("no_runner")
var ErrNoPublication = errors.New("no_publication")
var ErrPublicationStopped = errors.New("publication_stopped")
var ErrPublicationVersion = errors.New("version outside publication history")

type LocalPublication struct {
	ID                 string                              `json:"id"`
	EnvironmentID      string                              `json:"environment_id"`
	DeploymentID       string                              `json:"-"`
	ActiveDeploymentID string                              `json:"-"`
	Version            string                              `json:"version"`
	Generation         int64                               `json:"generation"`
	Status             string                              `json:"status"`
	DesiredState       string                              `json:"desired_state"`
	Attempt            int                                 `json:"attempt"`
	Image              string                              `json:"image"`
	ContainerID        string                              `json:"container_id"`
	Port               int                                 `json:"-"`
	Listen             *string                             `json:"listen"`
	HealthURL          string                              `json:"health_url"`
	Reason             *localdocker.Failure                `json:"reason"`
	LogRef             string                              `json:"log_ref"`
	Created            time.Time                           `json:"created_at"`
	Started            *time.Time                          `json:"started_at"`
	Finished           *time.Time                          `json:"finished_at"`
	Config             domain.LocalDockerEnvironmentConfig `json:"-"`
	RunnerID           string                              `json:"-"`
	Operation          string                              `json:"-"`
}

const localCols = `id::text,env_id::text,COALESCE(deployment_id::text,''),COALESCE(active_deployment_id::text,''),version,generation,status,desired_state,attempt,image_digest,container_id,COALESCE(host_port,0),health_url,reason,log_ref,created_at,started_at,finished_at,config,runner_id,operation`

func scanLocal(row pgx.Row) (p LocalPublication, e error) {
	var config, reason []byte
	e = row.Scan(&p.ID, &p.EnvironmentID, &p.DeploymentID, &p.ActiveDeploymentID, &p.Version, &p.Generation, &p.Status, &p.DesiredState, &p.Attempt, &p.Image, &p.ContainerID, &p.Port, &p.HealthURL, &reason, &p.LogRef, &p.Created, &p.Started, &p.Finished, &config, &p.RunnerID, &p.Operation)
	if e != nil {
		return p, nf(e)
	}
	if e = json.Unmarshal(config, &p.Config); e != nil {
		return p, e
	}
	if len(reason) > 0 {
		if e = json.Unmarshal(reason, &p.Reason); e != nil {
			return p, e
		}
	}
	if p.Port > 0 {
		v := fmt.Sprintf("127.0.0.1:%d", p.Port)
		p.Listen = &v
	}
	return p, nil
}
func (p LocalPublication) Job(projectID string) localdocker.Job {
	return localdocker.Job{ProjectID: projectID, EnvironmentID: p.EnvironmentID, PublicationID: p.ID, Version: p.Version, Generation: p.Generation, Config: p.Config, ContainerID: p.ContainerID, Image: p.Image, Port: p.Port, DesiredState: p.DesiredState, Attempt: p.Attempt}
}
func (s *Store) LocalPublication(ctx context.Context, envID string) (LocalPublication, error) {
	return scanLocal(s.Pool.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE env_id=$1 ORDER BY generation DESC LIMIT 1`, envID))
}
func (s *Store) LocalPublicationByDeployment(ctx context.Context, depID string) (LocalPublication, error) {
	return scanLocal(s.Pool.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE active_deployment_id=$1`, depID))
}
func (s *Store) CurrentLocalPublication(ctx context.Context, envID string) (LocalPublication, error) {
	// Неудачное обновление не прячет работающий адрес. Причина обновления
	// остаётся рядом с прежним поколением и в общей истории deployments.
	latest, e := s.LocalPublication(ctx, envID)
	if e != nil {
		return latest, e
	}
	if latest.Status == "failed" {
		current, ce := scanLocal(s.Pool.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE env_id=$1 AND status IN('running','unhealthy','stopped','unknown') AND container_id<>'' ORDER BY generation DESC LIMIT 1`, envID))
		if ce == nil {
			current.Reason = latest.Reason
			return current, nil
		}
	}
	return latest, nil
}
func localRunner(ctx context.Context, tx pgx.Tx, envID string) (bool, error) {
	var ok bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runners r JOIN environments e ON e.id=$1 WHERE r.status<>'offline' AND NOT r.draining AND r.capabilities @> ARRAY['deploy','docker'] AND r.capabilities @> e.runner_caps AND r.features @> ARRAY['local_docker_v1'] AND (NOT EXISTS(SELECT 1 FROM environment_secrets es WHERE es.env_id=e.id) OR (r.secure AND cardinality(r.capabilities)=2 AND r.features @> ARRAY['local_docker_secrets_v1'])) AND (NOT EXISTS(SELECT 1 FROM local_publications p WHERE p.env_id=e.id AND p.runner_id<>'') OR r.id=(SELECT runner_id FROM local_publications p WHERE p.env_id=e.id AND p.runner_id<>'' ORDER BY generation DESC LIMIT 1)))`, envID).Scan(&ok)
	return ok, e
}
func localBusy(ctx context.Context, tx pgx.Tx, envID string) (bool, error) {
	var busy bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE env_id=$1 AND status IN('queued','deploying','verifying'))`, envID).Scan(&busy)
	return busy, e
}

func (s *Store) EnqueueLocalPublication(ctx context.Context, envID, version, initiator string, auto bool) (LocalPublication, error) {
	var p LocalPublication
	e := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		env, e := scanEnv(tx.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1 FOR UPDATE`, envID))
		if e != nil {
			return nf(e)
		}
		if env.ExecType != domain.ExecLocalDocker {
			return ErrConflict
		}
		var deleting bool
		if e = tx.QueryRow(ctx, `SELECT local_delete_requested FROM environments WHERE id=$1`, envID).Scan(&deleting); e != nil {
			return e
		}
		if deleting {
			return ErrConflict
		}
		if env.Paused && !auto {
			return ErrEnvironmentPaused
		}
		if e = env.Config.Validate(env.ExecType); e != nil {
			return e
		}
		busy, e := localBusy(ctx, tx, envID)
		if e != nil {
			return e
		}
		if busy {
			if !auto {
				return ErrPublicationBusy
			}
			if _, e = tx.Exec(ctx, `INSERT INTO local_pending_publications(env_id,version) VALUES($1,$2) ON CONFLICT(env_id) DO UPDATE SET version=EXCLUDED.version`, envID, version); e != nil {
				return e
			}
			p, e = scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE env_id=$1 ORDER BY generation DESC LIMIT 1`, envID))
			if errors.Is(e, ErrNotFound) {
				e = tx.QueryRow(ctx, `SELECT id::text FROM deployments WHERE env_id=$1 AND status IN('queued','deploying','verifying') ORDER BY created_at DESC LIMIT 1`, envID).Scan(&p.ActiveDeploymentID)
				p.Version = version
				p.Status = "preparing"
			}
			return e
		}

		if auto {
			var stopped bool
			if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT desired_state='stopped' FROM local_publications WHERE env_id=$1 AND container_id<>'' ORDER BY generation DESC LIMIT 1),false)`, envID).Scan(&stopped); e != nil {
				return e
			}
			if stopped {
				_, e = tx.Exec(ctx, `INSERT INTO local_pending_publications(env_id,version) VALUES($1,$2) ON CONFLICT(env_id) DO UPDATE SET version=EXCLUDED.version`, envID, version)
				if e != nil {
					return e
				}
				p, e = scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE env_id=$1 AND container_id<>'' ORDER BY generation DESC LIMIT 1`, envID))
				return e
			}
		} else {
			if _, e = tx.Exec(ctx, `DELETE FROM local_pending_publications WHERE env_id=$1`, envID); e != nil {
				return e
			}
		}

		ok, e := localRunner(ctx, tx, envID)
		if e != nil {
			return e
		}
		if !ok && !auto {
			return ErrNoDockerRunner
		}
		var depID string
		e = tx.QueryRow(ctx, `INSERT INTO deployments(env_id,version,initiator) VALUES($1,$2,$3) RETURNING id::text`, envID, version, initiator).Scan(&depID)
		if e != nil {
			return e
		}
		cfg, _ := json.Marshal(env.Config.LocalDocker)
		p, e = scanLocal(tx.QueryRow(ctx, `INSERT INTO local_publications(env_id,deployment_id,active_deployment_id,version,generation,config,log_ref) VALUES($1,$2::uuid,$2::uuid,$3,(SELECT COALESCE(max(generation),0)+1 FROM local_publications WHERE env_id=$1),$4,$2::text) RETURNING `+localCols, envID, depID, version, cfg))
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO local_publication_secrets(publication_id,name,value_enc) SELECT $1,name,value_enc FROM environment_secrets WHERE env_id=$2`, p.ID, envID); e != nil {
			return e
		}
		_, e = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: initiator, Type: "environment.publish", ProjectID: env.ProjectID, Text: "публикация локального окружения: " + env.Name, Payload: map[string]any{"environment_id": envID, "publication_id": p.ID, "generation": p.Generation, "version": version}})
		return e
	})
	return p, e
}

// QueueLocalOperation использует общую очередь и runner lease публикаций.
// Сверка, stop и restart не создают release и не меняют версию поколения.
func (s *Store) QueueLocalOperation(ctx context.Context, envID, action, initiator string) (LocalPublication, error) {
	var p LocalPublication
	e := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		env, e := scanEnv(tx.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1 FOR UPDATE`, envID))
		if e != nil {
			return nf(e)
		}
		if env.ExecType != domain.ExecLocalDocker {
			return ErrConflict
		}
		busy, e := localBusy(ctx, tx, envID)
		if e != nil {
			return e
		}
		if busy {
			return ErrPublicationBusy
		}
		p, e = scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE env_id=$1 AND (container_id<>'' OR $2='cleanup') ORDER BY generation DESC LIMIT 1 FOR UPDATE`, envID, action))
		if errors.Is(e, ErrNotFound) {
			return ErrNoPublication
		}
		if e != nil {
			return e
		}
		if action == "stop" && p.Status == "stopped" && p.DesiredState == "stopped" {
			return nil
		}
		if action == "restart" && p.DesiredState == "stopped" {
			return ErrPublicationStopped
		}
		switch action {
		case "stop", "restart", "reconcile", "logs", "remove", "cleanup":
		default:
			return ErrConflict
		}
		ok, e := localRunner(ctx, tx, envID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNoDockerRunner
		}
		var depID string
		e = tx.QueryRow(ctx, `INSERT INTO deployments(env_id,version,initiator) VALUES($1,$2,$3) RETURNING id::text`, envID, p.Version, initiator).Scan(&depID)
		if e != nil {
			return e
		}
		if action == "restart" {
			p.Status = "starting"
		}
		if action == "stop" {
			p.DesiredState = "stopped"
			p.Status = "stopping"
		}
		p, e = scanLocal(tx.QueryRow(ctx, `UPDATE local_publications SET active_deployment_id=$2,operation=$3,desired_state=$4,status=$5,deadline_at=now()+interval '35 minutes' WHERE id=$1 RETURNING `+localCols, p.ID, depID, action, p.DesiredState, p.Status))
		if e != nil {
			return e
		}
		if action == "stop" || action == "restart" {
			_, e = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: initiator, Type: "environment." + action, ProjectID: env.ProjectID, Text: "локальное окружение: " + action, Payload: map[string]any{"environment_id": envID, "publication_id": p.ID}})
		}
		return e
	})
	return p, e
}
func (s *Store) LocalVersionInHistory(ctx context.Context, envID, version string) (bool, error) {
	var ok bool
	e := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_publications WHERE env_id=$1 AND version=$2)`, envID, version).Scan(&ok)
	return ok, e
}
func (s *Store) LastLocalVersion(ctx context.Context, envID string) (string, error) {
	var v string
	e := s.Pool.QueryRow(ctx, `SELECT version FROM local_publications WHERE env_id=$1 AND image_digest<>'' AND container_id<>'' ORDER BY generation DESC LIMIT 1`, envID).Scan(&v)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", ErrNoPublication
	}
	return v, e
}

// AcceptLocalResult делает CAS по живой операции и владельцу runner'а.
// Финал общей deployments-цепочки вызывается после него и идемпотентен.
func (s *Store) AcceptLocalResult(ctx context.Context, depID, runnerID string, res localdocker.Result) (LocalPublication, bool, error) {
	var p LocalPublication
	accepted := false
	e := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var envID string
		e := tx.QueryRow(ctx, `SELECT env_id::text FROM deployments WHERE id=$1 AND runner_id=$2 AND ended_at IS NULL FOR UPDATE`, depID, runnerID).Scan(&envID)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		p, e = scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE active_deployment_id=$1 FOR UPDATE`, depID))
		if e != nil {
			return e
		}
		var sensitive bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_publication_secrets WHERE publication_id=$1)`, p.ID).Scan(&sensitive); e != nil {
			return e
		}
		if sensitive {
			res = localdocker.ProtectResult(res)
		}
		var repeated bool
		if e = tx.QueryRow(ctx, `SELECT COALESCE(completed_operation_id=$2::uuid,false) FROM local_publications WHERE id=$1`, p.ID, depID).Scan(&repeated); e != nil {
			return e
		}
		if repeated {
			accepted = true
			return nil
		}

		status := res.Status
		if p.Operation == "cleanup" {
			status = p.Status
			res.Reason = p.Reason
		}
		if status == "building" || status == "starting" || status == "verifying" {
			expected := map[string]string{"building": "queued", "starting": "building", "verifying": "starting"}[status]
			if p.Operation != "publish" || p.Status != expected {
				return nil
			}
		} else {
			switch status {
			case "running", "unhealthy", "stopped", "failed", "unknown":
			default:
				return ErrConflict
			}
			if p.Operation == "publish" && (p.Status == "running" || p.Status == "failed") {
				accepted = true
				return nil
			}
		}
		if status == "unhealthy" && p.Operation == "reconcile" {
			var n int
			e = tx.QueryRow(ctx, `UPDATE local_publications SET health_failures=health_failures+1 WHERE id=$1 RETURNING health_failures`, p.ID).Scan(&n)
			if e != nil {
				return e
			}
			if n < 3 {
				status = p.Status
			}
		}
		if p.Operation == "publish" && status == "failed" {
			res.ContainerID = ""
			p.ContainerID = ""
			res.Port = 0
			p.Port = 0
			res.HealthURL = ""
			p.HealthURL = ""
		}
		if res.ContainerID != "" {
			p.ContainerID = res.ContainerID
		}
		if res.Image != "" {
			p.Image = res.Image
		}
		if res.Port != 0 {
			p.Port = res.Port
		}
		if res.HealthURL != "" {
			p.HealthURL = res.HealthURL
		}
		reason, _ := json.Marshal(res.Reason)
		var projectID string
		e = tx.QueryRow(ctx, `SELECT project_id::text FROM environments WHERE id=$1`, envID).Scan(&projectID)
		if e != nil {
			return e
		}
		if p.Operation == "remove" && status == "stopped" {
			if res.ResourcesRemoved {
				if _, e = tx.Exec(ctx, `UPDATE environments SET local_cleanup_completed=true WHERE id=$1`, envID); e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `UPDATE local_publications SET container_id='',image_digest='',host_port=NULL,health_url='' WHERE env_id=$1`, envID); e != nil {
					return e
				}
			}
			p.ContainerID = ""
			p.Image = ""
			p.Port = 0
			p.HealthURL = ""
			res.ContainerID = ""
			res.Image = ""
			res.Port = 0
			res.HealthURL = ""
		}
		oldStatus := p.Status
		p, e = scanLocal(tx.QueryRow(ctx, `UPDATE local_publications SET completed_operation_id=CASE WHEN $2 IN('building','starting','verifying') THEN completed_operation_id ELSE active_deployment_id END,status=$2,runner_id=$3,container_id=$4,image_digest=$5,host_port=NULLIF($6,0),health_url=$7,reason=$8,attempt=$9,checked_at=now(),started_at=COALESCE(started_at,now()),deadline_at=now()+interval '35 minutes',finished_at=CASE WHEN $2 IN('running','failed','stopped') THEN now() ELSE finished_at END,health_failures=CASE WHEN $11 THEN 0 ELSE health_failures END,container_log=CASE WHEN $10<>'' THEN $10 ELSE container_log END WHERE id=$1 RETURNING `+localCols, p.ID, status, runnerID, p.ContainerID, p.Image, p.Port, p.HealthURL, reason, res.Attempt, localTail(res.Logs), res.Status == "running"))
		if e != nil {
			return e
		}
		accepted = true
		if p.Operation == "reconcile" {
			output := ""
			if res.Reason != nil {
				output = res.Reason.Message
			}
			if _, e = tx.Exec(ctx, `INSERT INTO monitor_runs(env_id,ok,output) VALUES($1,$2,$3)`, envID, res.Status == "running", output); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE local_publications SET builder_log='',container_log='' WHERE env_id=$1 AND generation<(SELECT COALESCE(max(generation),0)-2 FROM local_publications WHERE env_id=$1)`, envID); e != nil {
			return e
		}

		if oldStatus != status {
			kind := "environment.reconciled"
			if status == "failed" {
				kind = "environment.publication_failed"
			}
			if status == "unhealthy" {
				kind = "environment.publication_unhealthy"
			}
			_, e = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: kind, ProjectID: projectID, Text: "локальная публикация: " + status, Payload: map[string]any{"environment_id": envID, "publication_id": p.ID, "status": status}})
		}
		return e
	})
	return p, accepted, e
}
func localTail(s string) string {
	if len(s) > localdocker.LogLimit {
		s = s[len(s)-localdocker.LogLimit:]
		for len(s) > 0 && (s[0]&0xc0) == 0x80 {
			s = s[1:]
		}
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
}
func (s *Store) SaveLocalBuilderLog(ctx context.Context, depID string, b []byte) error {
	sensitive, err := s.DeploymentHasApplicationSecrets(ctx, depID)
	if err != nil {
		return err
	}
	if sensitive {
		b = []byte(localdocker.SensitiveLogMessage)
	}
	_, e := s.Pool.Exec(ctx, `UPDATE local_publications SET builder_log=$2 WHERE deployment_id=$1`, depID, localTail(string(b)))
	return e
}
func (s *Store) LocalLogs(ctx context.Context, envID, stream string) (string, error) {
	p, e := s.CurrentLocalPublication(ctx, envID)
	if stream == "builder" {
		p, e = s.LocalPublication(ctx, envID)
	}
	if e != nil {
		return "", e
	}
	var sensitive bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_publication_secrets WHERE publication_id=$1)`, p.ID).Scan(&sensitive); err != nil {
		return "", err
	}
	if sensitive {
		return localdocker.SensitiveLogMessage, nil
	}
	if stream == "container" && p.Status == "stopped" {
		return "", ErrNotFound
	}
	col := "builder_log"
	if stream == "container" {
		col = "container_log"
	}
	var logs string
	e = s.Pool.QueryRow(ctx, `SELECT `+col+` FROM local_publications WHERE id=$1`, p.ID).Scan(&logs)
	if e == nil && logs == "" {
		e = ErrNotFound
	}
	return logs, e
}
func (s *Store) FinishLocalOperation(ctx context.Context, depID, runnerID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		claimed, e := finishDeploymentTx(ctx, tx, depID, runnerID, "done", "")
		if e != nil || !claimed {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE runners SET status='idle',deployment_id=NULL WHERE deployment_id=$1`, depID)
		return e
	})
}
func (s *Store) DueLocalPublications(ctx context.Context) ([]string, error) {
	rows, e := s.Pool.Query(ctx, `SELECT DISTINCT p.env_id::text FROM local_publications p WHERE (p.generation=(SELECT max(generation) FROM local_publications WHERE env_id=p.env_id AND container_id<>'') OR (NOT EXISTS(SELECT 1 FROM local_publications WHERE env_id=p.env_id AND container_id<>'') AND p.generation=(SELECT max(generation) FROM local_publications WHERE env_id=p.env_id))) AND (p.checked_at IS NULL OR p.checked_at<now()-interval '15 seconds') AND NOT EXISTS(SELECT 1 FROM deployments d WHERE d.env_id=p.env_id AND d.status IN('queued','deploying','verifying'))`)
	if e != nil {
		return nil, e
	}
	return collectIDs(rows)
}

func (s *Store) FailLocalByDeployment(ctx context.Context, depID, detail string) error {
	if _, e := s.Pool.Exec(ctx, `UPDATE local_environment_preparations SET status='failed',reason=$2,updated_at=now() WHERE deployment_id=$1 AND status IN('queued','preparing')`, depID, detail); e != nil {
		return e
	}

	reason, _ := json.Marshal(&localdocker.Failure{Code: "agent_lost", Message: detail})
	_, e := s.Pool.Exec(ctx, `UPDATE local_publications SET status=CASE WHEN operation='publish' THEN 'failed' ELSE 'unknown' END,reason=CASE WHEN reason IS NULL OR reason='null'::jsonb THEN $2::jsonb ELSE reason END,finished_at=now(),checked_at=now() WHERE active_deployment_id=$1 AND status NOT IN('failed','running','stopped')`, depID, reason)
	return e
}
func (s *Store) RetiredLocalPublications(ctx context.Context) ([]LocalPublication, error) {
	rows, e := s.Pool.Query(ctx, `SELECT `+localCols+` FROM local_publications p WHERE p.status IN('running','unhealthy','unknown') AND EXISTS(SELECT 1 FROM local_publications n WHERE n.env_id=p.env_id AND n.generation>p.generation AND n.container_id<>'' AND n.status IN('running','unhealthy','stopped','unknown'))`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []LocalPublication
	for rows.Next() {
		p, e := scanLocal(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) QueueRetiredLocalStop(ctx context.Context, pubID string) (LocalPublication, error) {
	return s.queueLocalTarget(ctx, pubID, "stop")
}
func (s *Store) queueLocalTarget(ctx context.Context, pubID, action string) (LocalPublication, error) {
	var p LocalPublication
	e := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var envID string
		e := tx.QueryRow(ctx, `SELECT env_id::text FROM local_publications WHERE id=$1`, pubID).Scan(&envID)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR UPDATE`, envID); e != nil {
			return e
		}
		busy, e := localBusy(ctx, tx, envID)
		if e != nil {
			return e
		}
		if busy {
			return ErrPublicationBusy
		}
		p, e = scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_publications WHERE id=$1 FOR UPDATE`, pubID))
		if e != nil {
			return e
		}
		if p.Status == "stopped" && action == "stop" {
			return nil
		}
		var depID string
		e = tx.QueryRow(ctx, `INSERT INTO deployments(env_id,version,initiator) VALUES($1,$2,'platform') RETURNING id::text`, envID, p.Version).Scan(&depID)
		if e != nil {
			return e
		}
		p, e = scanLocal(tx.QueryRow(ctx, `UPDATE local_publications SET desired_state='stopped',status='stopping',operation=$3,active_deployment_id=$2 WHERE id=$1 RETURNING `+localCols, pubID, depID, action))
		return e
	})
	return p, e
}

func (s *Store) FlushPendingLocalPublications(ctx context.Context) error {
	rows, e := s.Pool.Query(ctx, `SELECT q.env_id::text,q.version FROM local_pending_publications q JOIN environments e ON e.id=q.env_id WHERE NOT e.paused AND NOT COALESCE((SELECT desired_state='stopped' FROM local_publications WHERE env_id=e.id AND container_id<>'' ORDER BY generation DESC LIMIT 1),false) AND NOT EXISTS(SELECT 1 FROM deployments d WHERE d.env_id=q.env_id AND d.status IN('queued','deploying','verifying'))`)
	if e != nil {
		return e
	}
	type pending struct{ env, version string }
	var all []pending
	for rows.Next() {
		var p pending
		if e = rows.Scan(&p.env, &p.version); e != nil {
			rows.Close()
			return e
		}
		all = append(all, p)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, p := range all {
		queued, qe := s.EnqueueLocalPublication(ctx, p.env, p.version, "auto", true)
		e = qe
		if e == nil && queued.Version == p.version && queued.Status == "queued" {
			_, e = s.Pool.Exec(ctx, `DELETE FROM local_pending_publications WHERE env_id=$1 AND version=$2`, p.env, p.version)
		}
		if e != nil && !errors.Is(e, ErrPublicationBusy) {
			return e
		}
	}
	return nil
}

func (s *Store) LocalKeepPublications(ctx context.Context, envID string) ([]string, error) {
	rows, e := s.Pool.Query(ctx, `SELECT id::text FROM local_publications WHERE env_id=$1 AND desired_state='running' AND status NOT IN('failed','stopped')`, envID)
	if e != nil {
		return nil, e
	}
	return collectIDs(rows)
}

func (s *Store) RequestLocalDeletion(ctx context.Context, envID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR UPDATE`, envID); e != nil {
			return e
		}
		var busy bool
		e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d LEFT JOIN local_publications p ON p.active_deployment_id=d.id WHERE d.env_id=$1 AND d.status IN('queued','deploying','verifying') AND NOT COALESCE(p.operation IN('reconcile','logs','cleanup','stop','remove'),false))`, envID).Scan(&busy)
		if e != nil {
			return e
		}
		if busy {
			return ErrPublicationBusy
		}
		var running bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_publications WHERE env_id=$1 AND container_id<>'' AND desired_state<>'stopped')`, envID).Scan(&running); e != nil {
			return e
		}
		if running {
			return fmt.Errorf("перед удалением остановите публикацию: %w", ErrConflict)
		}
		_, e = tx.Exec(ctx, `UPDATE environments SET local_delete_requested=true,paused=true WHERE id=$1`, envID)
		return e
	})
}
func (s *Store) LastLocalRemoval(ctx context.Context, envID, pubID string) (bool, error) {
	var last bool
	e := s.Pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM local_publications WHERE env_id=$1 AND id<>$2 AND (image_digest<>'' OR container_id<>''))`, envID, pubID).Scan(&last)
	return last, e
}
func (s *Store) ProcessLocalDeletions(ctx context.Context) error {
	rows, e := s.Pool.Query(ctx, `SELECT id::text FROM environments WHERE local_delete_requested`)
	if e != nil {
		return e
	}
	ids, e := collectIDs(rows)
	if e != nil {
		return e
	}
	for _, envID := range ids {
		var id string
		e = s.Pool.QueryRow(ctx, `SELECT id::text FROM local_publications WHERE env_id=$1 AND ((image_digest<>'' OR container_id<>'') OR NOT (SELECT local_cleanup_completed FROM environments WHERE id=$1)) ORDER BY (image_digest<>'' OR container_id<>'') DESC,generation LIMIT 1`, envID).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			if e = s.DeleteEnvironment(ctx, envID); e != nil && !errors.Is(e, ErrConflict) {
				return e
			}
			continue
		}
		if e != nil {
			return e
		}
		if _, e = s.queueLocalTarget(ctx, id, "remove"); e != nil && !errors.Is(e, ErrPublicationBusy) {
			return e
		}
	}
	return nil
}

func (s *Store) LocalMonitorView(ctx context.Context, envID string) (MonitorView, error) {
	v := MonitorView{EnvID: envID, State: MonitorUnknown, Runs: []MonitorRun{}}
	p, e := s.CurrentLocalPublication(ctx, envID)
	if errors.Is(e, ErrNotFound) {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	switch p.Status {
	case "running":
		v.State = MonitorOK
	case "unhealthy", "failed":
		v.State = MonitorFail
	case "stopped", "stopping":
		v.State = MonitorDisabled
	}
	if p.Reason != nil && v.State != MonitorOK {
		v.Reason = p.Reason.Message
	}
	if e = s.Pool.QueryRow(ctx, `SELECT checked_at,health_failures FROM local_publications WHERE id=$1`, p.ID).Scan(&v.LastAt, &v.ConsecutiveFail); e != nil {
		return v, e
	}
	if v.LastAt != nil && v.State != MonitorDisabled {
		next := v.LastAt.Add(15 * time.Second)
		v.NextAt = &next
	}
	rows, e := s.Pool.Query(ctx, `SELECT at,ok,duration_ms,output FROM monitor_runs WHERE env_id=$1 ORDER BY at DESC,id DESC LIMIT 20`, envID)
	if e != nil {
		return v, e
	}
	defer rows.Close()
	for rows.Next() {
		var run MonitorRun
		if e = rows.Scan(&run.At, &run.OK, &run.DurationMs, &run.Output); e != nil {
			return v, e
		}
		v.Runs = append(v.Runs, run)
	}
	return v, rows.Err()
}
