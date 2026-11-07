package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Цикл разработки (add-sdlc-cycle, спека sdlc-cycle): фаза не хранится,
// store читает строки, а относит их к фазам пакет cycle. Здесь же хук
// событий задачи: поля phase_from и phase_to при смене фазы.

// phaseEventTypes — события задачи, которые могут менять её фазу.
var phaseEventTypes = map[string]bool{
	"task.status": true, "task.step": true, "task.wait": true,
	"task.review_passed": true, "task.merge_deferred": true, "task.merge_conflict": true,
}

// cycleTaskCols — задача для отнесения к фазе.
const cycleTaskCols = `t.id::text, t.epic_id::text, t.num, t.title, t.kind, t.status, t.step_id, t.process, t.merged_at,
	t.updated_at, ep.status = 'running',
	t.status IN ('blocked','failed') OR EXISTS (SELECT 1 FROM attention a WHERE a.task_id = t.id AND a.status <> 'resolved')`

func scanCycleTask(row pgx.Row) (cycle.Task, error) {
	var t cycle.Task
	var num int64
	var proc []byte
	err := row.Scan(&t.ID, &t.EpicID, &num, &t.Title, &t.Kind, &t.Status, &t.StepID, &proc, &t.MergedAt,
		&t.Since, &t.EpicRunning, &t.NeedsHuman)
	if err != nil {
		return t, err
	}
	t.Num = int(num)
	if len(proc) > 0 {
		var p struct {
			Steps []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"steps"`
		}
		if json.Unmarshal(proc, &p) == nil {
			for _, s := range p.Steps {
				t.Process = append(t.Process, cycle.Step{ID: s.ID, Kind: s.Kind})
				if s.ID == t.StepID && s.Kind == "merge" && t.Status == string(domain.TaskReview) {
					t.AwaitingMerge = true
				}
			}
		}
	}
	return t, nil
}

// releaseCutTx — граница релиза проекта: окружения с автозапуском и их
// последняя успешная публикация.
func releaseCutTx(ctx context.Context, q dbtx, projectID string) (cycle.ReleaseCut, error) {
	var c cycle.ReleaseCut
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM environments WHERE project_id=$1 AND trigger='auto'),
		       (SELECT max(d.version_at) FROM deployments d JOIN environments e ON e.id = d.env_id
		        WHERE e.project_id=$1 AND e.trigger='auto' AND d.status='done')`, projectID).
		Scan(&c.AutoEnv, &c.LastCreated)
	return c, err
}

// withPhase дописывает в событие задачи phase_from и phase_to, если фаза
// задачи после изменения отличается от последней записанной. Фаза до
// изменения — phase_to последнего такого события задачи (без него задача
// считается вне цикла).
func withPhase(ctx context.Context, tx pgx.Tx, e *EventInput) error {
	if e.TaskID == "" || !phaseEventTypes[e.Type] {
		return nil
	}
	payload, ok := e.Payload.(map[string]any)
	if !ok && e.Payload != nil {
		return nil
	}
	if _, set := payload["phase_to"]; set {
		return nil
	}
	t, err := scanCycleTask(tx.QueryRow(ctx, `SELECT `+cycleTaskCols+`
		FROM tasks t JOIN epics ep ON ep.id = t.epic_id WHERE t.id=$1`, e.TaskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("фаза задачи: %w", err)
	}
	projectID := e.ProjectID
	if projectID == "" {
		if err := tx.QueryRow(ctx, `SELECT ep.project_id::text FROM tasks t JOIN epics ep ON ep.id=t.epic_id WHERE t.id=$1`,
			e.TaskID).Scan(&projectID); err != nil {
			return err
		}
	}
	cut, err := releaseCutTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	after := cycle.PhaseOfTask(t, cut).Phase
	var before *string
	err = tx.QueryRow(ctx, `
		SELECT payload->>'phase_to' FROM events
		WHERE task_id=$1 AND payload ? 'phase_to' ORDER BY id DESC LIMIT 1`, e.TaskID).Scan(&before)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	from := cycle.Phase("")
	if before != nil {
		from = cycle.Phase(*before)
	}
	if from == after {
		return nil
	}
	if payload == nil {
		payload = map[string]any{}
	} else {
		cp := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			cp[k] = v
		}
		payload = cp
	}
	payload["phase_from"] = cycle.PhasePtr(from)
	payload["phase_to"] = cycle.PhasePtr(after)
	e.Payload = payload
	return nil
}

// CycleBusy — занятость агентов control plane (задания Релиз-инженера и
// Инженера сопровождения ведёт оркестратор в памяти).
type CycleBusy map[string]*cycle.Work

// LoadCycle — агрегат цикла проекта в одной согласованной транзакции.
func (s *Store) LoadCycle(ctx context.Context, projectID, period string, loc *time.Location, busy CycleBusy) (cycle.View, error) {
	var in cycle.Input
	if period == "" {
		period = cycle.DefaultPeriod
	}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		from, err := cycle.PeriodStart(period, now, loc)
		if err != nil {
			return err
		}
		in = cycle.Input{ProjectID: projectID, Period: period, GeneratedAt: now,
			DonePeriod: map[cycle.Phase]int{}, Available: map[cycle.Phase]bool{}}
		if in.Cut, err = releaseCutTx(ctx, tx, projectID); err != nil {
			return err
		}
		if err := loadCycleTasks(ctx, tx, projectID, &in); err != nil {
			return err
		}
		if err := loadCycleEpics(ctx, tx, projectID, &in); err != nil {
			return err
		}
		if err := loadCycleOps(ctx, tx, projectID, &in); err != nil {
			return err
		}
		if err := loadCycleDiscovery(ctx, tx, projectID, from, &in); err != nil {
			return err
		}
		if err := loadCycleArchitecture(ctx, tx, projectID, from, &in); err != nil {
			return err
		}
		if err := loadCycleAgents(ctx, tx, projectID, busy, &in); err != nil {
			return err
		}
		if err := loadCycleArtifacts(ctx, tx, projectID, &in); err != nil {
			return err
		}
		return loadCyclePeriod(ctx, tx, projectID, from, &in)
	})
	if err != nil {
		return cycle.View{}, err
	}
	return cycle.Build(in), nil
}

