package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Исследование продукта (add-product-discovery, спека backend/product-discovery):
// интервью пользователей, версии исследования Аналитиком, кэш конспектов,
// утверждение бэклога в Epic'и и зависимости между Epic'ами.

// Лимиты интервью проекта (спека «Интервью проекта»).
const (
	MaxInterviews     = 50
	MaxInterviewChars = 200000
)

// Статусы версии исследования.
const (
	RunRunning    = "running"
	RunReady      = "ready"
	RunFailed     = "failed"
	RunApproved   = "approved"
	RunSuperseded = "superseded"
)

// Этапы анализа.
const (
	StageSummaries = "summaries"
	StageSynthesis = "synthesis"
	StageDone      = "done"
)

// Ошибки исследования (api-contract add-product-discovery).
var (
	ErrInterviewTooLarge = fmt.Errorf("интервью длиннее %d символов", MaxInterviewChars)
	ErrInterviewsLimit   = fmt.Errorf("в проекте уже %d интервью", MaxInterviews)
	ErrNoInterviews      = errors.New("в проекте нет интервью для анализа")
	ErrRunNotEditable    = errors.New("бэклог можно править только у версии в статусе ready")
	ErrRunNotApprovable  = errors.New("утвердить можно только актуальную версию в статусе ready")
	ErrEpicClosed        = errors.New("зависимости завершённого или архивированного Epic'а не меняются")
)

// AnalysisRunningError — в проекте уже идёт анализ.
type AnalysisRunningError struct{ RunID string }

func (e *AnalysisRunningError) Error() string {
	return "анализ уже идёт: " + e.RunID
}

// BacklogConflictError — правка поверх устаревшей ревизии бэклога.
type BacklogConflictError struct{ Rev int }

func (e *BacklogConflictError) Error() string {
	return fmt.Sprintf("бэклог изменён в другом месте, текущая ревизия %d", e.Rev)
}

// DiscoveryFieldError — ошибка проверки с полем (`400 invalid` с `field`).
type DiscoveryFieldError struct{ Field, Msg string }

func (e *DiscoveryFieldError) Error() string { return e.Field + ": " + e.Msg }

func (e *DiscoveryFieldError) Unwrap() error { return ErrInvalid }

// ─── интервью ────────────────────────────────────────────────────────────

// DiscoveryInterview — интервью проекта (форма Interview в api-contract).
type DiscoveryInterview struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"-"`
	Title          string    `json:"title"`
	RespondentRole string    `json:"respondent_role"`
	HeldAt         *string   `json:"held_at,omitempty"`
	Chars          int       `json:"chars"`
	Lines          int       `json:"lines"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	Deleted        bool      `json:"deleted,omitempty"`
	SHA256         string    `json:"-"`
	Text           string    `json:"text,omitempty"`
}

// NewInterview — добавляемое интервью. HeldAt — дата YYYY-MM-DD или пусто.
type NewInterview struct {
	Title          string
	RespondentRole string
	HeldAt         string
	Text           string
}

const interviewCols = `i.id, i.project_id, i.title, i.respondent_role, to_char(i.held_at, 'YYYY-MM-DD'),
	i.chars, i.lines, COALESCE(u.login,''), i.created_at, i.deleted_at IS NOT NULL, i.sha256`

func scanInterview(row pgx.Row, withText bool) (DiscoveryInterview, error) {
	var iv DiscoveryInterview
	dest := []any{&iv.ID, &iv.ProjectID, &iv.Title, &iv.RespondentRole, &iv.HeldAt,
		&iv.Chars, &iv.Lines, &iv.CreatedBy, &iv.CreatedAt, &iv.Deleted, &iv.SHA256}
	if withText {
		dest = append(dest, &iv.Text)
	}
	err := row.Scan(dest...)
	return iv, err
}

// AddInterview добавляет интервью: текст нормализуется (переводы строк,
// NFC), проверяются пустота и лимиты. Число интервью проекта считается под
// блокировкой строки проекта: параллельные загрузки не превысят лимит.
func (s *Store) AddInterview(ctx context.Context, projectID, userID string, in NewInterview) (DiscoveryInterview, error) {
	// Нулевой символ не хранится в text Postgres: отказ с полем, а не 500.
	for field, v := range map[string]string{"title": in.Title, "respondent_role": in.RespondentRole, "text": in.Text} {
		if strings.ContainsRune(v, 0) {
			return DiscoveryInterview{}, &DiscoveryFieldError{Field: field, Msg: "текст содержит нулевые символы: файл не текстовый или повреждён"}
		}
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "title", Msg: "укажите название интервью"}
	}
	if utf8.RuneCountInString(title) > 200 {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "title", Msg: "название длиннее 200 символов"}
	}
	role := strings.TrimSpace(in.RespondentRole)
	if utf8.RuneCountInString(role) > 200 {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "respondent_role", Msg: "роль длиннее 200 символов"}
	}
	if !utf8.ValidString(in.Text) {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "text", Msg: "текст не в кодировке UTF-8"}
	}
	text := discovery.NormalizeText(in.Text)
	if strings.TrimSpace(text) == "" {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "text", Msg: "пустое интервью"}
	}
	chars := utf8.RuneCountInString(text)
	if chars > MaxInterviewChars {
		return DiscoveryInterview{}, ErrInterviewTooLarge
	}
	var heldAt any
	if h := strings.TrimSpace(in.HeldAt); h != "" {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return DiscoveryInterview{}, &DiscoveryFieldError{Field: "held_at", Msg: "дата в формате ГГГГ-ММ-ДД"}
		}
		heldAt = h
	}
	var out DiscoveryInterview
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM discovery_interviews WHERE project_id=$1 AND deleted_at IS NULL`, projectID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxInterviews {
			return ErrInterviewsLimit
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO discovery_interviews (project_id, title, respondent_role, held_at, text, sha256, chars, lines, created_by)
			VALUES ($1,$2,$3,$4::date,$5,$6,$7,$8,NULLIF($9,'')::uuid) RETURNING id`,
			projectID, title, role, heldAt, text, discovery.TextSHA256(text), chars, discovery.CountLines(text), userID).Scan(&id); err != nil {
			return err
		}
		var err error
		out, err = scanInterview(tx.QueryRow(ctx, `SELECT `+interviewCols+`, i.text FROM discovery_interviews i
			LEFT JOIN users u ON u.id = i.created_by WHERE i.id=$1`, id), true)
		return err
	})
	return out, err
}

