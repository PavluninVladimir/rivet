package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Хосты окружений (add-environment-provisioning, design решение 1). Все
// переходы: CAS по состоянию в одной транзакции с эскалациями, событием
// и правкой конфигурации окружения. Решения людей и отправка создания и
// удаления проверяются и здесь, и CHECK-ограничениями таблицы.

const hostCols = `h.id::text, h.env_id::text, e.project_id::text, h.provider, COALESCE(h.connection_id, ''), h.spec, h.spec_labels,
	h.state, h.prev_state, h.requested_by,
	h.quote_amount, h.quote_currency, h.quote_period, h.balance_amount, h.balance_currency,
	h.approved_by, h.approved_at, h.approved_amount, h.create_sent_at,
	h.server_id, h.action_id, h.address, h.host_key, h.waiting_payment, h.port,
	h.job_id, h.job_kind, h.job_runner, h.job_started_at, h.attempts, h.needs_retry,
	h.remove_volumes, h.delete_requested_by, h.delete_confirmed_by, h.delete_sent_at,
	h.paid_till, h.cost_amount, h.cost_currency, h.cost_period, h.cost_checked_at,
	h.failure, h.detail, h.next_attempt_at, h.created_at, h.updated_at, h.ended_at`

const hostFrom = ` FROM env_hosts h JOIN environments e ON e.id = h.env_id `

// liveStates: SQL-список неконечных состояний.
const terminalStatesSQL = `('deleted','rejected','failed')`

func scanHost(row pgx.Row) (domain.EnvHost, error) {
	var h domain.EnvHost
	var spec, labels []byte
	err := row.Scan(&h.ID, &h.EnvID, &h.ProjectID, &h.Provider, &h.ConnectionID, &spec, &labels,
		&h.State, &h.PrevState, &h.RequestedBy,
		&h.QuoteAmount, &h.QuoteCurrency, &h.QuotePeriod, &h.BalanceAmount, &h.BalanceCurrency,
		&h.ApprovedBy, &h.ApprovedAt, &h.ApprovedAmount, &h.CreateSentAt,
		&h.ServerID, &h.ActionID, &h.Address, &h.HostKey, &h.WaitingPayment, &h.Port,
		&h.JobID, &h.JobKind, &h.JobRunner, &h.JobStartedAt, &h.Attempts, &h.NeedsRetry,
		&h.RemoveVolumes, &h.DeleteRequestedBy, &h.DeleteConfirmedBy, &h.DeleteSentAt,
		&h.PaidTill, &h.CostAmount, &h.CostCurrency, &h.CostPeriod, &h.CostCheckedAt,
		&h.Failure, &h.Detail, &h.NextAttemptAt, &h.Created, &h.Updated, &h.Ended)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(labels, &h.SpecLabels); err != nil {
		return h, err
	}
	return h, json.Unmarshal(spec, &h.Spec)
}

// ErrHostLive: у окружения уже есть хост не в конечном состоянии.
var ErrHostLive = fmt.Errorf("у окружения уже есть хост: %w", ErrConflict)

// ErrPriceChanged: подтверждена сумма, которая не равна текущей цене.
var ErrPriceChanged = errors.New("цена изменилась: подтвердите новую")

// ErrHostState: действие не в своём состоянии хоста.
type ErrHostState struct{ State string }

func (e *ErrHostState) Error() string {
	return "действие недоступно в состоянии хоста " + e.State
}

// RequestHost создаёт хост окружения по его привязке: расчёт цены у
// платного провайдера, сразу создание у бесплатного решает оркестратор.
// Открытые эскалации прежних хостов окружения закрываются: новый запрос:
// ответ человека на них.
func (s *Store) RequestHost(ctx context.Context, env domain.Environment, actor string) (domain.EnvHost, error) {
	p := env.Config.Provision
	if p == nil {
		return domain.EnvHost{}, errors.Join(ErrInvalid, errors.New("окружение не привязано к провайдеру"))
	}
	spec, err := json.Marshal(p)
	if err != nil {
		return domain.EnvHost{}, err
	}
	var conn any
	if p.Connection != "" {
		conn = p.Connection
	}
	var id string
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO env_hosts (env_id, provider, connection_id, spec, state, requested_by, port)
			VALUES ($1,$2,$3,$4,'quoting',$5,$6)
			ON CONFLICT (env_id) WHERE state NOT IN `+terminalStatesSQL+` DO NOTHING
			RETURNING id::text`, env.ID, p.Provider, conn, spec, actor, p.Port).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHostLive
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE attention SET status='resolved', resolved_at=now()
			WHERE status <> 'resolved' AND host_id IN (SELECT id FROM env_hosts WHERE env_id=$1 AND id <> $2)`,
			env.ID, id); err != nil {
			return err
		}
		return hostEvent(ctx, tx, env.ProjectID, env.ID, id, domain.HostQuoting, "request", actor,
			"запрошен хост окружения "+env.Name)
	})
	if err != nil {
		return domain.EnvHost{}, err
	}
	return s.GetHost(ctx, id)
}

