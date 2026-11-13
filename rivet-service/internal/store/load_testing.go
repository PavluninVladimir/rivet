package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type LoadError struct{ Code, Message string }

func (e *LoadError) Unwrap() error       { return ErrConflict }
func (e *LoadError) Error() string       { return e.Code }
func loadErr(code, message string) error { return &LoadError{code, message} }

type LoadPrompt struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	FirstReleaseID string    `json:"first_release_id"`
	Updated        time.Time `json:"updated_at"`
}
type LoadRun struct {
	Stopping      bool   `json:"stopping"`
	ID            string `json:"id"`
	RequestID     string `json:"-"`
	EnvironmentID string `json:"environment_id"`
	ProjectID     string `json:"project_id"`
	loadtest.Binding
	Profile      loadtest.Profile    `json:"profile"`
	Limits       loadtest.Limits     `json:"-"`
	Target       loadtest.Target     `json:"-"`
	State        string              `json:"state"`
	Reason       *loadtest.Failure   `json:"reason"`
	RunnerID     string              `json:"runner_id"`
	AssignmentID string              `json:"-"`
	BootID       string              `json:"-"`
	Stage        string              `json:"stage"`
	Created      time.Time           `json:"created_at"`
	Started      *time.Time          `json:"started_at"`
	Finished     *time.Time          `json:"finished_at"`
	Deadline     *time.Time          `json:"deadline_at"`
	Report       *loadtest.Report    `json:"report"`
	Evidence     []loadtest.Evidence `json:"-"`
}

const loadRunCols = `r.id::text,r.request_id::text,r.env_id::text,r.project_id::text,r.release_id::text,r.publication_id::text,r.version,r.generation,r.profile,r.limits,r.target,r.state,r.reason,r.runner_id,r.assignment_id,r.runner_boot_id,r.stage,r.cancel_pending,r.created_at,r.started_at,r.finished_at,r.deadline_at,r.report,r.evidence`

func scanLoadRun(row pgx.Row) (LoadRun, error) {
	var r LoadRun
	var profile, limits, target, reason, report, evidence []byte
	err := row.Scan(&r.ID, &r.RequestID, &r.EnvironmentID, &r.ProjectID, &r.ReleaseID, &r.PublicationID, &r.Version, &r.Generation, &profile, &limits, &target, &r.State, &reason, &r.RunnerID, &r.AssignmentID, &r.BootID, &r.Stage, &r.Stopping, &r.Created, &r.Started, &r.Finished, &r.Deadline, &report, &evidence)
	if err != nil {
		return r, nf(err)
	}
	for _, v := range []struct {
		raw []byte
		dst any
	}{{profile, &r.Profile}, {limits, &r.Limits}, {target, &r.Target}, {reason, &r.Reason}, {report, &r.Report}, {evidence, &r.Evidence}} {
		if len(v.raw) > 0 {
			if err := json.Unmarshal(v.raw, v.dst); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}
func loadEvent(ctx context.Context, tx pgx.Tx, kind string, r LoadRun) error {
	_, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorScheduler, Type: kind, ProjectID: r.ProjectID, Text: "нагрузочное тестирование: " + r.State, Payload: map[string]any{"environment_id": r.EnvironmentID, "release_id": r.ReleaseID, "run_id": r.ID, "state": r.State, "stage": r.Stage, "stopping": r.Stopping}})
	return err
}

