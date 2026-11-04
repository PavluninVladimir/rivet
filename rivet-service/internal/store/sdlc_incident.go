package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/ops"
	"github.com/PavluninVladimir/rivet/internal/redact"
)

// Инциденты сопровождения (add-sdlc-cycle, спека operations): сбой
// проверки наблюдения или провал публикации становится инцидентом с
// дедупликацией по отпечатку. Решения принимает ops.Decide, store только
// читает входы и пишет результат под advisory lock проекта.

// Ограничения доказательств: 200 строк и 64 КиБ на каждую часть.
const (
	evidenceLines = 200
	evidenceBytes = 64 << 10
)

// IncidentEvidence — доказательства инцидента (замаскированы и обрезаны).
type IncidentEvidence struct {
	Stage       string `json:"stage,omitempty"`
	CheckOutput string `json:"check_output,omitempty"`
	Logs        string `json:"logs,omitempty"`
	DeployLog   string `json:"deploy_log,omitempty"`
}

// text — доказательства одним текстом для описания задачи-бага и задания.
func (e IncidentEvidence) text() string {
	var b strings.Builder
	add := func(title, s string) {
		if strings.TrimSpace(s) == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("== " + title + " ==\n" + s)
	}
	if e.Stage != "" {
		add("этап", e.Stage)
	}
	add("вывод проверки", e.CheckOutput)
	add("логи", e.Logs)
	add("лог публикации", e.DeployLog)
	return b.String()
}

// incidentState — инцидент с тем, что нужно для решений ops.
type incidentState struct {
	ops.Incident
	ProjectID, EnvID, DeploymentID string
	BugTaskID, BugTaskStatus       string
	BugCancelledBy                 string
	NeedsHuman, BugPending         bool
	DescriptionStatus              string
	MitigatedAt                    *time.Time
}

const incidentStateCols = `i.id::text, i.source, i.status, i.severity, i.occurrences, i.occurrences_at_mitigation,
	i.first_seen, i.resolved_at, i.skipped_daily, i.project_id::text, i.env_id::text,
	COALESCE(i.deployment_id::text, ''), COALESCE(i.bug_task_id::text, ''), COALESCE(bt.status, ''),
	COALESCE(bt.attempt_used, 0), i.bug_cancelled_by, i.needs_human, i.bug_pending, i.description_status, i.mitigated_at`

const incidentStateFrom = `incidents i LEFT JOIN tasks bt ON bt.id = i.bug_task_id`

func scanIncidentState(row pgx.Row) (incidentState, error) {
	var st incidentState
	var attempts int
	err := row.Scan(&st.ID, &st.Source, &st.Status, &st.Severity, &st.Occurrences, &st.OccurrencesAtMitigation,
		&st.FirstSeen, &st.ResolvedAt, &st.SkippedDaily, &st.ProjectID, &st.EnvID,
		&st.DeploymentID, &st.BugTaskID, &st.BugTaskStatus, &attempts, &st.BugCancelledBy,
		&st.NeedsHuman, &st.BugPending, &st.DescriptionStatus, &st.MitigatedAt)
	if err != nil {
		return st, err
	}
	st.BugState = bugState(st.BugTaskID, st.BugTaskStatus, st.BugCancelledBy, attempts)
	return st, nil
}

