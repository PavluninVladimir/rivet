package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/ops"
	"github.com/PavluninVladimir/rivet/internal/redact"
)

// Наблюдение за окружениями (add-sdlc-cycle, спека operations «Проверки
// наблюдения»): состояние по окружению, расписание, история проверок.

// Параметры планировщика наблюдения.
const (
	// MonitorPostpone — перенос проверки при идущей публикации в окружение.
	MonitorPostpone = 30 * time.Second
	// monitorPendingStale — проверка без результата дольше этого считается
	// потерянной и ставится заново.
	monitorPendingStale = 3 * time.Minute
	// monitorRunOutput — хвост вывода в истории проверок (4 КиБ).
	monitorRunOutput = 4 << 10
	// MonitorRunsRetention — ретенция истории проверок.
	MonitorRunsRetention = 7 * 24 * time.Hour
	// MonitorRunnerControlPlane — исполнитель URL-проверки.
	MonitorRunnerControlPlane = "control-plane"
)

// Состояния наблюдения окружения.
const (
	MonitorOK       = "ok"
	MonitorFail     = "fail"
	MonitorUnknown  = "unknown"
	MonitorDisabled = "disabled"
)

// MonitorDue — окружение, которому пора на проверку.
type MonitorDue struct {
	Env       domain.Environment
	ProjectID string
	Repo      string
	// Deploying — идёт публикация в это окружение: проверка переносится.
	Deploying bool
}

