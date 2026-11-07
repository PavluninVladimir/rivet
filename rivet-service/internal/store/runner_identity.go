package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Привязка токена регистрации к идентичности runner'а (change
// add-runner-identity, спека runners «Токены регистрации runner'ов»).

// ErrRunnerIdentity: действующий токен не выпущен для этого runner'а.
// Подробную причину несёт *IdentityError: она идёт в событие отказа, а
// наружу runner'у уходит единый текст без списка runner'ов токена.
var ErrRunnerIdentity = errors.New("токен регистрации не выпущен для этого runner'а")

// IdentityError: отказ по идентичности с причиной для event log.
type IdentityError struct{ Reason string }

func (e *IdentityError) Error() string        { return e.Reason }
func (e *IdentityError) Is(target error) bool { return target == ErrRunnerIdentity }

// AuthorizeRunner проверяет, что токен tokenID выпущен для runner'а
// runnerID. firstUse разрешает привязать токен без привязок к этому
// runner'у (регистрация); канал его не привязывает. Привязка при первой
// регистрации атомарна: строка токена блокируется, идентификатор runner'а
// берётся advisory-блокировкой, а занятый другим действующим токеном
// идентификатор не забирается. Отозванный или истёкший к этому моменту
// токен даёт ErrNotFound. bound: токен привязан этим вызовом.
func (s *Store) AuthorizeRunner(ctx context.Context, tokenID, runnerID string, firstUse bool) (bound bool, err error) {
	ok, empty, err := tokenBinding(ctx, s.Pool, tokenID, runnerID)
	if err != nil {
		return false, err
	}
	switch {
	case ok:
		return false, nil
	case !empty:
		return false, &IdentityError{Reason: "токен выпущен для других runner'ов"}
	case !firstUse:
		return false, &IdentityError{Reason: "токен ещё не привязан: канал открывается только после регистрации"}
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Под блокировкой токен перепроверяется: его могли отозвать или он
		// мог истечь после проверки секрета в интерцепторе. Недействующий
		// токен даёт ErrNotFound, то есть единый отказ по токену.
		var name string
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT name, revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
			FROM runner_tokens WHERE id=$1 FOR UPDATE`, tokenID).Scan(&name, &active); err != nil {
			return nf(err)
		}
		if !active {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rivet:runner-binding:' || $1, 0))`, runnerID); err != nil {
			return err
		}
		// Повторно под блокировкой: параллельная первая регистрация могла
		// успеть привязать токен.
		ok, empty, err := tokenBinding(ctx, tx, tokenID, runnerID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !empty {
			return &IdentityError{Reason: "токен выпущен для других runner'ов"}
		}
		var other string
		err = tx.QueryRow(ctx, `
			SELECT t.name FROM runner_token_bindings b JOIN runner_tokens t ON t.id = b.token_id
			WHERE b.runner_id=$1 AND b.token_id<>$2 AND t.revoked_at IS NULL
			  AND (t.expires_at IS NULL OR t.expires_at > now())
			ORDER BY b.bound_at LIMIT 1`, runnerID, tokenID).Scan(&other)
		if err == nil {
			return &IdentityError{Reason: "runner " + runnerID + " привязан к другому действующему токену «" + other + "»"}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO runner_token_bindings (token_id, runner_id, source) VALUES ($1,$2,'first_use')`,
			tokenID, runnerID); err != nil {
			return err
		}
		bound = true
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "runner_token.bound",
			Text:    "токен регистрации «" + name + "» привязан к runner'у " + runnerID + " при первой регистрации",
			Payload: map[string]any{"token_id": tokenID, "token_name": name, "runner_id": runnerID},
		})
		return err
	})
	if err != nil {
		bound = false
	}
	return bound, err
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// tokenBinding: выпущен ли токен для runner'а (ok) и нет ли у токена
// привязок вовсе (empty).
func tokenBinding(ctx context.Context, q queryRower, tokenID, runnerID string) (ok, empty bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT COALESCE(bool_or(runner_id = $2), false), count(*) = 0
		FROM runner_token_bindings WHERE token_id=$1`, tokenID, runnerID).Scan(&ok, &empty)
	return ok, empty, err
}
