package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func finishLoadTx(ctx context.Context, tx pgx.Tx, r *LoadRun, state string, reason *loadtest.Failure, report *loadtest.Report, release bool) error {
	rawReason, _ := json.Marshal(reason)
	rawReport, _ := json.Marshal(report)
	_, err := tx.Exec(ctx, `UPDATE load_test_runs SET state=$2,reason=$3,report=$4,finished_at=now(),cancel_pending=$5 WHERE id=$1`, r.ID, state, rawReason, rawReport, !release && r.RunnerID != "")
	if err != nil {
		return err
	}
	if release {
		if err = releaseLoadRunnerTx(ctx, tx, *r); err != nil {
			return err
		}
	}
	r.State, r.Reason, r.Report = state, reason, report
	r.Stopping = !release && r.RunnerID != ""
	return loadEvent(ctx, tx, "load_test.status", *r)
}
func (s *Store) CancelLoadTest(ctx context.Context, id string) (out LoadRun, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var e error
		out, e = scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.id=$1 FOR UPDATE`, id))
		if e != nil {
			return e
		}
		if loadtest.Terminal(out.State) {
			return nil
		}
		var report *loadtest.Report
		if len(out.Evidence) > 0 {
			v, e := loadtest.BuildReport(out.Profile, out.Binding, loadtest.Result{State: "cancelled", Stages: out.Evidence})
			if e == nil {
				report = &v
			}
		}
		return finishLoadTx(ctx, tx, &out, "cancelled", &loadtest.Failure{Code: "cancelled", Message: "отменено пользователем"}, report, out.RunnerID == "")
	})
	return out, err
}
func minLimits(a, b loadtest.Limits) loadtest.Limits {
	return loadtest.Limits{MaxVUs: min(a.MaxVUs, b.MaxVUs), MaxRPS: min(a.MaxRPS, b.MaxRPS), MaxTargetDurationS: min(a.MaxTargetDurationS, b.MaxTargetDurationS), MaxRunDurationS: min(a.MaxRunDurationS, b.MaxRunDurationS)}
}
func (s *Store) ClaimLoadTest(ctx context.Context, limits loadtest.Limits) (out LoadRun, ok bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r, e := scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.state='queued' AND NOT EXISTS(SELECT 1 FROM deployments d WHERE d.env_id=r.env_id AND d.status IN('deploying','verifying')) AND EXISTS(SELECT 1 FROM runners rn WHERE rn.agent='' AND rn.capabilities=ARRAY['load']::text[] AND rn.features @> ARRAY['k6_load_v1']::text[] AND rn.host=r.target->>'host' AND rn.status='idle' AND NOT rn.draining AND NOT rn.retired AND rn.load_test_id IS NULL AND rn.boot_id<>'' AND (NOT (r.profile?'auth') OR rn.secure)) ORDER BY r.created_at,r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1`))
		if errors.Is(e, ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = lockLocalExecutionClaim(ctx, tx, r.EnvironmentID); e != nil {
			return e
		}
		var publicationBusy bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE env_id=$1 AND status IN('deploying','verifying'))`, r.EnvironmentID).Scan(&publicationBusy); e != nil {
			return e
		}
		if publicationBusy {
			return refreshLoadReasonTx(ctx, tx, r)
		}
		current, e := loadTarget(ctx, tx, r.EnvironmentID)
		if errors.Is(e, ErrNotFound) || e == nil && !reflect.DeepEqual(current, r.Target) {
			return finishLoadTx(ctx, tx, &r, "interrupted", &loadtest.Failure{Code: "version_changed", Message: "публикация изменилась до назначения; нужен явный повтор"}, nil, true)
		}
		if e != nil {
			return e
		}
		effective := minLimits(r.Limits, limits)
		if r.Profile.Validate(effective) != nil {
			return finishLoadTx(ctx, tx, &r, "config_needed", &loadtest.Failure{Code: "limits_changed", Message: "лимиты установки изменились; проверьте профиль"}, nil, true)
		}
		var runnerID, boot string
		e = tx.QueryRow(ctx, `SELECT id,boot_id FROM runners WHERE agent='' AND capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND host=$1 AND status='idle' AND NOT draining AND NOT retired AND load_test_id IS NULL AND boot_id<>'' AND (NOT $2 OR secure) ORDER BY last_seen DESC,id FOR UPDATE SKIP LOCKED LIMIT 1`, r.Target.Host, r.Profile.Auth != nil).Scan(&runnerID, &boot)
		if errors.Is(e, pgx.ErrNoRows) {
			reason := loadtest.Failure{Code: "no_load_runner", Message: "ожидает выделенный k6-runner на хосте публикации"}
			var anyRunner, sameHost, secure bool
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runners WHERE agent='' AND capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND status<>'offline' AND NOT draining),EXISTS(SELECT 1 FROM runners WHERE agent='' AND capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND host=$1 AND status<>'offline' AND NOT draining),EXISTS(SELECT 1 FROM runners WHERE agent='' AND capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND host=$1 AND status<>'offline' AND secure AND NOT draining)`, r.Target.Host).Scan(&anyRunner, &sameHost, &secure)
			if e != nil {
				return e
			}
			if anyRunner && !sameHost {
				reason = loadtest.Failure{Code: "wrong_host", Message: "выделенный k6-runner должен работать на хосте публикации"}
			} else if sameHost && r.Profile.Auth != nil && !secure {
				reason = loadtest.Failure{Code: "insecure_channel", Message: "для тестового секрета нужен защищённый канал k6-runner"}
			} else if sameHost {
				reason = loadtest.Failure{Code: "runner_busy", Message: "выделенный k6-runner занят либо недоступен"}
			}
			raw, _ := json.Marshal(reason)
			tag, e := tx.Exec(ctx, `UPDATE load_test_runs SET reason=$2 WHERE id=$1 AND reason IS DISTINCT FROM $2::jsonb`, r.ID, raw)
			if e != nil {
				return e
			}
			if tag.RowsAffected() > 0 {
				r.Reason = &reason
				return loadEvent(ctx, tx, "load_test.status", r)
			}
			return nil
		}
		if e != nil {
			return e
		}
		assignment := uuid.NewString()
		rawLimits, _ := json.Marshal(effective)
		r, e = scanLoadRun(tx.QueryRow(ctx, `UPDATE load_test_runs AS r SET state='running',runner_id=$2,runner_boot_id=$3,assignment_id=$4,limits=$5,reason=NULL,stage='preflight',started_at=now(),deadline_at=now()+make_interval(secs=>$6) WHERE id=$1 RETURNING `+loadRunCols, r.ID, runnerID, boot, assignment, rawLimits, effective.MaxRunDurationS))
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE runners SET status='running',load_test_id=$2,ctx_pct=NULL WHERE id=$1`, runnerID, r.ID); e != nil {
			return e
		}
		out, ok = r, true
		return loadEvent(ctx, tx, "load_test.status", r)
	})
	return out, ok, err
}
func (s *Store) RequeueUndeliveredLoad(ctx context.Context, r LoadRun) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		row, e := scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.id=$1 FOR UPDATE`, r.ID))
		if e != nil {
			return e
		}
		if row.State != "running" || row.AssignmentID != r.AssignmentID || len(row.Evidence) > 0 || row.Stage != "preflight" {
			return nil
		}
		if _, e = tx.Exec(ctx, `UPDATE load_test_runs SET state='queued',runner_id='',runner_boot_id='',assignment_id='',stage='',started_at=NULL,deadline_at=NULL,reason='{"code":"no_channel","message":"канал runner недоступен; нагрузка не отправлена"}' WHERE id=$1`, r.ID); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE runners SET load_test_id=NULL,status=CASE WHEN status='offline' THEN 'offline' ELSE 'idle' END WHERE id=$1 AND load_test_id=$2`, r.RunnerID, r.ID)
		return e
	})
}
func loadOwner(ctx context.Context, tx pgx.Tx, r LoadRun) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runners WHERE id=$1 AND load_test_id=$2 AND boot_id=$3 AND status='running')`, r.RunnerID, r.ID, r.BootID).Scan(&live)
	return live, err
}
func (s *Store) AcceptLoadResult(ctx context.Context, runnerID, runID, assignment, stage string, result loadtest.Result) (out LoadRun, accepted bool, err error) {
	terminalProof := validLoadTerminal(result.State)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r, e := scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.id=$1 FOR UPDATE`, runID))
		if errors.Is(e, ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		if loadtest.Terminal(r.State) && r.RunnerID == runnerID && r.AssignmentID == assignment && validLoadTerminal(result.State) {
			out = r
			return releaseLoadRunnerTx(ctx, tx, r)
		}
		if r.State != "running" || r.RunnerID != runnerID || r.AssignmentID != assignment || r.Deadline == nil || r.Deadline.Before(time.Now()) {
			return nil
		}
		live, e := loadOwner(ctx, tx, r)
		if e != nil {
			return e
		}
		if !live {
			return nil
		}
		current, e := loadTarget(ctx, tx, r.EnvironmentID)
		if e != nil && !errors.Is(e, ErrNotFound) {
			return e
		}
		if e != nil || !reflect.DeepEqual(current, r.Target) {
			out = r
			accepted = true
			reason := &loadtest.Failure{Code: "version_changed", Message: "публикация изменилась во время проверки"}
			version := result.K6Version
			if r.Report != nil {
				version = r.Report.K6Version
			}
			partial, _ := loadtest.BuildReport(r.Profile, r.Binding, loadtest.Result{State: "interrupted", Reason: reason, K6Version: version, Stages: r.Evidence})
			return finishLoadTx(ctx, tx, &out, "interrupted", reason, &partial, terminalProof)
		}
		if len(result.Stages) < len(r.Evidence) {
			return nil
		}
		if !sameLoadEvidence(result.Stages[:len(r.Evidence)], r.Evidence) && len(r.Evidence) > 0 {
			return fmt.Errorf("completed load evidence changed")
		}
		switch result.State {
		case "running", "passed", "failed", "inconclusive", "config_needed", "cancelled", "interrupted":
		default:
			return fmt.Errorf("invalid load result state")
		}
		if result.State == "running" {
			phases := r.Profile.Phases()
			if len(result.Stages) >= len(phases) || stage != phases[len(result.Stages)].Name {
				return fmt.Errorf("invalid load phase checkpoint")
			}
		}
		if len(stage) > 40 {
			return fmt.Errorf("invalid load stage")
		}
		report, e := loadtest.BuildReport(r.Profile, r.Binding, result)
		if e == nil && result.State == "running" && len(report.Stages) > 0 && report.Stages[len(report.Stages)-1].Status != "passed" {
			e = fmt.Errorf("cannot grant load after nonpassing stage")
		}
		if e != nil {
			version := result.K6Version
			if r.Report != nil {
				version = r.Report.K6Version
			}
			result = loadtest.Result{State: "inconclusive", K6Version: version, Stages: r.Evidence, Reason: &loadtest.Failure{Code: "invalid_summary", Message: "измерения k6 не прошли проверку"}}
			report, _ = loadtest.BuildReport(r.Profile, r.Binding, result)
		}
		rawEvidence, _ := json.Marshal(result.Stages)
		rawReport, _ := json.Marshal(report)
		if _, e = tx.Exec(ctx, `UPDATE load_test_runs SET stage=$2,evidence=$3,report=$4 WHERE id=$1`, r.ID, stage, rawEvidence, rawReport); e != nil {
			return e
		}
		r.Stage, r.Evidence, r.Report = stage, result.Stages, &report
		accepted, out = true, r
		if result.State == "running" {
			return loadEvent(ctx, tx, "load_test.progress", r)
		}
		state := loadtest.ReportState(report)
		return finishLoadTx(ctx, tx, &out, state, result.Reason, &report, terminalProof)
	})
	return out, accepted, err
}

// ReconcileLoadRuns returns cancelled ownerships so the engine can stop native
// processes. It never requeues an uncertain run or silently targets a new app.
func (s *Store) ReconcileLoadRuns(ctx context.Context) (stopped []LoadRun, err error) {
	stopped = []LoadRun{}
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM load_test_runs WHERE state IN('running','queued') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			r, e := scanLoadRun(tx.QueryRow(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE r.id=$1 FOR UPDATE`, id))
			if e != nil {
				return e
			}
			if loadtest.Terminal(r.State) {
				return nil
			}
			current, e := loadTarget(ctx, tx, r.EnvironmentID)
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
			reason := (*loadtest.Failure)(nil)
			if e != nil || !reflect.DeepEqual(current, r.Target) {
				reason = &loadtest.Failure{Code: "version_changed", Message: "рабочая публикация изменилась; нужен явный повтор"}
			}
			if r.State == "running" {
				live, e := loadOwner(ctx, tx, r)
				if e != nil {
					return e
				}
				if !live {
					reason = &loadtest.Failure{Code: "runner_lost", Message: "назначение прервано; нагрузка автоматически не повторяется"}
				}
				if r.Deadline == nil || r.Deadline.Before(time.Now()) {
					reason = &loadtest.Failure{Code: "deadline", Message: "истёк конечный дедлайн нагрузки"}
				}
			}
			if reason == nil {
				if r.State == "queued" {
					return refreshLoadReasonTx(ctx, tx, r)
				}
				return nil
			}
			var report *loadtest.Report
			if len(r.Evidence) > 0 {
				v, e := loadtest.BuildReport(r.Profile, r.Binding, loadtest.Result{State: "interrupted", Reason: reason, Stages: r.Evidence})
				if e == nil {
					report = &v
				}
			}
			if e = finishLoadTx(ctx, tx, &r, "interrupted", reason, report, r.RunnerID == "" || r.Deadline != nil && r.Deadline.Before(time.Now())); e != nil {
				return e
			}
			stopped = append(stopped, r)
			return nil
		})
		if err != nil {
			return stopped, err
		}
	}
	return stopped, nil
}
func (s *Store) LoadRunnerRefs(ctx context.Context) (map[string]*domain.LoadTestRef, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.id,r.project_id::text,r.env_id::text,r.release_id::text,CASE WHEN r.cancel_pending THEN 'stopping' ELSE r.stage END,r.runner_id FROM load_test_runs r JOIN runners rn ON rn.load_test_id=r.id WHERE (r.state='running' OR r.cancel_pending)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*domain.LoadTestRef{}
	for rows.Next() {
		var ref domain.LoadTestRef
		var runner string
		if err = rows.Scan(&ref.ID, &ref.ProjectID, &ref.EnvironmentID, &ref.ReleaseID, &ref.Stage, &runner); err != nil {
			return nil, err
		}
		out[runner] = &ref
	}
	return out, rows.Err()
}

