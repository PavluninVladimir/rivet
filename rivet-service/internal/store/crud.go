package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// ─── projects ────────────────────────────────────────────────────────────

func (s *Store) CreateProject(ctx context.Context, name, repo string, checks []domain.Check) (domain.Project, error) {
	raw, _ := json.Marshal(checks)
	p := domain.Project{Name: name, Repo: repo, Checks: checks}
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO projects (name, repo, checks) VALUES ($1,$2,$3) RETURNING id, created_at`,
		name, repo, raw).Scan(&p.ID, &p.Created)
	return p, err
}

func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	var p domain.Project
	var raw []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT id, name, repo, checks, created_at FROM projects WHERE id=$1`, id).
		Scan(&p.ID, &p.Name, &p.Repo, &raw, &p.Created)
	if err != nil {
		return p, nf(err)
	}
	err = json.Unmarshal(raw, &p.Checks)
	return p, err
}

func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, name, repo, checks, created_at FROM projects ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		var p domain.Project
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.Repo, &raw, &p.Created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &p.Checks)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── epics ───────────────────────────────────────────────────────────────

func (s *Store) CreateEpic(ctx context.Context, projectID, title, goal string) (domain.Epic, error) {
	e := domain.Epic{ProjectID: projectID, Title: title, Goal: goal, Status: domain.EpicPlanned}
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO epics (project_id, title, goal) VALUES ($1,$2,$3) RETURNING id, created_at`,
		projectID, title, goal).Scan(&e.ID, &e.Created)
	return e, err
}

func (s *Store) GetEpic(ctx context.Context, id string) (domain.Epic, error) {
	var e domain.Epic
	err := s.Pool.QueryRow(ctx,
		`SELECT id, project_id, title, goal, status, created_at FROM epics WHERE id=$1`, id).
		Scan(&e.ID, &e.ProjectID, &e.Title, &e.Goal, &e.Status, &e.Created)
	return e, nf(err)
}

func (s *Store) ListEpics(ctx context.Context, projectID string) ([]domain.Epic, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, project_id, title, goal, status, created_at FROM epics WHERE project_id=$1 ORDER BY created_at`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Epic
	for rows.Next() {
		var e domain.Epic
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Title, &e.Goal, &e.Status, &e.Created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── tasks ───────────────────────────────────────────────────────────────

type NewTask struct {
	Title        string
	Description  string
	Estimate     int
	Capabilities []string
	Criteria     []domain.Criterion
	Deps         []string // id задач-зависимостей
	AttemptLimit int
}

func (s *Store) CreateTask(ctx context.Context, epicID string, in NewTask) (domain.Task, error) {
	if in.Estimate <= 0 {
		in.Estimate = 1
	}
	if in.AttemptLimit <= 0 {
		in.AttemptLimit = 3
	}
	if len(in.Capabilities) == 0 {
		in.Capabilities = []string{"coding"}
	}
	crit, _ := json.Marshal(in.Criteria)
	var t domain.Task
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO tasks (epic_id, title, description, estimate, capabilities, criteria, attempt_limit)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING id, num, created_at, updated_at`,
			epicID, in.Title, in.Description, in.Estimate, in.Capabilities, crit, in.AttemptLimit).
			Scan(&t.ID, &t.Num, &t.Created, &t.Updated)
		if err != nil {
			return err
		}
		for _, dep := range in.Deps {
			if _, err := tx.Exec(ctx,
				`INSERT INTO task_deps (task_id, dep_id) VALUES ($1,$2)`, t.ID, dep); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return t, err
	}
	t.EpicID, t.Title, t.Description = epicID, in.Title, in.Description
	t.Status, t.Estimate, t.Capabilities = domain.TaskQueued, in.Estimate, in.Capabilities
	t.Criteria, t.Deps, t.AttemptLimit = in.Criteria, in.Deps, in.AttemptLimit
	return t, nil
}

const taskCols = `t.id, t.epic_id, t.num, t.title, t.description, t.status, t.estimate,
	t.capabilities, t.criteria, t.attempt_used, t.attempt_limit,
	COALESCE(t.runner_id,''), COALESCE(t.branch,''), COALESCE(t.pr_url,''), COALESCE(t.block_reason,''),
	COALESCE(t.blocked_by::text,''), t.created_at, t.updated_at`

func scanTask(row pgx.Row) (domain.Task, error) {
	var t domain.Task
	var crit []byte
	err := row.Scan(&t.ID, &t.EpicID, &t.Num, &t.Title, &t.Description, &t.Status, &t.Estimate,
		&t.Capabilities, &crit, &t.AttemptUsed, &t.AttemptLimit,
		&t.RunnerID, &t.Branch, &t.PRURL, &t.BlockReason, &t.BlockedBy, &t.Created, &t.Updated)
	if err != nil {
		return t, err
	}
	_ = json.Unmarshal(crit, &t.Criteria)
	return t, nil
}

func (s *Store) taskDeps(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT dep_id FROM task_deps WHERE task_id=$1`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (domain.Task, error) {
	t, err := scanTask(s.Pool.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks t WHERE t.id=$1`, id))
	if err != nil {
		return t, nf(err)
	}
	t.Deps, err = s.taskDeps(ctx, id)
	return t, err
}

func (s *Store) ListEpicTasks(ctx context.Context, epicID string) ([]domain.Task, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+taskCols+` FROM tasks t WHERE t.epic_id=$1 ORDER BY t.num`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Deps, err = s.taskDeps(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ─── runners ─────────────────────────────────────────────────────────────

func (s *Store) UpsertRunner(ctx context.Context, r domain.Runner) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO runners (id, agent, model, host, capabilities, status, last_seen)
		VALUES ($1,$2,$3,$4,$5,'idle',now())
		ON CONFLICT (id) DO UPDATE SET agent=$2, model=$3, host=$4, capabilities=$5,
			status='idle', task_id=NULL, last_seen=now()`,
		r.ID, r.Agent, r.Model, r.Host, r.Capabilities)
	return err
}

// TouchRunner обновляет heartbeat; ctxPct == nil — заполненность неизвестна.
func (s *Store) TouchRunner(ctx context.Context, id string, ctxPct *int) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE runners SET last_seen=now(), ctx_pct=$2 WHERE id=$1`, id, ctxPct)
	return err
}

func (s *Store) ListRunners(ctx context.Context) ([]domain.Runner, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, agent, model, host, capabilities, status, COALESCE(task_id::text,''), ctx_pct, draining, last_seen
		FROM runners ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Runner
	for rows.Next() {
		var r domain.Runner
		if err := rows.Scan(&r.ID, &r.Agent, &r.Model, &r.Host, &r.Capabilities,
			&r.Status, &r.TaskID, &r.CtxPct, &r.Draining, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetRunnerDraining(ctx context.Context, id string, draining bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runners SET draining=$2 WHERE id=$1`, id, draining)
	return err
}

// ─── attention ───────────────────────────────────────────────────────────

func (s *Store) ListAttention(ctx context.Context) ([]domain.Attention, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_id, task_id, reason, message, status, COALESCE(claimed_by,''), created_at
		FROM attention WHERE status <> 'resolved' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attention
	for rows.Next() {
		var a domain.Attention
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.Reason, &a.Message,
			&a.Status, &a.ClaimedBy, &a.Created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ClaimAttention(ctx context.Context, id, user string) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE attention SET status='claimed', claimed_by=$2 WHERE id=$1 AND status='open'`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ─── sessions ────────────────────────────────────────────────────────────

func (s *Store) CreateSession(ctx context.Context, in domain.Session) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO sessions (task_id, attempt, driver_kind, driver_id, agent, model, depth, scope)
		VALUES (NULLIF($1,'')::uuid,$2,$3,$4,$5,$6,$7,NULLIF($8,''))
		RETURNING id`,
		in.TaskID, in.Attempt, in.DriverKind, in.DriverID, in.Agent, in.Model, in.Depth, in.Scope).Scan(&id)
	return id, err
}

// EndSession закрывает сессию и подводит итог токенов по usage-записям её
// задачи с момента started_at (design add-usage-metering, решение 6).
// NULL — ни одна запись не содержала токенов.
func (s *Store) EndSession(ctx context.Context, id, transcriptRef string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sessions s SET ended_at=now(), transcript_ref=NULLIF($2,''),
			tokens=(SELECT SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0))
			        FROM usage_records u
			        WHERE u.task_id = s.task_id AND u.ts >= s.started_at
			          AND (u.tokens_in IS NOT NULL OR u.tokens_out IS NOT NULL))
		WHERE s.id=$1`,
		id, transcriptRef)
	return err
}

// ─── usage ───────────────────────────────────────────────────────────────

// UsageInput — запись метеринга. nil в Tokens*/CostUSD означает «источник не
// сообщил значение» (спека observability «Учёт usage»), не ноль.
type UsageInput struct {
	SourceMsgID string // идемпотентность billing-grade
	ProjectID   string
	EpicID      string
	TaskID      string
	RunnerID    string
	Model       string
	TokensIn    *int64
	TokensOut   *int64
	CostUSD     *float64
	DurationS   int
}

func (s *Store) RecordUsage(ctx context.Context, u UsageInput) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO usage_records (source_msg_id, project_id, epic_id, task_id, runner_id, model,
			tokens_in, tokens_out, cost_usd, duration_s)
		VALUES (NULLIF($1,''),$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,NULLIF($5,''),$6,$7,$8,$9,$10)
		ON CONFLICT (source_msg_id) DO NOTHING`,
		u.SourceMsgID, u.ProjectID, u.EpicID, u.TaskID, u.RunnerID, u.Model,
		u.TokensIn, u.TokensOut, u.CostUSD, u.DurationS)
	return err
}

