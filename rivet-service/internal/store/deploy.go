package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Окружения публикации и деплой-конвейер (спека backend/deployment,
// design implement-deployment). Инварианты очереди держит БД: partial
// unique индексы «одна queued» и «одна активная» на окружение (0006).

const envCols = `id::text, project_id::text, name, exec_type, trigger, config, paused, created_at`

func scanEnv(row pgx.Row) (domain.Environment, error) {
	var e domain.Environment
	var cfg []byte
	if err := row.Scan(&e.ID, &e.ProjectID, &e.Name, &e.ExecType, &e.Trigger, &cfg, &e.Paused, &e.Created); err != nil {
		return e, err
	}
	return e, json.Unmarshal(cfg, &e.Config)
}

const depCols = `id::text, env_id::text, version, status, initiator, runner_id,
	detail, rollback, COALESCE(log_ref,''), created_at, started_at, ended_at`

func scanDeployment(row pgx.Row) (domain.Deployment, error) {
	var d domain.Deployment
	err := row.Scan(&d.ID, &d.EnvID, &d.Version, &d.Status, &d.Initiator, &d.RunnerID,
		&d.Detail, &d.Rollback, &d.LogRef, &d.Created, &d.Started, &d.Ended)
	return d, err
}

// CreateEnvironment создаёт окружение; дубль имени в проекте — ErrConflict.
func (s *Store) CreateEnvironment(ctx context.Context, e domain.Environment) (domain.Environment, error) {
	cfg, err := json.Marshal(e.Config)
	if err != nil {
		return e, err
	}
	out, err := scanEnv(s.Pool.QueryRow(ctx, `
		INSERT INTO environments (project_id, name, exec_type, trigger, config)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (project_id, name) DO NOTHING
		RETURNING `+envCols, e.ProjectID, e.Name, e.ExecType, e.Trigger, cfg))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("окружение %q: %w", e.Name, ErrConflict)
	}
	return out, err
}

// UpdateEnvironment заменяет имя, правило запуска и конфигурацию целиком
// (PATCH по контракту — replace, не merge).
func (s *Store) UpdateEnvironment(ctx context.Context, e domain.Environment) (domain.Environment, error) {
	cfg, err := json.Marshal(e.Config)
	if err != nil {
		return e, err
	}
	out, err := scanEnv(s.Pool.QueryRow(ctx, `
		UPDATE environments SET name=$2, trigger=$3, config=$4
		WHERE id=$1
		RETURNING `+envCols, e.ID, e.Name, e.Trigger, cfg))
	if isUnique(err) {
		return out, fmt.Errorf("окружение %q: %w", e.Name, ErrConflict)
	}
	return out, nf(err)
}

