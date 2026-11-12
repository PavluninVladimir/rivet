package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Подключение репозитория к проекту (change add-repo-onboarding): учётные
// данные хранятся шифротекстом, наружу отдаются владелец и префикс.

const credCols = `id::text, provider, base_url, owner, token_prefix, state, state_reason, state_message, checked_at, created_at`

func scanCredential(row pgx.Row) (domain.ScmCredential, error) {
	var c domain.ScmCredential
	err := row.Scan(&c.ID, &c.Provider, &c.BaseURL, &c.Owner, &c.TokenPrefix,
		&c.State, &c.StateReason, &c.StateMessage, &c.CheckedAt, &c.Created)
	return c, err
}

// NewRepoConnection — параметры подключения репозитория к проекту.
type NewRepoConnection struct {
	Provider      string
	BaseURL       string
	RepoPath      string
	DefaultBranch string
	// Token — секрет хостинга; пусто означает работу на глобальном токене
	// установки (проекты до этого изменения и e2e-стенд).
	Token      string
	TokenOwner string
}

// tokenPrefix — узнаваемый кусочек токена. Короткий токен не показываем
// вовсе: «префикс» длиной с сам секрет раскрыл бы его целиком.
func tokenPrefix(token string) string {
	const n = 8
	if len(token) <= n+4 {
		return ""
	}
	return token[:n]
}

// newWebhookSecret — секрет подписи событий на проект.
func newWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CreateProjectWithRepo создаёт проект вместе с подключением репозитория:
// учётные данные, секрет webhook и членство создателя — одной транзакцией.
func (s *Store) CreateProjectWithRepo(ctx context.Context, name string, checks []domain.Check,
	creatorID string, conn NewRepoConnection, box *secretbox.Box) (domain.Project, error) {

	raw, _ := json.Marshal(checks)
	if conn.DefaultBranch == "" {
		conn.DefaultBranch = "main"
	}
	// Секрет на проект выдаётся только вместе с собственными учётными
	// данными: у проекта без токена (устаревшая форма, стенд) хостинг
	// настроен на общий секрет установки, и свой секрет сломал бы приём.
	secret := ""
	if conn.Token != "" {
		var err error
		if secret, err = newWebhookSecret(); err != nil {
			return domain.Project{}, err
		}
	}
	p := domain.Project{
		Name: name, Checks: checks, Provider: conn.Provider, BaseURL: conn.BaseURL,
		RepoPath: conn.RepoPath, DefaultBranch: conn.DefaultBranch, WebhookSecret: secret,
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Создатель становится владельцем, поэтому его учётная запись
		// блокируется на чтение первой: параллельная деактивация дождётся
		// коммита и увидит новый проект, а не оставит его без владельца.
		var disabled bool
		if err := tx.QueryRow(ctx,
			`SELECT disabled FROM users WHERE id=$1 FOR SHARE`, creatorID).Scan(&disabled); err != nil {
			return nf(err)
		}
		if disabled {
			return fmt.Errorf("учётная запись отключена: %w", ErrConflict)
		}
		credID, err := insertCredential(ctx, tx, conn, box)
		if err != nil {
			return err
		}
		p.CredentialID = credID
		var cred any
		if credID != "" {
			cred = credID
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (name, checks, provider, base_url, repo_path,
				default_branch, webhook_secret, credential_id)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8::uuid)
			RETURNING id, created_at`,
			name, raw, conn.Provider, conn.BaseURL, conn.RepoPath,
			conn.DefaultBranch, secret, cred).Scan(&p.ID, &p.Created); err != nil {
			return err
		}
		// Создатель проекта становится его владельцем (спека domain-model).
		_, err = tx.Exec(ctx,
			`INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,'owner')`,
			p.ID, creatorID)
		return err
	})
	return p, err
}

// insertCredential сохраняет шифротекст токена; пустой токен — учётных
// данных нет (работаем на глобальном токене установки).
func insertCredential(ctx context.Context, tx pgx.Tx, conn NewRepoConnection, box *secretbox.Box) (string, error) {
	if conn.Token == "" {
		return "", nil
	}
	if !box.Enabled() {
		return "", secretbox.ErrNoKey
	}
	enc, err := box.Seal(conn.Token)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO scm_credentials (provider, base_url, owner, token_prefix, token_enc, state, checked_at)
		VALUES ($1,$2,$3,$4,$5,'ok',now())
		RETURNING id::text`,
		conn.Provider, conn.BaseURL, conn.TokenOwner, tokenPrefix(conn.Token), enc).Scan(&id)
	return id, err
}