func loadCycleTasks(ctx context.Context, tx pgx.Tx, projectID string, in *cycle.Input) error {
	// Незавершённые задачи и смерженные после последней публикации с
	// автозапуском (ожидают релиза).
	rows, err := tx.Query(ctx, `SELECT `+cycleTaskCols+`
		FROM tasks t JOIN epics ep ON ep.id = t.epic_id
		WHERE ep.project_id=$1 AND ep.status <> 'archived'
		  AND (t.status NOT IN ('done','cancelled')
		       OR (t.status = 'done' AND $2 AND t.merged_at IS NOT NULL
		           AND ($3::timestamptz IS NULL OR t.merged_at > $3)))`,
		projectID, in.Cut.AutoEnv, in.Cut.LastCreated)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanCycleTask(rows)
		if err != nil {
			return err
		}
		in.Tasks = append(in.Tasks, t)
	}
	return rows.Err()
}

func loadCycleEpics(ctx context.Context, tx pgx.Tx, projectID string, in *cycle.Input) error {
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, e.title, e.kind, e.status, e.created_at
		FROM epics e WHERE e.project_id=$1 AND e.status NOT IN ('done','archived')`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e cycle.Epic
		if err := rows.Scan(&e.ID, &e.Title, &e.Kind, &e.Status, &e.Since); err != nil {
			return err
		}
		in.Epics = append(in.Epics, e)
	}
	return rows.Err()
}

func loadCycleOps(ctx context.Context, tx pgx.Tx, projectID string, in *cycle.Input) error {
	rows, err := tx.Query(ctx, `
		SELECT d.id::text, e.name || ' · ' || left(d.version, 12), d.status, d.rollback, COALESCE(d.started_at, d.created_at)
		FROM deployments d JOIN environments e ON e.id = d.env_id
		WHERE e.project_id=$1 AND d.status IN ('queued','deploying','verifying')`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d cycle.Deployment
		if err := rows.Scan(&d.ID, &d.Title, &d.Status, &d.Rollback, &d.Since); err != nil {
			rows.Close()
			return err
		}
		in.Deployments = append(in.Deployments, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `
		SELECT i.id::text, CASE WHEN i.title <> '' THEN i.title ELSE i.error_line END, i.env_id::text, i.status,
			i.needs_human OR (i.bug_task_id IS NULL AND NOT i.bug_pending AND EXISTS (
				SELECT 1 FROM attention a WHERE a.project_id = i.project_id AND a.reason = 'OPS_LIMIT' AND a.status <> 'resolved')),
			CASE WHEN i.status = 'mitigated' THEN COALESCE(i.mitigated_at, i.first_seen) ELSE i.first_seen END
		FROM incidents i WHERE i.project_id=$1 AND i.status IN ('open','mitigated')`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var i cycle.Incident
		if err := rows.Scan(&i.ID, &i.Title, &i.EnvID, &i.Status, &i.NeedsHuman, &i.Since); err != nil {
			rows.Close()
			return err
		}
		in.Incidents = append(in.Incidents, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `
		SELECT e.id::text, e.name, COALESCE(ms.state, 'unknown'), COALESCE(ms.last_change_at, e.created_at)
		FROM environments e LEFT JOIN monitor_state ms ON ms.env_id = e.id
		WHERE e.project_id=$1 AND COALESCE((e.config->'monitor'->>'enabled')::boolean, false)`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e cycle.EnvMonitor
		if err := rows.Scan(&e.EnvID, &e.Name, &e.State, &e.Since); err != nil {
			rows.Close()
			return err
		}
		in.Envs = append(in.Envs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Сопровождение «есть данные», когда у проекта настроено наблюдение
	// или были инциденты. Исследование и архитектуру читают
	// loadCycleDiscovery и loadCycleArchitecture.
	var opsAvail bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM environments WHERE project_id=$1 AND COALESCE((config->'monitor'->>'enabled')::boolean, false))
		    OR EXISTS (SELECT 1 FROM incidents WHERE project_id=$1)`, projectID).Scan(&opsAvail); err != nil {
		return err
	}
	in.Available[cycle.Operations] = opsAvail
	return nil
}

