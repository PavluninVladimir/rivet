package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// ─── projects ────────────────────────────────────────────────────────────

// CreateProject создаёт проект; создатель становится его участником в той же
// транзакции (design add-users-and-access, решение 6).
// CreateProject — проект на глобальном токене установки (устаревшая форма
// запроса и e2e-стенд). Подключение репозитория с учётными данными —
// CreateProjectWithRepo.
func (s *Store) CreateProject(ctx context.Context, name, repo string, checks []domain.Check, creatorID string) (domain.Project, error) {
	provider := "github"
	baseURL := "https://github.com"
	return s.CreateProjectWithRepo(ctx, name, checks, creatorID, NewRepoConnection{
		Provider: provider, BaseURL: baseURL, RepoPath: repo, DefaultBranch: "main",
	}, nil)
}

// projectCols — колонки проекта в порядке scanProject.
const projectCols = `id, name, checks, provider, base_url, repo_path, default_branch,
	COALESCE(credential_id::text,''), COALESCE(webhook_secret,''), webhook_registered, created_at`

func scanProject(row pgx.Row) (domain.Project, error) {
	var p domain.Project
	var raw []byte
	if err := row.Scan(&p.ID, &p.Name, &raw, &p.Provider, &p.BaseURL, &p.RepoPath,
		&p.DefaultBranch, &p.CredentialID, &p.WebhookSecret, &p.WebhookRegistered, &p.Created); err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p.Checks)
}

