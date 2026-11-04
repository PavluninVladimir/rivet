package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/overview"
)

// Обзор проекта для руководителя (add-project-lead-view, спека
// backend/project-overview). Store собирает согласованный снимок данных
// проекта одной транзакцией REPEATABLE READ READ ONLY, расчёт делает чистый
// пакет internal/overview.

// OverviewQuery — что собрать.
type OverviewQuery struct {
	ProjectID string
	// UserID и Login — зритель: для «моих шагов» (отчёт их не использует).
	UserID string
	Login  string
	Now    time.Time
	Period overview.Period
	Loc    *time.Location
	// Report — данные для отчёта: записи usage за всю жизнь Epic'ов и задач
	// (расход пересчитывается из записей до среза) и последняя выкатка
	// каждого окружения до начала периода.
	Report bool
	// Live — признак текущего отчёта (выставляет API).
	Live bool
}

// costKnownSQL — стоимость записи usage известна: значение есть, и ноль
// допустим только у модели с ценой в каталоге подключений. Ноль у модели
// без прайса значит «цены нет», а не бесплатный вызов (design, решение 5).
// prefix — алиас таблицы usage_records с точкой или пусто.
func costKnownSQL(prefix string) string {
	return fmt.Sprintf(`(%[1]scost_usd IS NOT NULL AND (%[1]scost_usd > 0 OR %[1]smodel IN (
		SELECT m->>'id' FROM model_connections mc, jsonb_array_elements(mc.models) m
		WHERE COALESCE((m->>'input_price')::bigint, 0) > 0 OR COALESCE((m->>'output_price')::bigint, 0) > 0)))`, prefix)
}

// overviewCut — момент среза: min(конец периода, now). Обзор считается на
// now, отчёт за прошедший период на конец периода (design, решение 7).
func overviewCut(q OverviewQuery) time.Time {
	cut := q.Period.To
	if !q.Now.IsZero() && q.Now.Before(cut) {
		cut = q.Now
	}
	return cut
}

// overviewWindowStart — самое раннее время, от которого нужны сессии,
// usage и события: начало периода, месяц для расхода с начала месяца, 7
// суток для «последних 7 суток», окно темпа. Окна отсчитываются от среза,
// поэтому отчёт за прошедший период не зависит от момента запроса.
func overviewWindowStart(q OverviewQuery, th overview.Thresholds) time.Time {
	loc := q.Loc
	if loc == nil {
		loc = time.UTC
	}
	cut := overviewCut(q)
	m := cut.Add(-time.Nanosecond).In(loc)
	start := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, loc)
	for _, t := range []time.Time{
		q.Period.From,
		cut.Add(-7 * 24 * time.Hour),
		cut.Add(-time.Duration(th.VelocityWindowDays) * 24 * time.Hour),
	} {
		if t.Before(start) {
			start = t
		}
	}
	return start
}

// OverviewInput собирает снимок данных проекта для overview.Build и
// overview.Report.
func (s *Store) OverviewInput(ctx context.Context, q OverviewQuery) (overview.Input, error) {
	th := overview.DefaultThresholds()
	in := overview.Input{
		ProjectID: q.ProjectID, Now: q.Now, Period: q.Period, Location: q.Loc,
		Live: q.Live, Thresholds: th,
	}
	// Каталог шаблонов и действующая политика читаются до транзакции: они
	// меняются редко и не влияют на согласованность разделов между собой.
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return in, err
	}
	for _, r := range roles {
		in.Roles = append(in.Roles, overview.Role{ID: r.ID, Title: r.Title, Color: r.Color})
	}
	eff, err := s.EffectivePolicy(ctx, q.ProjectID)
	if err != nil {
		return in, err
	}
	in.DailyTokenBudget = eff.Presets.DailyTokenBudget
	in.MonthlyBudgetUSD = eff.Presets.MonthlyBudgetUSD

	since := overviewWindowStart(q, th)
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, q.ProjectID).Scan(&in.ProjectName); err != nil {
			return nf(err)
		}
		steps := []func(context.Context, pgx.Tx, *overview.Input, OverviewQuery, time.Time) error{
			overviewEpics, overviewTasks, overviewSessions, overviewUsage, overviewStepRuns,
			overviewMergeConflicts, overviewEscalations, overviewMergeWaits, overviewMySteps,
			overviewDeployments,
		}
		for _, step := range steps {
			if err := step(ctx, tx, &in, q, since); err != nil {
				return err
			}
		}
		return nil
	})
	return in, err
}

