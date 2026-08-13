package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Аутентификация и учётные записи (спека access-policy «Аутентификация
// пользователей», design add-users-and-access, решения 1–3, 11).

// ErrLastAdmin — нельзя деактивировать последнего активного администратора.
var ErrLastAdmin = errors.New("нельзя деактивировать последнего администратора")

// ErrLastMember — проект не может остаться без участников.
var ErrLastMember = errors.New("нельзя удалить последнего участника проекта")

// ErrConflict — нарушение уникальности (занятый login, повторное членство).
var ErrConflict = errors.New("conflict")

// SessionTTL — скользящий срок жизни сессии консоли.
const SessionTTL = 7 * 24 * time.Hour

const userCols = `id, login, name, is_admin, disabled, created_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Login, &u.Name, &u.Admin, &u.Disabled, &u.Created)
	return u, err
}

// ─── users ───────────────────────────────────────────────────────────────

func (s *Store) CreateUser(ctx context.Context, login, name, password string, admin bool) (domain.User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return domain.User{}, err
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, `
		INSERT INTO users (login, name, password_hash, is_admin)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (login) DO NOTHING
		RETURNING `+userCols, login, name, string(hash), admin))
	if errors.Is(err, pgx.ErrNoRows) {
		return u, fmt.Errorf("login %q: %w", login, ErrConflict)
	}
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// Authenticate проверяет пару логин/пароль. Неверный логин и неверный пароль
// неразличимы для вызывающего (единый ErrNotFound); деактивированный
// пользователь тоже не проходит.
func (s *Store) Authenticate(ctx context.Context, login, password string) (domain.User, error) {
	var u domain.User
	var hash string
	err := s.Pool.QueryRow(ctx, `
		SELECT `+userCols+`, password_hash FROM users WHERE login=$1 AND NOT disabled`, login).
		Scan(&u.ID, &u.Login, &u.Name, &u.Admin, &u.Disabled, &u.Created, &hash)
	if err != nil {
		// Выравниваем стоимость ответа: bcrypt считается и для неизвестного логина.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0xM1I8PXlOD7rD1JZ8mSyyzXwEq"), []byte(password))
		return domain.User{}, ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return domain.User{}, ErrNotFound
	}
	return u, nil
}

// SetUserState правит name/disabled. Деактивация в той же транзакции удаляет
// сессии и PAT (реактивация их не воскрешает — design, решение 11) и
// отклоняется для последнего активного администратора.
func (s *Store) SetUserState(ctx context.Context, id string, name *string, disabled *bool) (domain.User, error) {
	var u domain.User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return nf(err)
		}
		if name != nil {
			u.Name = *name
		}
		if disabled != nil && *disabled && !u.Disabled {
			if u.Admin {
				var admins int
				if err := tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM users WHERE is_admin AND NOT disabled AND id <> $1`, id).Scan(&admins); err != nil {
					return err
				}
				if admins == 0 {
					return ErrLastAdmin
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM access_tokens WHERE user_id=$1`, id); err != nil {
				return err
			}
		}
		if disabled != nil {
			u.Disabled = *disabled
		}
		_, err = tx.Exec(ctx, `UPDATE users SET name=$2, disabled=$3 WHERE id=$1`, id, u.Name, u.Disabled)
		return err
	})
	return u, err
}

// ─── секреты ─────────────────────────────────────────────────────────────

func newSecret(prefix string) (secret, hash string) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand не отдаёт ошибок на поддерживаемых платформах
	}
	secret = prefix + hex.EncodeToString(raw)
	return secret, hashSecret(secret)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ─── сессии консоли ──────────────────────────────────────────────────────

func (s *Store) CreateAuthSession(ctx context.Context, userID string) (secret string, err error) {
	secret, hash := newSecret("rvs_")
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO auth_sessions (user_id, token_hash, expires_at) VALUES ($1,$2,now()+$3)`,
		userID, hash, SessionTTL)
	return secret, err
}

// UserBySession возвращает пользователя живой сессии и продлевает её
// (скользящий TTL). Истёкшая, отозванная или чужая сессия — ErrNotFound.
func (s *Store) UserBySession(ctx context.Context, secret string) (domain.User, error) {
	u, err := scanUser(s.Pool.QueryRow(ctx, `
		UPDATE auth_sessions a SET expires_at = now()+$2
		FROM users u
		WHERE a.token_hash=$1 AND a.expires_at > now() AND u.id=a.user_id AND NOT u.disabled
		RETURNING `+prefixCols("u"), hashSecret(secret), SessionTTL))
	return u, nf(err)
}

func (s *Store) DeleteAuthSession(ctx context.Context, secret string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM auth_sessions WHERE token_hash=$1`, hashSecret(secret))
	return err
}

// ─── personal access tokens ──────────────────────────────────────────────

func (s *Store) CreateAccessToken(ctx context.Context, userID, name string, expires *time.Time) (domain.AccessToken, string, error) {
	secret, hash := newSecret("rvt_")
	t := domain.AccessToken{Name: name, Prefix: secret[:12], ExpiresAt: expires}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO access_tokens (user_id, name, prefix, token_hash, expires_at)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		userID, name, t.Prefix, hash, expires).Scan(&t.ID, &t.Created)
	return t, secret, err
}

// UserByAccessToken аутентифицирует Bearer-секрет и отмечает использование.
func (s *Store) UserByAccessToken(ctx context.Context, secret string) (domain.User, error) {
	if !strings.HasPrefix(secret, "rvt_") {
		return domain.User{}, ErrNotFound
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, `
		UPDATE access_tokens t SET last_used_at = now()
		FROM users u
		WHERE t.token_hash=$1 AND (t.expires_at IS NULL OR t.expires_at > now())
		  AND u.id=t.user_id AND NOT u.disabled
		RETURNING `+prefixCols("u"), hashSecret(secret)))
	return u, nf(err)
}

func (s *Store) ListAccessTokens(ctx context.Context, userID string) ([]domain.AccessToken, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, prefix, created_at, expires_at, last_used_at
		FROM access_tokens WHERE user_id=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AccessToken
	for rows.Next() {
		var t domain.AccessToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Created, &t.ExpiresAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAccessToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM access_tokens WHERE id=$1 AND user_id=$2`, tokenID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// prefixCols — userCols с префиксом таблицы для join-запросов.
func prefixCols(t string) string {
	cols := strings.Split(userCols, ", ")
	for i, c := range cols {
		cols[i] = t + "." + c
	}
	return strings.Join(cols, ", ")
}