// ProjectToken расшифровывает токен проекта. Пустая строка без ошибки —
// у проекта нет своих учётных данных (глобальный токен установки).
func (s *Store) ProjectToken(ctx context.Context, projectID string, box *secretbox.Box) (string, error) {
	var enc []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT c.token_enc FROM projects p
		JOIN scm_credentials c ON c.id = p.credential_id
		WHERE p.id = $1`, projectID).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return box.Open(enc)
}

// ProjectCredentialState — токен проекта, id и состояние его учётных данных
// одним запросом: адаптер конвейера отмечает итог операций именно по этим
// учётным данным. Пустой токен без ошибки — у проекта нет своих.
type ProjectCredentialState struct {
	Token  string
	CredID string
	State  string
}

func (s *Store) ProjectTokenState(ctx context.Context, projectID string, box *secretbox.Box) (ProjectCredentialState, error) {
	var enc []byte
	var out ProjectCredentialState
	err := s.Pool.QueryRow(ctx, `
		SELECT c.token_enc, c.id::text, c.state FROM projects p
		JOIN scm_credentials c ON c.id = p.credential_id
		WHERE p.id = $1`, projectID).Scan(&enc, &out.CredID, &out.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectCredentialState{}, nil
	}
	if err != nil {
		return ProjectCredentialState{}, err
	}
	out.Token, err = box.Open(enc)
	return out, err
}

// ProjectCredentialsInvalid — учётные данные проекта нерабочие: новые
// стадии проекта не запускаются.
func (s *Store) ProjectCredentialsInvalid(ctx context.Context, projectID string) (bool, error) {
	var invalid bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM projects p JOIN scm_credentials c ON c.id = p.credential_id
			WHERE p.id = $1 AND c.state = 'invalid')`, projectID).Scan(&invalid)
	return invalid, err
}

// ProjectCredential — метаданные учётных данных проекта (без секрета).
func (s *Store) ProjectCredential(ctx context.Context, projectID string) (domain.ScmCredential, error) {
	c, err := scanCredential(s.Pool.QueryRow(ctx, `
		SELECT `+credColsPrefixed("c")+` FROM projects p
		JOIN scm_credentials c ON c.id = p.credential_id
		WHERE p.id = $1`, projectID))
	return c, nf(err)
}

func credColsPrefixed(a string) string {
	cols := strings.Split(credCols, ", ")
	for i, c := range cols {
		cols[i] = a + "." + strings.TrimPrefix(c, "")
	}
	return strings.Join(cols, ", ")
}

// ReplaceCredential заменяет учётные данные проекта без его пересоздания.
func (s *Store) ReplaceCredential(ctx context.Context, projectID string, conn NewRepoConnection, box *secretbox.Box) error {
	if !box.Enabled() {
		return secretbox.ErrNoKey
	}
	enc, err := box.Seal(conn.Token)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var old *string
		if err := tx.QueryRow(ctx,
			`SELECT credential_id::text FROM projects WHERE id=$1`, projectID).Scan(&old); err != nil {
			return nf(err)
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO scm_credentials (provider, base_url, owner, token_prefix, token_enc, state, checked_at)
			VALUES ($1,$2,$3,$4,$5,'ok',now())
			RETURNING id::text`,
			conn.Provider, conn.BaseURL, conn.TokenOwner, tokenPrefix(conn.Token), enc).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE projects SET credential_id=$2::uuid WHERE id=$1`, projectID, id); err != nil {
			return err
		}
		if old != nil && *old != "" {
			// Старая запись больше не нужна: один проект — одни учётные данные.
			if _, err := tx.Exec(ctx, `DELETE FROM scm_credentials WHERE id=$1::uuid`, *old); err != nil {
				return err
			}
		}
		// Новые учётные данные проверены: эскалация о нерабочих снимается,
		// конвейер проекта продолжает работу.
		return resolveProjectEscalationTx(ctx, tx, projectID, domain.AttScmCredentials)
	})
}