// overviewEpics — Epic'и проекта (без импортированной истории) с полным
// расходом и моментом появления плана.
func overviewEpics(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, _ time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT e.id, e.title, e.status, e.created_at,
		       COALESCE((SELECT max(ev.ts) FROM events ev
		                 WHERE ev.project_id = e.project_id AND ev.epic_id = e.id AND ev.type = 'epic.decomposed'),
		                (SELECT min(t.created_at) FROM tasks t WHERE t.epic_id = e.id)),
		       e.started_at, e.done_at, e.due_at, e.token_budget,
		       COALESCE(u.records, 0), COALESCE(u.cost_records, 0), COALESCE(u.tin, 0), COALESCE(u.tout, 0),
		       COALESCE(u.cost, 0), COALESCE(u.dur, 0), COALESCE(u.tokens, 0)
		FROM epics e
		LEFT JOIN LATERAL (
			SELECT count(*) AS records, count(*) FILTER (WHERE `+costKnownSQL("")+`) AS cost_records,
			       SUM(COALESCE(tokens_in, 0)) AS tin, SUM(COALESCE(tokens_out, 0)) AS tout,
			       SUM(COALESCE(cost_usd, 0))::float8 AS cost, SUM(duration_s) AS dur,
			       SUM(COALESCE(tokens_in, 0) + COALESCE(tokens_out, 0))
			           FILTER (WHERE tokens_in IS NOT NULL OR tokens_out IS NOT NULL) AS tokens
			FROM usage_records WHERE epic_id = e.id) u ON true
		WHERE e.project_id = $1 AND e.source_key IS NULL
		ORDER BY e.created_at, e.id`, q.ProjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e overview.Epic
		var used int64
		if err := rows.Scan(&e.ID, &e.Title, &e.Status, &e.CreatedAt, &e.PlannedAt, &e.StartedAt, &e.DoneAt,
			&e.DueAt, &e.TokenBudget, &e.Spend.Records, &e.Spend.CostRecords, &e.Spend.TokensIn,
			&e.Spend.TokensOut, &e.Spend.CostUSD, &e.Spend.DurationS, &used); err != nil {
			return err
		}
		// Остановка по бюджету — то же правило, что у планировщика
		// (ExceededEpicBudgets): работающий Epic, учтённый расход достиг
		// бюджета.
		e.BudgetExhausted = e.Status == overview.EpicRunning && e.TokenBudget != nil && used >= *e.TokenBudget
		if e.BudgetExhausted {
			e.PauseReason = "исчерпан бюджет токенов"
		}
		in.Epics = append(in.Epics, e)
	}
	return rows.Err()
}

// overviewTasks — задачи Epic'ов обзора: время, попытки, последнее событие,
// ожидание runner'а и полный расход.
func overviewTasks(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, _ time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.epic_id, t.num, t.title, t.status, t.estimate, t.created_at, t.started_at, t.done_at,
		       t.attempt_used, t.attempt_limit, le.ts, CASE WHEN t.wait_reason <> '' THEN t.wait_since END,
		       COALESCE(u.records, 0), COALESCE(u.cost_records, 0), COALESCE(u.tin, 0), COALESCE(u.tout, 0),
		       COALESCE(u.cost, 0), COALESCE(u.dur, 0)
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id AND e.project_id = $1 AND e.source_key IS NULL
		LEFT JOIN LATERAL (SELECT ev.ts FROM events ev WHERE ev.task_id = t.id ORDER BY ev.id DESC LIMIT 1) le ON true
		LEFT JOIN LATERAL (
			SELECT count(*) AS records, count(*) FILTER (WHERE `+costKnownSQL("")+`) AS cost_records,
			       SUM(COALESCE(tokens_in, 0)) AS tin, SUM(COALESCE(tokens_out, 0)) AS tout,
			       SUM(COALESCE(cost_usd, 0))::float8 AS cost, SUM(duration_s) AS dur
			FROM usage_records WHERE task_id = t.id) u ON true
		ORDER BY t.num`, q.ProjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t overview.Task
		var num int64
		if err := rows.Scan(&t.ID, &t.EpicID, &num, &t.Title, &t.Status, &t.Estimate, &t.CreatedAt, &t.StartedAt,
			&t.DoneAt, &t.AttemptUsed, &t.AttemptLimit, &t.LastEventAt, &t.WaitingRunnerSince,
			&t.Spend.Records, &t.Spend.CostRecords, &t.Spend.TokensIn, &t.Spend.TokensOut, &t.Spend.CostUSD,
			&t.Spend.DurationS); err != nil {
			return err
		}
		t.Num = int(num)
		in.Tasks = append(in.Tasks, t)
	}
	return rows.Err()
}

