package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Версии политик и действующая политика проекта (change add-policy-presets,
// спека access-policy «Версионирование и аудит решений»). История
// append-only: активная версия области — с наибольшим номером.

const (
	PolicyScopeInstallation = "installation"
	PolicyScopeProject      = "project"
)

// PolicyVersion — версия политики области в форме api-contract.
type PolicyVersion struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ProjectID *string         `json:"project_id"`
	Version   int             `json:"version"`
	Hash      string          `json:"hash"`
	Content   json.RawMessage `json:"content"`
	CreatedAt time.Time       `json:"created_at"`
	CreatedBy string          `json:"created_by"`
}

// EffectivePolicy — действующая политика проекта и версии, из которых она
// собрана. Hash — sha256 канонического JSON действующего документа:
// решения точек принуждения ссылаются на него.
type EffectivePolicy struct {
	Presets      policy.Presets
	Hash         string
	Overrides    policy.Overrides
	Installation *PolicyVersion
	Project      *PolicyVersion
}

// Ref — ссылка на версию политики в payload события решения.
func (e EffectivePolicy) Ref() map[string]any {
	ref := map[string]any{
		"policy_version":       e.Hash,
		"installation_version": nil,
		"project_version":      nil,
	}
	if e.Installation != nil {
		ref["installation_version"] = e.Installation.ID
	}
	if e.Project != nil {
		ref["project_version"] = e.Project.ID
	}
	return ref
}

const policyCols = `id::text, scope, project_id::text, version, hash, content, created_at, created_by`

func scanPolicyVersion(row pgx.Row) (PolicyVersion, error) {
	var v PolicyVersion
	err := row.Scan(&v.ID, &v.Scope, &v.ProjectID, &v.Version, &v.Hash, &v.Content, &v.CreatedAt, &v.CreatedBy)
	return v, err
}