// Состояния учётных данных хостинга.
const (
	CredentialOK      = "ok"
	CredentialInvalid = "invalid"
)

// CredentialMark — наблюдение за учётными данными проекта: состояние,
// причина (код и текст) у нерабочего и что его выявило (операция хостинга,
// git на runner'е, повторная проверка).
type CredentialMark struct {
	State   string
	Reason  string
	Message string
	Source  string
}

// MarkCredential меняет состояние учётных данных проекта одной
// транзакцией. Переход в нерабочее пишет событие project.credentials и
// открывает эскалацию SCM_CREDENTIALS, возврат в рабочее — событие и
// закрытие эскалации. Повтор того же состояния обновляет причину, время
// проверки и текст открытой эскалации. changed — состояние сменилось.
// credID — учётные данные, которыми шла операция: если их уже заменили,
// наблюдение устарело и ничего не меняет.
func (s *Store) MarkCredential(ctx context.Context, projectID, credID string, m CredentialMark) (changed bool, err error) {
	if m.State != CredentialOK && m.State != CredentialInvalid {
		return false, fmt.Errorf("недопустимое состояние учётных данных %q", m.State)
	}
	if m.State == CredentialOK {
		m.Reason, m.Message = "", ""
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var old, oldMessage, provider, repoPath string
		err := tx.QueryRow(ctx, `
			SELECT c.state, c.state_message, p.provider, p.repo_path FROM projects p
			JOIN scm_credentials c ON c.id = p.credential_id
			WHERE p.id = $1 AND c.id = $2::uuid FOR UPDATE OF c`, projectID, credID).Scan(&old, &oldMessage, &provider, &repoPath)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE scm_credentials SET state=$2, state_reason=$3, state_message=$4, checked_at=now()
			WHERE id=$1::uuid`, credID, m.State, m.Reason, clipRunes(m.Message, 500)); err != nil {
			return err
		}
		// Причина нерабочего подключения сменилась: текст открытой
		// эскалации тот же, что в настройках.
		if old == CredentialInvalid && m.State == CredentialInvalid && clipRunes(m.Message, 500) != oldMessage {
			_, err := tx.Exec(ctx, `
				UPDATE attention SET message=$3
				WHERE project_id=$1 AND reason=$2 AND status <> 'resolved'
				  AND task_id IS NULL AND deployment_id IS NULL`,
				projectID, string(domain.AttScmCredentials), credentialEscalation(m.Message))
			return err
		}
		// Событие и эскалация — только на переходе между рабочим и
		// нерабочим: unchecked → ok ничего не сообщает.
		if old == m.State || (old != CredentialInvalid && m.State != CredentialInvalid) {
			return nil
		}
		changed = true
		payload := map[string]any{"state": m.State, "provider": provider, "repo_path": repoPath, "source": m.Source}
		text := "подключение к хостингу снова работает — конвейер проекта продолжается"
		if m.State == CredentialInvalid {
			payload["reason"], payload["message"] = m.Reason, m.Message
			text = "учётные данные хостинга не работают: " + m.Message +
				" — конвейер проекта остановлен до замены токена"
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, Type: "project.credentials",
			ProjectID: projectID, Text: text, Payload: payload,
		}); err != nil {
			return err
		}
		if m.State == CredentialOK {
			return resolveProjectEscalationTx(ctx, tx, projectID, domain.AttScmCredentials)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO attention (project_id, task_id, reason, message)
			VALUES ($1, NULL, $2, $3)
			ON CONFLICT DO NOTHING`, projectID, string(domain.AttScmCredentials), credentialEscalation(m.Message))
		return err
	})
	return changed, err
}

// credentialEscalation — текст эскалации о нерабочих учётных данных.
func credentialEscalation(reason string) string {
	return "учётные данные хостинга перестали действовать: " + reason +
		". Замените токен в настройках проекта: задачи продолжат конвейер, попытки не расходуются"
}