// GetHost: хост по идентификатору.
func (s *Store) GetHost(ctx context.Context, id string) (domain.EnvHost, error) {
	h, err := scanHost(s.Pool.QueryRow(ctx, `SELECT `+hostCols+hostFrom+`WHERE h.id=$1`, id))
	return h, nf(err)
}

// CurrentHost: живой хост окружения, иначе последний; ErrNotFound: хостов
// не было.
func (s *Store) CurrentHost(ctx context.Context, envID string) (domain.EnvHost, error) {
	h, err := scanHost(s.Pool.QueryRow(ctx, `SELECT `+hostCols+hostFrom+`
		WHERE h.env_id=$1
		ORDER BY (h.state NOT IN `+terminalStatesSQL+`) DESC, h.created_at DESC LIMIT 1`, envID))
	return h, nf(err)
}

// CurrentHosts: текущие хосты окружений проекта (для списка окружений).
func (s *Store) CurrentHosts(ctx context.Context, projectID string) (map[string]domain.EnvHost, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (h.env_id) `+hostCols+hostFrom+`
		WHERE e.project_id=$1
		ORDER BY h.env_id, (h.state NOT IN `+terminalStatesSQL+`) DESC, h.created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]domain.EnvHost{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out[h.EnvID] = h
	}
	return out, rows.Err()
}

// LiveHost: хост окружения не в конечном состоянии; ok=false: его нет.
func (s *Store) LiveHost(ctx context.Context, envID string) (domain.EnvHost, bool, error) {
	h, err := scanHost(s.Pool.QueryRow(ctx, `SELECT `+hostCols+hostFrom+`
		WHERE h.env_id=$1 AND h.state NOT IN `+terminalStatesSQL, envID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, false, nil
	}
	return h, err == nil, err
}

// processStates: состояния, которые ведёт тик (решения людей ждут их).
var processStates = []string{domain.HostQuoting, domain.HostApproved, domain.HostCreating,
	domain.HostBootstrapping, domain.HostReady, domain.HostDeleting}

// ClaimDueHost захватывает хост, которому пора на обработку тиком: захват
// с арендой, чтобы второй процесс rivetd не вёл тот же хост одновременно.
func (s *Store) ClaimDueHost(ctx context.Context, lease time.Duration, skip []string) (domain.EnvHost, bool, error) {
	if skip == nil {
		skip = []string{}
	}
	var id string
	err := s.Pool.QueryRow(ctx, `
		UPDATE env_hosts SET lease_until = now() + $1::interval
		WHERE id = (
			SELECT id FROM env_hosts
			WHERE state = ANY($2) AND NOT needs_retry AND next_attempt_at <= now()
			  AND (lease_until IS NULL OR lease_until < now())
			  AND NOT (id::text = ANY($3))
			ORDER BY next_attempt_at
			FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id::text`, lease.String(), processStates, skip).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvHost{}, false, nil
	}
	if err != nil {
		return domain.EnvHost{}, false, err
	}
	h, err := s.GetHost(ctx, id)
	return h, err == nil, err
}