// The original publication's release is used, never a technical health job.
func loadTarget(ctx context.Context, q dbtx, envID string) (loadtest.Target, error) {
	var t loadtest.Target
	err := q.QueryRow(ctx, `SELECT e.project_id::text,e.id::text,rel.id::text,p.id::text,p.version,p.generation,p.container_id,p.host_port,COALESCE((p.config->>'container_port')::int,0),rn.host
 FROM environments e JOIN local_publications p ON p.env_id=e.id
 JOIN releases rel ON rel.deployment_id=p.deployment_id JOIN runners rn ON rn.id=p.runner_id
 WHERE e.id=$1 AND e.exec_type='local_docker' AND NOT e.local_delete_requested
 AND p.status='running' AND p.desired_state='running' AND p.container_id<>'' AND rn.host<>''
 AND NOT EXISTS(SELECT 1 FROM local_publications newer WHERE newer.env_id=e.id AND newer.generation>=p.generation AND newer.status IN ('queued','building','starting','verifying','stopping'))
 ORDER BY p.generation DESC LIMIT 1`, envID).Scan(&t.ProjectID, &t.EnvironmentID, &t.ReleaseID, &t.PublicationID, &t.Version, &t.Generation, &t.ContainerID, &t.Port, &t.ContainerPort, &t.Host)
	return t, nf(err)
}
func ensureLoadPlanTx(ctx context.Context, tx pgx.Tx, releaseID string) error {
	var id, envID, projectID string
	err := tx.QueryRow(ctx, `INSERT INTO load_test_plans(env_id,first_release_id)
 SELECT e.id,rel.id FROM releases rel JOIN environments e ON e.id=rel.env_id
 JOIN local_publications p ON p.deployment_id=rel.deployment_id
 WHERE rel.id=$1 AND e.exec_type='local_docker' AND NOT e.local_delete_requested AND p.status='running' AND p.desired_state='running'
 ON CONFLICT(env_id) DO NOTHING RETURNING id::text,env_id::text,(SELECT project_id::text FROM environments WHERE id=env_id)`, releaseID).Scan(&id, &envID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return loadEvent(ctx, tx, "load_test.prompt_changed", LoadRun{ID: id, EnvironmentID: envID, ProjectID: projectID, State: "awaiting_target", Binding: loadtest.Binding{ReleaseID: releaseID}})
}
func (s *Store) ReconcileLoadPlans(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON(e.id) rel.id::text FROM environments e JOIN local_publications p ON p.env_id=e.id JOIN releases rel ON rel.deployment_id=p.deployment_id WHERE e.exec_type='local_docker' AND NOT e.local_delete_requested AND p.status='running' AND p.desired_state='running' AND NOT EXISTS(SELECT 1 FROM load_test_plans lp WHERE lp.env_id=e.id) ORDER BY e.id,p.generation DESC`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return ensureLoadPlanTx(ctx, tx, id) }); err != nil {
			return err
		}
	}
	return nil
}

type LoadView struct {
	EnvironmentID string            `json:"environment_id"`
	Name          string            `json:"name"`
	ProjectID     string            `json:"project_id"`
	Supported     bool              `json:"supported"`
	Ready         bool              `json:"ready"`
	Reason        *loadtest.Failure `json:"reason"`
	Binding       *loadtest.Binding `json:"binding"`
	Limits        loadtest.Limits   `json:"limits"`
	CanManage     bool              `json:"can_manage"`
	Prompt        *LoadPrompt       `json:"prompt"`
	SavedProfile  *loadtest.Profile `json:"saved_profile"`
	Latest        *LoadRun          `json:"latest_run"`
}

func (s *Store) LoadTestingView(ctx context.Context, env domain.Environment, limits loadtest.Limits) (LoadView, error) {
	v := LoadView{EnvironmentID: env.ID, ProjectID: env.ProjectID, Name: env.Name, Supported: env.ExecType == domain.ExecLocalDocker, Limits: limits}
	if !v.Supported {
		v.Reason = &loadtest.Failure{Code: "unsupported_environment", Message: "V1 поддерживает только local_docker"}
		return v, nil
	}
	t, err := loadTarget(ctx, s.Pool, env.ID)
	if err == nil {
		v.Ready = true
		v.Binding = &t.Binding
	} else if !errors.Is(err, ErrNotFound) {
		return v, err
	} else {
		v.Reason = &loadtest.Failure{Code: "publication_not_ready", Message: "нужна текущая здоровая рабочая публикация"}
	}
	var p LoadPrompt
	var raw []byte
	err = s.Pool.QueryRow(ctx, `SELECT id::text,state,first_release_id::text,updated_at,profile FROM load_test_plans WHERE env_id=$1`, env.ID).Scan(&p.ID, &p.State, &p.FirstReleaseID, &p.Updated, &raw)
	if err == nil {
		v.Prompt = &p
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &v.SavedProfile); err != nil {
				return v, err
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	run, err := scanLoadRun(s.Pool.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.env_id=$1 ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, env.ID))
	if err == nil {
		v.Latest = &run
	} else if !errors.Is(err, ErrNotFound) {
		return v, err
	}
	return v, nil
}
func (s *Store) DeferLoadPrompt(ctx context.Context, envID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID, releaseID string
		err := tx.QueryRow(ctx, `UPDATE load_test_plans lp SET state='deferred',updated_at=now() WHERE lp.env_id=$1 AND lp.state='awaiting_target' RETURNING (SELECT project_id::text FROM environments WHERE id=lp.env_id),first_release_id::text`, envID).Scan(&projectID, &releaseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return loadEvent(ctx, tx, "load_test.prompt_changed", LoadRun{EnvironmentID: envID, ProjectID: projectID, State: "deferred", Binding: loadtest.Binding{ReleaseID: releaseID}})
	})
}

type LoadStart struct {
	RequestID string           `json:"request_id"`
	Binding   loadtest.Binding `json:"binding"`
	Profile   loadtest.Profile `json:"profile"`
}

func (s *Store) StartLoadTest(ctx context.Context, envID string, in LoadStart, l loadtest.Limits) (out LoadRun, err error) {
	if _, e := uuid.Parse(in.RequestID); e != nil {
		return out, &loadtest.FieldError{Field: "request_id", Message: "нужен UUID идемпотентности"}
	}
	if err = in.Profile.Validate(l); err != nil {
		return out, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var kind string
		if e := tx.QueryRow(ctx, `SELECT exec_type FROM environments WHERE id=$1 FOR UPDATE`, envID).Scan(&kind); e != nil {
			return nf(e)
		}
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "load-request/"+in.RequestID); e != nil {
			return e
		}
		if kind != domain.ExecLocalDocker {
			return loadErr("unsupported_environment", "V1 поддерживает только local_docker")
		}
		prior, e := scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE request_id=$1`, in.RequestID))
		if e == nil {
			if prior.EnvironmentID != envID || prior.Binding != in.Binding || !reflect.DeepEqual(prior.Profile, in.Profile) {
				return loadErr("idempotency_conflict", "request_id уже использован для других параметров")
			}
			out = prior
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		t, e := loadTarget(ctx, tx, envID)
		if errors.Is(e, ErrNotFound) {
			return loadErr("publication_not_ready", "нужна текущая здоровая публикация")
		}
		if e != nil {
			return e
		}
		if t.Binding != in.Binding {
			return loadErr("version_changed", "рабочая версия изменилась; проверьте актуальную привязку")
		}
		var busy bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM load_test_runs WHERE env_id=$1 AND (state IN('queued','running') OR cancel_pending))`, envID).Scan(&busy); e != nil {
			return e
		}
		if busy {
			return loadErr("load_test_busy", "нагрузочный тест уже ожидает или выполняется")
		}
		if in.Profile.Auth != nil {
			var exists bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_secrets WHERE env_id=$1 AND name=$2)`, envID, in.Profile.Auth.SecretName).Scan(&exists); e != nil {
				return e
			}
			if !exists {
				return &loadtest.FieldError{Field: "auth.secret_name", Message: "нет выбранного тестового секрета окружения"}
			}
		}
		profile, _ := json.Marshal(in.Profile)
		limits, _ := json.Marshal(l)
		target, _ := json.Marshal(t)
		out, e = scanLoadRun(tx.QueryRow(ctx, `INSERT INTO load_test_runs AS r(request_id,env_id,project_id,release_id,publication_id,version,generation,profile,limits,target,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'{"code":"no_load_runner","message":"ожидает выделенный k6-runner на хосте публикации"}') RETURNING `+loadRunCols, in.RequestID, envID, t.ProjectID, t.ReleaseID, t.PublicationID, t.Version, t.Generation, profile, limits, target))
		if e != nil {
			return e
		}
		if e = ensureLoadPlanTx(ctx, tx, t.ReleaseID); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE load_test_plans SET state='answered',profile=$2,updated_at=now() WHERE env_id=$1`, envID, profile); e != nil {
			return e
		}
		return loadEvent(ctx, tx, "load_test.status", out)
	})
	return out, err
}
func (s *Store) GetLoadRun(ctx context.Context, id string) (LoadRun, error) {
	return scanLoadRun(s.Pool.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.id=$1`, id))
}
func (s *Store) LoadRunForViewer(ctx context.Context, id, viewer string) (LoadRun, error) {
	return scanLoadRun(s.Pool.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r JOIN project_members m ON m.project_id=r.project_id AND m.user_id=$2 WHERE r.id=$1`, id, viewer))
}
func (s *Store) LoadHistory(ctx context.Context, envID, releaseID, before string, limit int) ([]LoadRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE ($1='' OR r.env_id=NULLIF($1,'')::uuid) AND ($2='' OR r.release_id=NULLIF($2,'')::uuid) AND ($3='' OR (r.created_at,r.id)<(SELECT created_at,id FROM load_test_runs WHERE id=NULLIF($3,'')::uuid AND ($1='' OR env_id=NULLIF($1,'')::uuid) AND ($2='' OR release_id=NULLIF($2,'')::uuid))) ORDER BY r.created_at DESC,r.id DESC LIMIT $4`, envID, releaseID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoadRun{}
	for rows.Next() {
		r, e := scanLoadRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Explicitly scoped to a live load assignment; it never returns all app secrets.
func (s *Store) LoadAuthValue(ctx context.Context, run LoadRun, box *secretbox.Box) (string, error) {
	if run.Profile.Auth == nil {
		return "", nil
	}
	if !box.Enabled() {
		return "", secretbox.ErrNoKey
	}
	var encrypted []byte
	err := s.Pool.QueryRow(ctx, `SELECT es.value_enc FROM environment_secrets es JOIN load_test_runs l ON l.env_id=es.env_id JOIN runners r ON r.load_test_id=l.id WHERE l.id=$1 AND l.state='running' AND l.runner_id=$2 AND l.assignment_id=$3 AND r.id=$2 AND r.secure AND r.boot_id=l.runner_boot_id AND l.deadline_at>now() AND es.name=$4`, run.ID, run.RunnerID, run.AssignmentID, run.Profile.Auth.SecretName).Scan(&encrypted)
	if err != nil {
		return "", nf(err)
	}
	value, err := box.Open(encrypted)
	return value, err
}