// TouchCredentialRecheck отмечает попытку повторной проверки при любом её
// исходе: очередь проверок движется и при недоступном хостинге.
func (s *Store) TouchCredentialRecheck(ctx context.Context, credID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE scm_credentials SET recheck_at=now() WHERE id=$1::uuid`, credID)
	return err
}

// resolveProjectEscalationTx закрывает открытую эскалацию уровня проекта
// с указанной причиной внутри транзакции.
func resolveProjectEscalationTx(ctx context.Context, tx pgx.Tx, projectID string, reason domain.AttentionReason) error {
	_, err := tx.Exec(ctx, `
		UPDATE attention SET status='resolved', resolved_at=now()
		WHERE project_id=$1 AND reason=$2 AND status <> 'resolved'
		  AND task_id IS NULL AND deployment_id IS NULL`, projectID, string(reason))
	return err
}

// InvalidCredentialProjects — проекты с нерабочими учётными данными: им
// ничего не назначается, пока подключение не восстановится.
func (s *Store) InvalidCredentialProjects(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id::text FROM projects p
		JOIN scm_credentials c ON c.id = p.credential_id
		WHERE c.state = 'invalid' ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// CredentialsToRecheck — проекты с нерабочими учётными данными, которые не
// проверялись дольше every: повторная проверка возвращает их в работу, если
// доступ восстановили на хостинге без замены токена.
func (s *Store) CredentialsToRecheck(ctx context.Context, every time.Duration, limit int) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+projectCols+` FROM projects WHERE id IN (
			SELECT p.id FROM projects p
			JOIN scm_credentials c ON c.id = p.credential_id
			WHERE c.state = 'invalid'
			  AND COALESCE(GREATEST(c.recheck_at, c.checked_at), 'epoch') < now() - make_interval(secs => $1)
			ORDER BY COALESCE(GREATEST(c.recheck_at, c.checked_at), 'epoch') LIMIT $2)
		ORDER BY created_at`, every.Seconds(), limit)
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

// SetProjectRepoMeta обновляет метаданные подключения (ветка по умолчанию).
func (s *Store) SetProjectRepoMeta(ctx context.Context, projectID, defaultBranch string) error {
	if defaultBranch == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE projects SET default_branch=$2 WHERE id=$1`, projectID, defaultBranch)
	return err
}

// UpdateProjectSettings правит название и проверки проекта (api-contract:
// checks заменяются целиком).
func (s *Store) UpdateProjectSettings(ctx context.Context, projectID, name string, checks []domain.Check) (domain.Project, error) {
	raw, err := json.Marshal(checks)
	if err != nil {
		return domain.Project{}, err
	}
	p, err := scanProject(s.Pool.QueryRow(ctx, `
		UPDATE projects SET name=$2, checks=$3 WHERE id=$1
		RETURNING `+projectCols, projectID, name, raw))
	return p, nf(err)
}

// ProjectsByRepo — проекты с таким путём репозитория у провайдера. Их
// может быть несколько: один и тот же путь встречается на разных
// инстансах (github.com и self-hosted), а событие инстанс не называет —
// поэтому выбор делает проверка подписи по секрету каждого кандидата.
func (s *Store) ProjectsByRepo(ctx context.Context, provider, repoPath string) ([]domain.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+projectCols+` FROM projects
		WHERE provider=$1 AND lower(repo_path)=lower($2)
		ORDER BY created_at`, provider, repoPath)
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

// EnsureWebhookSecret выдаёт проекту собственный секрет, если его ещё нет
// (проект переходит с общего секрета установки на свой). Возвращает
// актуальный секрет.
func (s *Store) EnsureWebhookSecret(ctx context.Context, projectID string) (string, error) {
	secret, err := newWebhookSecret()
	if err != nil {
		return "", err
	}
	var out string
	err = s.Pool.QueryRow(ctx, `
		UPDATE projects SET webhook_secret = COALESCE(webhook_secret, $2)
		WHERE id=$1 RETURNING webhook_secret`, projectID, secret).Scan(&out)
	return out, nf(err)
}

// SetWebhookRegistered отмечает, что подписка на события создана системой.
func (s *Store) SetWebhookRegistered(ctx context.Context, projectID string, ok bool) error {
	// Время регистрации: у общего хука репозитория секрет последнего
	// зарегистрировавшегося проекта (add-architecture-c4, переподписка).
	_, err := s.Pool.Exec(ctx, `
		UPDATE projects SET webhook_registered=$2,
			webhook_registered_at = CASE WHEN $2 THEN now() ELSE webhook_registered_at END
		WHERE id=$1`, projectID, ok)
	return err
}