func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	p, err := scanProject(s.Pool.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id=$1`, id))
	if err != nil {
		return p, nf(err)
	}
	return p, err
}

// ListProjects — только проекты пользователя (слой 1 access-policy;
// исключения для админа нет, спека domain-model).
func (s *Store) ListProjects(ctx context.Context, userID string) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+projectCols+` FROM projects p
		JOIN project_members m ON m.project_id = p.id AND m.user_id = $1
		ORDER BY p.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
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

// CreateTask создаёт задачу. Лимит попыток, если не задан явно, берётся из
// действующей политики проекта на момент создания (спека orchestration
// «Лимит из политики проекта»); изменение политики созданные задачи не трогает.
func (s *Store) CreateTask(ctx context.Context, epicID string, in NewTask) (domain.Task, error) {
	if in.Estimate <= 0 {
		in.Estimate = 1
	}
	if in.AttemptLimit <= 0 {
		epic, err := s.GetEpic(ctx, epicID)
		if err != nil {
			return domain.Task{}, err
		}
		eff, err := s.EffectivePolicy(ctx, epic.ProjectID)
		if err != nil {
			return domain.Task{}, err
		}
		in.AttemptLimit = eff.Presets.AttemptLimit
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
	t.capabilities, t.criteria, t.attempt_used, t.attempt_limit, t.review_rejections,
	COALESCE(t.runner_id,''), COALESCE(t.branch,''), COALESCE(t.pr_url,''), COALESCE(t.block_reason,''),
	COALESCE(t.blocked_by::text,''), t.created_at, t.updated_at`

func scanTask(row pgx.Row) (domain.Task, error) {
	var t domain.Task
	var crit []byte
	err := row.Scan(&t.ID, &t.EpicID, &t.Num, &t.Title, &t.Description, &t.Status, &t.Estimate,
		&t.Capabilities, &crit, &t.AttemptUsed, &t.AttemptLimit, &t.ReviewRejections,
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

// upsertRunnerSQL — регистрация/переподключение: reconnect сбрасывает
// занятость целиком, и задачу, и публикацию (активную публикацию перед этим
// проваливает вызывающий — Register). $6 — токен регистрации (RegisterRunner).
const upsertRunnerSQL = `
		INSERT INTO runners (id, agent, model, host, capabilities, adapter, depth, status, last_seen, token_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'idle',now(),$8)
		ON CONFLICT (id) DO UPDATE SET agent=$2, model=$3, host=$4, capabilities=$5,
			adapter=$6, depth=$7,
			status='idle', task_id=NULL, deployment_id=NULL, ctx_pct=NULL, last_seen=now()`

// normalizeAdapter — значения адаптера и глубины по умолчанию для
// внутренних потребителей и старых вызовов (обёртка, минимальная глубина).
func normalizeAdapter(r domain.Runner) domain.Runner {
	if r.Adapter == "" {
		r.Adapter = "wrap"
	}
	switch r.Depth {
	case domain.DepthFull, domain.DepthPartial, domain.DepthMinimal:
	default:
		r.Depth = domain.DepthMinimal
	}
	return r
}

// UpsertRunner — регистрация без токена (внутренние потребители и тесты);
// протокол использует RegisterRunner.
func (s *Store) UpsertRunner(ctx context.Context, r domain.Runner) error {
	r = normalizeAdapter(r)
	_, err := s.Pool.Exec(ctx, upsertRunnerSQL, r.ID, r.Agent, r.Model, r.Host, r.Capabilities, r.Adapter, r.Depth, nil)
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
		SELECT id, agent, model, host, capabilities, status, COALESCE(task_id::text,''), ctx_pct, draining, last_seen, adapter, depth
		FROM runners ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Runner
	for rows.Next() {
		var r domain.Runner
		if err := rows.Scan(&r.ID, &r.Agent, &r.Model, &r.Host, &r.Capabilities,
			&r.Status, &r.TaskID, &r.CtxPct, &r.Draining, &r.LastSeen, &r.Adapter, &r.Depth); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetRunnerDraining(ctx context.Context, id string, draining bool) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runners SET draining=$2 WHERE id=$1`, id, draining)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ─── attention ───────────────────────────────────────────────────────────

// ListAttention — эскалации проектов пользователя (слой 1 access-policy).
func (s *Store) ListAttention(ctx context.Context, userID string) ([]domain.Attention, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.id, a.project_id, COALESCE(a.task_id::text,''), COALESCE(a.deployment_id::text,''),
		       a.reason, a.message, a.status, COALESCE(a.claimed_by,''), a.created_at
		FROM attention a
		JOIN project_members m ON m.project_id = a.project_id AND m.user_id = $1
		WHERE a.status <> 'resolved' ORDER BY a.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attention
	for rows.Next() {
		var a domain.Attention
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.DeploymentID, &a.Reason, &a.Message,
			&a.Status, &a.ClaimedBy, &a.Created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimAttention берёт эскалацию в работу; эскалации чужих проектов
// неотличимы от несуществующих (404-семантика). Событие attention.claimed
// уходит в SSE проекта: остальные участники видят, кто разбирает, без
// перезагрузки (спека team-visibility «Взятие эскалации в работу»).
func (s *Store) ClaimAttention(ctx context.Context, id, login, userID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID, taskID string
		err := tx.QueryRow(ctx, `
			UPDATE attention SET status='claimed', claimed_by=$2 WHERE id=$1 AND status='open'
			AND project_id IN (SELECT project_id FROM project_members WHERE user_id=$3)
			RETURNING project_id::text, COALESCE(task_id::text,'')`, id, login, userID).
			Scan(&projectID, &taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: login, Type: "attention.claimed",
			ProjectID: projectID, TaskID: taskID,
			Text:    "эскалацию разбирает " + login,
			Payload: map[string]any{"attention_id": id, "claimed_by": login},
		})
		return err
	})
}

// ─── sessions ────────────────────────────────────────────────────────────

// sessionTextCap — предел prompt/outcome сессии: поля индексируются FTS,
// не место для мегабайтных текстов (обрезка по рунам).
const sessionTextCap = 2000

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Store) CreateSession(ctx context.Context, in domain.Session) (string, error) {
	in.Prompt = clipRunes(in.Prompt, sessionTextCap)
	// files: NULL — недоступно для способа подключения; при полной глубине
	// список начинается пустым («недоступно ≠ пусто», спека agent-integration).
	var files []string
	if in.Depth == domain.DepthFull {
		files = []string{}
	}
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO sessions (task_id, attempt, driver_kind, driver_id, agent, model, depth, scope, files, prompt)
		VALUES (NULLIF($1,'')::uuid,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10)
		RETURNING id`,
		in.TaskID, in.Attempt, in.DriverKind, in.DriverID, in.Agent, in.Model, in.Depth, in.Scope, files, in.Prompt).Scan(&id)
	return id, err
}

// SetSessionLastStep — текст последнего шага сессии (реестр активных
// сессий, спека team-visibility «Последний шаг в реестре»).
func (s *Store) SetSessionLastStep(ctx context.Context, sessionID, text string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE sessions SET last_step=$2 WHERE id=$1`, sessionID, text)
	return err
}