func scanIncidentStates(rows pgx.Rows) ([]incidentState, error) {
	defer rows.Close()
	var out []incidentState
	for rows.Next() {
		st, err := scanIncidentState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// bugState — состояние задачи-бага по её строке: отмена человеком
// (логин в bug_cancelled_by) отличается от отмены системой (system), не
// начатая — в очереди без попыток.
func bugState(taskID, status, cancelledBy string, attempts int) ops.BugState {
	human := cancelledBy != "" && cancelledBy != "system"
	switch {
	case taskID == "" || status == string(domain.TaskCancelled):
		switch {
		case human:
			return ops.BugCancelledByHuman
		case cancelledBy == "system":
			return ops.BugCancelled
		}
		return ops.BugNone
	case status == string(domain.TaskDone):
		return ops.BugDone
	case (status == string(domain.TaskQueued) || status == string(domain.TaskReady)) && attempts == 0:
		return ops.BugNotStarted
	}
	return ops.BugActive
}

// lockProjectOps — сериализация решений сопровождения проекта: чтение
// счётчиков, ops.Decide и запись инцидента, бага и эскалации идут под
// одним advisory lock на транзакцию (гонка двух окружений проекта).
func lockProjectOps(ctx context.Context, tx pgx.Tx, projectID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ops:' || $1::text))`, projectID)
	return err
}

// opsCounts — счётчики лимитов проекта. Решение «баг после описания»
// (bug_pending) занимает место в лимите так же, как созданная задача.
func opsCounts(ctx context.Context, tx pgx.Tx, projectID, excludeID string, now time.Time) (ops.Counts, error) {
	var c ops.Counts
	err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM tasks t JOIN epics e ON e.id = t.epic_id
		   WHERE e.project_id = $1 AND t.kind = 'bug' AND t.incident_id IS NOT NULL
		     AND t.status NOT IN ('done','cancelled'))
		  + (SELECT count(*) FROM incidents WHERE project_id = $1 AND bug_pending AND status IN ('open','mitigated')),
		  (SELECT count(*) FROM incidents WHERE project_id = $1 AND first_seen > $3::timestamptz - interval '24 hours'
		     AND id::text <> $2),
		  EXISTS (SELECT 1 FROM attention WHERE project_id = $1 AND reason = 'OPS_LIMIT' AND status <> 'resolved')`,
		projectID, excludeID, now).Scan(&c.UnfinishedBugs, &c.OpenedLast24h, &c.EscalationOpen)
	return c, err
}

// FailureInput — проваленная проверка наблюдения или публикация.
type FailureInput struct {
	EnvID  string
	Source string // ops.SourceMonitor | ops.SourceRelease
	// Output — вывод проверки (monitor) или хвост лога публикации (release).
	Output string
	Logs   string
	Stage  string
	// DeploymentID — упавшая публикация (release).
	DeploymentID string
	// ConsecutiveFail — проваленных проверок окружения подряд, включая эту.
	ConsecutiveFail int
}

// FailureResult — что произошло с инцидентом.
type FailureResult struct {
	IncidentID string
	Action     ops.Action
	// Describe — нужно задание описания (открытие и переоткрытие).
	Describe bool
	// BugTaskID — созданная сразу задача-баг (повтор с готовым описанием).
	BugTaskID string
	EpicID    string
}

// RecordFailure — сбой окружения: открыть, повторить или переоткрыть
// инцидент по отпечатку, решить про задачу-баг и эскалацию.
func (s *Store) RecordFailure(ctx context.Context, in FailureInput) (FailureResult, error) {
	var res FailureResult
	limit := s.bugAttemptLimitOrDefault(ctx, in.EnvID)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var e error
		res, e = recordFailureTx(ctx, tx, in, limit)
		return e
	})
	return res, err
}

// bugAttemptLimitOrDefault — лимит попыток задачи-бага; сбой чтения
// политики не прерывает цепочку провала и наблюдения: 0 даёт createBugTx
// значение по умолчанию.
func (s *Store) bugAttemptLimitOrDefault(ctx context.Context, envID string) int {
	limit, err := s.bugAttemptLimitForEnv(ctx, envID)
	if err != nil {
		slog.Warn("лимит попыток задачи-бага: политика проекта не прочитана, берётся значение по умолчанию", "env", envID, "err", err)
		return 0
	}
	return limit
}

// bugAttemptLimitForEnv — лимит попыток задачи-бага из политики проекта.
func (s *Store) bugAttemptLimitForEnv(ctx context.Context, envID string) (int, error) {
	var projectID string
	if err := s.Pool.QueryRow(ctx, `SELECT project_id::text FROM environments WHERE id=$1`, envID).Scan(&projectID); err != nil {
		return 0, nf(err)
	}
	eff, err := s.EffectivePolicy(ctx, projectID)
	if err != nil {
		return 0, err
	}
	return eff.Presets.AttemptLimit, nil
}

func recordFailureTx(ctx context.Context, tx pgx.Tx, in FailureInput, attemptLimit int) (FailureResult, error) {
	var res FailureResult
	var projectID, envName string
	if err := tx.QueryRow(ctx, `SELECT project_id::text, name FROM environments WHERE id=$1`, in.EnvID).
		Scan(&projectID, &envName); err != nil {
		return res, nf(err)
	}
	if err := lockProjectOps(ctx, tx, projectID); err != nil {
		return res, err
	}
	now := time.Now()
	// Второе маскирование на control plane: runner мог быть старым или
	// чужим. Отпечаток — по полному замаскированному выводу до обрезки.
	output := redact.String(CleanText(in.Output))
	logs := redact.String(CleanText(in.Logs))
	in.Stage = CleanText(in.Stage)
	fp := ops.Fingerprint(in.EnvID, in.Source, output)
	errLine := ops.ErrorLine(output)
	ev := IncidentEvidence{Stage: in.Stage}
	if in.Source == ops.SourceRelease {
		ev.DeployLog = ops.Tail(output, evidenceLines, evidenceBytes)
	} else {
		ev.CheckOutput = ops.Tail(output, evidenceLines, evidenceBytes)
		ev.Logs = ops.Tail(logs, evidenceLines, evidenceBytes)
	}
	evRaw, _ := json.Marshal(ev)

	existing, err := scanIncidentState(tx.QueryRow(ctx, `
		SELECT `+incidentStateCols+` FROM `+incidentStateFrom+`
		WHERE i.project_id = $1 AND i.fingerprint = $2
		ORDER BY (i.status IN ('open','mitigated')) DESC, i.resolved_at DESC NULLS LAST
		LIMIT 1 FOR UPDATE OF i`, projectID, fp))
	var exPtr *ops.Incident
	switch {
	case err == nil:
		exPtr = &existing.Incident
		// Баг ждёт описания: для решения он уже есть и не начат.
		if existing.BugPending && existing.BugState == ops.BugNone {
			existing.BugState = ops.BugNotStarted
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return res, err
	}
	counts, err := opsCounts(ctx, tx, projectID, existing.ID, now)
	if err != nil {
		return res, err
	}
	fails := in.ConsecutiveFail
	if fails < 1 {
		fails = 1
	}
	d := ops.Decide(exPtr, ops.Failure{Now: now, ConsecutiveFail: fails}, counts, ops.DefaultLimits())
	res.Action, res.Describe = d.Action, d.Describe
	title := clipRunes(errLine, 200)

	switch d.Action {
	case ops.ActionNew:
		if err := tx.QueryRow(ctx, `
			INSERT INTO incidents (project_id, env_id, source, fingerprint, status, severity, title, error_line,
				first_seen, last_seen, occurrences, evidence, skipped_daily, needs_human, deployment_id)
			VALUES ($1,$2,$3,$4,'open',$5,$6,$7,$8,$8,1,$9,$10,$11,NULLIF($12,'')::uuid)
			RETURNING id::text`,
			projectID, in.EnvID, in.Source, fp, string(d.Severity), title, errLine, now, evRaw,
			d.SkippedDaily, d.NeedsHuman, in.DeploymentID).Scan(&res.IncidentID); err != nil {
			return res, err
		}
		existing = incidentState{Incident: ops.Incident{ID: res.IncidentID, Source: in.Source, Status: ops.StatusOpen,
			Severity: d.Severity, Occurrences: 1, FirstSeen: now}, ProjectID: projectID, EnvID: in.EnvID}
		if err := incidentHistoryTx(ctx, tx, res.IncidentID, "open", "system", "открыт: "+clipRunes(errLine, 300)); err != nil {
			return res, err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, Type: "incident.opened", ProjectID: projectID,
			Text: fmt.Sprintf("инцидент %s: %s", envName, clipRunes(errLine, 200)),
			Payload: map[string]any{"incident_id": res.IncidentID, "env_id": in.EnvID, "source": in.Source,
				"severity": string(d.Severity), "title": title},
		}); err != nil {
			return res, err
		}
	default:
		res.IncidentID = existing.ID
		reopen := d.Action == ops.ActionReopen
		regress := d.Action == ops.ActionRegress
		if _, err := tx.Exec(ctx, `
			UPDATE incidents SET status=$2, severity=$3, occurrences=$4, last_seen=$5, evidence=$6,
				skipped_daily = skipped_daily OR $7, needs_human = needs_human OR $8,
				resolved_at = CASE WHEN $9 THEN NULL ELSE resolved_at END,
				resolution  = CASE WHEN $9 THEN '' ELSE resolution END,
				description_status = CASE WHEN $9 THEN 'pending' ELSE description_status END,
				description_requested_at = CASE WHEN $9 THEN now() ELSE description_requested_at END,
				description_attempts = CASE WHEN $9 THEN 0 ELSE description_attempts END,
				mitigated_at = CASE WHEN $9 OR $10 THEN NULL ELSE mitigated_at END,
				occurrences_at_mitigation = CASE WHEN $9 OR $10 THEN 0 ELSE occurrences_at_mitigation END,
				mitigated_release_id = CASE WHEN $9 OR $10 THEN NULL ELSE mitigated_release_id END,
				bug_task_id = CASE WHEN ($9 OR $10) AND $11 THEN NULL ELSE bug_task_id END,
				bug_cancelled_by = CASE WHEN ($9 OR $10) AND $11 THEN '' ELSE bug_cancelled_by END,
				deployment_id = COALESCE(NULLIF($12,'')::uuid, deployment_id)
			WHERE id=$1`,
			existing.ID, string(d.Status), string(d.Severity), d.Occurrences, now, evRaw,
			d.SkippedDaily, d.NeedsHuman, reopen, regress, existing.BugState == ops.BugDone, in.DeploymentID); err != nil {
			return res, err
		}
		if reopen || regress {
			note := "переоткрыт: сбой повторился после закрытия"
			if regress {
				note = "исправление не помогло: сбой повторился после смягчения"
			}
			if err := incidentHistoryTx(ctx, tx, existing.ID, "open", "system", note); err != nil {
				return res, err
			}
		}
		if (reopen || regress) && existing.BugState == ops.BugDone {
			existing.BugTaskID, existing.BugState = "", ops.BugNone
		}
		existing.Status, existing.Severity, existing.Occurrences = d.Status, d.Severity, d.Occurrences
		payload := map[string]any{"incident_id": existing.ID, "status": string(d.Status),
			"severity": string(d.Severity), "occurrences": d.Occurrences}
		if existing.BugTaskID != "" {
			payload["bug_task_id"] = existing.BugTaskID
		}
		text := fmt.Sprintf("инцидент %s повторился: %d раз", envName, d.Occurrences)
		if reopen {
			text = fmt.Sprintf("инцидент %s переоткрыт", envName)
		} else if regress {
			text = fmt.Sprintf("инцидент %s вернулся в работу: исправление не помогло", envName)
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, Type: "incident.updated", ProjectID: projectID,
			Text: text, Payload: payload,
		}); err != nil {
			return res, err
		}
	}

	switch d.Bug {
	case ops.BugActionCreate:
		switch {
		case d.Describe || existing.DescriptionStatus == "pending":
			// Баг создаётся, когда Инженер сопровождения опишет инцидент.
			if _, err := tx.Exec(ctx, `UPDATE incidents SET bug_pending=true WHERE id=$1`, res.IncidentID); err != nil {
				return res, err
			}
		case existing.DescriptionStatus == "template":
			// Описание по шаблону несёт сырые логи: задачу-баг из них
			// агенту ставит только владелец («создать баг»).
			if err := templateBugNeedsHumanTx(ctx, tx, res.IncidentID, string(existing.Status)); err != nil {
				return res, err
			}
		default:
			taskID, epicID, err := createBugTx(ctx, tx, res.IncidentID, attemptLimit, "system")
			if err != nil {
				return res, err
			}
			res.BugTaskID, res.EpicID = taskID, epicID
		}
	}
	if d.Escalate {
		if err := opsLimitEscalationTx(ctx, tx, projectID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// templateBugNeedsHumanTx — баг по шаблонному описанию ждёт подтверждения
// владельца: в описании сырые логи, и агенту-разработчику их без человека
// не отдаём (защита от инструкций в логах).
func templateBugNeedsHumanTx(ctx context.Context, tx pgx.Tx, incID, status string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE incidents SET bug_pending=false, needs_human=true WHERE id=$1 AND NOT needs_human`, incID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return incidentHistoryTx(ctx, tx, incID, status, "system",
		"описание по шаблону: задача-баг ждёт подтверждения владельца")
}

// opsLimitEscalationTx — одна открытая эскалация «сопровождение превысило
// лимит» на проект с перечнем инцидентов без багов.
func opsLimitEscalationTx(ctx context.Context, tx pgx.Tx, projectID string) error {
	rows, err := tx.Query(ctx, `
		SELECT i.title, e.name FROM incidents i JOIN environments e ON e.id = i.env_id
		WHERE i.project_id=$1 AND i.status IN ('open','mitigated') AND i.bug_task_id IS NULL AND NOT i.bug_pending
		ORDER BY i.first_seen LIMIT 20`, projectID)
	if err != nil {
		return err
	}
	var list []string
	for rows.Next() {
		var title, env string
		if err := rows.Scan(&title, &env); err != nil {
			rows.Close()
			return err
		}
		list = append(list, "• "+env+": "+clipRunes(title, 160))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	msg := "Сопровождение превысило лимит: задачи-баги по инцидентам не создаются (не больше 3 незавершённых и 5 новых инцидентов в сутки)."
	if len(list) > 0 {
		msg += "\nИнциденты без задачи-бага:\n" + strings.Join(list, "\n")
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO attention (project_id, reason, message)
		SELECT $1, $2, $3
		WHERE NOT EXISTS (SELECT 1 FROM attention WHERE project_id=$1 AND reason=$2 AND status <> 'resolved'
		                  AND task_id IS NULL AND deployment_id IS NULL)
		ON CONFLICT DO NOTHING`, projectID, string(domain.AttOpsLimit), msg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	_, err = appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "attention.created", ProjectID: projectID,
		Text:    "сопровождение превысило лимит: нужен человек",
		Payload: map[string]any{"reason": string(domain.AttOpsLimit)},
	})
	return err
}

// incidentHistoryTx — строка истории статусов инцидента.
func incidentHistoryTx(ctx context.Context, tx pgx.Tx, incID, status, actor, note string) error {
	_, err := tx.Exec(ctx, `INSERT INTO incident_events (incident_id, status, actor, note) VALUES ($1,$2,$3,$4)`,
		incID, status, actor, clipRunes(note, 500))
	return err
}

// maintenanceEpicTx — Epic «Сопровождение» проекта: создаётся при первой
// необходимости сразу в running, план считается утверждённым.
func maintenanceEpicTx(ctx context.Context, tx pgx.Tx, projectID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		SELECT id::text FROM epics WHERE project_id=$1 AND kind='maintenance' AND status IN ('running','paused','planned')
		ORDER BY created_at LIMIT 1 FOR UPDATE`, projectID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO epics (project_id, title, goal, status, kind)
		VALUES ($1, 'Сопровождение', 'Исправление сбоев работающего приложения по инцидентам наблюдения и публикаций.', 'running', 'maintenance')
		RETURNING id::text`, projectID).Scan(&id); err != nil {
		return "", err
	}
	_, err = appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.status", ProjectID: projectID, EpicID: id,
		Text:    "создан Epic «Сопровождение» для задач-багов по инцидентам",
		Payload: map[string]any{"status": "running", "kind": domain.EpicMaintenance},
	})
	return id, err
}

// createBugTx — задача вида bug в Epic «Сопровождение» по готовому (или
// шаблонному) описанию инцидента.
func createBugTx(ctx context.Context, tx pgx.Tx, incID string, attemptLimit int, actor string) (taskID, epicID string, err error) {
	var projectID, envName, errLine, status string
	var descRaw, evRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT i.project_id::text, e.name, i.error_line, i.description, i.evidence, i.status
		FROM incidents i JOIN environments e ON e.id = i.env_id WHERE i.id=$1 FOR UPDATE OF i`, incID).
		Scan(&projectID, &envName, &errLine, &descRaw, &evRaw, &status); err != nil {
		return "", "", nf(err)
	}
	var ev IncidentEvidence
	_ = json.Unmarshal(evRaw, &ev)
	evidence := ev.text()
	d, ok := unmarshalDescription(descRaw)
	if !ok || d.Bug.Title == "" {
		d = ops.TemplateDescription(envName, errLine, evidence)
	}
	// Автоматический баг уходит агенту без человека: сырых логов в нём нет,
	// только строка ошибки и ссылка на инцидент. Баг, подтверждённый
	// владельцем, несёт доказательства целиком.
	auto := actor == "system"
	if auto {
		evidence = ""
	}
	title, description, criteria := ops.BugTask(d, envName, evidence)
	if auto {
		description += ops.IncidentRef(incID, errLine)
	}
	crit := make([]domain.Criterion, 0, len(criteria))
	for _, c := range criteria {
		crit = append(crit, domain.Criterion{Text: c})
	}
	critRaw, _ := json.Marshal(crit)
	if epicID, err = maintenanceEpicTx(ctx, tx, projectID); err != nil {
		return "", "", err
	}
	if attemptLimit <= 0 {
		attemptLimit = 3
	}
	var num int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO tasks (epic_id, title, description, estimate, capabilities, criteria, attempt_limit, kind, incident_id)
		VALUES ($1,$2,$3,1,'{coding}',$4,$5,'bug',$6)
		RETURNING id::text, num`, epicID, clipRunes(title, 200), description, critRaw, attemptLimit, incID).
		Scan(&taskID, &num); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE incidents SET bug_task_id=$2, bug_pending=false, bug_cancelled_by='', needs_human=false WHERE id=$1`,
		incID, taskID); err != nil {
		return "", "", err
	}
	if err := incidentHistoryTx(ctx, tx, incID, status, actor, fmt.Sprintf("создана задача-баг task-%d", num)); err != nil {
		return "", "", err
	}
	if _, err := appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "task.status", ProjectID: projectID, EpicID: epicID, TaskID: taskID,
		Text:    fmt.Sprintf("задача-баг task-%d по инциденту %s: %s", num, envName, clipRunes(title, 120)),
		Payload: map[string]any{"status": "queued", "kind": domain.TaskBug, "incident_id": incID},
	}); err != nil {
		return "", "", err
	}
	_, err = appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "incident.updated", ProjectID: projectID,
		Text:    fmt.Sprintf("по инциденту %s создана задача-баг task-%d", envName, num),
		Payload: map[string]any{"incident_id": incID, "status": status, "bug_task_id": taskID},
	})
	return taskID, epicID, err
}