// loadCycleAgents — команда с текущей работой: сессии runner'ов из
// запусков шагов, задания control plane из памяти оркестратора, идущие
// анализы Аналитика из версий исследования.
func loadCycleAgents(ctx context.Context, tx pgx.Tx, projectID string, busy CycleBusy, in *cycle.Input) error {
	planner, _, err := plannerModelTx(ctx, tx, false)
	if err != nil {
		return err
	}
	analyses, err := cycleAnalyses(ctx, tx, projectID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name, a.role_id, ro.title, ro.capability, a.connection_id, a.model,
			COALESCE(ro.default_model->>'connection_id', ''), COALESCE(ro.default_model->>'model', ''),
			a.enabled AND ro.enabled,
			COALESCE(w.task_id::text, ''), COALESCE(w.num, 0), COALESCE(w.title, ''), COALESCE(w.step_kind, ''),
			COALESCE(w.project_id::text, ''), COALESCE(w.runner_id, ''),
			-- подходящий online-runner по тем же правилам, что подбор при
			-- назначении (WaitingTeamRuns): capability роли, профиль, модель
			ro.capability <> '' AND EXISTS (SELECT 1 FROM runners x WHERE x.status <> 'offline' AND NOT x.draining AND `+agentRunner("x")+`
			        AND x.capabilities @> ARRAY[ro.capability]
			        AND x.agent = COALESCE(NULLIF(a.agent, ''), NULLIF(ro.agent, ''), x.agent)
			        AND (COALESCE(NULLIF(a.model, ''), ro.default_model->>'model', '') = ''
			             OR COALESCE(NULLIF(a.model, ''), ro.default_model->>'model') = ANY(x.models)
			             OR x.model = COALESCE(NULLIF(a.model, ''), ro.default_model->>'model')))
		FROM team_agents a JOIN agent_roles ro ON ro.id = a.role_id
		LEFT JOIN LATERAL (
			SELECT r.task_id, t.num, t.title, e.project_id, r.step_kind, r.runner_id
			FROM task_step_runs r JOIN tasks t ON t.id = r.task_id JOIN epics e ON e.id = t.epic_id
			WHERE r.agent_id = a.id AND r.verdict IS NULL
			ORDER BY r.id DESC LIMIT 1) w ON true
		ORDER BY a.position, a.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a cycle.Agent
		var defConn, defModel, taskID, title, stepKind, workProject string
		var num int64
		if err := rows.Scan(&a.ID, &a.Name, &a.RoleID, &a.RoleTitle, &a.Capability, &a.ConnectionID, &a.Model,
			&defConn, &defModel, &a.Enabled, &taskID, &num, &title, &stepKind, &workProject, &a.RunnerID,
			&a.RunnerOnline); err != nil {
			return err
		}
		switch {
		case a.RoleID == domain.RoleLead:
			// Модель Тимлида — настройка планировщика (add-agent-roles).
			a.ConnectionID, a.Model = planner.ConnectionID, planner.Model
		case a.Model == "":
			a.ConnectionID, a.Model = defConn, defModel
		}
		switch {
		case taskID != "" && workProject == projectID:
			a.Current = &cycle.Work{Kind: "task", ID: taskID, Num: int(num), Title: title, Action: stepAction(stepKind)}
		case taskID != "":
			a.Current, a.RunnerID = cycle.Elsewhere(stepAction(stepKind)), ""
		default:
			a.RunnerID = ""
		}
		for _, w := range []*cycle.Work{busy[a.ID], analyses[a.ID]} {
			if w == nil || a.Current != nil {
				continue
			}
			if w.ProjectID == "" || w.ProjectID == projectID {
				a.Current = w
			} else {
				a.Current = cycle.Elsewhere(w.Action)
			}
		}
		in.Agents = append(in.Agents, a)
	}
	return rows.Err()
}