// ReleaseHost снимает аренду и назначает следующую обработку. Если хост
// успел измениться после захвата (итог задания runner'а, действие
// человека, переход самого тика), время обработки остаётся тем, что
// записал этот переход: иначе срок ожидания задания затёр бы «сразу»
// готового хоста.
func (s *Store) ReleaseHost(ctx context.Context, id string, next, claimedUpdated time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE env_hosts SET lease_until=NULL,
			next_attempt_at = CASE WHEN updated_at = $3 THEN $2 ELSE next_attempt_at END
		WHERE id=$1`, id, next, claimedUpdated)
	return err
}

// SQL: выражение вместо значения колонки в HostChange.Set.
type SQL string

// HostAttention: эскалация хоста.
type HostAttention struct {
	Reason  domain.AttentionReason
	Message string
}

// HostChange: переход хоста (design, решение 1).
type HostChange struct {
	// From: допустимые состояния; пусто: любое.
	From []string
	// To: новое состояние; пусто: прежнее.
	To string
	// Set: колонки и значения (только из hostColumns).
	Set map[string]any
	// Where: дополнительное условие CAS. Значения только через Args:
	// {1}, {2}… в тексте заменяются параметрами запроса.
	Where string
	Args  []any
	// Open: эскалации открыть (сообщение обновляется у уже открытой).
	Open []HostAttention
	// Resolve: причины эскалаций закрыть; ResolveAll: закрыть все.
	Resolve    []domain.AttentionReason
	ResolveAll bool
	// Env: правка конфигурации окружения в той же транзакции.
	Env func(*domain.EnvConfig)
	// Event: текст события environment.host; пусто: без события.
	Event  string
	Action string
	Actor  string
}

// hostColumns: колонки, которые меняет HostChange.
var hostColumns = map[string]bool{
	"prev_state": true, "spec_labels": true, "quote_amount": true, "quote_currency": true, "quote_period": true,
	"balance_amount": true, "balance_currency": true, "approved_by": true, "approved_at": true,
	"approved_amount": true, "create_sent_at": true, "server_id": true, "action_id": true,
	"address": true, "host_key": true, "waiting_payment": true, "job_id": true, "job_kind": true,
	"job_runner": true, "job_started_at": true, "attempts": true, "needs_retry": true,
	"remove_volumes": true, "delete_requested_by": true, "delete_confirmed_by": true,
	"delete_sent_at": true, "paid_till": true, "cost_amount": true, "cost_currency": true,
	"cost_period": true, "cost_checked_at": true, "failure": true, "detail": true,
	"next_attempt_at": true, "lease_until": true, "ended_at": true,
}

// ChangeHost: CAS-переход хоста; ok=false: хост не в допустимом
// состоянии (переход сделал кто-то другой).
func (s *Store) ChangeHost(ctx context.Context, id string, c HostChange) (domain.EnvHost, bool, error) {
	var out domain.EnvHost
	changed := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		changed, err = changeHostTx(ctx, tx, id, c)
		if err != nil || !changed {
			return err
		}
		out, err = scanHost(tx.QueryRow(ctx, `SELECT `+hostCols+hostFrom+`WHERE h.id=$1`, id))
		return err
	})
	if err == nil && !changed {
		out, err = s.GetHost(ctx, id)
	}
	return out, changed, err
}

func changeHostTx(ctx context.Context, tx pgx.Tx, id string, c HostChange) (bool, error) {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	if c.To != "" {
		args = append(args, c.To)
		sets = append(sets, fmt.Sprintf("state = $%d", len(args)))
	}
	keys := make([]string, 0, len(c.Set))
	for k := range c.Set {
		if !hostColumns[k] {
			return false, fmt.Errorf("env_hosts: колонка %q не меняется переходом", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if expr, ok := c.Set[k].(SQL); ok {
			sets = append(sets, k+" = "+string(expr))
			continue
		}
		args = append(args, c.Set[k])
		sets = append(sets, fmt.Sprintf("%s = $%d", k, len(args)))
	}
	where := "id = $1"
	if len(c.From) > 0 {
		args = append(args, c.From)
		where += fmt.Sprintf(" AND state = ANY($%d)", len(args))
	}
	if c.Where != "" {
		cond := c.Where
		for i, a := range c.Args {
			args = append(args, a)
			cond = strings.ReplaceAll(cond, fmt.Sprintf("{%d}", i+1), fmt.Sprintf("$%d", len(args)))
		}
		where += " AND (" + cond + ")"
	}
	var envID, projectID, envName, state string
	err := tx.QueryRow(ctx, `
		WITH up AS (UPDATE env_hosts SET `+strings.Join(sets, ", ")+` WHERE `+where+` RETURNING env_id, state)
		SELECT e.id::text, e.project_id::text, e.name, up.state FROM up JOIN environments e ON e.id = up.env_id`,
		args...).Scan(&envID, &projectID, &envName, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if c.ResolveAll {
		if _, err := tx.Exec(ctx, `UPDATE attention SET status='resolved', resolved_at=now()
			WHERE host_id=$1 AND status <> 'resolved'`, id); err != nil {
			return false, err
		}
	} else if len(c.Resolve) > 0 {
		reasons := make([]string, 0, len(c.Resolve))
		for _, r := range c.Resolve {
			reasons = append(reasons, string(r))
		}
		if _, err := tx.Exec(ctx, `UPDATE attention SET status='resolved', resolved_at=now()
			WHERE host_id=$1 AND reason = ANY($2) AND status <> 'resolved'`, id, reasons); err != nil {
			return false, err
		}
	}
	for _, a := range c.Open {
		if _, err := tx.Exec(ctx, `
			INSERT INTO attention (project_id, host_id, reason, message) VALUES ($1,$2,$3,$4)
			ON CONFLICT (host_id, reason) WHERE status <> 'resolved' AND host_id IS NOT NULL
			DO UPDATE SET message = EXCLUDED.message`,
			projectID, id, string(a.Reason), left("Окружение "+envName+": "+a.Message, 4000)); err != nil {
			return false, err
		}
	}
	if c.Env != nil {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT config FROM environments WHERE id=$1 FOR UPDATE`, envID).Scan(&raw); err != nil {
			return false, err
		}
		var cfg domain.EnvConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return false, err
		}
		c.Env(&cfg)
		upd, err := json.Marshal(cfg)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET config=$2 WHERE id=$1`, envID, upd); err != nil {
			return false, err
		}
	}
	if c.Event != "" {
		if err := hostEvent(ctx, tx, projectID, envID, id, state, c.Action, c.Actor,
			"окружение "+envName+": "+c.Event); err != nil {
			return false, err
		}
	}
	return true, nil
}

// hostEvent: событие environment.host в event log и SSE проекта.
func hostEvent(ctx context.Context, tx pgx.Tx, projectID, envID, hostID, state, action, actor, text string) error {
	kind, actorID := domain.ActorScheduler, ""
	if actor != "" {
		kind, actorID = domain.ActorUser, actor
	}
	payload := map[string]any{"environment_id": envID, "host_id": hostID, "state": state}
	if action != "" {
		payload["action"] = action
	}
	_, err := appendEvent(ctx, tx, EventInput{ActorKind: kind, ActorID: actorID, Type: "environment.host",
		ProjectID: projectID, Text: text, Payload: payload})
	return err
}

// HostEnvAndProject: окружение хоста (для оркестратора).
func (s *Store) HostEnvironment(ctx context.Context, hostID string) (domain.Environment, string, error) {
	var envID, repo string
	if err := s.Pool.QueryRow(ctx, `
		SELECT h.env_id::text, p.repo_path FROM env_hosts h JOIN environments e ON e.id=h.env_id
		JOIN projects p ON p.id=e.project_id WHERE h.id=$1`, hostID).Scan(&envID, &repo); err != nil {
		return domain.Environment{}, "", nf(err)
	}
	env, err := s.GetEnvironment(ctx, envID)
	return env, repo, err
}

// HostRunner: online deploy-runner для задания хоста: capability deploy и
// окружения, возможность host_jobs. Свободные первыми; задание runner не
// занимает (как проверка наблюдения).
func (s *Store) HostRunner(ctx context.Context, env domain.Environment) (string, bool, error) {
	caps := env.RunnerCaps
	if caps == nil {
		caps = []string{}
	}
	var id string
	err := s.Pool.QueryRow(ctx, `
		SELECT id FROM runners
		WHERE status <> 'offline' AND NOT draining
		  AND capabilities @> ARRAY['deploy'] AND capabilities @> $1::text[]
		  AND 'host_jobs' = ANY(features)
		ORDER BY (status = 'idle') DESC, cardinality(capabilities), last_seen DESC LIMIT 1`, caps).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// HostByJob: хост, чьё задание с этим идентификатором выполняется.
func (s *Store) HostByJob(ctx context.Context, jobID string) (domain.EnvHost, error) {
	h, err := scanHost(s.Pool.QueryRow(ctx, `SELECT `+hostCols+hostFrom+`WHERE h.job_id=$1 AND h.job_id <> ''`, jobID))
	return h, nf(err)
}

// UsedLocalPorts: порты локальных хостов окружений установки, кроме
// окружения except.
func (s *Store) UsedLocalPorts(ctx context.Context, except string) (map[int]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT (e.config->'provision'->>'port')::int, p.name || '/' || e.name
		FROM environments e JOIN projects p ON p.id = e.project_id
		WHERE e.config->'provision'->>'provider' = 'local'
		  AND COALESCE((e.config->'provision'->>'port')::int, 0) > 0
		  AND e.id::text <> $1`, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var port int
		var name string
		if err := rows.Scan(&port, &name); err != nil {
			return nil, err
		}
		out[port] = name
	}
	return out, rows.Err()
}

// ReleaseHostJobs: задания хоста runner'а, чья сессия закрылась: задание
// прервано вместе с ней (не провалено), повтор сразу без расхода попытки.
func (s *Store) ReleaseHostJobs(ctx context.Context, runnerID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE env_hosts SET job_id='', job_kind='', job_runner='', job_started_at=NULL,
			next_attempt_at=now(), updated_at=now()
		WHERE job_runner=$1 AND job_id <> '' AND state NOT IN `+terminalStatesSQL, runnerID)
	return err
}