func validLoadTerminal(state string) bool {
	switch state {
	case "passed", "failed", "inconclusive", "config_needed", "cancelled", "interrupted":
		return true
	}
	return false
}
func releaseLoadRunnerTx(ctx context.Context, tx pgx.Tx, r LoadRun) error {
	if _, err := tx.Exec(ctx, `UPDATE runners SET load_test_id=NULL,status=CASE WHEN status='offline' THEN 'offline' ELSE 'idle' END WHERE id=$1 AND load_test_id=$2`, r.RunnerID, r.ID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE load_test_runs SET cancel_pending=false WHERE id=$1 AND cancel_pending`, r.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		r.Stopping = false
		return loadEvent(ctx, tx, "load_test.status", r)
	}
	return nil
}
func interruptLoadEnvTx(ctx context.Context, tx pgx.Tx, envID string) error {
	rows, err := tx.Query(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE env_id=$1 AND state IN('queued','running') FOR UPDATE`, envID)
	if err != nil {
		return err
	}
	var runs []LoadRun
	for rows.Next() {
		r, e := scanLoadRun(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, r := range runs {
		reason := &loadtest.Failure{Code: "publication_changed", Message: "публикация изменяется; предыдущая нагрузка прервана"}
		var report *loadtest.Report
		if len(r.Evidence) > 0 {
			v, e := loadtest.BuildReport(r.Profile, r.Binding, loadtest.Result{State: "interrupted", Reason: reason, Stages: r.Evidence})
			if e == nil {
				report = &v
			}
		}
		if err = finishLoadTx(ctx, tx, &r, "interrupted", reason, report, r.RunnerID == ""); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) PendingLoadCancels(ctx context.Context) ([]LoadRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+loadRunCols+` FROM load_test_runs r WHERE cancel_pending ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var out []LoadRun
	for rows.Next() {
		r, e := scanLoadRun(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if r.Deadline == nil || r.Deadline.Before(time.Now()) {
			if err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return releaseLoadRunnerTx(ctx, tx, r) }); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
func (s *Store) LoadAssignmentActive(ctx context.Context, r LoadRun) (bool, error) {
	var active bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM load_test_runs l JOIN runners rn ON rn.load_test_id=l.id WHERE l.id=$1 AND l.state='running' AND l.assignment_id=$2 AND l.runner_id=$3 AND l.runner_boot_id=rn.boot_id AND l.deadline_at>now())`, r.ID, r.AssignmentID, r.RunnerID).Scan(&active)
	return active, err
}

// Diagnostics are independent of compatible-only claiming: an unrunnable oldest
// entry neither blocks other hosts nor retains a stale reason.
func refreshLoadReasonTx(ctx context.Context, tx pgx.Tx, r LoadRun) error {
	var anyRunner, sameHost, secure, idle, publicationBusy bool
	err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM runners WHERE capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND agent='' AND status<>'offline' AND NOT draining AND NOT retired AND boot_id<>''),
 EXISTS(SELECT 1 FROM runners WHERE capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND agent='' AND status<>'offline' AND NOT draining AND NOT retired AND boot_id<>'' AND host=$1),
 EXISTS(SELECT 1 FROM runners WHERE capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND agent='' AND status<>'offline' AND NOT draining AND NOT retired AND boot_id<>'' AND host=$1 AND secure),
 EXISTS(SELECT 1 FROM runners WHERE capabilities=ARRAY['load']::text[] AND features @> ARRAY['k6_load_v1']::text[] AND agent='' AND status='idle' AND NOT draining AND NOT retired AND boot_id<>'' AND host=$1 AND load_test_id IS NULL AND (NOT $2 OR secure)), EXISTS(SELECT 1 FROM deployments WHERE env_id=$3 AND status IN('deploying','verifying'))`, r.Target.Host, r.Profile.Auth != nil, r.EnvironmentID).Scan(&anyRunner, &sameHost, &secure, &idle, &publicationBusy)
	if err != nil {
		return err
	}
	reason := loadtest.Failure{Code: "no_load_runner", Message: "ожидает выделенный k6-runner на хосте публикации"}
	switch {
	case publicationBusy:
		reason = loadtest.Failure{Code: "publication_busy", Message: "ожидает завершения операции Docker-публикации"}
	case idle:
		reason = loadtest.Failure{Code: "waiting_assignment", Message: "ожидает назначения выделенному k6-runner"}
	case anyRunner && !sameHost:
		reason = loadtest.Failure{Code: "wrong_host", Message: "выделенный k6-runner должен работать на хосте публикации"}
	case sameHost && r.Profile.Auth != nil && !secure:
		reason = loadtest.Failure{Code: "insecure_channel", Message: "для тестового секрета нужен защищённый канал k6-runner"}
	case sameHost:
		reason = loadtest.Failure{Code: "runner_busy", Message: "выделенный k6-runner занят либо недоступен"}
	}
	raw, _ := json.Marshal(reason)
	tag, err := tx.Exec(ctx, `UPDATE load_test_runs SET reason=$2 WHERE id=$1 AND reason IS DISTINCT FROM $2::jsonb`, r.ID, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		r.Reason = &reason
		return loadEvent(ctx, tx, "load_test.status", r)
	}
	return nil
}

// PostgreSQL jsonb normalizes object ordering and whitespace. Compare semantic
// summaries rather than RawMessage bytes when fencing immutable prior stages.
func sameLoadEvidence(a, b []loadtest.Evidence) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Phase != b[i].Phase || a[i].ExitCode != b[i].ExitCode {
			return false
		}
		var x, y any
		if json.Unmarshal(a[i].Summary, &x) != nil || json.Unmarshal(b[i].Summary, &y) != nil || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// Both claim paths lock this key, then recheck the other queue in a fresh
// READ COMMITTED statement. Locks are per environment; execution stays parallel.
func lockLocalExecutionClaim(ctx context.Context, tx pgx.Tx, envID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "local-execution/"+envID)
	return err
}