// ListTaskSessions — история сессий задачи по возрастанию started_at
// (дельта observability «Просмотр сохранённых транскриптов»). Стадия
// лежит в Scope, tokens nullable: nil = источник не сообщил.
func (s *Store) ListTaskSessions(ctx context.Context, taskID string) ([]domain.Session, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, COALESCE(task_id::text,''), attempt, driver_kind, driver_id,
		       agent, model, depth, COALESCE(scope,''), COALESCE(transcript_ref,''),
		       tokens, started_at, ended_at, files, prompt, outcome, last_step
		FROM sessions WHERE task_id=$1 ORDER BY started_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Session
	for rows.Next() {
		var v domain.Session
		if err := rows.Scan(&v.ID, &v.TaskID, &v.Attempt, &v.DriverKind, &v.DriverID,
			&v.Agent, &v.Model, &v.Depth, &v.Scope, &v.TranscriptRef,
			&v.Tokens, &v.Started, &v.Ended, &v.Files, &v.Prompt, &v.Outcome, &v.LastStep); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SessionTranscriptForViewer — transcript_ref сессии, только если viewer
// участник проекта её задачи. Один scoped-запрос: чужая и несуществующая
// сессия неотличимы (паттерн TaskProjectForViewer).
func (s *Store) SessionTranscriptForViewer(ctx context.Context, sessionID, viewerID string) (string, error) {
	var ref string
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(ss.transcript_ref,'') FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		JOIN epics e ON e.id = t.epic_id
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE ss.id = $1`, sessionID, viewerID).Scan(&ref)
	return ref, nf(err)
}

// OpenSession — id открытой сессии задачи (пустая строка, если её нет).
// Нужна Engine'у после рестарта rivetd: карта сессий в памяти пуста, а
// runner доносит результаты стадий, назначенных до рестарта.
func (s *Store) OpenSession(ctx context.Context, taskID string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		SELECT id::text FROM sessions
		WHERE task_id=$1 AND ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1`, taskID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RunnerDepth — глубина данных адаптера runner'а (для создания сессии).
func (s *Store) RunnerDepth(ctx context.Context, runnerID string) (domain.SessionDepth, error) {
	var d domain.SessionDepth
	err := s.Pool.QueryRow(ctx, `SELECT depth FROM runners WHERE id=$1`, runnerID).Scan(&d)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DepthMinimal, nil
	}
	return d, err
}

// sessionFilesCap — предел накопленных путей на сессию: защита event log и
// DTO от разгона (спека «Шаги сессии»: список уникальных путей).
const sessionFilesCap = 500

// AppendSessionFiles добавляет затронутые файлы к сессии полной глубины:
// уникальные, в порядке первого появления, не больше sessionFilesCap.
// Сессии с files IS NULL (минимальная глубина) не трогаются.
func (s *Store) AppendSessionFiles(ctx context.Context, sessionID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE sessions SET files = (
			SELECT array_agg(p ORDER BY ord) FROM (
				SELECT p, min(ord) AS ord FROM unnest(files || $2::text[]) WITH ORDINALITY AS u(p, ord)
				GROUP BY p ORDER BY ord LIMIT $3
			) q)
		WHERE id=$1 AND files IS NOT NULL`, sessionID, paths, sessionFilesCap)
	return err
}

// EndSession закрывает сессию и подводит итог токенов по usage-записям её
// задачи с момента started_at (design add-usage-metering, решение 6).
// NULL — ни одна запись не содержала токенов. Закрытие атомарно захватывает
// ещё открытую сессию (`ended_at IS NULL`): false — её уже закрыл другой
// путь (отмена, потеря runner'а), и вызывающий обязан отбросить сообщение
// стадии, а не продолжать реакции конвейера (design add-session-visibility,
// решение 4).
// outcome — итог сессии для истории (текст результата стадии или вопрос
// blocked, спека team-visibility «История сессий и запросов с поиском»).
func (s *Store) EndSession(ctx context.Context, id, transcriptRef, outcome string) (bool, error) {
	outcome = clipRunes(outcome, sessionTextCap)
	tag, err := s.Pool.Exec(ctx, `
		UPDATE sessions s SET ended_at=now(), transcript_ref=NULLIF($2,''), outcome=$3,
			tokens=(SELECT SUM(COALESCE(u.tokens_in,0)+COALESCE(u.tokens_out,0))
			        FROM usage_records u
			        WHERE u.task_id = s.task_id AND u.ts >= s.started_at
			          AND (u.tokens_in IS NOT NULL OR u.tokens_out IS NOT NULL))
		WHERE s.id=$1 AND s.ended_at IS NULL`,
		id, transcriptRef, outcome)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
// Label — название проекта, Epic или задачи для соответствующих группировок
// (api-contract add-operations-management); для runner'а и модели = Key.
type UsageRow struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
	CostUSD   *float64 `json:"cost_usd"`
	Duration  int64    `json:"duration_s"`
}

// UsageScope — границы агрегации: проекты зрителя или вся установка.
type UsageScope struct {
	// ViewerID — чьи проекты (слой 1 access-policy); игнорируется при Installation.
	ViewerID string
	// Installation — установочный срез по всем проектам; право администратора
	// проверяет API (спека observability «Установочный срез по проектам»).
	Installation bool
}

// UsageSummary агрегирует usage за полуинтервал [from, to) по проектам
// зрителя либо по всей установке; нулевое время — граница не задана.
func (s *Store) UsageSummary(ctx context.Context, scope UsageScope, groupBy string, from, to time.Time) ([]UsageRow, error) {
	type grouping struct{ key, label, join string }
	g := map[string]grouping{
		"epic": {"COALESCE(u.epic_id::text,'—')", "COALESCE(e.title,'')",
			"LEFT JOIN epics e ON e.id = u.epic_id"},
		"task": {"COALESCE(u.task_id::text,'—')", "COALESCE(t.title,'')",
			"LEFT JOIN tasks t ON t.id = u.task_id"},
		"project": {"u.project_id::text", "COALESCE(p.name,'')",
			"LEFT JOIN projects p ON p.id = u.project_id"},
		"runner": {"COALESCE(u.runner_id,'—')", "COALESCE(u.runner_id,'')", ""},
		"model":  {"COALESCE(NULLIF(u.model,''),'—')", "COALESCE(NULLIF(u.model,''),'')", ""},
	}[groupBy]
	if g.key == "" {
		g = grouping{"COALESCE(u.epic_id::text,'—')", "COALESCE(e.title,'')",
			"LEFT JOIN epics e ON e.id = u.epic_id"}
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+g.key+` AS k, MIN(`+g.label+`), SUM(u.tokens_in), SUM(u.tokens_out), SUM(u.cost_usd)::float8, SUM(u.duration_s)
		FROM usage_records u `+g.join+`
		WHERE ($1::timestamptz IS NULL OR u.ts >= $1) AND ($2::timestamptz IS NULL OR u.ts < $2)
		  AND ($4 OR u.project_id IN (SELECT project_id FROM project_members WHERE user_id = $3))
		GROUP BY k ORDER BY k`, nullableTime(from), nullableTime(to), scope.ViewerID, scope.Installation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Label, &r.TokensIn, &r.TokensOut, &r.CostUSD, &r.Duration); err != nil {
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