// applyTransitionTx — смена статуса инцидента по ops.Transition.
func applyTransitionTx(ctx context.Context, tx pgx.Tx, st incidentState, tr ops.Transition, relID, actor, note string) error {
	switch tr.To {
	case ops.StatusMitigated:
		if _, err := tx.Exec(ctx, `
			UPDATE incidents SET status='mitigated', mitigated_at=now(), occurrences_at_mitigation=$2,
				mitigated_release_id=NULLIF($3,'')::uuid WHERE id=$1`, st.ID, tr.OccurrencesAtMitigation, relID); err != nil {
			return err
		}
	case ops.StatusResolved:
		if _, err := tx.Exec(ctx, `
			UPDATE incidents SET status='resolved', resolved_at=now(), resolution=$2, bug_pending=false,
				needs_human=false,
				mitigated_release_id = COALESCE(mitigated_release_id, NULLIF($3,'')::uuid)
			WHERE id=$1`, st.ID, string(tr.Resolution), relID); err != nil {
			return err
		}
	default:
		if _, err := tx.Exec(ctx, `UPDATE incidents SET status=$2 WHERE id=$1`, st.ID, string(tr.To)); err != nil {
			return err
		}
	}
	if tr.CancelBug && st.BugTaskID != "" {
		if err := cancelBugTx(ctx, tx, st); err != nil {
			return err
		}
	}
	if err := incidentHistoryTx(ctx, tx, st.ID, string(tr.To), actor, note); err != nil {
		return err
	}
	if tr.To == ops.StatusResolved {
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: actorKind(actor), ActorID: actorID(actor), Type: "incident.resolved", ProjectID: st.ProjectID,
			Text:    "инцидент закрыт: " + resolutionText(tr.Resolution),
			Payload: map[string]any{"incident_id": st.ID, "resolution": string(tr.Resolution)},
		})
		return err
	}
	payload := map[string]any{"incident_id": st.ID, "status": string(tr.To), "severity": string(st.Severity),
		"occurrences": st.Occurrences}
	if relID != "" {
		payload["release_id"] = relID
	}
	if st.BugTaskID != "" {
		payload["bug_task_id"] = st.BugTaskID
	}
	_, err := appendEvent(ctx, tx, EventInput{
		ActorKind: actorKind(actor), ActorID: actorID(actor), Type: "incident.updated", ProjectID: st.ProjectID,
		Text: "инцидент смягчён: задача-баг вошла в релиз", Payload: payload,
	})
	return err
}