// ListInterviews — интервью проекта без удалённых, без текста.
func (s *Store) ListInterviews(ctx context.Context, projectID string) ([]DiscoveryInterview, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+interviewCols+` FROM discovery_interviews i
		LEFT JOIN users u ON u.id = i.created_by
		WHERE i.project_id=$1 AND i.deleted_at IS NULL ORDER BY i.created_at, i.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscoveryInterview{}
	for rows.Next() {
		iv, err := scanInterview(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// GetInterview — интервью проекта с текстом. Удалённое тоже отдаётся
// (Deleted): цитаты прошлых версий открываются в контексте.
func (s *Store) GetInterview(ctx context.Context, projectID, id string) (DiscoveryInterview, error) {
	iv, err := scanInterview(s.Pool.QueryRow(ctx, `SELECT `+interviewCols+`, i.text FROM discovery_interviews i
		LEFT JOIN users u ON u.id = i.created_by WHERE i.id=$1 AND i.project_id=$2`, id, projectID), true)
	return iv, nf(err)
}

// interviewsForAnalysis — интервью проекта с текстом для анализа.
func (s *Store) InterviewsForAnalysis(ctx context.Context, projectID string) ([]DiscoveryInterview, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+interviewCols+`, i.text FROM discovery_interviews i
		LEFT JOIN users u ON u.id = i.created_by
		WHERE i.project_id=$1 AND i.deleted_at IS NULL ORDER BY i.created_at, i.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoveryInterview
	for rows.Next() {
		iv, err := scanInterview(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// DeleteInterview — мягкое удаление: интервью исчезает из списка и новых
// анализов, версии с ним помечаются устаревшими, текст остаётся для цитат.
func (s *Store) DeleteInterview(ctx context.Context, projectID, id string) (DiscoveryInterview, error) {
	var out DiscoveryInterview
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Тот же замок, что у старта анализа: набор версии не включит
		// интервью, удалённое одновременно с запуском.
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE discovery_interviews SET deleted_at=now()
			WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL`, id, projectID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE discovery_runs SET outdated=true
			WHERE project_id=$1 AND interviews @> jsonb_build_array(jsonb_build_object('id', $2::text))`, projectID, id); err != nil {
			return err
		}
		out, err = scanInterview(tx.QueryRow(ctx, `SELECT `+interviewCols+` FROM discovery_interviews i
			LEFT JOIN users u ON u.id = i.created_by WHERE i.id=$1`, id), false)
		return err
	})
	return out, err
}

// ─── версии исследования ─────────────────────────────────────────────────

// RunInterview — интервью в наборе версии.
type RunInterview struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	SHA256 string `json:"sha256"`
}

// ApprovedEpic — итог утверждения по строке бэклога.
type ApprovedEpic struct {
	Key    string `json:"key"`
	EpicID string `json:"epic_id"`
	Action string `json:"action"` // created | linked
}

// DecomposeState — ход декомпозиции созданного Epic'а.
type DecomposeState struct {
	Status string `json:"status"` // pending | running | done | failed
	Detail string `json:"detail,omitempty"`
}

// DiscoveryRun — версия исследования (строка discovery_runs).
type DiscoveryRun struct {
	ID             string
	ProjectID      string
	Version        int
	Status         string
	Stage          string
	ProgressDone   int
	ProgressTotal  int
	Interviews     []RunInterview
	AgentID        string
	AgentName      string
	RoleTitle      string
	Model          string
	ConnectionID   string
	InstructionSHA string
	Result         *discovery.Synthesis
	Dropped        []discovery.Dropped
	Defects        []string
	Backlog        []discovery.BacklogEpic
	BacklogRev     int
	Outdated       bool
	Error          string
	TokensIn       *int64
	TokensOut      *int64
	CostUSD        *float64
	DurationS      int
	ApprovedEpics  []ApprovedEpic
	Decompose      map[string]DecomposeState
	StartedAt      time.Time
	FinishedAt     *time.Time
	ApprovedAt     *time.Time
	ApprovedBy     string
}

const discRunCols = `r.id, r.project_id, r.version, r.status, r.stage, r.progress_done, r.progress_total,
	r.interviews, r.agent_id, r.agent_name, r.role_title, r.model, r.connection_id, r.instruction_sha,
	r.result, r.dropped, r.defects, r.backlog, r.backlog_rev, r.outdated, r.error,
	r.tokens_in, r.tokens_out, r.cost_usd::float8, r.duration_s, r.approved_epics, r.decompose,
	r.started_at, r.finished_at, r.approved_at, COALESCE(u.login,'')`

const discRunFrom = ` FROM discovery_runs r LEFT JOIN users u ON u.id = r.approved_by`

