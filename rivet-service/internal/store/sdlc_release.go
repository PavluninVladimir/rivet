package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/ops"
)

// Релизы (add-sdlc-cycle, спека release-engineering): успешная публикация
// окружения фиксирует состав по времени merge, а Релиз-инженер пишет к ней
// release notes.

// dbtx — общее у пула и транзакции: чтение и запись.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Release — релиз окружения.
type Release struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"project_id"`
	EnvID        string            `json:"env_id"`
	EnvName      string            `json:"env_name"`
	DeploymentID string            `json:"deployment_id"`
	Version      string            `json:"version"`
	PrevVersion  string            `json:"prev_version,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	Initiator    string            `json:"initiator"`
	Composition  []ops.ReleaseItem `json:"composition"`
	NotesMD      string            `json:"notes_md,omitempty"`
	NotesStatus  string            `json:"notes_status"`
	NotesAgent   *AgentRef         `json:"notes_agent"`
	NotesModel   string            `json:"-"`
	DroppedItems int               `json:"dropped_items"`
	// ByTime — состав по времени merge у публикации, запущенной человеком.
	ByTime bool `json:"composition_by_time"`
	// NotesRequested — когда запрошены release notes (залежавшийся pending).
	NotesRequested time.Time `json:"-"`
}

// AgentRef — агент команды в DTO: id и имя.
type AgentRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const releaseCols = `r.id, r.project_id::text, r.env_id::text, e.name, r.deployment_id::text, r.version,
	r.prev_version, r.created_at, r.initiator, r.composition, r.notes_md, r.notes_status,
	COALESCE(r.notes_agent_id, ''), COALESCE(ta.name, ''), r.notes_model, r.dropped_items, r.by_time, r.notes_requested_at`

const releaseFrom = `releases r JOIN environments e ON e.id = r.env_id
	LEFT JOIN team_agents ta ON ta.id = r.notes_agent_id`

func scanRelease(row pgx.Row) (Release, error) {
	var r Release
	var comp []byte
	var agentID, agentName string
	err := row.Scan(&r.ID, &r.ProjectID, &r.EnvID, &r.EnvName, &r.DeploymentID, &r.Version,
		&r.PrevVersion, &r.CreatedAt, &r.Initiator, &comp, &r.NotesMD, &r.NotesStatus,
		&agentID, &agentName, &r.NotesModel, &r.DroppedItems, &r.ByTime, &r.NotesRequested)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(comp, &r.Composition)
	if r.Composition == nil {
		r.Composition = []ops.ReleaseItem{}
	}
	if agentID != "" {
		r.NotesAgent = &AgentRef{ID: agentID, Name: agentName}
	}
	return r, nil
}

// GetRelease — релиз по id.
func (s *Store) GetRelease(ctx context.Context, id string) (Release, error) {
	r, err := scanRelease(s.Pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM `+releaseFrom+` WHERE r.id=$1`, id))
	return r, nf(err)
}

// ReleaseByDeployment — релиз успешной публикации; ErrNotFound, если его нет.
func (s *Store) ReleaseByDeployment(ctx context.Context, depID string) (Release, error) {
	r, err := scanRelease(s.Pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM `+releaseFrom+` WHERE r.deployment_id=$1`, depID))
	return r, nf(err)
}

// ListReleases — релизы проекта от новых к старым; курсор before — id
// релиза, после которого продолжить.
func (s *Store) ListReleases(ctx context.Context, projectID, envID, before string, limit int) ([]Release, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+releaseCols+` FROM `+releaseFrom+`
		WHERE r.project_id=$1 AND ($2 = '' OR r.env_id::text = $2)
		  AND ($3 = '' OR (r.created_at, r.id) < (SELECT created_at, id FROM releases WHERE id::text=$3))
		ORDER BY r.created_at DESC, r.id DESC LIMIT $4`, projectID, envID, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, "", err
		}
		r.NotesMD = ""
		out = append(out, r)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