// syncMonitorStates — строки состояния у окружений с наблюдением и
// отметка disabled у выключенных.
func (s *Store) syncMonitorStates(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO monitor_state (env_id, state, next_at)
		SELECT id, 'unknown', now() FROM environments
		WHERE COALESCE((config->'monitor'->>'enabled')::boolean, false)
		ON CONFLICT (env_id) DO NOTHING`); err != nil {
		return err
	}
	rows, err := s.Pool.Query(ctx, `
		UPDATE monitor_state ms SET state='disabled', reason='', pending_check_id='', pending_runner='',
			pending_since=NULL, recheck_at=NULL, last_change_at=now()
		FROM environments e
		WHERE e.id = ms.env_id AND ms.state <> 'disabled'
		  AND NOT COALESCE((e.config->'monitor'->>'enabled')::boolean, false)
		RETURNING ms.env_id::text, e.project_id::text`)
	if err != nil {
		return err
	}
	type changed struct{ env, project string }
	var list []changed
	for rows.Next() {
		var c changed
		if err := rows.Scan(&c.env, &c.project); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range list {
		if _, err := s.AppendEvent(ctx, EventInput{
			ActorKind: domain.ActorSystem, Type: "monitor.state", ProjectID: c.project,
			Text:    "наблюдение окружения выключено",
			Payload: map[string]any{"env_id": c.env, "state": MonitorDisabled, "previous": nil},
		}); err != nil {
			return err
		}
	}
	// Включённое снова наблюдение: состояние unknown до первой проверки.
	_, err = s.Pool.Exec(ctx, `
		UPDATE monitor_state ms SET state='unknown', reason='', next_at=now(), last_change_at=now()
		FROM environments e
		WHERE e.id = ms.env_id AND ms.state = 'disabled'
		  AND COALESCE((e.config->'monitor'->>'enabled')::boolean, false)`)
	return err
}

// DueMonitors — окружения, которым пора на проверку: по интервалу или
// повторная после публикации, без идущей проверки.
func (s *Store) DueMonitors(ctx context.Context) ([]MonitorDue, error) {
	if err := s.syncMonitorStates(ctx); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id::text, e.project_id::text, e.name, e.exec_type, e.trigger, e.config, e.paused, e.runner_caps,
			e.created_at, p.id::text, p.repo_path,
			-- Откладывает только идущая выкатка или откат: публикация в очереди
			-- (окружение на паузе) наблюдение не замораживает.
			EXISTS (SELECT 1 FROM deployments d WHERE d.env_id = e.id AND d.status IN ('deploying','verifying'))
		FROM monitor_state ms
		JOIN environments e ON e.id = ms.env_id
		JOIN projects p ON p.id = e.project_id
		WHERE e.exec_type<>'local_docker' AND COALESCE((e.config->'monitor'->>'enabled')::boolean, false)
		  AND (ms.next_at <= now() OR ms.recheck_at <= now())
		  AND (ms.pending_check_id = '' OR ms.pending_since < now() - $1::interval)
		ORDER BY LEAST(ms.next_at, COALESCE(ms.recheck_at, ms.next_at))`, monitorPendingStale.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitorDue
	for rows.Next() {
		var d MonitorDue
		var cfg []byte
		if err := rows.Scan(&d.Env.ID, &d.Env.ProjectID, &d.Env.Name, &d.Env.ExecType, &d.Env.Trigger, &cfg,
			&d.Env.Paused, &d.Env.RunnerCaps, &d.Env.Created, &d.ProjectID, &d.Repo, &d.Deploying); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(cfg, &d.Env.Config); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PostponeMonitor — перенос проверки (идёт публикация).
func (s *Store) PostponeMonitor(ctx context.Context, envID string, d time.Duration) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE monitor_state SET
			next_at = GREATEST(next_at, now() + $2::interval),
			recheck_at = CASE WHEN recheck_at IS NOT NULL AND recheck_at <= now() THEN now() + $2::interval ELSE recheck_at END
		WHERE env_id=$1`, envID, d.String())
	return err
}

// MonitorRunner — online-runner для проверки окружения: deploy и
// capability окружения, объявлена feature monitor, а для URL-проверки ещё
// monitor_url (runner первой версии исполнил бы пустую команду как успех).
// Свободные первыми.
func (s *Store) MonitorRunner(ctx context.Context, env domain.Environment, urlCheck bool) (string, bool, error) {
	caps := env.RunnerCaps
	if caps == nil {
		caps = []string{}
	}
	features := []string{"monitor"}
	if urlCheck {
		features = append(features, "monitor_url")
	}
	var id string
	err := s.Pool.QueryRow(ctx, `
		SELECT id FROM runners
		WHERE status <> 'offline' AND NOT draining
		  AND capabilities @> ARRAY['deploy'] AND capabilities @> $1::text[]
 AND NOT (capabilities @> ARRAY['docker'] AND features @> ARRAY['local_docker_v1'])
		  AND features @> $2::text[]
          AND (NOT $4 OR EXISTS (SELECT 1 FROM env_hosts h WHERE h.env_id=$3::uuid AND h.state='ready' AND h.local_runner=runners.id))
		ORDER BY (status = 'idle') DESC, cardinality(capabilities), last_seen DESC LIMIT 1`, caps, features, env.ID, env.Config.Provision != nil && env.Config.Provision.Provider == domain.ProvisionLocal).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// StartMonitorCheck — проверка отправлена исполнителю.
func (s *Store) StartMonitorCheck(ctx context.Context, envID, checkID, runnerID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE monitor_state SET pending_check_id=$2, pending_runner=$3, pending_since=now()
		WHERE env_id=$1 AND (pending_check_id = '' OR pending_since < now() - $4::interval)`,
		envID, checkID, runnerID, monitorPendingStale.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MonitorCheckEnv — окружение идущей проверки и её исполнитель.
func (s *Store) MonitorCheckEnv(ctx context.Context, checkID string) (envID, runnerID string, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT env_id::text, pending_runner FROM monitor_state WHERE pending_check_id=$1`, checkID).
		Scan(&envID, &runnerID)
	return envID, runnerID, nf(err)
}

// monitorStateChangeTx — событие monitor.state при смене состояния.
func monitorStateChangeTx(ctx context.Context, tx pgx.Tx, projectID, envID, envName, prev, state, reason string) error {
	if prev == state {
		return nil
	}
	text := fmt.Sprintf("наблюдение %s: ", envName)
	switch state {
	case MonitorOK:
		text += "проверка проходит"
	case MonitorFail:
		text += "проверка не прошла"
	case MonitorUnknown:
		text += "результат неизвестен: " + reason
	default:
		text += state
	}
	var previous any = prev
	if prev == "" {
		previous = nil
	}
	_, err := appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "monitor.state", ProjectID: projectID, Text: text,
		Payload: map[string]any{"env_id": envID, "state": state, "previous": previous},
	})
	return err
}

// MonitorUnknown — проверка не выполнена (нет runner'а): состояние unknown
// с причиной, инцидент не открывается.
func (s *Store) MonitorUnknown(ctx context.Context, envID, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var prev, projectID, envName string
		var cfg []byte
		if err := tx.QueryRow(ctx, `
			SELECT ms.state, e.project_id::text, e.name, e.config FROM monitor_state ms
			JOIN environments e ON e.id = ms.env_id WHERE ms.env_id=$1 FOR UPDATE OF ms`, envID).
			Scan(&prev, &projectID, &envName, &cfg); err != nil {
			return nf(err)
		}
		return monitorUnknownTx(ctx, tx, projectID, envID, envName, prev, reason, monitorInterval(cfg))
	})
}

// monitorUnknownTx — состояние unknown, ожидание снято, следующая проверка
// через after (0 — на ближайшем тике).
func monitorUnknownTx(ctx context.Context, tx pgx.Tx, projectID, envID, envName, prev, reason string, after time.Duration) error {
	if _, err := tx.Exec(ctx, `
		UPDATE monitor_state SET state='unknown', reason=$2, next_at=now() + $3::interval,
			pending_check_id='', pending_runner='', pending_since=NULL,
			recheck_at = CASE WHEN recheck_at <= now() THEN NULL ELSE recheck_at END,
			last_change_at = CASE WHEN state <> 'unknown' THEN now() ELSE last_change_at END
		WHERE env_id=$1`, envID, clipRunes(reason, 300), after.String()); err != nil {
		return err
	}
	return monitorStateChangeTx(ctx, tx, projectID, envID, envName, prev, MonitorUnknown, reason)
}

// CleanText — текст, который можно записать в Postgres: валидный UTF-8 без
// NUL (вывод программ бывает бинарным или обрезанным посреди символа).
func CleanText(s string) string { return ops.Clean(s) }

// MonitorCheck — идущая проверка наблюдения.
type MonitorCheck struct {
	EnvID, CheckID, RunnerID string
	// RunnerOnline — исполнитель в сети (для причины unknown).
	RunnerOnline bool
}

// StaleMonitorChecks — проверки без результата дольше age (полного
// бюджета проверки с запасом).
func (s *Store) StaleMonitorChecks(ctx context.Context, age time.Duration) ([]MonitorCheck, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ms.env_id::text, ms.pending_check_id, ms.pending_runner,
			EXISTS (SELECT 1 FROM runners r WHERE r.id = ms.pending_runner AND r.status <> 'offline')
		FROM monitor_state ms
		WHERE ms.pending_check_id <> '' AND ms.pending_since < now() - $1::interval`, age.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitorCheck
	for rows.Next() {
		var c MonitorCheck
		if err := rows.Scan(&c.EnvID, &c.CheckID, &c.RunnerID, &c.RunnerOnline); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MonitorCheckUnknown — проверка checkID без результата: unknown с
// причиной, если это всё ещё текущая проверка окружения (новую не снимает).
// retryNow — проверка потеряна (обрыв доставки, рестарт rivetd) и
// повторяется на ближайшем тике; иначе (адрес закрыт политикой, задание не
// доставлено) следующая — через интервал окружения.
func (s *Store) MonitorCheckUnknown(ctx context.Context, envID, checkID, reason string, retryNow bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var prev, projectID, envName string
		var cfg []byte
		err := tx.QueryRow(ctx, `
			SELECT ms.state, e.project_id::text, e.name, e.config FROM monitor_state ms
			JOIN environments e ON e.id = ms.env_id WHERE ms.env_id=$1 AND ms.pending_check_id=$2 FOR UPDATE OF ms`,
			envID, checkID).Scan(&prev, &projectID, &envName, &cfg)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		after := time.Duration(0)
		if !retryNow {
			after = monitorInterval(cfg)
		}
		return monitorUnknownTx(ctx, tx, projectID, envID, envName, prev, reason, after)
	})
}

// AbandonControlPlaneChecks — старт rivetd: URL-проверки, которые вёл
// прошлый процесс, потеряны. Они получают unknown и повторяются сразу.
func (s *Store) AbandonControlPlaneChecks(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT env_id::text, pending_check_id FROM monitor_state
		WHERE pending_runner=$1 AND pending_check_id <> ''`, MonitorRunnerControlPlane)
	if err != nil {
		return err
	}
	type check struct{ env, id string }
	var list []check
	for rows.Next() {
		var c check
		if err := rows.Scan(&c.env, &c.id); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range list {
		if err := s.MonitorCheckUnknown(ctx, c.env, c.id, "rivetd перезапущен во время проверки", true); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseMonitorChecks — runner отключился или переподключился: его
// проверки прерваны, ожидание снимается, проверка повторяется сразу.
// Вызывающий отвечает за то, что это проверки именно прерванной сессии.
func (s *Store) ReleaseMonitorChecks(ctx context.Context, runnerID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE monitor_state SET pending_check_id='', pending_runner='', pending_since=NULL, next_at=now()
		WHERE pending_runner=$1 AND pending_check_id <> ''`, runnerID)
	return err
}

// ResetMonitor — настройки наблюдения изменились: идущая проверка
// отменяется, следующая — сразу.
func (s *Store) ResetMonitor(ctx context.Context, envID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE monitor_state SET pending_check_id='', pending_runner='', pending_since=NULL, next_at=now()
		WHERE env_id=$1`, envID)
	return err
}

func monitorInterval(cfg []byte) time.Duration {
	var c domain.EnvConfig
	if err := json.Unmarshal(cfg, &c); err != nil || c.Monitor == nil {
		return time.Duration(domain.MonitorIntervalDefault) * time.Minute
	}
	return c.Monitor.Interval()
}

// MonitorResultInput — результат проверки наблюдения.
type MonitorResultInput struct {
	EnvID, CheckID string
	OK             bool
	Output, Logs   string
	DurationMs     int64
}

// MonitorOutcome — итог записи результата.
type MonitorOutcome struct {
	// Stale — результат чужой или устаревшей проверки, ничего не записано.
	Stale   bool
	State   string
	Failure *FailureResult
}

// RecordMonitorResult — результат проверки: история, состояние, событие
// при смене, а дальше инцидент (провал) или закрытие инцидентов (успех).
// Всё в одной транзакции под advisory lock проекта.
func (s *Store) RecordMonitorResult(ctx context.Context, in MonitorResultInput) (MonitorOutcome, error) {
	var out MonitorOutcome
	limit := s.bugAttemptLimitOrDefault(ctx, in.EnvID)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID, envName, prev, pending string
		var cfg []byte
		var fails, oks int
		var since *time.Time
		if err := tx.QueryRow(ctx, `SELECT project_id::text FROM environments WHERE id=$1`, in.EnvID).Scan(&projectID); err != nil {
			return nf(err)
		}
		if err := lockProjectOps(ctx, tx, projectID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT ms.state, ms.pending_check_id, ms.pending_since, ms.consecutive_fail, ms.consecutive_ok, e.name, e.config
			FROM monitor_state ms JOIN environments e ON e.id = ms.env_id
			WHERE ms.env_id=$1 FOR UPDATE OF ms`, in.EnvID).
			Scan(&prev, &pending, &since, &fails, &oks, &envName, &cfg); err != nil {
			return nf(err)
		}
		if pending == "" || pending != in.CheckID || prev == MonitorDisabled {
			out.Stale = true
			return nil
		}
		// Проверка шла одновременно с публикацией (выкатка началась или
		// закончилась после её старта): результат относится к переходному
		// состоянию окружения. Инцидент не открывается, проверка повторится.
		var overlapped bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM deployments d WHERE d.env_id = $1 AND (
				d.status IN ('deploying','verifying')
				OR ($2::timestamptz IS NOT NULL AND (d.started_at >= $2 OR d.ended_at >= $2))))`,
			in.EnvID, since).Scan(&overlapped); err != nil {
			return err
		}
		if overlapped {
			out.State = MonitorUnknown
			return monitorUnknownTx(ctx, tx, projectID, in.EnvID, envName, prev, "публикация во время проверки", 0)
		}
		output := redact.String(CleanText(in.Output))
		logs := redact.String(CleanText(in.Logs))
		state := MonitorOK
		if in.OK {
			fails, oks = 0, oks+1
		} else {
			state, fails, oks = MonitorFail, fails+1, 0
		}
		out.State = state
		interval := monitorInterval(cfg)
		runOut := ops.Tail(output, 200, monitorRunOutput)
		if _, err := tx.Exec(ctx, `
			UPDATE monitor_state SET state=$2, reason='', next_at=now() + $3::interval,
				pending_check_id='', pending_runner='', pending_since=NULL,
				recheck_at = CASE WHEN recheck_at <= now() THEN NULL ELSE recheck_at END,
				last_at=now(), last_output=$4, consecutive_fail=$5, consecutive_ok=$6,
				last_change_at = CASE WHEN state <> $2 THEN now() ELSE last_change_at END
			WHERE env_id=$1`, in.EnvID, state, interval.String(), runOut, fails, oks); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO monitor_runs (env_id, ok, duration_ms, output) VALUES ($1,$2,$3,$4)`,
			in.EnvID, in.OK, in.DurationMs, runOut); err != nil {
			return err
		}
		if err := monitorStateChangeTx(ctx, tx, projectID, in.EnvID, envName, prev, state, ""); err != nil {
			return err
		}
		if !in.OK {
			res, err := recordFailureTx(ctx, tx, FailureInput{EnvID: in.EnvID, Source: ops.SourceMonitor,
				Output: output, Logs: logs, ConsecutiveFail: fails}, limit)
			if err != nil {
				return err
			}
			out.Failure = &res
			return nil
		}
		return monitorOKTx(ctx, tx, projectID, in.EnvID, oks)
	})
	return out, err
}