// DeleteEnvironment удаляет окружение с историей публикаций;
// выполняющаяся публикация — ErrConflict. Строки публикаций лочатся до
// проверки: параллельный StartNextDeployment (FOR UPDATE SKIP LOCKED)
// пропустит залоченную очередь, а уже стартовавшая публикация дождётся
// коммита и будет видна как активная — окружение не удалится из-под неё.
func (s *Store) DeleteEnvironment(ctx context.Context, envID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM deployments WHERE env_id=$1 FOR UPDATE`, envID); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM deployments
			WHERE env_id=$1 AND status IN ('deploying','verifying'))`, envID).Scan(&active); err != nil {
			return err
		}
		if active {
			return fmt.Errorf("идёт публикация: %w", ErrConflict)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM environments WHERE id=$1`, envID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// EnvironmentForViewer — окружение, только если viewer участник его проекта
// (один scoped-запрос, паттерн TaskProjectForViewer).
func (s *Store) EnvironmentForViewer(ctx context.Context, envID, viewerID string) (domain.Environment, error) {
	e, err := scanEnv(s.Pool.QueryRow(ctx, `
		SELECT `+envColsPrefixed("e")+` FROM environments e
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE e.id = $1`, envID, viewerID))
	return e, nf(err)
}

func envColsPrefixed(a string) string {
	return a + `.id::text, ` + a + `.project_id::text, ` + a + `.name, ` + a + `.exec_type, ` +
		a + `.trigger, ` + a + `.config, ` + a + `.paused, ` + a + `.created_at`
}

// ListEnvironments — окружения проекта по имени.
func (s *Store) ListEnvironments(ctx context.Context, projectID string) ([]domain.Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+envCols+` FROM environments WHERE project_id=$1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Environment
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastDeployment — последняя публикация окружения (ErrNotFound, если их нет).
func (s *Store) LastDeployment(ctx context.Context, envID string) (domain.Deployment, error) {
	d, err := scanDeployment(s.Pool.QueryRow(ctx, `
		SELECT `+depCols+` FROM deployments
		WHERE env_id=$1 ORDER BY created_at DESC LIMIT 1`, envID))
	return d, nf(err)
}

// ListDeployments — история публикаций окружения по убыванию created_at.
func (s *Store) ListDeployments(ctx context.Context, envID string, limit int) ([]domain.Deployment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+depCols+` FROM deployments
		WHERE env_id=$1 ORDER BY created_at DESC LIMIT $2`, envID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EnqueueDeployment ставит публикацию окружения в очередь. Уже стоящая
// queued-публикация коалесцируется: обновляются только version и initiator
// (спека deployment «Режимы запуска»). Возвращает актуальную queued-запись.
func (s *Store) EnqueueDeployment(ctx context.Context, envID, version, initiator string) (domain.Deployment, error) {
	d, err := scanDeployment(s.Pool.QueryRow(ctx, `
		INSERT INTO deployments (env_id, version, initiator)
		VALUES ($1,$2,$3)
		ON CONFLICT (env_id) WHERE status='queued'
		DO UPDATE SET version=EXCLUDED.version, initiator=EXCLUDED.initiator
		RETURNING `+depCols, envID, version, initiator))
	return d, err
}

// EnqueueAutoDeployments ставит публикации всех auto-окружений проекта после
// merge (коалесценция та же); окружения на паузе получают/обновляют queued —
// её подхватят после resume.
func (s *Store) EnqueueAutoDeployments(ctx context.Context, projectID, version string) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM environments WHERE project_id=$1 AND trigger='auto'`, projectID)
	if err != nil {
		return err
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return err
	}
	for _, envID := range ids {
		if _, err := s.EnqueueDeployment(ctx, envID, version, "auto"); err != nil {
			return err
		}
	}
	return nil
}

// SetEnvPaused — пауза/возобновление автопубликаций окружения.
func (s *Store) SetEnvPaused(ctx context.Context, envID string, paused bool) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE environments SET paused=$2 WHERE id=$1`, envID, paused)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeployAssignment — публикация, готовая к отправке deploy-runner'у.
type DeployAssignment struct {
	Deployment domain.Deployment
	Env        domain.Environment
	ProjectID  string
	Repo       string
	RunnerID   string
}

// StartNextDeployment берёт queued-публикацию окружения без активной
// публикации и паузы, назначает idle runner с capability deploy
// (status deploying у обоих). ok=false — нечего запускать.
func (s *Store) StartNextDeployment(ctx context.Context) (DeployAssignment, bool, error) {
	var a DeployAssignment
	started := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var depID, envID, runnerID string
		err := tx.QueryRow(ctx, `
			SELECT d.id, d.env_id, r.id
			FROM deployments d
			JOIN environments e ON e.id = d.env_id AND NOT e.paused
			JOIN LATERAL (
				SELECT r.id FROM runners r
				WHERE r.status = 'idle' AND NOT r.draining
				  AND r.capabilities @> ARRAY['deploy']
				ORDER BY r.last_seen DESC
				FOR UPDATE OF r SKIP LOCKED
				LIMIT 1
			) r ON true
			WHERE d.status = 'queued'
			  AND NOT EXISTS (SELECT 1 FROM deployments a
			                  WHERE a.env_id = d.env_id AND a.status IN ('deploying','verifying'))
			ORDER BY d.created_at
			FOR UPDATE OF d SKIP LOCKED
			LIMIT 1`).Scan(&depID, &envID, &runnerID)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deployments SET status='deploying', runner_id=$2, started_at=now()
			WHERE id=$1`, depID, runnerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runners SET status='deploying', task_id=NULL, deployment_id=$2, ctx_pct=NULL
			WHERE id=$1`, runnerID, depID); err != nil {
			return err
		}
		var projectID, repo string
		if err := tx.QueryRow(ctx, `
			SELECT p.id::text, p.repo FROM environments e JOIN projects p ON p.id=e.project_id
			WHERE e.id=$1`, envID).Scan(&projectID, &repo); err != nil {
			return err
		}
		env, err := scanEnv(tx.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1`, envID))
		if err != nil {
			return err
		}
		dep, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+depCols+` FROM deployments WHERE id=$1`, depID))
		if err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "deploy.status",
			ProjectID: projectID,
			Text:      fmt.Sprintf("публикация %s: деплой версии %s (runner %s)", env.Name, dep.Version, runnerID),
			Payload: map[string]any{"environment_id": envID, "deployment_id": depID,
				"status": "deploying", "version": dep.Version},
		}); err != nil {
			return err
		}
		a = DeployAssignment{Deployment: dep, Env: env, ProjectID: projectID, Repo: repo, RunnerID: runnerID}
		started = true
		return nil
	})
	return a, started, err
}

// MarkDeploymentRollingBack переводит активную публикацию владельца в фазу
// отката (durable) и фиксирует причину провала в detail; false — публикацию
// уже финализировали или она не наша (stale).
func (s *Store) MarkDeploymentRollingBack(ctx context.Context, depID, runnerID, detail string) (bool, error) {
	// started_at обновляется: rollback-джоба получает свой полный дедлайн
	// watchdog'а, а не остаток от исходного деплоя.
	tag, err := s.Pool.Exec(ctx, `
		UPDATE deployments SET rollback=true, detail=left($3, 8000), started_at=now()
		WHERE id=$1 AND runner_id=$2 AND ended_at IS NULL AND NOT rollback`, depID, runnerID, detail)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RunnerActiveDeployment — активная публикация runner'а (пустая строка,
// если её нет): reconnect runner'а убивает его деплой-goroutine, публикацию
// нужно провалить сразу, не дожидаясь watchdog.
func (s *Store) RunnerActiveDeployment(ctx context.Context, runnerID string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(deployment_id::text,'') FROM runners WHERE id=$1`, runnerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// DeployStageVerifying — CAS-переход deploying → verifying от владельца
// (deploy ok); false — переход не наш (stale/чужой/повторный результат).
func (s *Store) DeployStageVerifying(ctx context.Context, depID, runnerID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE deployments SET status='verifying'
		WHERE id=$1 AND runner_id=$2 AND status='deploying'`, depID, runnerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FinishDeployment — голый CAS-финал (для тестов и особых путей);
// продуктовые финалы — CompleteDeployment/FailDeployment одной транзакцией.
func (s *Store) FinishDeployment(ctx context.Context, depID, runnerID, status, detail string) (bool, error) {
	claimed := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		claimed, err = finishDeploymentTx(ctx, tx, depID, runnerID, status, detail)
		return err
	})
	return claimed, err
}

// SetDeploymentLog фиксирует ссылку на сохранённый лог публикации.
func (s *Store) SetDeploymentLog(ctx context.Context, depID, logRef string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deployments SET log_ref=NULLIF($2,'') WHERE id=$1`, depID, logRef)
	return err
}

// GetDeployment — публикация по id.
func (s *Store) GetDeployment(ctx context.Context, depID string) (domain.Deployment, error) {
	d, err := scanDeployment(s.Pool.QueryRow(ctx, `
		SELECT `+depCols+` FROM deployments WHERE id=$1`, depID))
	return d, nf(err)
}

// GetEnvironment — окружение по id.
func (s *Store) GetEnvironment(ctx context.Context, envID string) (domain.Environment, error) {
	e, err := scanEnv(s.Pool.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1`, envID))
	return e, nf(err)
}

// LastSuccessfulVersion — версия последней успешной публикации окружения
// до указанной (для отката); пустая строка — откатываться некуда.
func (s *Store) LastSuccessfulVersion(ctx context.Context, envID, beforeDepID string) (string, error) {
	var v string
	err := s.Pool.QueryRow(ctx, `
		SELECT version FROM deployments
		WHERE env_id=$1 AND id<>$2 AND status='done'
		ORDER BY ended_at DESC LIMIT 1`, envID, beforeDepID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// finishDeploymentTx — атомарный CAS-финал внутри транзакции: захват только
// открытой публикации, для результатов runner'а — с проверкой владельца.
func finishDeploymentTx(ctx context.Context, tx pgx.Tx, depID, runnerID, status, detail string) (bool, error) {
	owner := ` AND runner_id=$4`
	args := []any{depID, status, detail, runnerID}
	if runnerID == "" {
		owner = ""
		args = args[:3]
	}
	// Финал дописывает причину к уже накопленному detail (фаза отката
	// хранит там исходный провал), а не затирает его.
	tag, err := tx.Exec(ctx, `
		UPDATE deployments SET
			detail=left(CASE WHEN detail='' OR $3='' THEN detail || $3
			            ELSE detail || '; ' || $3 END, 8000),
			status=$2, ended_at=now()
		WHERE id=$1 AND ended_at IS NULL`+owner, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CompleteDeployment — успешный финал одной транзакцией: done, событие,
// освобождение runner'а. Частичный сбой откатывает всё — повтор результата
// восстановит цепочку целиком (идемпотентность at-least-once доставки).
func (s *Store) CompleteDeployment(ctx context.Context, depID, runnerID string) (bool, error) {
	claimed := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		if claimed, err = finishDeploymentTx(ctx, tx, depID, runnerID, "done", ""); err != nil || !claimed {
			return err
		}
		var envID, envName, projectID, version string
		if err := tx.QueryRow(ctx, `
			SELECT d.env_id::text, e.name, e.project_id::text, d.version
			FROM deployments d JOIN environments e ON e.id=d.env_id
			WHERE d.id=$1`, depID).Scan(&envID, &envName, &projectID, &version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runners SET status='idle', deployment_id=NULL WHERE deployment_id=$1`, depID); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "deploy.status",
			ProjectID: projectID,
			Text:      fmt.Sprintf("публикация %s: версия %s выполнена", envName, version),
			Payload: map[string]any{"environment_id": envID, "deployment_id": depID,
				"status": "done", "version": version},
		})
		return err
	})
	return claimed, err
}