// stepAction — действие агента по типу шага.
func stepAction(kind string) string {
	switch kind {
	case "code":
		return "пишет код"
	case "test":
		return "тестирует"
	case "review":
		return "проверяет"
	case "prompt":
		return "выполняет задание"
	}
	return "работает"
}

// loadCycleArtifacts — последние артефакты фаз со ссылками.
func loadCycleArtifacts(ctx context.Context, tx pgx.Tx, projectID string, in *cycle.Input) error {
	add := func(ph cycle.Phase, a cycle.Artifact) {
		a.Phase = ph
		in.Artifacts = append(in.Artifacts, a)
	}
	// Планирование: утверждённые планы (Epic'и, запущенные после плана).
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, e.title, COALESCE((SELECT max(ev.ts) FROM events ev WHERE ev.epic_id = e.id AND ev.type = 'epic.status'
			AND ev.payload->>'status' = 'running'), e.created_at)
		FROM epics e WHERE e.project_id=$1 AND e.kind <> 'maintenance' AND e.status IN ('running','paused','done')
		ORDER BY 3 DESC LIMIT 5`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a cycle.Artifact
		if err := rows.Scan(&a.ID, &a.Title, &a.At); err != nil {
			rows.Close()
			return err
		}
		a.Kind, a.Ref = "plan", cycle.Ref{View: "epic", ID: a.ID}
		add(cycle.Planning, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Разработка: PR задач. Задача описания архитектуры шла в фазе
	// «Архитектура»: её PR и вердикты в артефакты разработки, тестирования
	// и ревью не входят.
	rows, err = tx.Query(ctx, `
		SELECT t.id::text, 'task-' || t.num || ' · ' || t.title, t.pr_url, t.updated_at
		FROM tasks t JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id=$1 AND COALESCE(t.pr_url, '') <> '' AND t.kind <> 'architecture'
		ORDER BY t.updated_at DESC LIMIT 5`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a cycle.Artifact
		var url string
		if err := rows.Scan(&a.ID, &a.Title, &url, &a.At); err != nil {
			rows.Close()
			return err
		}
		a.Kind, a.Ref = "pr", cycle.Ref{URL: url}
		add(cycle.Development, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Тестирование и ревью: вердикты шагов test и review.
	rows, err = tx.Query(ctx, `
		SELECT ev.id::text, ev.type, ev.text, ev.ts, ev.task_id::text
		FROM events ev JOIN tasks t ON t.id = ev.task_id
		WHERE ev.project_id=$1 AND t.kind <> 'architecture'
		  AND ev.type IN ('task.verdict', 'task.review_passed', 'task.review_external', 'task.checks_external')
		ORDER BY ev.id DESC LIMIT 20`, projectID)
	if err != nil {
		return err
	}
	tested, reviewed := 0, 0
	for rows.Next() {
		var a cycle.Artifact
		var typ, taskID string
		if err := rows.Scan(&a.ID, &typ, &a.Title, &a.At, &taskID); err != nil {
			rows.Close()
			return err
		}
		a.Ref = cycle.Ref{View: "task", ID: taskID}
		switch typ {
		case "task.checks_external":
			if tested < cycle.MaxArtifacts {
				a.Kind = "check"
				add(cycle.Testing, a)
				tested++
			}
		case "task.verdict":
			if tested < cycle.MaxArtifacts {
				a.Kind = "check"
				add(cycle.Testing, a)
				tested++
			}
		default:
			if reviewed < cycle.MaxArtifacts {
				a.Kind = "review"
				add(cycle.Review, a)
				reviewed++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Релиз: релизы с release notes.
	rows, err = tx.Query(ctx, `
		SELECT r.id::text, e.name || ' · ' || left(r.version, 12), r.created_at
		FROM releases r JOIN environments e ON e.id = r.env_id WHERE r.project_id=$1
		ORDER BY r.created_at DESC LIMIT 5`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a cycle.Artifact
		if err := rows.Scan(&a.ID, &a.Title, &a.At); err != nil {
			rows.Close()
			return err
		}
		a.Kind, a.Ref = "release", cycle.Ref{View: "release", ID: a.ID}
		add(cycle.Release, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Сопровождение: инциденты.
	rows, err = tx.Query(ctx, `
		SELECT i.id::text, CASE WHEN i.title <> '' THEN i.title ELSE i.error_line END, i.last_seen
		FROM incidents i WHERE i.project_id=$1 ORDER BY i.last_seen DESC LIMIT 5`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a cycle.Artifact
		if err := rows.Scan(&a.ID, &a.Title, &a.At); err != nil {
			rows.Close()
			return err
		}
		a.Kind, a.Ref = "incident", cycle.Ref{View: "incident", ID: a.ID}
		add(cycle.Operations, a)
	}
	rows.Close()
	return rows.Err()
}

// loadCyclePeriod — завершённые за период, последний релиз и расход.
func loadCyclePeriod(ctx context.Context, tx pgx.Tx, projectID string, from time.Time, in *cycle.Input) error {
	var devDone, released, resolved, planned int
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM tasks t JOIN epics e ON e.id = t.epic_id
		   WHERE e.project_id=$1 AND t.status='done' AND t.kind <> 'architecture' AND t.merged_at >= $2),
		  (SELECT COALESCE(sum(jsonb_array_length(r.composition)), 0) FROM releases r WHERE r.project_id=$1 AND r.created_at >= $2),
		  (SELECT count(*) FROM incidents WHERE project_id=$1 AND status='resolved' AND resolved_at >= $2),
		  (SELECT count(DISTINCT ev.epic_id) FROM events ev WHERE ev.project_id=$1 AND ev.type='epic.status'
		     AND ev.payload->>'status' = 'running' AND ev.ts >= $2)`, projectID, from).
		Scan(&devDone, &released, &resolved, &planned); err != nil {
		return err
	}
	// Завершённая задача прошла разработку, тестирование и ревью. Задача
	// описания архитектуры шла в фазе «Архитектура» и считается там.
	in.DonePeriod[cycle.Planning] = planned
	in.DonePeriod[cycle.Development] = devDone
	in.DonePeriod[cycle.Testing] = devDone
	in.DonePeriod[cycle.Review] = devDone
	in.DonePeriod[cycle.Release] = released
	in.DonePeriod[cycle.Operations] = resolved

	var lr cycle.LastRelease
	err := tx.QueryRow(ctx, `
		SELECT r.id::text, r.env_id::text, e.name, r.version, r.created_at
		FROM releases r JOIN environments e ON e.id = r.env_id WHERE r.project_id=$1
		ORDER BY r.created_at DESC LIMIT 1`, projectID).Scan(&lr.ID, &lr.EnvID, &lr.EnvName, &lr.Version, &lr.CreatedAt)
	switch {
	case err == nil:
		in.LastRelease = &lr
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	var tokens int64
	var cost *float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(COALESCE(tokens_in, 0) + COALESCE(tokens_out, 0)), 0), sum(cost_usd)::float8
		FROM usage_records WHERE project_id=$1 AND ts >= $2`, projectID, from).Scan(&tokens, &cost); err != nil {
		return err
	}
	in.Tokens, in.SpendUSD = tokens, cost
	return nil
}