// monitorOKTx — успешная проверка закрывает инциденты наблюдения
// окружения: смягчённые по успехам после смягчения, открытые без бага в
// работе как самоустранившиеся.
func monitorOKTx(ctx context.Context, tx pgx.Tx, projectID, envID string, oks int) error {
	rows, err := tx.Query(ctx, `
		SELECT `+incidentStateCols+` FROM `+incidentStateFrom+`
		WHERE i.project_id=$1 AND i.env_id=$2 AND i.source='monitor' AND i.status IN ('open','mitigated')
		ORDER BY i.first_seen FOR UPDATE OF i`, projectID, envID)
	if err != nil {
		return err
	}
	incs, err := scanIncidentStates(rows)
	if err != nil {
		return err
	}
	for _, st := range incs {
		n := oks
		if st.Status == ops.StatusMitigated && st.MitigatedAt != nil {
			var since int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM monitor_runs WHERE env_id=$1 AND ok AND at > $2`,
				envID, *st.MitigatedAt).Scan(&since); err != nil {
				return err
			}
			if since < n {
				n = since
			}
		}
		tr, ok := ops.OnCheckOK(st.Incident, n, ops.DefaultLimits())
		if !ok {
			continue
		}
		note := fmt.Sprintf("%d проверки подряд прошли", n)
		if err := applyTransitionTx(ctx, tx, st, tr, "", "system", note); err != nil {
			return err
		}
	}
	return nil
}

// PruneMonitorRuns — ретенция истории проверок.
func (s *Store) PruneMonitorRuns(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM monitor_runs WHERE at < now() - $1::interval`, MonitorRunsRetention.String())
	return err
}

// MonitorRun — проверка в истории.
type MonitorRun struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	DurationMs int64     `json:"duration_ms"`
	Output     string    `json:"output"`
}

