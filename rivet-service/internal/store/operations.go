package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Эксплуатация установки (change add-operations-management): токены
// регистрации runner'ов и провайдеры модели декомпозиции.

// ErrRevoked — токен уже отозван.
var ErrRevoked = errors.New("токен уже отозван")

// ErrInvalid — некорректный ввод (первое сохранение провайдера без ключа).
var ErrInvalid = errors.New("некорректный запрос")

// ErrUnknownProvider — провайдер модели не из списка поддерживаемых.
var ErrUnknownProvider = errors.New("неизвестный провайдер модели")

// LLMProviders — поддерживаемые провайдеры декомпозиции.
var LLMProviders = []string{"anthropic", "deepseek"}

func knownProvider(p string) bool {
	for _, k := range LLMProviders {
		if k == p {
			return true
		}
	}
	return false
}

// ─── runner tokens ────────────────────────────────────────────────────────

const runnerTokenCols = `t.id, t.name, t.prefix, u.login, t.created_at, t.expires_at, t.last_used_at, t.revoked_at`

func scanRunnerToken(row pgx.Row) (domain.RunnerToken, error) {
	var t domain.RunnerToken
	err := row.Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedBy, &t.Created, &t.ExpiresAt, &t.LastUsed, &t.RevokedAt)
	return t, err
}

// CreateRunnerToken выпускает токен регистрации; секрет возвращается один
// раз, в базе остаётся хэш. Событие runner_token.created — в той же
// транзакции.
func (s *Store) CreateRunnerToken(ctx context.Context, name string, expires *time.Time, creatorID, creatorLogin string) (domain.RunnerToken, string, error) {
	secret, hash := newSecret("rrt_")
	var t domain.RunnerToken
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		t, err = scanRunnerToken(tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO runner_tokens (name, prefix, token_hash, created_by, expires_at)
				VALUES ($1,$2,$3,$4,$5) RETURNING *)
			SELECT `+runnerTokenCols+` FROM ins t JOIN users u ON u.id = t.created_by`,
			name, secret[:12], hash, creatorID, expires))
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: creatorLogin, Type: "runner_token.created",
			Text:    "создан токен регистрации runner'ов «" + name + "»",
			Payload: map[string]any{"token_id": t.ID, "name": name},
		})
		return err
	})
	return t, secret, err
}

// RunnerTokenBySecret проверяет секрет регистрации: неизвестный, отозванный
// и просроченный токены дают один и тот же ErrNotFound (единый отказ по
// спеке). Успех отмечает использование.
func (s *Store) RunnerTokenBySecret(ctx context.Context, secret string) (domain.RunnerToken, error) {
	if !strings.HasPrefix(secret, "rrt_") {
		return domain.RunnerToken{}, ErrNotFound
	}
	t, err := scanRunnerToken(s.Pool.QueryRow(ctx, `
		UPDATE runner_tokens t SET last_used_at = now()
		FROM users u
		WHERE t.token_hash=$1 AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())
		  AND u.id = t.created_by
		RETURNING `+runnerTokenCols, hashSecret(secret)))
	return t, nf(err)
}

func (s *Store) ListRunnerTokens(ctx context.Context) ([]domain.RunnerToken, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+runnerTokenCols+` FROM runner_tokens t JOIN users u ON u.id = t.created_by
		ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RunnerToken
	for rows.Next() {
		t, err := scanRunnerToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeRunnerToken отзывает токен: новые регистрации и переподключения по
// нему отклоняются, открытые соединения не трогаются (design).
func (s *Store) RevokeRunnerToken(ctx context.Context, id, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name string
		var revoked *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT name, revoked_at FROM runner_tokens WHERE id=$1 FOR UPDATE`, id).Scan(&name, &revoked); err != nil {
			return nf(err)
		}
		if revoked != nil {
			return ErrRevoked
		}
		if _, err := tx.Exec(ctx, `UPDATE runner_tokens SET revoked_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "runner_token.revoked",
			Text:    "отозван токен регистрации runner'ов «" + name + "»",
			Payload: map[string]any{"token_id": id, "name": name},
		})
		return err
	})
}

// RegisterRunner — UpsertRunner с фиксацией токена регистрации и событием
// runner.registered в одной транзакции (спека runners «Регистрация
// фиксируется»).
func (s *Store) RegisterRunner(ctx context.Context, r domain.Runner, token domain.RunnerToken) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r = normalizeAdapter(r)
		if _, err := tx.Exec(ctx, upsertRunnerSQL+`, token_id=$8`,
			r.ID, r.Agent, r.Model, r.Host, r.Capabilities, r.Adapter, r.Depth,
			tokenID(token), r.ContextChannel); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorRunner, ActorID: r.ID, Type: "runner.registered",
			Text: "runner " + r.ID + " зарегистрирован (" + r.Agent + ", " + r.Host + ")",
			Payload: map[string]any{
				"agent": r.Agent, "host": r.Host,
				"token_id": token.ID, "token_name": token.Name,
			},
		})
		return err
	})
}

// ─── провайдеры модели ────────────────────────────────────────────────────

const llmCols = `p.provider, p.key_prefix, p.model, p.active, p.state, p.check_detail, p.checked_at, p.updated_at, u.login`

func scanLLM(row pgx.Row) (domain.LLMProvider, error) {
	var p domain.LLMProvider
	err := row.Scan(&p.Provider, &p.KeyPrefix, &p.Model, &p.Active, &p.State, &p.CheckDetail,
		&p.CheckedAt, &p.UpdatedAt, &p.UpdatedBy)
	return p, err
}

