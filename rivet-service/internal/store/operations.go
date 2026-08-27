package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Эксплуатация установки (change add-operations-management): токены
// регистрации runner'ов и провайдеры модели декомпозиции.

// ErrRevoked — токен уже отозван.
var ErrRevoked = errors.New("токен уже отозван")

// ErrInvalid — некорректный ввод (первое сохранение провайдера без ключа).
var ErrInvalid = errors.New("некорректный запрос")

// ErrUnknownProvider — провайдер модели не из списка поддерживаемых.
var ErrUnknownProvider = errors.New("неизвестный провайдер модели")

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
			tokenID(token), r.ContextChannel, runnerModels(r), runnerStages(r)); err != nil {
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

// tokenID — nil вместо пустого id (runner без токена в тестах).
func tokenID(t domain.RunnerToken) *string {
	if t.ID == "" {
		return nil
	}
	return &t.ID
}