// MonitorView — состояние наблюдения окружения (MonitorState контракта).
type MonitorView struct {
	EnvID           string       `json:"env_id"`
	State           string       `json:"state"`
	Reason          string       `json:"reason,omitempty"`
	LastAt          *time.Time   `json:"last_at,omitempty"`
	NextAt          *time.Time   `json:"next_at,omitempty"`
	ConsecutiveFail int          `json:"consecutive_fail"`
	ConsecutiveOK   int          `json:"consecutive_ok"`
	Runs            []MonitorRun `json:"runs"`
}

// GetMonitor — состояние наблюдения и последние 20 проверок.
func (s *Store) GetMonitor(ctx context.Context, env domain.Environment) (MonitorView, error) {
	if env.ExecType == domain.ExecLocalDocker {
		return s.LocalMonitorView(ctx, env.ID)
	}
	v := MonitorView{EnvID: env.ID, State: MonitorDisabled, Runs: []MonitorRun{}}
	var next time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT state, reason, last_at, next_at, consecutive_fail, consecutive_ok FROM monitor_state WHERE env_id=$1`, env.ID).
		Scan(&v.State, &v.Reason, &v.LastAt, &next, &v.ConsecutiveFail, &v.ConsecutiveOK)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if env.Config.Monitor != nil && env.Config.Monitor.Enabled {
			v.State = MonitorUnknown
		}
	case err != nil:
		return v, err
	default:
		if v.State != MonitorDisabled {
			v.NextAt = &next
		}
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT at, ok, duration_ms, output FROM monitor_runs WHERE env_id=$1 ORDER BY at DESC, id DESC LIMIT 20`, env.ID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var r MonitorRun
		if err := rows.Scan(&r.At, &r.OK, &r.DurationMs, &r.Output); err != nil {
			return v, err
		}
		v.Runs = append(v.Runs, r)
	}
	return v, rows.Err()
}