// LLMProviderInput — что меняет администратор; nil-поля не трогаются.
type LLMProviderInput struct {
	Provider string
	Key      *string
	Model    *string
	Active   *bool
	// Проверка ключа (если Key задан) выполняется вызывающим до записи.
	State       domain.LLMProviderState
	CheckDetail string
}

// UpsertLLMProvider сохраняет провайдера: ключ шифруется, активность
// переключается на него одного. Первое сохранение без ключа — ErrInvalid.
func (s *Store) UpsertLLMProvider(ctx context.Context, in LLMProviderInput, box *secretbox.Box, actorID, actorLogin string) (domain.LLMProvider, error) {
	if !knownProvider(in.Provider) {
		return domain.LLMProvider{}, ErrUnknownProvider
	}
	var enc []byte
	prefix := ""
	if in.Key != nil {
		if !box.Enabled() {
			return domain.LLMProvider{}, secretbox.ErrNoKey
		}
		var err error
		if enc, err = box.Seal(*in.Key); err != nil {
			return domain.LLMProvider{}, err
		}
		prefix = tokenPrefix(*in.Key)
	}
	var out domain.LLMProvider
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM llm_providers WHERE provider=$1)`, in.Provider).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if in.Key == nil {
				return ErrInvalid
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO llm_providers (provider, key_prefix, key_enc, state, check_detail, checked_at, updated_by)
				VALUES ($1,$2,$3,$4,$5,now(),$6)`,
				in.Provider, prefix, enc, in.State, in.CheckDetail, actorID); err != nil {
				return err
			}
		} else if in.Key != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE llm_providers SET key_prefix=$2, key_enc=$3, state=$4, check_detail=$5, checked_at=now()
				WHERE provider=$1`, in.Provider, prefix, enc, in.State, in.CheckDetail); err != nil {
				return err
			}
		}
		if in.Model != nil {
			if _, err := tx.Exec(ctx, `UPDATE llm_providers SET model=$2 WHERE provider=$1`, in.Provider, *in.Model); err != nil {
				return err
			}
		}
		if in.Active != nil {
			if *in.Active {
				if _, err := tx.Exec(ctx, `UPDATE llm_providers SET active=false WHERE active`); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE llm_providers SET active=$2 WHERE provider=$1`, in.Provider, *in.Active); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE llm_providers SET updated_at=now(), updated_by=$2 WHERE provider=$1`, in.Provider, actorID); err != nil {
			return err
		}
		var err error
		out, err = scanLLM(tx.QueryRow(ctx,
			`SELECT `+llmCols+` FROM llm_providers p JOIN users u ON u.id=p.updated_by WHERE p.provider=$1`, in.Provider))
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "llm_provider.updated",
			Text:    "изменён провайдер модели " + in.Provider,
			Payload: map[string]any{"provider": in.Provider, "active": out.Active, "state": out.State},
		})
		return err
	})
	return out, err
}

// SetLLMProviderCheck записывает результат повторной проверки ключа.
func (s *Store) SetLLMProviderCheck(ctx context.Context, provider string, state domain.LLMProviderState, detail string) (domain.LLMProvider, error) {
	p, err := scanLLM(s.Pool.QueryRow(ctx, `
		WITH up AS (
			UPDATE llm_providers SET state=$2, check_detail=$3, checked_at=now() WHERE provider=$1 RETURNING *)
		SELECT `+llmCols+` FROM up p JOIN users u ON u.id=p.updated_by`, provider, state, detail))
	return p, nf(err)
}

func (s *Store) ListLLMProviders(ctx context.Context) ([]domain.LLMProvider, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT `+llmCols+` FROM llm_providers p JOIN users u ON u.id=p.updated_by ORDER BY p.provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LLMProvider
	for rows.Next() {
		p, err := scanLLM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LLMProviderKey расшифровывает ключ провайдера (для проверки и планировщика).
func (s *Store) LLMProviderKey(ctx context.Context, provider string, box *secretbox.Box) (string, error) {
	var enc []byte
	if err := s.Pool.QueryRow(ctx, `SELECT key_enc FROM llm_providers WHERE provider=$1`, provider).Scan(&enc); err != nil {
		return "", nf(err)
	}
	return box.Open(enc)
}

// ActiveLLMProvider — активный провайдер с расшифрованным ключом; ErrNotFound,
// если в базе активного нет (тогда планировщик берётся из окружения).
func (s *Store) ActiveLLMProvider(ctx context.Context, box *secretbox.Box) (domain.LLMProvider, string, error) {
	var enc []byte
	var p domain.LLMProvider
	err := s.Pool.QueryRow(ctx, `
		SELECT `+llmCols+`, p.key_enc FROM llm_providers p JOIN users u ON u.id=p.updated_by WHERE p.active`).
		Scan(&p.Provider, &p.KeyPrefix, &p.Model, &p.Active, &p.State, &p.CheckDetail,
			&p.CheckedAt, &p.UpdatedAt, &p.UpdatedBy, &enc)
	if err != nil {
		return p, "", nf(err)
	}
	key, err := box.Open(enc)
	return p, key, err
}

// DeleteLLMProvider удаляет провайдера вместе с ключом.
func (s *Store) DeleteLLMProvider(ctx context.Context, provider, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM llm_providers WHERE provider=$1`, provider)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "llm_provider.removed",
			Text:    "удалён провайдер модели " + provider,
			Payload: map[string]any{"provider": provider},
		})
		return err
	})
}

// tokenID — nil вместо пустого id (runner без токена в тестах).
func tokenID(t domain.RunnerToken) *string {
	if t.ID == "" {
		return nil
	}
	return &t.ID
}