func actorKind(actor string) domain.ActorKind {
	if actor == "" || actor == "system" {
		return domain.ActorSystem
	}
	return domain.ActorUser
}

func actorID(actor string) string {
	if actor == "system" {
		return ""
	}
	return actor
}

func resolutionText(r ops.Resolution) string {
	switch r {
	case ops.ResolutionFixed:
		return "исправлен"
	case ops.ResolutionSelf:
		return "самоустранился"
	case ops.ResolutionManual:
		return "закрыт вручную"
	}
	return string(r)
}

// cancelBugTx — отмена не начатой задачи-бага системой (самоустранение).
func cancelBugTx(ctx context.Context, tx pgx.Tx, st incidentState) error {
	var epicID string
	var num int64
	err := tx.QueryRow(ctx, `
		UPDATE tasks SET status='cancelled', updated_at=now()
		WHERE id=$1 AND status IN ('queued','ready') RETURNING epic_id::text, num`, st.BugTaskID).Scan(&epicID, &num)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE incidents SET bug_cancelled_by='system' WHERE id=$1`, st.ID); err != nil {
		return err
	}
	_, err = appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorSystem, Type: "task.status", ProjectID: st.ProjectID, EpicID: epicID, TaskID: st.BugTaskID,
		Text:    fmt.Sprintf("задача-баг task-%d отменена: инцидент самоустранился", num),
		Payload: map[string]any{"status": "cancelled", "reason": "incident_self_resolved"},
	})
	return err
}

// bugCancelledByHumanTx — человек отменил задачу-баг: баг автоматически не
// пересоздаётся, инцидент ждёт решения человека.
func bugCancelledByHumanTx(ctx context.Context, tx pgx.Tx, taskID, login string) error {
	if login == "" {
		login = "user"
	}
	var incID, projectID, status string
	err := tx.QueryRow(ctx, `
		UPDATE incidents SET bug_cancelled_by=$2, needs_human=true, bug_pending=false
		WHERE bug_task_id=$1 AND status IN ('open','mitigated')
		RETURNING id::text, project_id::text, status`, taskID, login).Scan(&incID, &projectID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := incidentHistoryTx(ctx, tx, incID, status, login, "задача-баг отменена человеком: нужен человек"); err != nil {
		return err
	}
	_, err = appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorUser, ActorID: login, Type: "incident.updated", ProjectID: projectID,
		Text:    "задача-баг инцидента отменена человеком: инцидент ждёт решения",
		Payload: map[string]any{"incident_id": incID, "status": status, "needs_human": true},
	})
	return err
}

// ─── описание ────────────────────────────────────────────────────────────

// IncidentForDescription — данные задания incident_description.
type IncidentForDescription struct {
	ID, ProjectID, EnvID, EnvName, Source, ErrorLine string
	Occurrences                                      int
	Evidence                                         IncidentEvidence
	DeploymentID                                     string
	// SkippedDaily — инцидент сверх суточного лимита: описание по шаблону,
	// без вызова модели (стоимость шквала не растёт).
	SkippedDaily bool
}

// IncidentForJob — инцидент для задания описания или разбора.
func (s *Store) IncidentForJob(ctx context.Context, id string) (IncidentForDescription, error) {
	var d IncidentForDescription
	var evRaw []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT i.id::text, i.project_id::text, i.env_id::text, e.name, i.source, i.error_line, i.occurrences,
			i.evidence, COALESCE(i.deployment_id::text, ''), i.skipped_daily
		FROM incidents i JOIN environments e ON e.id = i.env_id WHERE i.id=$1`, id).
		Scan(&d.ID, &d.ProjectID, &d.EnvID, &d.EnvName, &d.Source, &d.ErrorLine, &d.Occurrences, &evRaw, &d.DeploymentID, &d.SkippedDaily)
	if err != nil {
		return d, nf(err)
	}
	_ = json.Unmarshal(evRaw, &d.Evidence)
	return d, nil
}