// activePolicyVersion — активная версия области; nil, если версий ещё нет.
func activePolicyVersion(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope, projectID string) (*PolicyVersion, error) {
	v, err := scanPolicyVersion(q.QueryRow(ctx, `
		SELECT `+policyCols+` FROM policy_versions
		WHERE scope=$1 AND ($2 = '' OR project_id = $2::uuid)
		ORDER BY version DESC LIMIT 1`, scope, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// InstallationPolicy — пресеты установки и их активная версия (nil —
// ни одной версии не сохраняли, действуют значения по умолчанию).
func (s *Store) InstallationPolicy(ctx context.Context) (policy.Presets, *PolicyVersion, error) {
	v, err := activePolicyVersion(ctx, s.Pool, PolicyScopeInstallation, "")
	if err != nil || v == nil {
		return policy.Defaults(), nil, err
	}
	var p policy.Presets
	if err := json.Unmarshal(v.Content, &p); err != nil {
		return policy.Defaults(), nil, fmt.Errorf("политика установки v%d: %w", v.Version, err)
	}
	return p.Normalize(), v, nil
}

// EffectivePolicy — действующая политика проекта: установка, перекрытая
// переопределениями проекта. Читается при каждом решении (два индексных
// чтения), кэш не нужен; изменение вступает в силу со следующего решения.
func (s *Store) EffectivePolicy(ctx context.Context, projectID string) (EffectivePolicy, error) {
	inst, instV, err := s.InstallationPolicy(ctx)
	if err != nil {
		return EffectivePolicy{}, err
	}
	out := EffectivePolicy{Installation: instV}
	projV, err := activePolicyVersion(ctx, s.Pool, PolicyScopeProject, projectID)
	if err != nil {
		return EffectivePolicy{}, err
	}
	if projV != nil {
		if err := json.Unmarshal(projV.Content, &out.Overrides); err != nil {
			return EffectivePolicy{}, fmt.Errorf("политика проекта v%d: %w", projV.Version, err)
		}
		out.Project = projV
	}
	out.Presets = policy.Effective(inst, out.Overrides)
	out.Hash = policy.Hash(out.Presets)
	return out, nil
}

// PolicyFromProjectVersion — действующая политика, собранная из конкретной
// версии проекта (ответ PUT не должен зависеть от параллельных сохранений).
func (s *Store) PolicyFromProjectVersion(ctx context.Context, v PolicyVersion) (EffectivePolicy, error) {
	inst, instV, err := s.InstallationPolicy(ctx)
	if err != nil {
		return EffectivePolicy{}, err
	}
	out := EffectivePolicy{Installation: instV, Project: &v}
	if err := json.Unmarshal(v.Content, &out.Overrides); err != nil {
		return EffectivePolicy{}, fmt.Errorf("политика проекта v%d: %w", v.Version, err)
	}
	out.Presets = policy.Effective(inst, out.Overrides)
	out.Hash = policy.Hash(out.Presets)
	return out, nil
}

// SaveInstallationPolicy создаёт новую версию пресетов установки и пишет
// событие policy.activated уровня установки (лента аудита). Автор — логин.
func (s *Store) SaveInstallationPolicy(ctx context.Context, p policy.Presets, login string) (PolicyVersion, error) {
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return PolicyVersion{}, err
	}
	return s.savePolicyVersion(ctx, PolicyScopeInstallation, "", p, login)
}

// SaveProjectPolicy создаёт новую версию переопределений проекта и пишет
// событие policy.activated проекта.
func (s *Store) SaveProjectPolicy(ctx context.Context, projectID string, o policy.Overrides, login string) (PolicyVersion, error) {
	if err := o.Validate(); err != nil {
		return PolicyVersion{}, err
	}
	return s.savePolicyVersion(ctx, PolicyScopeProject, projectID, o, login)
}

// ProjectsWithGitPolicy — проекты, чья политика живёт в репозитории:
// их файл читает синхронизация оркестратора.
func (s *Store) ProjectsWithGitPolicy(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+projectCols+` FROM projects
		WHERE policy_source = 'git' ORDER BY created_at`)
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

// SetProjectPolicySource переключает источник политики проекта.
func (s *Store) SetProjectPolicySource(ctx context.Context, projectID, source string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE projects SET policy_source=$2, policy_file_id='' WHERE id=$1`, projectID, source)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetProjectPolicyFileID запоминает версию файла, из которой создана
// последняя версия политики: по ней синхронизация видит, что содержимое
// не менялось, и лишних версий не создаёт. false — источник уже не git
// (переключили, пока файл читался): запись не нужна.
func (s *Store) SetProjectPolicyFileID(ctx context.Context, projectID, fileID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE projects SET policy_file_id=$2 WHERE id=$1 AND policy_source='git'`, projectID, fileID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SaveProjectPolicyFromGit создаёт версию политики из файла репозитория
// и запоминает идентификатор файла одной транзакцией: сбой между ними
// давал бы дубликаты версий на следующем проходе. Версия создаётся, только
// пока источник — git; false — источник переключили, пока файл читался.
func (s *Store) SaveProjectPolicyFromGit(ctx context.Context, projectID string, o policy.Overrides, fileID, login string) (bool, error) {
	if err := o.Validate(); err != nil {
		return false, err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return false, err
	}
	hash := policy.Hash(o)
	saved := false
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Строка проекта лочится: переключение источника ждёт конца
		// синхронизации, а синхронизация видит актуальный источник.
		// Порядок локов — строка проекта, затем advisory lock области:
		// savePolicyVersion берёт только advisory lock и строку проекта не
		// трогает, поэтому обратного порядка (и дедлока) нет.
		var source string
		if err := tx.QueryRow(ctx,
			`SELECT policy_source FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&source); err != nil {
			return nf(err)
		}
		if source != policy.SourceGit {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('policy:' || $1 || ':' || $2))`, PolicyScopeProject, projectID); err != nil {
			return err
		}
		v, err := scanPolicyVersion(tx.QueryRow(ctx, `
			INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by)
			VALUES ($1, $2::uuid,
				(SELECT COALESCE(MAX(version),0)+1 FROM policy_versions
				 WHERE scope=$1 AND project_id=$2::uuid),
				$3, $4, $5)
			RETURNING `+policyCols, PolicyScopeProject, projectID, hash, raw, login))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE projects SET policy_file_id=$2 WHERE id=$1`, projectID, fileID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, Type: "policy.activated", ProjectID: projectID,
			Text: fmt.Sprintf("политика проекта v%d из репозитория (%s)", v.Version, shortHash(v.Hash)),
			Payload: map[string]any{"scope": PolicyScopeProject, "version": v.Version,
				"hash": v.Hash, "source": policy.SourceGit, "commit": fileID},
		}); err != nil {
			return err
		}
		saved = true
		return nil
	})
	return saved, err
}

func (s *Store) savePolicyVersion(ctx context.Context, scope, projectID string, content any, login string) (PolicyVersion, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return PolicyVersion{}, err
	}
	hash := policy.Hash(content)
	var v PolicyVersion
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Номер версии — следующий в области. Сохранения одной области
		// сериализуются advisory-lock'ом на транзакцию: два параллельных PUT
		// получают соседние номера, а не падают на уникальном индексе.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('policy:' || $1 || ':' || $2))`, scope, projectID); err != nil {
			return err
		}
		v, err = scanPolicyVersion(tx.QueryRow(ctx, `
			INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by)
			VALUES ($1, NULLIF($2,'')::uuid,
				(SELECT COALESCE(MAX(version),0)+1 FROM policy_versions
				 WHERE scope=$1 AND ($2 = '' OR project_id=$2::uuid)),
				$3, $4, $5)
			RETURNING `+policyCols, scope, projectID, hash, raw, login))
		if err != nil {
			return err
		}
		payload := map[string]any{"scope": scope, "version": v.Version, "hash": hash}
		if projectID != "" {
			payload["project_id"] = projectID
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: login, Type: "policy.activated",
			ProjectID: projectID,
			Text:      fmt.Sprintf("активирована версия политики %d (%s)", v.Version, shortHash(hash)),
			Payload:   payload,
		})
		return err
	})
	return v, err
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ListPolicyVersions — история версий области, новые сверху.
func (s *Store) ListPolicyVersions(ctx context.Context, scope, projectID string) ([]PolicyVersion, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+policyCols+` FROM policy_versions
		WHERE scope=$1 AND ($2 = '' OR project_id = $2::uuid)
		ORDER BY version DESC`, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyVersion{}
	for rows.Next() {
		v, err := scanPolicyVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ─── лимит попыток задачи ────────────────────────────────────────────────

// SetTaskAttemptLimit меняет лимит попыток задачи (PATCH /tasks/{id}).
// Отклоняется значение меньше 1 и меньше уже израсходованных попыток.
func (s *Store) SetTaskAttemptLimit(ctx context.Context, taskID string, limit int) (domain.Task, error) {
	if limit < 1 {
		return domain.Task{}, fmt.Errorf("%w: лимит попыток должен быть не меньше 1", ErrInvalid)
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var used int
		if err := tx.QueryRow(ctx,
			`SELECT attempt_used FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&used); err != nil {
			return nf(err)
		}
		if limit < used {
			return fmt.Errorf("%w: лимит %d меньше израсходованных попыток (%d)", ErrInvalid, limit, used)
		}
		_, err := tx.Exec(ctx,
			`UPDATE tasks SET attempt_limit=$2, updated_at=now() WHERE id=$1`, taskID, limit)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return s.GetTask(ctx, taskID)
}

// ─── дневной бюджет токенов ──────────────────────────────────────────────

// DayStartUTC — начало текущих суток по UTC (граница бюджета).
func DayStartUTC(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// DailyTokenUsage — засчитанные токены с начала суток по UTC: по проектам
// и суммарно по установке. NULL не считается нулём (спека usage:
// «недоступно ≠ ноль»), поэтому строки без токенов бюджет не расходуют.
func (s *Store) DailyTokenUsage(ctx context.Context, since time.Time) (map[string]int64, int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT project_id::text, SUM(COALESCE(tokens_in,0)+COALESCE(tokens_out,0))
		FROM usage_records
		WHERE ts >= $1 AND (tokens_in IS NOT NULL OR tokens_out IS NOT NULL)
		GROUP BY project_id`, since)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int64{}
	var total int64
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, 0, err
		}
		out[id] = sum
		total += sum
	}
	return out, total, rows.Err()
}

// ProjectsWithRunningEpics — проекты, которым планировщик может что-то
// назначить: по ним считается бюджет в начале Tick.
func (s *Store) ProjectsWithRunningEpics(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT project_id::text FROM epics WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// BudgetState — состояние бюджета проекта для DTO (api-contract).
type BudgetState struct {
	DailyTokens *int64     `json:"daily_tokens"`
	UsedToday   int64      `json:"used_today"`
	PausedUntil *time.Time `json:"paused_until"`
	// PausedScope — чей бюджет исчерпан: project или installation; пусто,
	// если паузы нет.
	PausedScope string `json:"paused_scope,omitempty"`
}

// ProjectBudgets вычисляет состояние бюджета проектов на чтение: лимит из
// действующей политики, засчитано сегодня, пауза до начала следующих суток,
// если исчерпан бюджет проекта или установки. Usage и политика установки
// читаются один раз на вызов (список проектов).
func (s *Store) ProjectBudgets(ctx context.Context, projectIDs []string, now time.Time) (map[string]BudgetState, error) {
	inst, _, err := s.InstallationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	since := DayStartUTC(now)
	byProject, total, err := s.DailyTokenUsage(ctx, since)
	if err != nil {
		return nil, err
	}
	next := since.Add(24 * time.Hour)
	out := make(map[string]BudgetState, len(projectIDs))
	for _, id := range projectIDs {
		eff, err := s.EffectivePolicy(ctx, id)
		if err != nil {
			return nil, err
		}
		st := BudgetState{DailyTokens: eff.Presets.DailyTokenBudget, UsedToday: byProject[id]}
		switch {
		case inst.DailyTokenBudget != nil && total >= *inst.DailyTokenBudget:
			st.PausedScope = PolicyScopeInstallation
		case st.DailyTokens != nil && st.UsedToday >= *st.DailyTokens:
			st.PausedScope = PolicyScopeProject
		}
		if st.PausedScope != "" {
			t := next
			st.PausedUntil = &t
		}
		out[id] = st
	}
	return out, nil
}

// ProjectBudget — состояние бюджета одного проекта.
func (s *Store) ProjectBudget(ctx context.Context, projectID string, now time.Time) (BudgetState, error) {
	m, err := s.ProjectBudgets(ctx, []string{projectID}, now)
	if err != nil {
		return BudgetState{}, err
	}
	return m[projectID], nil
}

// ─── вспомогательное для точек принуждения ───────────────────────────────

// Escalate создаёт эскалацию по задаче вне транзакции перехода статуса
// (метаправило политики: задача остаётся в review, но нужен человек).
func (s *Store) Escalate(ctx context.Context, projectID, taskID string, reason domain.AttentionReason, msg string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO attention (project_id, task_id, reason, message)
		VALUES ($1,$2,$3,$4)`, projectID, taskID, string(reason), msg)
	return err
}

// EscalateProjectOnce — эскалация уровня проекта без задачи и публикации
// (движок политик недоступен): одна открытая на проект и причину, повтор
// молча игнорируется частичным уникальным индексом.
func (s *Store) EscalateProjectOnce(ctx context.Context, projectID string, reason domain.AttentionReason, msg string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO attention (project_id, task_id, reason, message)
		VALUES ($1, NULL, $2, $3)
		ON CONFLICT DO NOTHING`, projectID, string(reason), msg)
	return err
}

// ResolveProjectEscalation закрывает открытые эскалации уровня проекта с
// указанной причиной: движок снова отвечает — эскалация «движок недоступен»
// больше не нужна и не должна занимать место (одна открытая на проект).
func (s *Store) ResolveProjectEscalation(ctx context.Context, projectID string, reason domain.AttentionReason) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE attention SET status='resolved', resolved_at=now()
		WHERE project_id=$1 AND reason=$2 AND status <> 'resolved'
		  AND task_id IS NULL AND deployment_id IS NULL`, projectID, string(reason))
	return err
}

// AutoEnvironmentNames — окружения проекта с автоматическим запуском
// публикации (для события deploy.deferred).
func (s *Store) AutoEnvironmentNames(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT name FROM environments WHERE project_id=$1 AND trigger='auto' ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// CompleteTask — merge выполнен: done, runner свободен, открытые эскалации
// задачи закрыты (POLICY_CHANGE после подтверждения человеком).
func (s *Store) CompleteTask(ctx context.Context, taskID string, ev EventInput) error {
	return s.TransitionTask(ctx, taskID, domain.TaskDone, ev, func(tx pgx.Tx) error {
		if err := freeRunner(ctx, tx, taskID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE attention SET status='resolved', resolved_at=now()
			WHERE task_id=$1 AND status <> 'resolved'`, taskID)
		return err
	})
}