// MonitorStates — состояние наблюдения окружений проекта (поле
// Environment.monitor_state).
func (s *Store) MonitorStates(ctx context.Context, projectID string) (map[string]MonitorBrief, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ms.env_id::text, ms.state, ms.reason, ms.last_at FROM monitor_state ms JOIN environments e ON e.id = ms.env_id
		WHERE e.project_id=$1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MonitorBrief{}
	for rows.Next() {
		var id string
		var b MonitorBrief
		if err := rows.Scan(&id, &b.State, &b.Reason, &b.LastAt); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// MonitorBrief — состояние наблюдения окружения без истории проверок
// (поля DTO окружения).
type MonitorBrief struct {
	State  string
	Reason string
	LastAt *time.Time
}

// RecordReleaseFailure — инцидент провала публикации, который не записался
// вместе с провалом (incident_pending): запись и снятие отметки одной
// транзакцией (откат, пауза и эскалация уже выполнены).
// Повтор записи инцидента провала публикации.
const (
	// IncidentRetryMax — попыток записи; после них отметка снимается с
	// событием, повтор прекращается.
	IncidentRetryMax = 5
	// incidentRetryBase — пауза после первой неудачи, дальше вдвое больше,
	// не больше часа.
	incidentRetryBase = 30 * time.Second
)

// RecordReleaseFailure — инцидент провала публикации, который не записался
// вместе с провалом (incident_pending): захват отметки, запись и её снятие
// одной транзакцией (откат, пауза и эскалация уже выполнены). Отметку,
// которую обрабатывает другой процесс rivetd, захват пропускает:
// claimed=false. При ошибке записи попытка учитывается, следующая — после
// растущей паузы.
func (s *Store) RecordReleaseFailure(ctx context.Context, depID, stage, log string) (res FailureResult, claimed bool, err error) {
	var envID string
	if err := s.Pool.QueryRow(ctx, `SELECT env_id::text FROM deployments WHERE id=$1`, depID).Scan(&envID); err != nil {
		return res, false, nf(err)
	}
	limit := s.bugAttemptLimitOrDefault(ctx, envID)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id string
		e := tx.QueryRow(ctx, `
			UPDATE deployments SET incident_pending=false
			WHERE id = (SELECT id FROM deployments WHERE id=$1 AND incident_pending FOR UPDATE SKIP LOCKED)
			RETURNING id::text`, depID).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		claimed = true
		res, e = recordFailureTx(ctx, tx, FailureInput{EnvID: envID, Source: ops.SourceRelease, Output: log,
			Stage: stage, DeploymentID: depID, ConsecutiveFail: 1}, limit)
		return e
	})
	if err == nil {
		return res, claimed, nil
	}
	// Запись не удалась: отметка осталась (транзакция откатилась), попытка
	// учитывается с паузой; после последней повтор прекращается с событием.
	var pending bool
	var projectID string
	if uerr := s.Pool.QueryRow(ctx, `
		UPDATE deployments d SET incident_attempts = d.incident_attempts + 1,
			incident_retry_at = now() + LEAST(interval '1 hour', $2::interval * power(2, d.incident_attempts)),
			incident_pending = d.incident_attempts + 1 < $3
		FROM environments e WHERE e.id = d.env_id AND d.id=$1 AND d.incident_pending
		RETURNING d.incident_pending, e.project_id::text`, depID, incidentRetryBase.String(), IncidentRetryMax).
		Scan(&pending, &projectID); uerr == nil && !pending {
		_, _ = s.AppendEvent(ctx, EventInput{ActorKind: domain.ActorSystem, Type: "deploy.incident_failed", ProjectID: projectID,
			Text:    fmt.Sprintf("инцидент провала публикации не записан за %d попыток: %v", IncidentRetryMax, err),
			Payload: map[string]any{"deployment_id": depID}})
	}
	return res, false, err
}

// PendingDeploymentIncidents — провалы публикаций, инцидент которых не
// записался и пора повторить: по одному на проект за проход (проект с
// постоянным сбоем не держит очередь остальных).
func (s *Store) PendingDeploymentIncidents(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM (
			SELECT DISTINCT ON (e.project_id) d.id::text AS id, d.incident_retry_at
			FROM deployments d JOIN environments e ON e.id = d.env_id
			WHERE d.incident_pending AND (d.incident_retry_at IS NULL OR d.incident_retry_at <= now())
			ORDER BY e.project_id, d.incident_retry_at NULLS FIRST
		) q ORDER BY incident_retry_at NULLS FIRST LIMIT 20`)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}