// overviewSessions — сессии задач проекта, пересекающие окно, и незакрытые.
// Окно начинается не позже запуска работающих Epic'ов: запасной темп Epic'а
// считается с момента его запуска. Для отчёта работающими на срез могли
// быть и Epic'и, завершённые позже начала периода, их запуск тоже входит в
// окно. Роль и агент — из запуска шага по сессии.
func overviewSessions(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	from := since
	for _, e := range in.Epics {
		active := e.Status == overview.EpicRunning || e.Status == overview.EpicPaused
		if q.Report {
			active = active || e.DoneAt == nil || !e.DoneAt.Before(q.Period.From)
		}
		if active && e.StartedAt != nil && e.StartedAt.Before(from) {
			from = *e.StartedAt
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.task_id, t.epic_id, s.driver_kind = 'user', COALESCE(r.role, ''), COALESCE(r.agent_id, ''),
		       COALESCE(ta.name, ''), s.started_at, s.ended_at
		FROM sessions s
		JOIN tasks t ON t.id = s.task_id
		JOIN epics e ON e.id = t.epic_id AND e.project_id = $1 AND e.source_key IS NULL
		LEFT JOIN LATERAL (SELECT sr.role, sr.agent_id FROM task_step_runs sr
		                   WHERE sr.session_id = s.id ORDER BY sr.id LIMIT 1) r ON true
		LEFT JOIN team_agents ta ON ta.id = r.agent_id
		WHERE s.ended_at IS NULL OR s.ended_at >= $2
		ORDER BY s.started_at, s.id`, q.ProjectID, from)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.Session
		if err := rows.Scan(&x.ID, &x.TaskID, &x.EpicID, &x.Human, &x.RoleID, &x.AgentID, &x.AgentName,
			&x.Start, &x.End); err != nil {
			return err
		}
		in.Sessions = append(in.Sessions, x)
	}
	return rows.Err()
}

// overviewUsage — записи usage проекта с окна. Для отчёта — ещё все записи
// Epic'ов, которые могут попасть в отчёт: не завершённых до начала периода
// (расход Epic'ов и задач пересчитывается из записей до среза). Роль
// записи — из запуска шага её сессии, у запросов control plane без сессии
// (Аналитик, add-product-discovery, Релиз-инженер и Инженер сопровождения,
// add-sdlc-cycle) — из самой записи, иначе «без роли».
func overviewUsage(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT u.ts, COALESCE(u.epic_id::text, ''), COALESCE(u.task_id::text, ''), COALESCE(r.role, u.role, ''),
		       u.model, COALESCE(u.tokens_in, 0), COALESCE(u.tokens_out, 0), COALESCE(u.cost_usd, 0)::float8,
		       `+costKnownSQL("u.")+`, u.duration_s
		FROM usage_records u
		LEFT JOIN epics e ON e.id = u.epic_id
		LEFT JOIN LATERAL (SELECT sr.role FROM task_step_runs sr
		                   WHERE u.session_id IS NOT NULL AND sr.session_id = u.session_id
		                   ORDER BY sr.id LIMIT 1) r ON true
		WHERE u.project_id = $1 AND (e.id IS NULL OR e.source_key IS NULL)
		  AND (u.ts >= $2 OR ($3 AND u.epic_id IS NOT NULL AND (e.done_at IS NULL OR e.done_at >= $4)))
		ORDER BY u.ts, u.id`, q.ProjectID, since, q.Report, q.Period.From)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u overview.UsageRecord
		if err := rows.Scan(&u.TS, &u.EpicID, &u.TaskID, &u.RoleID, &u.Model, &u.TokensIn, &u.TokensOut,
			&u.CostUSD, &u.CostKnown, &u.DurationS); err != nil {
			return err
		}
		in.Usage = append(in.Usage, u)
	}
	return rows.Err()
}

// overviewStepRuns — запуски шагов с вердиктом: вид шага, роль, время
// вердикта. Для «review с первого раза» нужны все запуски задач,
// завершённых в окне, а не только запуски окна.
func overviewStepRuns(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT r.task_id, r.step_kind, r.role, r.verdict, COALESCE(r.finished_at, r.created_at)
		FROM task_step_runs r
		JOIN tasks t ON t.id = r.task_id
		JOIN epics e ON e.id = t.epic_id AND e.project_id = $1 AND e.source_key IS NULL
		WHERE r.verdict IS NOT NULL AND r.verdict <> 'cancelled'
		  AND (COALESCE(r.finished_at, r.created_at) >= $2 OR t.done_at >= $2)
		ORDER BY COALESCE(r.finished_at, r.created_at), r.id`, q.ProjectID, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.StepRun
		if err := rows.Scan(&x.TaskID, &x.StepKind, &x.RoleID, &x.Verdict, &x.At); err != nil {
			return err
		}
		in.StepRuns = append(in.StepRuns, x)
	}
	return rows.Err()
}

// overviewMergeConflicts — события конфликта при merge с окна.
func overviewMergeConflicts(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT task_id, ts, COALESCE((payload->>'exhausted')::boolean, false)
		FROM events
		WHERE project_id = $1 AND type = 'task.merge_conflict' AND ts >= $2 AND task_id IS NOT NULL
		ORDER BY ts, id`, q.ProjectID, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.MergeConflict
		if err := rows.Scan(&x.TaskID, &x.At, &x.Exhausted); err != nil {
			return err
		}
		in.MergeConflicts = append(in.MergeConflicts, x)
	}
	return rows.Err()
}