func scanDiscRun(row pgx.Row) (DiscoveryRun, error) {
	var r DiscoveryRun
	var interviews, result, dropped, defects, backlog, approved, decompose []byte
	err := row.Scan(&r.ID, &r.ProjectID, &r.Version, &r.Status, &r.Stage, &r.ProgressDone, &r.ProgressTotal,
		&interviews, &r.AgentID, &r.AgentName, &r.RoleTitle, &r.Model, &r.ConnectionID, &r.InstructionSHA,
		&result, &dropped, &defects, &backlog, &r.BacklogRev, &r.Outdated, &r.Error,
		&r.TokensIn, &r.TokensOut, &r.CostUSD, &r.DurationS, &approved, &decompose,
		&r.StartedAt, &r.FinishedAt, &r.ApprovedAt, &r.ApprovedBy)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(interviews, &r.Interviews)
	if len(result) > 0 && string(result) != "null" {
		var syn discovery.Synthesis
		if err := json.Unmarshal(result, &syn); err == nil {
			r.Result = &syn
		}
	}
	_ = json.Unmarshal(dropped, &r.Dropped)
	_ = json.Unmarshal(defects, &r.Defects)
	if len(backlog) > 0 {
		_ = json.Unmarshal(backlog, &r.Backlog)
	}
	if len(approved) > 0 {
		_ = json.Unmarshal(approved, &r.ApprovedEpics)
	}
	_ = json.Unmarshal(decompose, &r.Decompose)
	if r.Interviews == nil {
		r.Interviews = []RunInterview{}
	}
	if r.Dropped == nil {
		r.Dropped = []discovery.Dropped{}
	}
	if r.Defects == nil {
		r.Defects = []string{}
	}
	if r.Decompose == nil {
		r.Decompose = map[string]DecomposeState{}
	}
	return r, nil
}

// NewRun — запуск анализа: набор интервью, агент и модель.
type NewRun struct {
	Interviews     []RunInterview
	AgentID        string
	AgentName      string
	RoleTitle      string
	Model          string
	ConnectionID   string
	InstructionSHA string
}

// CreateRun создаёт версию исследования в статусе running. Второй анализ
// проекта отклоняется частичным уникальным индексом без гонок.
func (s *Store) CreateRun(ctx context.Context, projectID string, in NewRun) (DiscoveryRun, error) {
	if len(in.Interviews) == 0 {
		return DiscoveryRun{}, ErrNoInterviews
	}
	var out DiscoveryRun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		var err error
		out, err = createRunTx(ctx, tx, projectID, in)
		return err
	})
	return out, err
}

// lockProjectDiscovery — замок проекта для путей исследования и графа
// зависимостей Epic'ов. Режим FOR NO KEY UPDATE сериализует эти пути между
// собой, но не конфликтует с FK-проверками (FOR KEY SHARE), которые берут
// записи событий, usage и задач проекта. С FOR UPDATE планировщик (строка
// Epic'а, затем событие) вставал с утверждением в дедлок. Ключ
// lockProjects здесь не годится: SetUserState держит users и берёт его,
// а пути исследования вставляют строки с FK на users.
func lockProjectDiscovery(ctx context.Context, tx pgx.Tx, projectID string) (pgconn.CommandTag, error) {
	return tx.Exec(ctx, `SELECT 1 FROM projects WHERE id=$1 FOR NO KEY UPDATE`, projectID)
}

// createRunTx — вставка версии под уже взятым замком проекта.
func createRunTx(ctx context.Context, tx pgx.Tx, projectID string, in NewRun) (DiscoveryRun, error) {
	if len(in.Interviews) == 0 {
		return DiscoveryRun{}, ErrNoInterviews
	}
	ivs, _ := json.Marshal(in.Interviews)
	var running string
	err := tx.QueryRow(ctx, `SELECT id FROM discovery_runs WHERE project_id=$1 AND status='running'`, projectID).Scan(&running)
	if err == nil {
		return DiscoveryRun{}, &AnalysisRunningError{RunID: running}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DiscoveryRun{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO discovery_runs (project_id, version, status, stage, progress_total, interviews,
			agent_id, agent_name, role_title, model, connection_id, instruction_sha)
		VALUES ($1, (SELECT COALESCE(max(version),0)+1 FROM discovery_runs WHERE project_id=$1),
			'running', 'summaries', $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		projectID, len(in.Interviews), ivs, in.AgentID, in.AgentName, in.RoleTitle, in.Model, in.ConnectionID, in.InstructionSHA).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "discovery_runs_one_running" {
			return DiscoveryRun{}, &AnalysisRunningError{}
		}
		return DiscoveryRun{}, err
	}
	return scanDiscRun(tx.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.id=$1`, id))
}

// GetRun — версия исследования проекта.
func (s *Store) GetRun(ctx context.Context, projectID, id string) (DiscoveryRun, error) {
	r, err := scanDiscRun(s.Pool.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.id=$1 AND r.project_id=$2`, id, projectID))
	return r, nf(err)
}