// mergedCandidates — смерженные задачи проекта, которые могут войти в состав
// релиза, созданного в момент created после прошлой публикации prevCreated.
func mergedCandidates(ctx context.Context, q dbtx, projectID string, prevCreated *time.Time, created time.Time) ([]ops.MergedTask, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.num, t.title, t.kind, e.id, e.title, COALESCE(t.pr_url, ''),
		       COALESCE(t.incident_id::text, ''), t.description, t.criteria, t.merged_at
		FROM tasks t JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id = $1 AND t.merged_at IS NOT NULL AND t.merged_at <= $2
		  AND ($3::timestamptz IS NULL OR t.merged_at > $3)
		ORDER BY t.merged_at, t.num`, projectID, created, prevCreated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ops.MergedTask
	for rows.Next() {
		var m ops.MergedTask
		var crit []byte
		var num int64
		if err := rows.Scan(&m.TaskID, &num, &m.Title, &m.Kind, &m.EpicID, &m.EpicTitle, &m.PRURL,
			&m.IncidentID, &m.Description, &crit, &m.MergedAt); err != nil {
			return nil, err
		}
		m.Num = int(num)
		if m.Kind != domain.TaskBug && m.Kind != "architecture" {
			m.Kind = "work"
		}
		var cs []domain.Criterion
		_ = json.Unmarshal(crit, &cs)
		for _, c := range cs {
			m.Criteria = append(m.Criteria, c.Text)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReleaseComposition — полный состав релиза с описаниями задач (для заданий
// Релиз-инженеру).
func (s *Store) ReleaseComposition(ctx context.Context, rel Release) ([]ops.MergedTask, error) {
	ids := make([]string, 0, len(rel.Composition))
	for _, it := range rel.Composition {
		ids = append(ids, it.TaskID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id, t.num, t.title, t.kind, e.id, e.title, COALESCE(t.pr_url, ''),
		       COALESCE(t.incident_id::text, ''), t.description, t.criteria, COALESCE(t.merged_at, t.updated_at)
		FROM tasks t JOIN epics e ON e.id = t.epic_id
		WHERE t.id = ANY($1::uuid[]) ORDER BY t.merged_at, t.num`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ops.MergedTask
	for rows.Next() {
		var m ops.MergedTask
		var crit []byte
		var num int64
		if err := rows.Scan(&m.TaskID, &num, &m.Title, &m.Kind, &m.EpicID, &m.EpicTitle, &m.PRURL,
			&m.IncidentID, &m.Description, &crit, &m.MergedAt); err != nil {
			return nil, err
		}
		m.Num = int(num)
		if m.Kind != domain.TaskBug && m.Kind != "architecture" {
			m.Kind = "work"
		}
		var cs []domain.Criterion
		_ = json.Unmarshal(crit, &cs)
		for _, c := range cs {
			m.Criteria = append(m.Criteria, c.Text)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// createReleaseTx — релиз успешной публикации в транзакции CompleteDeployment:
// состав по времени merge, событие release.created, переходы инцидентов
// окружения (смягчение баг-релизом, закрытие инцидента публикации).
func createReleaseTx(ctx context.Context, tx pgx.Tx, depID string) error {
	var projectID, envID, envName, version, initiator string
	var created time.Time
	if err := tx.QueryRow(ctx, `
		SELECT e.project_id::text, d.env_id::text, e.name, d.version, d.initiator, d.version_at
		FROM deployments d JOIN environments e ON e.id = d.env_id WHERE d.id=$1`, depID).
		Scan(&projectID, &envID, &envName, &version, &initiator, &created); err != nil {
		return err
	}
	var prevCreated *time.Time
	var prevVersion string
	err := tx.QueryRow(ctx, `
		SELECT version_at, version FROM deployments
		WHERE env_id=$1 AND id<>$2 AND status='done' AND version_at <= $3
		ORDER BY version_at DESC LIMIT 1`, envID, depID, created).Scan(&prevCreated, &prevVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cands, err := mergedCandidates(ctx, tx, projectID, prevCreated, created)
	if err != nil {
		return err
	}
	// Переходы инцидентов окружения и отмена багов — под тем же advisory
	// lock проекта, что решения сопровождения (порядок блокировок один).
	if err := lockProjectOps(ctx, tx, projectID); err != nil {
		return err
	}
	comp := ops.Composition(cands, prevCreated, created)
	items := ops.Items(comp)
	raw, _ := json.Marshal(items)
	// Публикация, запущенная человеком, могла выкатить произвольную версию:
	// состав тогда только оценка по времени merge (design, риски).
	byTime := initiator != "auto"
	var relID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO releases (project_id, env_id, deployment_id, version, prev_version, initiator, created_at, composition, by_time)
		VALUES ($1,$2,$3,$4,$5,$6,now(),$7,$8)
		ON CONFLICT (deployment_id) DO NOTHING RETURNING id::text`,
		projectID, envID, depID, version, prevVersion, initiator, raw, byTime).Scan(&relID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // повтор финала: релиз уже есть
		}
		return err
	}
	text := fmt.Sprintf("релиз %s: версия %s, задач в составе %d", envName, shortVer(version), len(items))
	if byTime {
		text += " (состав по времени merge)"
	}
	// Задачи состава уходят из фазы релиза: событие несёт переход для
	// анимации экрана цикла.
	if _, err := appendEvent(ctx, tx, EventInput{
		ActorKind: domain.ActorScheduler, Type: "release.created", ProjectID: projectID,
		Text: text,
		Payload: map[string]any{"release_id": relID, "env_id": envID, "version": version, "tasks": len(items),
			"composition_by_time": byTime, "phase_from": "release", "phase_to": nil},
	}); err != nil {
		return err
	}
	// Повторная проверка наблюдения сразу и через 2 минуты после
	// публикации; проверка, начатая до выкатки, отменяется.
	if _, err := tx.Exec(ctx, `
		UPDATE monitor_state SET next_at = now(), recheck_at = now() + interval '2 minutes',
			pending_check_id = '', pending_runner = '', pending_since = NULL
		WHERE env_id = $1`, envID); err != nil {
		return err
	}
	return releaseIncidentsTx(ctx, tx, projectID, envID, relID, version, created, comp)
}

// releaseIncidentsTx — переходы активных инцидентов окружения при релизе.
func releaseIncidentsTx(ctx context.Context, tx pgx.Tx, projectID, envID, relID, version string, created time.Time, comp []ops.MergedTask) error {
	bugFor := map[string]bool{}
	for _, m := range comp {
		if m.Kind == domain.TaskBug && m.IncidentID != "" {
			bugFor[m.IncidentID] = true
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT `+incidentStateCols+` FROM `+incidentStateFrom+`
		WHERE i.project_id=$1 AND i.env_id=$2 AND i.status IN ('open','mitigated')
		FOR UPDATE OF i`, projectID, envID)
	if err != nil {
		return err
	}
	incs, err := scanIncidentStates(rows)
	if err != nil {
		return err
	}
	for _, st := range incs {
		facts := ops.ReleaseFacts{BugIncluded: bugFor[st.ID]}
		if st.Source == ops.SourceRelease {
			// Провал исправлен новой версией, а не повторной выкаткой той,
			// на которую откатились (prev_version упавшей публикации).
			var failedPrev string
			if st.DeploymentID != "" {
				_ = tx.QueryRow(ctx, `
					SELECT COALESCE((SELECT version FROM deployments p
					                 WHERE p.env_id = d.env_id AND p.status='done' AND p.version_at < d.version_at
					                 ORDER BY p.version_at DESC LIMIT 1), '')
					FROM deployments d WHERE d.id=$1`, st.DeploymentID).Scan(&failedPrev)
			}
			facts.NewVersion = created.After(st.FirstSeen) && version != failedPrev
		}
		tr, ok := ops.OnRelease(st.Incident, facts)
		if !ok {
			continue
		}
		if err := applyTransitionTx(ctx, tx, st, tr, relID, "system", "релиз "+shortVer(version)); err != nil {
			return err
		}
	}
	return nil
}

// SetReleaseNotes — результат задания release_notes.
func (s *Store) SetReleaseNotes(ctx context.Context, relID, md, status, agentID, model string, dropped int) error {
	_, err := s.setReleaseNotes(ctx, relID, nil, md, status, agentID, model, dropped)
	return err
}

// SetReleaseNotesJob — результат задания release notes: записывается,
// только если notes ещё ждут именно этот запрос. Устаревший результат
// (перегенерация во время задания) не затирает новый: written=false.
func (s *Store) SetReleaseNotesJob(ctx context.Context, relID string, requestedAt time.Time, md, status, agentID, model string, dropped int) (bool, error) {
	return s.setReleaseNotes(ctx, relID, &requestedAt, md, status, agentID, model, dropped)
}

// StartNotesJob — начало задания release notes: номер попытки и запрос.
// ok=false — notes уже не ждут.
func (s *Store) StartNotesJob(ctx context.Context, relID string) (requestedAt time.Time, attempt int, ok bool, err error) {
	err = s.Pool.QueryRow(ctx, `
		UPDATE releases SET notes_attempts = notes_attempts + 1 WHERE id=$1 AND notes_status='pending'
		RETURNING notes_requested_at, notes_attempts`, relID).Scan(&requestedAt, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return requestedAt, 0, false, nil
	}
	return requestedAt, attempt, err == nil, err
}

func (s *Store) setReleaseNotes(ctx context.Context, relID string, requestedAt *time.Time, md, status, agentID, model string, dropped int) (bool, error) {
	written := false
	md = CleanText(md)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var projectID string
		err := tx.QueryRow(ctx, `
			UPDATE releases SET notes_md=$2, notes_status=$3, notes_agent_id=NULLIF($4,''), notes_model=$5, dropped_items=$6
			WHERE id=$1 AND ($7::timestamptz IS NULL OR (notes_status='pending' AND notes_requested_at=$7))
			RETURNING project_id::text`, relID, md, status, agentID, model, dropped, requestedAt).Scan(&projectID)
		if requestedAt != nil && errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return nf(err)
		}
		written = true
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: agentID, Type: "release.notes_ready", ProjectID: projectID,
			Text:    "release notes готовы",
			Payload: map[string]any{"release_id": relID, "status": status, "dropped_items": dropped},
		})
		return err
	})
	return written, err
}