// overviewEscalations — открытые и взятые эскалации и те, что открылись или
// закрылись в окне. Эскалации публикаций без задачи тоже входят.
func overviewEscalations(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT a.id, COALESCE(a.task_id::text, ''), COALESCE(t.epic_id::text, ''), COALESCE(t.num, 0),
		       COALESCE(t.title, a.message), a.reason, a.status, COALESCE(a.claimed_by, ''),
		       a.created_at, a.resolved_at
		FROM attention a
		LEFT JOIN tasks t ON t.id = a.task_id
		WHERE a.project_id = $1
		  AND (a.status <> 'resolved' OR a.created_at >= $2 OR a.resolved_at >= $2)
		ORDER BY a.created_at, a.id`, q.ProjectID, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.Escalation
		var num int64
		if err := rows.Scan(&x.ID, &x.TaskID, &x.EpicID, &num, &x.Title, &x.Reason, &x.Status, &x.ClaimedBy,
			&x.OpenedAt, &x.ResolvedAt); err != nil {
			return err
		}
		x.TaskNum = int(num)
		in.Escalations = append(in.Escalations, x)
	}
	return rows.Err()
}

// overviewMergeWaits — задачи на шаге merge, ожидающие подтверждения
// человеком, с моментом начала ожидания (последнее review_passed).
func overviewMergeWaits(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, _ time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.epic_id, t.num, t.title,
		       COALESCE((SELECT max(ev.ts) FROM events ev WHERE ev.task_id = t.id AND ev.type = 'task.review_passed'),
		                t.updated_at)
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id AND e.project_id = $1 AND e.source_key IS NULL
		WHERE t.status = 'review' AND t.process IS NOT NULL
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements(
		                  CASE WHEN jsonb_typeof(t.process->'steps') = 'array' THEN t.process->'steps' ELSE '[]'::jsonb END) st
		              WHERE st->>'id' = t.step_id AND st->>'kind' = 'merge')
		ORDER BY t.num`, q.ProjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.MergeWait
		var num int64
		if err := rows.Scan(&x.TaskID, &x.EpicID, &num, &x.Title, &x.Since); err != nil {
			return err
		}
		x.TaskNum = int(num)
		in.MergeWaits = append(in.MergeWaits, x)
	}
	return rows.Err()
}