// CurrentRun — последняя версия не в статусе superseded; nil — версий нет.
func (s *Store) CurrentRun(ctx context.Context, projectID string) (*DiscoveryRun, error) {
	r, err := scanDiscRun(s.Pool.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+`
		WHERE r.project_id=$1 AND r.status <> 'superseded' ORDER BY r.version DESC LIMIT 1`, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRuns — версии исследования проекта, новые первыми.
func (s *Store) ListRuns(ctx context.Context, projectID string) ([]DiscoveryRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.project_id=$1 ORDER BY r.version DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscoveryRun{}
	for rows.Next() {
		r, err := scanDiscRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRunProgress — ход анализа: этап и число готовых конспектов.
func (s *Store) SetRunProgress(ctx context.Context, id, stage string, done, total int) error {
	// GREATEST: параллельные конспекты дописывают ход вразнобой, «3 из 5»
	// не сменяется на «2 из 5».
	_, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET stage=$2, progress_done=GREATEST(progress_done, $3), progress_total=$4
		WHERE id=$1 AND status='running'`, id, stage, done, total)
	return err
}

// AddRunUsage накапливает токены и стоимость запросов анализа. nil не
// превращает известное значение в неизвестное и наоборот: сумма идёт по
// сообщённым значениям.
func (s *Store) AddRunUsage(ctx context.Context, id string, in, out *int64, cost *float64, durationS int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET
		tokens_in = CASE WHEN $2::bigint IS NULL THEN tokens_in ELSE COALESCE(tokens_in,0) + $2 END,
		tokens_out = CASE WHEN $3::bigint IS NULL THEN tokens_out ELSE COALESCE(tokens_out,0) + $3 END,
		cost_usd = CASE WHEN $4::float8 IS NULL THEN cost_usd ELSE COALESCE(cost_usd,0) + $4 END,
		duration_s = duration_s + $5
		WHERE id=$1`, id, in, out, cost, durationS)
	return err
}

// FinishRunReady переводит версию в ready с результатом и начальным
// бэклогом (ревизия 1), а прежние неутверждённые версии проекта — в
// superseded. Если версия уже не running (прервана), ничего не меняется.
func (s *Store) FinishRunReady(ctx context.Context, id string, result discovery.Synthesis, dropped []discovery.Dropped, defects []string) (bool, error) {
	if dropped == nil {
		dropped = []discovery.Dropped{}
	}
	if defects == nil {
		defects = []string{}
	}
	res, _ := json.Marshal(result)
	backlog, _ := json.Marshal(result.Epics)
	dj, _ := json.Marshal(dropped)
	fj, _ := json.Marshal(defects)
	updated := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Общий порядок замков: проект, затем версии. Удаление интервью
		// помечает версии устаревшими под тем же замком проекта.
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT project_id FROM discovery_runs WHERE id=$1`, id).Scan(&projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		var version int
		err := tx.QueryRow(ctx, `UPDATE discovery_runs SET status='ready', stage='done', finished_at=now(),
			result=$2, backlog=$3, backlog_rev=1, dropped=$4, defects=$5, progress_done=progress_total
			WHERE id=$1 AND status='running' RETURNING project_id, version`, id, res, backlog, dj, fj).Scan(&projectID, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		updated = true
		_, err = tx.Exec(ctx, `UPDATE discovery_runs SET status='superseded'
			WHERE project_id=$1 AND version < $2 AND status IN ('ready','failed')`, projectID, version)
		return err
	})
	return updated, err
}

// FinishRunFailed переводит идущую версию в failed с причиной и дефектами.
func (s *Store) FinishRunFailed(ctx context.Context, id, reason string, defects []string, dropped []discovery.Dropped) (bool, error) {
	if defects == nil {
		defects = []string{}
	}
	if dropped == nil {
		dropped = []discovery.Dropped{}
	}
	fj, _ := json.Marshal(defects)
	dj, _ := json.Marshal(dropped)
	tag, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET status='failed', finished_at=now(), error=$2, defects=$3, dropped=$4
		WHERE id=$1 AND status='running'`, id, reason, fj, dj)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FailInterruptedRuns — анализы, прерванные перезапуском rivetd, получают
// failed с причиной (design, решение 5). Возвращает прерванные версии.
func (s *Store) FailInterruptedRuns(ctx context.Context) ([]DiscoveryRun, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE discovery_runs SET status='failed', finished_at=now(),
		error='анализ прерван перезапуском системы' WHERE status='running'
		RETURNING id, project_id, version, agent_id, agent_name, role_title, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoveryRun
	for rows.Next() {
		var r DiscoveryRun
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Version, &r.AgentID, &r.AgentName, &r.RoleTitle, &r.Model); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FailInterruptedDecompositions — декомпозиции утверждённого бэклога,
// прерванные перезапуском rivetd: pending и running становятся failed с
// причиной, Epic'и остаются planned без задач.
func (s *Store) FailInterruptedDecompositions(ctx context.Context) (int64, error) {
	// Страховка: строка, у чьего Epic'а уже есть план, завершена, а не
	// прервана (план и отметка «done» пишутся одной транзакцией в
	// CreatePlan).
	tag, err := s.Pool.Exec(ctx, `UPDATE discovery_runs r SET decompose = (
			SELECT jsonb_object_agg(k, CASE WHEN v->>'status' IN ('pending','running') THEN
				CASE WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(r.approved_epics, '[]'::jsonb)) a
					JOIN tasks tk ON tk.epic_id::text = a->>'epic_id' WHERE a->>'key' = k)
				THEN jsonb_build_object('status','done')
				ELSE jsonb_build_object('status','failed','detail','декомпозиция прервана перезапуском системы') END
				ELSE v END)
			FROM jsonb_each(r.decompose) AS t(k, v))
		WHERE EXISTS (SELECT 1 FROM jsonb_each(r.decompose) AS t(k, v) WHERE v->>'status' IN ('pending','running'))`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ─── кэш конспектов ──────────────────────────────────────────────────────

// CachedSummary — конспект интервью из кэша по тексту, модели и инструкции.
func (s *Store) CachedSummary(ctx context.Context, sha, model, instructionSHA string) (discovery.Summary, []discovery.Dropped, bool, error) {
	var raw, dropped []byte
	err := s.Pool.QueryRow(ctx, `SELECT summary, dropped FROM discovery_summaries
		WHERE interview_sha256=$1 AND model=$2 AND instruction_sha=$3`, sha, model, instructionSHA).Scan(&raw, &dropped)
	if errors.Is(err, pgx.ErrNoRows) {
		return discovery.Summary{}, nil, false, nil
	}
	if err != nil {
		return discovery.Summary{}, nil, false, err
	}
	var sum discovery.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return discovery.Summary{}, nil, false, nil
	}
	var d []discovery.Dropped
	_ = json.Unmarshal(dropped, &d)
	return sum, d, true, nil
}

// PutSummary кладёт конспект в кэш.
func (s *Store) PutSummary(ctx context.Context, sha, model, instructionSHA string, sum discovery.Summary, dropped []discovery.Dropped) error {
	if dropped == nil {
		dropped = []discovery.Dropped{}
	}
	raw, _ := json.Marshal(sum)
	dj, _ := json.Marshal(dropped)
	_, err := s.Pool.Exec(ctx, `INSERT INTO discovery_summaries (interview_sha256, model, instruction_sha, summary, dropped)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (interview_sha256, model, instruction_sha) DO UPDATE SET summary=EXCLUDED.summary, dropped=EXCLUDED.dropped, created_at=now()`,
		sha, model, instructionSHA, raw, dj)
	return err
}

// ─── бэклог и утверждение ────────────────────────────────────────────────

// ExistingEpicsForDiscovery — Epic'и проекта для Аналитика и проверки
// бэклога: название, цель, статус и названия связанных потребностей.
func (s *Store) ExistingEpicsForDiscovery(ctx context.Context, projectID string) ([]discovery.ExistingEpic, error) {
	return existingEpics(ctx, s.Pool, projectID)
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func existingEpics(ctx context.Context, q queryer, projectID string) ([]discovery.ExistingEpic, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id, e.title, e.goal, e.status,
			COALESCE((SELECT array_agg(DISTINCT n.need->>'title') FROM epic_needs n WHERE n.epic_id = e.id), '{}')
		FROM epics e WHERE e.project_id=$1 AND e.source_key IS NULL ORDER BY e.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []discovery.ExistingEpic
	for rows.Next() {
		var e discovery.ExistingEpic
		if err := rows.Scan(&e.ID, &e.Title, &e.Goal, &e.Status, &e.Needs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func needIDs(r DiscoveryRun) []string {
	var out []string
	if r.Result != nil {
		for _, n := range r.Result.Needs {
			out = append(out, n.ID)
		}
	}
	return out
}

// runQuotes — тексты подтверждённых цитат версии (проверка переноса речи
// респондентов в строки бэклога).
func runQuotes(r DiscoveryRun) []string {
	var out []string
	if r.Result != nil {
		for _, n := range r.Result.Needs {
			for _, q := range n.Evidence {
				out = append(out, q.Text)
			}
		}
	}
	return out
}

func lockRun(ctx context.Context, tx pgx.Tx, projectID, id string) (DiscoveryRun, error) {
	r, err := scanDiscRun(tx.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.id=$1 AND r.project_id=$2 FOR NO KEY UPDATE OF r`, id, projectID))
	return r, nf(err)
}

// SaveBacklog сохраняет бэклог с правками человека: только у версии в
// ready, только поверх текущей ревизии, с теми же проверками, что ответ
// Аналитика (кроме потребностей у добавленных человеком Epic'ов).
func (s *Store) SaveBacklog(ctx context.Context, projectID, runID string, rev int, epics []discovery.BacklogEpic) (DiscoveryRun, error) {
	var out DiscoveryRun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r, err := lockRun(ctx, tx, projectID, runID)
		if err != nil {
			return err
		}
		if r.Status != RunReady {
			return ErrRunNotEditable
		}
		if rev != r.BacklogRev {
			return &BacklogConflictError{Rev: r.BacklogRev}
		}
		existing, err := existingEpics(ctx, tx, projectID)
		if err != nil {
			return err
		}
		// Epic проекта с тем же названием мог появиться после синтеза:
		// строка связывается с ним, как при синтезе.
		epics = linkBacklog(epics, existing)
		if ferr := discovery.ValidateBacklog(epics, needIDs(r), existing); ferr != nil {
			return &DiscoveryFieldError{Field: ferr.Field, Msg: ferr.Msg}
		}
		if ferr := discovery.BacklogQuoteLeak(epics, runQuotes(r)); ferr != nil {
			return &DiscoveryFieldError{Field: ferr.Field, Msg: ferr.Msg}
		}
		bj, _ := json.Marshal(epics)
		if _, err := tx.Exec(ctx, `UPDATE discovery_runs SET backlog=$2, backlog_rev=backlog_rev+1 WHERE id=$1`, runID, bj); err != nil {
			return err
		}
		out, err = scanDiscRun(tx.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.id=$1`, runID))
		return err
	})
	return out, err
}

// linkBacklog связывает строки бэклога с Epic'ами проекта по названию и
// снимает зависимость строки на Epic, с которым она сама теперь связана.
func linkBacklog(epics []discovery.BacklogEpic, existing []discovery.ExistingEpic) []discovery.BacklogEpic {
	out := discovery.LinkExisting(epics, existing)
	for i := range out {
		if out[i].ExistingEpic == nil {
			continue
		}
		deps := out[i].DependsOn[:0:0]
		for _, d := range out[i].DependsOn {
			if d != *out[i].ExistingEpic {
				deps = append(deps, d)
			}
		}
		out[i].DependsOn = deps
	}
	return out
}

// ApproveResult — итог утверждения: версия, строки бэклога, порядок
// декомпозиции созданных Epic'ов (ключи бэклога) и Epic'и, у которых
// изменились зависимости (для событий epic.deps_changed).
type ApproveResult struct {
	Run         DiscoveryRun
	Epics       []ApprovedEpic
	Order       []string
	DepsChanged []string
}

// ApproveRun утверждает бэклог одной транзакцией (design, решение 7):
// создаёт Epic'и planned или связывает с существующими, пишет снимки
// потребностей и зависимости, проверяет граф зависимостей проекта целиком
// и переводит версию в approved. decompose отмечает созданные Epic'и
// ожидающими декомпозиции.
func (s *Store) ApproveRun(ctx context.Context, projectID, runID, userID string, rev int, decompose bool) (ApproveResult, error) {
	var res ApproveResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Порядок замков: проект (FOR NO KEY UPDATE), затем версия, затем
		// Epic'и. Граф зависимостей проекта меняется только под замком
		// проекта (как в SetEpicDeps): две правки не создадут цикл в обход
		// проверки.
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		r, err := lockRun(ctx, tx, projectID, runID)
		if err != nil {
			return err
		}
		if r.Status != RunReady || r.Outdated {
			return ErrRunNotApprovable
		}
		if rev != r.BacklogRev {
			return &BacklogConflictError{Rev: r.BacklogRev}
		}
		existing, err := existingEpics(ctx, tx, projectID)
		if err != nil {
			return err
		}
		// Свежий список Epic'ов проекта: Epic с тем же названием, созданный
		// после синтеза или правки, связывается, а не дублируется.
		r.Backlog = linkBacklog(r.Backlog, existing)
		if ferr := discovery.ValidateBacklog(r.Backlog, needIDs(r), existing); ferr != nil {
			return &DiscoveryFieldError{Field: ferr.Field, Msg: ferr.Msg}
		}
		if ferr := discovery.BacklogQuoteLeak(r.Backlog, runQuotes(r)); ferr != nil {
			return &DiscoveryFieldError{Field: ferr.Field, Msg: ferr.Msg}
		}
		order, err := discovery.TopoOrder(r.Backlog)
		if err != nil {
			return &DiscoveryFieldError{Field: "epics", Msg: err.Error()}
		}
		existingBy := map[string]discovery.ExistingEpic{}
		for _, e := range existing {
			existingBy[e.ID] = e
		}
		needBy := map[string]discovery.Need{}
		if r.Result != nil {
			for _, n := range r.Result.Needs {
				needBy[n.ID] = n
			}
		}
		keyToID := map[string]string{}
		var approved []ApprovedEpic
		for _, e := range r.Backlog {
			var prio any
			if e.Priority != "" {
				prio = e.Priority
			}
			if e.ExistingEpic != nil {
				ex, ok := existingBy[*e.ExistingEpic]
				if !ok || ex.Status == string(domain.EpicArchived) {
					return &DiscoveryFieldError{Field: "epics", Msg: "Epic проекта для связи не найден или в архиве: " + e.Title}
				}
				// Ссылка на версию меняется, только если строка приносит
				// потребности: иначе Epic потерял бы обоснование прошлой версии.
				run := any(nil)
				if len(e.Needs) > 0 {
					run = runID
				}
				if _, err := tx.Exec(ctx, `UPDATE epics SET discovery_run_id=COALESCE($2, discovery_run_id), priority=COALESCE($3, priority) WHERE id=$1`,
					ex.ID, run, prio); err != nil {
					return err
				}
				keyToID[e.Key] = ex.ID
				approved = append(approved, ApprovedEpic{Key: e.Key, EpicID: ex.ID, Action: "linked"})
				continue
			}
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO epics (project_id, title, goal, priority, discovery_run_id)
				VALUES ($1,$2,$3,$4,$5) RETURNING id`, projectID, strings.TrimSpace(e.Title), strings.TrimSpace(e.Goal), prio, runID).Scan(&id); err != nil {
				return err
			}
			keyToID[e.Key] = id
			approved = append(approved, ApprovedEpic{Key: e.Key, EpicID: id, Action: "created"})
		}
		changed := map[string]bool{}
		for _, e := range r.Backlog {
			epicID := keyToID[e.Key]
			// Завершённому Epic'у зависимости не добавляются: ждать ему
			// нечего, а правка зависимостей done запрещена.
			if e.ExistingEpic != nil && existingBy[*e.ExistingEpic].Status == string(domain.EpicDone) {
				e.DependsOn = nil
			}
			for _, nid := range e.Needs {
				n, ok := needBy[nid]
				if !ok {
					continue
				}
				nj, _ := json.Marshal(n)
				if _, err := tx.Exec(ctx, `INSERT INTO epic_needs (epic_id, run_id, need_id, need) VALUES ($1,$2,$3,$4)
					ON CONFLICT (epic_id, run_id, need_id) DO UPDATE SET need=EXCLUDED.need`, epicID, runID, nid, nj); err != nil {
					return err
				}
			}
			for _, dep := range e.DependsOn {
				target := dep
				if id, ok := keyToID[dep]; ok {
					target = id
				} else if _, ok := existingBy[dep]; !ok {
					return &DiscoveryFieldError{Field: "epics", Msg: "неизвестная зависимость " + dep + " у " + e.Title}
				}
				if target == epicID {
					continue
				}
				tag, err := tx.Exec(ctx, `INSERT INTO epic_deps (epic_id, depends_on) VALUES ($1,$2) ON CONFLICT DO NOTHING`, epicID, target)
				if err != nil {
					return err
				}
				if tag.RowsAffected() > 0 && e.ExistingEpic != nil {
					changed[epicID] = true
				}
			}
		}
		if err := projectDepsAcyclic(ctx, tx, projectID); err != nil {
			return err
		}
		created := map[string]DecomposeState{}
		var orderKeys []string
		for _, k := range order {
			for _, a := range approved {
				if a.Key == k && a.Action == "created" {
					orderKeys = append(orderKeys, k)
					if decompose {
						created[k] = DecomposeState{Status: "pending"}
					}
				}
			}
		}
		aj, _ := json.Marshal(approved)
		cj, _ := json.Marshal(created)
		bj, _ := json.Marshal(r.Backlog)
		if _, err := tx.Exec(ctx, `UPDATE discovery_runs SET status='approved', approved_at=now(),
			approved_by=NULLIF($2,'')::uuid, approved_epics=$3, decompose=$4, backlog=$5 WHERE id=$1`, runID, userID, aj, cj, bj); err != nil {
			return err
		}
		res.Run, err = scanDiscRun(tx.QueryRow(ctx, `SELECT `+discRunCols+discRunFrom+` WHERE r.id=$1`, runID))
		res.Epics, res.Order = approved, orderKeys
		for id := range changed {
			res.DepsChanged = append(res.DepsChanged, id)
		}
		sort.Strings(res.DepsChanged)
		return err
	})
	return res, err
}

// SetRunDecompose отмечает ход декомпозиции Epic'а строки бэклога.
func (s *Store) SetRunDecompose(ctx context.Context, runID, key, status, detail string) error {
	st, _ := json.Marshal(DecomposeState{Status: status, Detail: detail})
	_, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET decompose = jsonb_set(decompose, ARRAY[$2::text], $3::jsonb, true) WHERE id=$1`,
		runID, key, st)
	return err
}

// ─── зависимости Epic'ов ─────────────────────────────────────────────────

// projectDepsAcyclic проверяет граф зависимостей Epic'ов проекта целиком.
func projectDepsAcyclic(ctx context.Context, tx pgx.Tx, projectID string) error {
	rows, err := tx.Query(ctx, `SELECT d.epic_id, d.depends_on, e.title FROM epic_deps d
		JOIN epics e ON e.id = d.epic_id WHERE e.project_id=$1`, projectID)
	if err != nil {
		return err
	}
	g := map[string][]string{}
	titles := map[string]string{}
	for rows.Next() {
		var a, b, t string
		if err := rows.Scan(&a, &b, &t); err != nil {
			rows.Close()
			return err
		}
		g[a] = append(g[a], b)
		titles[a] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	cycle := discovery.FindCycle(g)
	if cycle == nil {
		return nil
	}
	names := make([]string, 0, len(cycle))
	for _, id := range cycle {
		if t := titles[id]; t != "" {
			names = append(names, "«"+t+"»")
		} else {
			names = append(names, id)
		}
	}
	return &DiscoveryFieldError{Field: "depends_on", Msg: "зависимости образуют цикл: " + strings.Join(names, " → ")}
}

// SetEpicDeps заменяет зависимости Epic'а. Epic'и другого проекта, сам
// Epic и цикл отклоняются, у done и archived зависимости не меняются.
func (s *Store) SetEpicDeps(ctx context.Context, epicID string, deps []string) (domain.Epic, error) {
	var out domain.Epic
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Порядок замков как у утверждения бэклога: проект, затем Epic, оба
		// FOR NO KEY UPDATE. Граф проекта меняется под замком проекта:
		// правка зависимостей и утверждение не создадут цикл в обход
		// проверки, а планировщик (Epic, затем событие с KEY SHARE на
		// проект) не встаёт с ними в дедлок.
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT project_id FROM epics WHERE id=$1`, epicID).Scan(&projectID); err != nil {
			return nf(err)
		}
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		var status domain.EpicStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM epics WHERE id=$1 FOR NO KEY UPDATE`, epicID).Scan(&status); err != nil {
			return nf(err)
		}
		if status == domain.EpicDone || status == domain.EpicArchived {
			return ErrEpicClosed
		}
		seen := map[string]bool{}
		var clean []string
		for _, d := range deps {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			if d == epicID {
				return &DiscoveryFieldError{Field: "depends_on", Msg: "Epic не может зависеть от самого себя"}
			}
			var p string
			if err := tx.QueryRow(ctx, `SELECT project_id FROM epics WHERE id::text=$1`, d).Scan(&p); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return &DiscoveryFieldError{Field: "depends_on", Msg: "Epic " + d + " не найден"}
				}
				return err
			}
			if p != projectID {
				return &DiscoveryFieldError{Field: "depends_on", Msg: "Epic другого проекта"}
			}
			clean = append(clean, d)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM epic_deps WHERE epic_id=$1`, epicID); err != nil {
			return err
		}
		for _, d := range clean {
			if _, err := tx.Exec(ctx, `INSERT INTO epic_deps (epic_id, depends_on) VALUES ($1,$2)`, epicID, d); err != nil {
				return err
			}
		}
		if err := projectDepsAcyclic(ctx, tx, projectID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT `+epicCols+` FROM epics WHERE id=$1`, epicID).Scan(epicDest(&out)...)
	})
	return out, err
}

// WaitingDeps — Epic'и с незавершёнными зависимостями: Epic → зависимости
// не в done (архивированная незавершённая зависимость тоже держит).
func (s *Store) WaitingDeps(ctx context.Context) (map[string][]domain.EpicRef, error) {
	rows, err := s.Pool.Query(ctx, `SELECT d.epic_id, e.id, e.title, e.status FROM epic_deps d
		JOIN epics e ON e.id = d.depends_on WHERE e.status <> 'done' ORDER BY d.epic_id, e.created_at, e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]domain.EpicRef{}
	for rows.Next() {
		var id string
		var ref domain.EpicRef
		if err := rows.Scan(&id, &ref.ID, &ref.Title, &ref.Status); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ref)
	}
	return out, rows.Err()
}

// EnrichEpics заполняет у Epic'ов приоритет, зависимости, незавершённые
// зависимости и обоснование из исследования (DTO Epic в api-contract).
func (s *Store) EnrichEpics(ctx context.Context, epics []domain.Epic) error {
	if len(epics) == 0 {
		return nil
	}
	ids := make([]string, len(epics))
	idx := map[string]int{}
	for i, e := range epics {
		ids[i] = e.ID
		idx[e.ID] = i
		epics[i].DependsOn = []string{}
		epics[i].WaitingFor = []domain.EpicRef{}
	}
	rows, err := s.Pool.Query(ctx, `SELECT e.id, e.priority, e.discovery_run_id, r.version FROM epics e
		LEFT JOIN discovery_runs r ON r.id = e.discovery_run_id WHERE e.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var prio, runID *string
		var version *int
		if err := rows.Scan(&id, &prio, &runID, &version); err != nil {
			rows.Close()
			return err
		}
		i := idx[id]
		epics[i].Priority = prio
		if runID != nil && version != nil {
			epics[i].Discovery = &domain.EpicDiscovery{RunID: *runID, Version: *version, Needs: []json.RawMessage{}}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.Pool.Query(ctx, `SELECT d.epic_id, e.id, e.title, e.status FROM epic_deps d
		JOIN epics e ON e.id = d.depends_on WHERE d.epic_id = ANY($1::uuid[]) ORDER BY e.created_at, e.id`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var ref domain.EpicRef
		if err := rows.Scan(&id, &ref.ID, &ref.Title, &ref.Status); err != nil {
			rows.Close()
			return err
		}
		i := idx[id]
		epics[i].DependsOn = append(epics[i].DependsOn, ref.ID)
		if ref.Status != string(domain.EpicDone) {
			epics[i].WaitingFor = append(epics[i].WaitingFor, ref)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Потребности — снимки версии, с которой Epic связан сейчас.
	rows, err = s.Pool.Query(ctx, `SELECT n.epic_id, n.need FROM epic_needs n JOIN epics e ON e.id = n.epic_id
		WHERE n.epic_id = ANY($1::uuid[]) AND n.run_id = e.discovery_run_id ORDER BY n.epic_id, n.need_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var need []byte
		if err := rows.Scan(&id, &need); err != nil {
			return err
		}
		if d := epics[idx[id]].Discovery; d != nil {
			d.Needs = append(d.Needs, json.RawMessage(need))
		}
	}
	return rows.Err()
}

// EpicNeeds — снимки потребностей Epic'а (контекст декомпозиции).
func (s *Store) EpicNeeds(ctx context.Context, epicID string) ([]discovery.Need, error) {
	rows, err := s.Pool.Query(ctx, `SELECT n.need FROM epic_needs n JOIN epics e ON e.id = n.epic_id
		WHERE n.epic_id=$1 AND n.run_id = e.discovery_run_id ORDER BY n.need_id`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []discovery.Need
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var n discovery.Need
		if err := json.Unmarshal(raw, &n); err == nil {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// ─── старт анализа, ход декомпозиции, сводки версий ─────────────────────

// ErrPlanExists — у Epic'а уже есть задачи: план создаётся только на
// пустой Epic (две одновременные декомпозиции не дают двух планов).
var ErrPlanExists = errors.New("у Epic уже есть задачи — декомпозиция выполняется на пустой план")

// StartRun создаёт версию по текущим интервью проекта: набор читается
// под замком проекта, поэтому интервью, удалённое одновременно с
// запуском, в версию не попадает. Возвращает версию и интервью с текстом.
func (s *Store) StartRun(ctx context.Context, projectID string, in NewRun) (DiscoveryRun, []DiscoveryInterview, error) {
	var out DiscoveryRun
	var ivs []DiscoveryInterview
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+interviewCols+`, i.text FROM discovery_interviews i
			LEFT JOIN users u ON u.id = i.created_by
			WHERE i.project_id=$1 AND i.deleted_at IS NULL ORDER BY i.created_at, i.id`, projectID)
		if err != nil {
			return err
		}
		for rows.Next() {
			iv, err := scanInterview(rows, true)
			if err != nil {
				rows.Close()
				return err
			}
			ivs = append(ivs, iv)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		in.Interviews = make([]RunInterview, 0, len(ivs))
		for _, iv := range ivs {
			in.Interviews = append(in.Interviews, RunInterview{ID: iv.ID, Title: iv.Title, SHA256: iv.SHA256})
		}
		out, err = createRunTx(ctx, tx, projectID, in)
		return err
	})
	return out, ivs, err
}

// MarkEpicDecompose отмечает ход декомпозиции Epic'а в строке
// утверждённого бэклога, если Epic создан утверждением: ручная кнопка и
// фоновая декомпозиция показывают одно состояние.
func (s *Store) MarkEpicDecompose(ctx context.Context, epicID, status, detail string) error {
	return markEpicDecompose(ctx, s.Pool, epicID, status, detail)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func markEpicDecompose(ctx context.Context, q execer, epicID, status, detail string) error {
	st, _ := json.Marshal(DecomposeState{Status: status, Detail: detail})
	_, err := q.Exec(ctx, `UPDATE discovery_runs r SET decompose = jsonb_set(r.decompose, ARRAY[k.key], $2::jsonb, true)
		FROM (SELECT d.id, a->>'key' AS key FROM discovery_runs d,
				jsonb_array_elements(COALESCE(d.approved_epics, '[]'::jsonb)) a
			WHERE a->>'epic_id' = $1) k
		WHERE r.id = k.id AND r.decompose ? k.key`, epicID, st)
	return err
}

// ListRunSummaries — версии проекта для истории: только поля сводки, без
// результата и бэклога.
func (s *Store) ListRunSummaries(ctx context.Context, projectID string) ([]DiscoveryRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, version, status, started_at, finished_at, agent_name, model, outdated
		FROM discovery_runs WHERE project_id=$1 ORDER BY version DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscoveryRun{}
	for rows.Next() {
		r := DiscoveryRun{ProjectID: projectID}
		if err := rows.Scan(&r.ID, &r.Version, &r.Status, &r.StartedAt, &r.FinishedAt, &r.AgentName, &r.Model, &r.Outdated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