// UsageRow — агрегат группы; null в токенах/стоимости = данных не было
// ни в одной записи группы (SUM по NULL), клиент показывает «—».
type UsageRow struct {
	Key       string   `json:"key"`
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
	CostUSD   *float64 `json:"cost_usd"`
	Duration  int64    `json:"duration_s"`
}

// UsageSummary агрегирует usage за полуинтервал [from, to); нулевое время —
// граница не задана.
func (s *Store) UsageSummary(ctx context.Context, groupBy string, from, to time.Time) ([]UsageRow, error) {
	col := map[string]string{
		"epic":   "COALESCE(epic_id::text,'—')",
		"task":   "COALESCE(task_id::text,'—')",
		"runner": "COALESCE(runner_id,'—')",
		"model":  "COALESCE(NULLIF(model,''),'—')",
	}[groupBy]
	if col == "" {
		col = "COALESCE(epic_id::text,'—')"
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+col+` AS k, SUM(tokens_in), SUM(tokens_out), SUM(cost_usd)::float8, SUM(duration_s)
		FROM usage_records
		WHERE ($1::timestamptz IS NULL OR ts >= $1) AND ($2::timestamptz IS NULL OR ts < $2)
		GROUP BY k ORDER BY k`, nullableTime(from), nullableTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.TokensIn, &r.TokensOut, &r.CostUSD, &r.Duration); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// EpicUsage — вклад задач Epic и итог по нему (api-contract add-usage-metering).
// total.Key = id Epic; nil-указатели пробрасывают семантику «данных нет».
func (s *Store) EpicUsage(ctx context.Context, epicID string) (rows []UsageRow, total *UsageRow, err error) {
	res, err := s.Pool.Query(ctx, `
		SELECT COALESCE(task_id::text,'—') AS k,
			SUM(tokens_in), SUM(tokens_out), SUM(cost_usd)::float8, SUM(duration_s)
		FROM usage_records WHERE epic_id=$1 GROUP BY k ORDER BY k`, epicID)
	if err != nil {
		return nil, nil, err
	}
	defer res.Close()
	for res.Next() {
		var r UsageRow
		if err := res.Scan(&r.Key, &r.TokensIn, &r.TokensOut, &r.CostUSD, &r.Duration); err != nil {
			return nil, nil, err
		}
		rows = append(rows, r)
	}
	if err := res.Err(); err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	total = &UsageRow{Key: epicID}
	for _, r := range rows {
		total.Duration += r.Duration
		total.TokensIn = addNullable(total.TokensIn, r.TokensIn)
		total.TokensOut = addNullable(total.TokensOut, r.TokensOut)
		total.CostUSD = addNullable(total.CostUSD, r.CostUSD)
	}
	return rows, total, nil
}

// addNullable складывает «возможно отсутствующие» значения: nil + nil = nil.
func addNullable[T int64 | float64](a, b *T) *T {
	if b == nil {
		return a
	}
	if a == nil {
		v := *b
		return &v
	}
	v := *a + *b
	return &v
}