// overviewMySteps — шаги человека в проекте, адресованные зрителю (логином
// или ролью проекта). Отчёт их не использует.
func overviewMySteps(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, _ time.Time) error {
	if q.Report || q.UserID == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id::text, r.task_id, t.epic_id, t.num, t.title, r.created_at
		FROM task_step_runs r
		JOIN tasks t ON t.id = r.task_id
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running' AND e.project_id = $1
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2::uuid
		WHERE r.verdict IS NULL AND (r.user_login = $3 OR (r.user_role <> '' AND r.user_role = m.role))
		ORDER BY r.created_at, r.id`, q.ProjectID, q.UserID, q.Login)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x overview.MyStep
		var num int64
		if err := rows.Scan(&x.RunID, &x.TaskID, &x.EpicID, &num, &x.Title, &x.Since); err != nil {
			return err
		}
		x.TaskNum = int(num)
		in.MySteps = append(in.MySteps, x)
	}
	return rows.Err()
}

// overviewDeployments — окружения проекта с последней выкаткой, выкатки с
// окна и последняя выкатка каждого окружения до начала периода (для среза
// отчёта).
func overviewDeployments(ctx context.Context, tx pgx.Tx, in *overview.Input, q OverviewQuery, since time.Time) error {
	envRows, err := tx.Query(ctx, `SELECT id::text, name FROM environments WHERE project_id = $1 ORDER BY name, id`, q.ProjectID)
	if err != nil {
		return err
	}
	var envs []overview.Environment
	for envRows.Next() {
		var e overview.Environment
		if err := envRows.Scan(&e.ID, &e.Name); err != nil {
			envRows.Close()
			return err
		}
		envs = append(envs, e)
	}
	envRows.Close()
	if err := envRows.Err(); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT d.id::text, d.env_id::text, d.version, d.status, d.initiator, d.created_at, d.ended_at, d.rollback,
		       d.id = (SELECT d2.id FROM deployments d2 WHERE d2.env_id = d.env_id ORDER BY d2.created_at DESC, d2.id DESC LIMIT 1)
		FROM deployments d
		JOIN environments v ON v.id = d.env_id AND v.project_id = $1
		WHERE d.created_at >= $2
		   OR d.id = (SELECT d2.id FROM deployments d2 WHERE d2.env_id = d.env_id ORDER BY d2.created_at DESC, d2.id DESC LIMIT 1)
		   OR d.id = (SELECT d3.id FROM deployments d3 WHERE d3.env_id = d.env_id AND d3.created_at < $3
		              ORDER BY d3.created_at DESC, d3.id DESC LIMIT 1)
		ORDER BY d.created_at, d.id`, q.ProjectID, since, q.Period.From)
	if err != nil {
		return err
	}
	defer rows.Close()
	last := map[string]overview.Deployment{}
	for rows.Next() {
		var d overview.Deployment
		var isLast bool
		if err := rows.Scan(&d.ID, &d.EnvID, &d.Version, &d.Status, &d.Initiator, &d.CreatedAt, &d.EndedAt,
			&d.Rollback, &isLast); err != nil {
			return err
		}
		in.Deployments = append(in.Deployments, d)
		if isLast {
			last[d.EnvID] = d
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range envs {
		if d, ok := last[envs[i].ID]; ok {
			d := d
			envs[i].Last = &d
		}
	}
	in.Environments = envs
	return nil
}

// ─── целевая дата Epic'а ─────────────────────────────────────────────────

// ErrEpicArchived — правка архивного Epic'а (409).
var ErrEpicArchived = fmt.Errorf("%w: архивный Epic не редактируется", ErrConflict)

// EpicPatch — правка Epic'а владельцем: бюджет в токенах и целевая дата.
// SetBudget и SetDue говорят, какие поля пришли; nil снимает значение.
type EpicPatch struct {
	SetBudget bool
	Budget    *int64
	SetDue    bool
	Due       *time.Time
}

// PatchEpic — единственный путь записи бюджета и целевой даты Epic'а.
// Правка идёт одной транзакцией под блокировкой строки: всё проверяется до
// записей, поэтому неверное поле не оставляет сохранённым другое. Целевая
// дата пишет событие epic.due_changed с прежним и новым значением, бюджет —
// событие epic.status. Архивный Epic не получает целевую дату (409).
// Возобновление назначений после правки бюджета — следующим проходом
// планировщика.
func (s *Store) PatchEpic(ctx context.Context, epicID string, p EpicPatch, actorLogin string) (domain.Epic, error) {
	if p.SetBudget && p.Budget != nil && *p.Budget < 1 {
		return domain.Epic{}, fmt.Errorf("%w: бюджет должен быть не меньше 1 токена", ErrInvalid)
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status, projectID string
		var before *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, project_id, due_at FROM epics WHERE id = $1 FOR UPDATE`, epicID).
			Scan(&status, &projectID, &before); err != nil {
			return nf(err)
		}
		if p.SetDue && status == string(domain.EpicArchived) {
			return ErrEpicArchived
		}
		if p.SetDue {
			if _, err := tx.Exec(ctx, `UPDATE epics SET due_at = $2 WHERE id = $1`, epicID, p.Due); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "epic.due_changed",
				ProjectID: projectID, EpicID: epicID, Text: dueChangeText(p.Due),
				Payload: map[string]any{"before": rfc3339(before), "after": rfc3339(p.Due)},
			}); err != nil {
				return err
			}
		}
		if p.SetBudget {
			if _, err := tx.Exec(ctx, `UPDATE epics SET token_budget = $2 WHERE id = $1`, epicID, p.Budget); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "epic.status",
				ProjectID: projectID, EpicID: epicID, Text: "бюджет Epic изменён",
				Payload: map[string]any{"token_budget": p.Budget},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Epic{}, ErrNotFound
		}
		return domain.Epic{}, err
	}
	return s.GetEpic(ctx, epicID)
}

func dueChangeText(due *time.Time) string {
	if due == nil {
		return "целевая дата Epic снята"
	}
	return "целевая дата Epic: " + due.UTC().Format("2006-01-02 15:04") + " UTC"
}

func rfc3339(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