// FailDeployment проваливает публикацию одной транзакцией: финал (CAS),
// пауза автопубликаций, эскалация DEPLOY_FAILED, событие, освобождение
// runner'а. Частичный сбой откатывает всё (см. CompleteDeployment).
func (s *Store) FailDeployment(ctx context.Context, depID, runnerID, status, detail string) (bool, error) {
	claimed := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		if claimed, err = finishDeploymentTx(ctx, tx, depID, runnerID, status, detail); err != nil || !claimed {
			return err
		}
		var envID, envName, projectID, version string
		if err := tx.QueryRow(ctx, `
			SELECT d.env_id::text, e.name, e.project_id::text, d.version
			FROM deployments d JOIN environments e ON e.id=d.env_id
			WHERE d.id=$1`, depID).Scan(&envID, &envName, &projectID, &version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET paused=true WHERE id=$1`, envID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO attention (project_id, deployment_id, reason, message)
			VALUES ($1,$2,$3,$4)`, projectID, depID, string(domain.AttDeployFailed),
			fmt.Sprintf("Публикация %s (версия %s) провалилась: %s", envName, version, left(detail, 2000))); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runners SET status='idle', deployment_id=NULL WHERE deployment_id=$1`, depID); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorScheduler, Type: "deploy.status",
			ProjectID: projectID,
			Text:      fmt.Sprintf("публикация %s провалилась (%s) — автопубликации приостановлены", envName, status),
			Payload: map[string]any{"environment_id": envID, "deployment_id": depID,
				"status": status, "version": version},
		})
		return err
	})
	return claimed, err
}

// TimedOutDeployments — активные публикации старше дедлайна (watchdog):
// runner жив, но джоба зависла или результат потерян.
func (s *Store) TimedOutDeployments(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM deployments
		WHERE status IN ('deploying','verifying')
		  AND started_at < now() - make_interval(secs => $1)`,
		int(olderThan.Seconds()))
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// DeploymentLogForViewer — log_ref публикации, только если viewer участник
// проекта (один scoped-запрос).
func (s *Store) DeploymentLogForViewer(ctx context.Context, depID, viewerID string) (string, error) {
	var ref string
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(d.log_ref,'') FROM deployments d
		JOIN environments e ON e.id = d.env_id
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE d.id = $1`, depID, viewerID).Scan(&ref)
	return ref, nf(err)
}

func left(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isUnique(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// DeploymentRefs — контекст публикации для событий и лога.
func (s *Store) DeploymentRefs(ctx context.Context, depID string) (projectID, envID, envName, version string, err error) {
	err = nf(s.Pool.QueryRow(ctx, `
		SELECT e.project_id::text, e.id::text, e.name, d.version
		FROM deployments d JOIN environments e ON e.id=d.env_id
		WHERE d.id=$1`, depID).Scan(&projectID, &envID, &envName, &version))
	return
}

// DeploymentOwned — принадлежит ли активная публикация runner'у
// (защита чанков лога и результатов от stale-replay; аналог OpenSession).
func (s *Store) DeploymentOwned(ctx context.Context, depID, runnerID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
		WHERE id=$1 AND runner_id=$2 AND ended_at IS NULL)`, depID, runnerID).Scan(&ok)
	return ok, err
}