// EvidenceText — доказательства одним текстом (для заданий агентам).
func (d IncidentForDescription) EvidenceText() string { return d.Evidence.text() }

// PendingIncidentDescriptions — инциденты, ждущие описания дольше age
// (подъём заданий после рестарта rivetd и после сбоя задания).
func (s *Store) PendingIncidentDescriptions(ctx context.Context, age time.Duration) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text FROM incidents WHERE description_status='pending' AND status IN ('open','mitigated')
		  AND description_requested_at <= now() - $1::interval
		ORDER BY first_seen`, age.String())
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// SetIncidentDescription — результат задания описания: суть, причина,
// шаги, предложение бага. Ждущий описания баг создаётся здесь же.
func (s *Store) SetIncidentDescription(ctx context.Context, id string, d ops.Description, status, agentID string) (bugTaskID, epicID string, err error) {
	_, bugTaskID, epicID, err = s.setIncidentDescription(ctx, id, nil, d, status, agentID)
	return bugTaskID, epicID, err
}

// SetIncidentDescriptionJob — результат задания описания: записывается,
// только если описание ещё ждёт именно этот запрос (requestedAt из начала
// задания). Устаревший результат (переоткрытие, параллельное задание) не
// затирает новый: written=false.
func (s *Store) SetIncidentDescriptionJob(ctx context.Context, id string, requestedAt time.Time, d ops.Description, status, agentID string) (written bool, bugTaskID, epicID string, err error) {
	return s.setIncidentDescription(ctx, id, &requestedAt, d, status, agentID)
}

// cleanDescription — описание без NUL и невалидного UTF-8 (ответ модели).
func cleanDescription(d ops.Description) ops.Description {
	d.Summary, d.Cause = CleanText(d.Summary), CleanText(d.Cause)
	d.Bug.Title, d.Bug.Description = CleanText(d.Bug.Title), CleanText(d.Bug.Description)
	steps := make([]string, 0, len(d.Reproduction))
	for _, r := range d.Reproduction {
		steps = append(steps, CleanText(r))
	}
	d.Reproduction = steps
	crit := make([]string, 0, len(d.Bug.Criteria))
	for _, c := range d.Bug.Criteria {
		crit = append(crit, CleanText(c))
	}
	d.Bug.Criteria = crit
	return d
}

func (s *Store) setIncidentDescription(ctx context.Context, id string, requestedAt *time.Time, d ops.Description, status, agentID string) (written bool, bugTaskID, epicID string, err error) {
	d = cleanDescription(d)
	var projectID string
	if err := s.Pool.QueryRow(ctx, `SELECT project_id::text FROM incidents WHERE id=$1`, id).Scan(&projectID); err != nil {
		return false, "", "", nf(err)
	}
	eff, err := s.EffectivePolicy(ctx, projectID)
	if err != nil {
		return false, "", "", err
	}
	raw := marshalDescription(d)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockProjectOps(ctx, tx, projectID); err != nil {
			return err
		}
		var pending bool
		var incStatus string
		title := clipRunes(oneLine(d.Summary), 200)
		err := tx.QueryRow(ctx, `
			UPDATE incidents SET description=$2, description_status=$3, description_agent_id=NULLIF($4,''),
				title = CASE WHEN $5 <> '' THEN $5 ELSE title END
			WHERE id=$1 AND ($6::timestamptz IS NULL OR (description_status='pending' AND description_requested_at=$6))
			RETURNING bug_pending, status`, id, raw, status, agentID, title, requestedAt).Scan(&pending, &incStatus)
		if requestedAt != nil && errors.Is(err, pgx.ErrNoRows) {
			return nil // результат устарел: описание уже записано или запрошено заново
		}
		if err != nil {
			return nf(err)
		}
		written = true
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: agentID, Type: "incident.updated", ProjectID: projectID,
			Text:    "инцидент описан: " + clipRunes(title, 160),
			Payload: map[string]any{"incident_id": id, "status": incStatus, "description_status": status},
		}); err != nil {
			return err
		}
		if !pending || (incStatus != "open" && incStatus != "mitigated") {
			return nil
		}
		if status == "template" {
			return templateBugNeedsHumanTx(ctx, tx, id, incStatus)
		}
		bugTaskID, epicID, err = createBugTx(ctx, tx, id, eff.Presets.AttemptLimit, "system")
		return err
	})
	return written, bugTaskID, epicID, err
}

// MaxOpsAttempts — попыток задания control plane с моделью; дальше текст
// собирается по шаблону без модели, и задание закрывается.
const MaxOpsAttempts = 3

// StartDescriptionJob — начало задания описания: номер попытки и запрос,
// к которому относится результат. ok=false — описание уже не ждут.
func (s *Store) StartDescriptionJob(ctx context.Context, id string) (requestedAt time.Time, attempt int, ok bool, err error) {
	err = s.Pool.QueryRow(ctx, `
		UPDATE incidents SET description_attempts = description_attempts + 1
		WHERE id=$1 AND description_status='pending' AND status IN ('open','mitigated')
		RETURNING description_requested_at, description_attempts`, id).Scan(&requestedAt, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return requestedAt, 0, false, nil
	}
	return requestedAt, attempt, err == nil, err
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// storedDescription — описание в базе вместе с признаком «доказательства
// уже в описании бага» (в ops.Description он не сериализуется).
type storedDescription struct {
	ops.Description
	EvidenceIncluded bool `json:"evidence_included,omitempty"`
}

func marshalDescription(d ops.Description) []byte {
	raw, _ := json.Marshal(storedDescription{Description: d, EvidenceIncluded: d.EvidenceIncluded})
	return raw
}

func unmarshalDescription(raw []byte) (ops.Description, bool) {
	var sd storedDescription
	if len(raw) == 0 || json.Unmarshal(raw, &sd) != nil {
		return ops.Description{}, false
	}
	sd.Description.EvidenceIncluded = sd.EvidenceIncluded
	return sd.Description, true
}

// ─── ручные действия ─────────────────────────────────────────────────────

// Ошибки ручных действий с инцидентом.
var (
	ErrIncidentResolved = errors.New("инцидент уже закрыт")
	ErrBugUnfinished    = errors.New("у инцидента уже есть незавершённая задача-баг")
)

// ResolveIncident — ручное закрытие владельцем с комментарием.
func (s *Store) ResolveIncident(ctx context.Context, id, comment, login string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := scanIncidentState(tx.QueryRow(ctx, `SELECT `+incidentStateCols+` FROM `+incidentStateFrom+`
			WHERE i.id=$1 FOR UPDATE OF i`, id))
		if err != nil {
			return nf(err)
		}
		tr, err := ops.ResolveManual(st.Incident, comment)
		switch {
		case errors.Is(err, ops.ErrAlreadyResolved):
			return ErrIncidentResolved
		case errors.Is(err, ops.ErrBadComment):
			return &FieldError{Field: "comment", Msg: err.Error()}
		case err != nil:
			return err
		}
		return applyTransitionTx(ctx, tx, st, tr, "", login, "закрыт вручную: "+strings.TrimSpace(comment))
	})
}

// RebugIncident — команда владельца «создать баг заново»: снимает отметку
// отмены человеком и создаёт задачу-баг с готовым описанием (лимиты
// автоматики к явной команде человека не применяются).
func (s *Store) RebugIncident(ctx context.Context, id, login string) (string, error) {
	var projectID string
	if err := s.Pool.QueryRow(ctx, `SELECT project_id::text FROM incidents WHERE id=$1`, id).Scan(&projectID); err != nil {
		return "", nf(err)
	}
	eff, err := s.EffectivePolicy(ctx, projectID)
	if err != nil {
		return "", err
	}
	var taskID string
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockProjectOps(ctx, tx, projectID); err != nil {
			return err
		}
		st, err := scanIncidentState(tx.QueryRow(ctx, `SELECT `+incidentStateCols+` FROM `+incidentStateFrom+`
			WHERE i.id=$1 FOR UPDATE OF i`, id))
		if err != nil {
			return nf(err)
		}
		if !st.Status.Active() {
			return ErrIncidentResolved
		}
		if st.BugState.Unfinished() {
			return ErrBugUnfinished
		}
		taskID, _, err = createBugTx(ctx, tx, id, eff.Presets.AttemptLimit, login)
		return err
	})
	return taskID, err
}

// ─── чтение ──────────────────────────────────────────────────────────────

// IncidentBug — задача-баг инцидента в DTO.
type IncidentBug struct {
	ID     string `json:"id"`
	Num    int64  `json:"num"`
	Status string `json:"status"`
}

// IncidentDescriptionView — описание инцидента в DTO.
type IncidentDescriptionView struct {
	Status       string          `json:"status"`
	Agent        *AgentRef       `json:"agent"`
	Summary      string          `json:"summary"`
	Cause        string          `json:"cause"`
	Reproduction []string        `json:"reproduction"`
	Bug          ops.BugProposal `json:"bug"`
}

// IncidentHistory — строка истории статусов.
type IncidentHistory struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	By     string    `json:"by"`
	Note   string    `json:"note"`
}

// Incident — инцидент в API.
type Incident struct {
	ID                 string                  `json:"id"`
	ProjectID          string                  `json:"project_id"`
	EnvID              string                  `json:"env_id"`
	EnvName            string                  `json:"env_name"`
	Source             string                  `json:"source"`
	Fingerprint        string                  `json:"fingerprint"`
	Status             string                  `json:"status"`
	Severity           string                  `json:"severity"`
	FirstSeen          time.Time               `json:"first_seen"`
	LastSeen           time.Time               `json:"last_seen"`
	Occurrences        int                     `json:"occurrences"`
	Title              string                  `json:"title"`
	Evidence           *IncidentEvidence       `json:"evidence,omitempty"`
	Description        IncidentDescriptionView `json:"description"`
	BugTask            *IncidentBug            `json:"bug_task"`
	NeedsHuman         bool                    `json:"needs_human"`
	BugCancelledBy     string                  `json:"bug_cancelled_by,omitempty"`
	SkippedDaily       bool                    `json:"skipped_daily"`
	DeploymentID       string                  `json:"deployment_id,omitempty"`
	MitigatedReleaseID string                  `json:"mitigated_release_id,omitempty"`
	ResolvedAt         *time.Time              `json:"resolved_at,omitempty"`
	Resolution         string                  `json:"resolution,omitempty"`
	History            []IncidentHistory       `json:"history,omitempty"`
}

const incidentCols = `i.id::text, i.project_id::text, i.env_id::text, e.name, i.source, i.fingerprint, i.status,
	i.severity, i.first_seen, i.last_seen, i.occurrences, i.title, i.error_line, i.evidence, i.description,
	i.description_status, COALESCE(i.description_agent_id, ''), COALESCE(da.name, ''),
	COALESCE(bt.id::text, ''), COALESCE(bt.num, 0), COALESCE(bt.status, ''),
	i.needs_human, i.bug_cancelled_by, i.skipped_daily, COALESCE(i.deployment_id::text, ''),
	COALESCE(i.mitigated_release_id::text, ''), i.resolved_at, i.resolution`

const incidentFrom = `incidents i JOIN environments e ON e.id = i.env_id
	LEFT JOIN tasks bt ON bt.id = i.bug_task_id
	LEFT JOIN team_agents da ON da.id = i.description_agent_id`

func scanIncident(row pgx.Row, full bool) (Incident, error) {
	var in Incident
	var errLine, descStatus, agentID, agentName, bugID, bugStatus string
	var bugNum int64
	var evRaw, descRaw []byte
	err := row.Scan(&in.ID, &in.ProjectID, &in.EnvID, &in.EnvName, &in.Source, &in.Fingerprint, &in.Status,
		&in.Severity, &in.FirstSeen, &in.LastSeen, &in.Occurrences, &in.Title, &errLine, &evRaw, &descRaw,
		&descStatus, &agentID, &agentName, &bugID, &bugNum, &bugStatus,
		&in.NeedsHuman, &in.BugCancelledBy, &in.SkippedDaily, &in.DeploymentID,
		&in.MitigatedReleaseID, &in.ResolvedAt, &in.Resolution)
	if err != nil {
		return in, err
	}
	if in.Title == "" {
		in.Title = errLine
	}
	var d ops.Description
	_ = json.Unmarshal(descRaw, &d)
	in.Description = IncidentDescriptionView{Status: descStatus, Summary: d.Summary, Cause: d.Cause,
		Reproduction: d.Reproduction, Bug: d.Bug}
	if in.Description.Reproduction == nil {
		in.Description.Reproduction = []string{}
	}
	if in.Description.Bug.Criteria == nil {
		in.Description.Bug.Criteria = []string{}
	}
	if agentID != "" {
		in.Description.Agent = &AgentRef{ID: agentID, Name: agentName}
	}
	if bugID != "" {
		in.BugTask = &IncidentBug{ID: bugID, Num: bugNum, Status: bugStatus}
	}
	if full {
		var ev IncidentEvidence
		_ = json.Unmarshal(evRaw, &ev)
		in.Evidence = &ev
	}
	return in, nil
}

// GetIncident — инцидент целиком: доказательства и история.
func (s *Store) GetIncident(ctx context.Context, id string) (Incident, error) {
	in, err := scanIncident(s.Pool.QueryRow(ctx, `SELECT `+incidentCols+` FROM `+incidentFrom+` WHERE i.id=$1`, id), true)
	if err != nil {
		return in, nf(err)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT h.at, h.status, h.actor, h.note FROM incident_events h WHERE h.incident_id=$1 ORDER BY h.id`, id)
	if err != nil {
		return in, err
	}
	defer rows.Close()
	in.History = []IncidentHistory{}
	for rows.Next() {
		var h IncidentHistory
		if err := rows.Scan(&h.At, &h.Status, &h.By, &h.Note); err != nil {
			return in, err
		}
		in.History = append(in.History, h)
	}
	return in, rows.Err()
}