// ErrNotesPending — генерация release notes уже идёт.
var ErrNotesPending = errors.New("release notes уже генерируются")

// RequestReleaseNotes — перегенерация release notes: статус pending.
func (s *Store) RequestReleaseNotes(ctx context.Context, relID string) (Release, error) {
	// Залежавшийся pending (задание потеряно) перегенерировать можно.
	tag, err := s.Pool.Exec(ctx, `
		UPDATE releases SET notes_status='pending', notes_requested_at=now(), notes_attempts=0
		WHERE id=$1 AND (notes_status <> 'pending' OR notes_requested_at < now() - $2::interval)`,
		relID, NotesStale.String())
	if err != nil {
		return Release{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetRelease(ctx, relID); err != nil {
			return Release{}, err
		}
		return Release{}, ErrNotesPending
	}
	return s.GetRelease(ctx, relID)
}

// NotesStale — через сколько ожидание release notes считается потерянным.
const NotesStale = 5 * time.Minute

// PendingReleaseNotes — релизы, ждущие release notes дольше age (после
// рестарта rivetd и после сбоя задания они поднимаются заново).
func (s *Store) PendingReleaseNotes(ctx context.Context, age time.Duration) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text FROM releases WHERE notes_status='pending' AND notes_requested_at <= now() - $1::interval
		ORDER BY created_at`, age.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// shortVer — короткая версия для текста событий.
func shortVer(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// DeploymentInfo — несостоявшийся релиз для разбора провала: окружение,
// версия и состав по тем же правилам, что у релиза.
type DeploymentInfo struct {
	ProjectID, EnvID, EnvName, ExecType string
	Version, PrevVersion, LogRef        string
	Composition                         []ops.MergedTask
}

// DeploymentComposition — состав публикации по времени merge (для
// задания deploy_analysis).
func (s *Store) DeploymentComposition(ctx context.Context, depID string) (DeploymentInfo, error) {
	var d DeploymentInfo
	var created time.Time
	if err := s.Pool.QueryRow(ctx, `
		SELECT e.project_id::text, d.env_id::text, e.name, e.exec_type, d.version, COALESCE(d.log_ref, ''), d.version_at
		FROM deployments d JOIN environments e ON e.id = d.env_id WHERE d.id=$1`, depID).
		Scan(&d.ProjectID, &d.EnvID, &d.EnvName, &d.ExecType, &d.Version, &d.LogRef, &created); err != nil {
		return d, nf(err)
	}
	var prevCreated *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT version_at, version FROM deployments
		WHERE env_id=$1 AND id<>$2 AND status='done' AND version_at <= $3
		ORDER BY version_at DESC LIMIT 1`, d.EnvID, depID, created).Scan(&prevCreated, &d.PrevVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	cands, err := mergedCandidates(ctx, s.Pool, d.ProjectID, prevCreated, created)
	if err != nil {
		return d, err
	}
	d.Composition = ops.Composition(cands, prevCreated, created)
	return d, nil
}

// IncidentTitles — названия инцидентов по id (для release notes).
func (s *Store) IncidentTitles(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, CASE WHEN title <> '' THEN title ELSE error_line END FROM incidents WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}