// IncidentProject — проект инцидента (проверка прав).
func (s *Store) IncidentProject(ctx context.Context, id string) (string, error) {
	var p string
	err := s.Pool.QueryRow(ctx, `SELECT project_id::text FROM incidents WHERE id=$1`, id).Scan(&p)
	return p, nf(err)
}

// ListIncidents — инциденты проекта: открытые, затем смягчённые, затем
// закрытые, внутри по last_seen убыванию. status: open, mitigated,
// resolved или active. Курсор before — id инцидента.
func (s *Store) ListIncidents(ctx context.Context, projectID, status, envID, before string, limit int) ([]Incident, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	statuses := []string{"open", "mitigated"}
	switch status {
	case "open", "mitigated", "resolved":
		statuses = []string{status}
	}
	rows, err := s.Pool.Query(ctx, `
		WITH ordered AS (
			SELECT i.id, CASE i.status WHEN 'open' THEN 0 WHEN 'mitigated' THEN 1 ELSE 2 END AS rank, i.last_seen
			FROM incidents i WHERE i.project_id=$1 AND i.status = ANY($2) AND ($3 = '' OR i.env_id::text = $3)
		), cursor AS (SELECT rank, last_seen, id FROM ordered WHERE id::text = $4)
		SELECT `+incidentCols+` FROM `+incidentFrom+` JOIN ordered o ON o.id = i.id
		WHERE $4 = '' OR (o.rank, -extract(epoch FROM o.last_seen), o.id) >
		      (SELECT rank, -extract(epoch FROM last_seen), id FROM cursor)
		ORDER BY o.rank, o.last_seen DESC, o.id LIMIT $5`, projectID, statuses, envID, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		in, err := scanIncident(rows, false)
		if err != nil {
			return nil, "", err
		}
		out = append(out, in)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

// ErrMaintenanceLocked — Epic «Сопровождение» нельзя архивировать или
// декомпозировать, пока у проекта есть открытые инциденты.
var ErrMaintenanceLocked = errors.New("нельзя декомпозировать и архивировать Epic «Сопровождение», пока у проекта есть открытые инциденты")

// maintenanceArchiveTx — проверка архивации Epic'а «Сопровождение» под тем
// же advisory lock, что создание задач-багов: между проверкой и переходом
// новый баг в этот Epic не появится.
func maintenanceArchiveTx(ctx context.Context, tx pgx.Tx, epicID string) error {
	var projectID, kind string
	if err := tx.QueryRow(ctx, `SELECT project_id::text, kind FROM epics WHERE id=$1`, epicID).Scan(&projectID, &kind); err != nil {
		return err
	}
	if kind != domain.EpicMaintenance {
		return nil
	}
	if err := lockProjectOps(ctx, tx, projectID); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents WHERE project_id=$1 AND status IN ('open','mitigated'))`,
		projectID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrMaintenanceLocked
	}
	return nil
}

// ActiveIncidents — открыт ли у проекта хоть один инцидент (запрет
// декомпозиции и архивации Epic'а «Сопровождение»).
func (s *Store) ActiveIncidents(ctx context.Context, projectID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents WHERE project_id=$1 AND status IN ('open','mitigated'))`,
		projectID).Scan(&ok)
	return ok, err
}
