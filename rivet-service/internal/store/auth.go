package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
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

// ErrWeakPassword — пароль короче минимальной длины. Проверка живёт здесь,
// чтобы её проходили все пути задания пароля, включая bootstrap.
var ErrWeakPassword = fmt.Errorf("пароль короче %d символов", domain.MinPasswordLen)

// ErrSamePassword — новый пароль совпадает с текущим.
var ErrSamePassword = errors.New("новый пароль совпадает с текущим")

// ErrBadPassword — неверный текущий пароль при смене.
var ErrBadPassword = errors.New("неверный текущий пароль")

// SessionTTL — скользящий срок жизни сессии консоли.
const SessionTTL = 7 * 24 * time.Hour

const userCols = `id, login, name, is_admin, disabled, created_at, must_change_password`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Login, &u.Name, &u.Admin, &u.Disabled, &u.Created, &u.MustChangePassword)
	return u, err
}

// hashPassword проверяет длину и считает bcrypt-хэш.
func hashPassword(password string) (string, error) {
	if len([]rune(password)) < domain.MinPasswordLen {
		return "", ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// ─── users ───────────────────────────────────────────────────────────────

func (s *Store) CreateUser(ctx context.Context, login, name, password string, admin bool) (domain.User, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return domain.User{}, err
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, `
		INSERT INTO users (login, name, password_hash, is_admin)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (login) DO NOTHING
		RETURNING `+userCols, login, name, hash, admin))
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

// GetUser — учётная запись по id; неизвестный id — ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id string) (domain.User, error) {
	u, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
	return u, nf(err)
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// EqualizeAuthCost сжигает стоимость bcrypt-проверки, не проверяя ничего:
// ветки отказа (неизвестный логин, окно backoff) не отличимы по таймингу
// от честной проверки пароля.
func EqualizeAuthCost(password string) {
	_ = bcrypt.CompareHashAndPassword(
		[]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0xM1I8PXlOD7rD1JZ8mSyyzXwEq"), []byte(password))
}

// Authenticate проверяет пару логин/пароль. Неверный логин и неверный пароль
// неразличимы для вызывающего (единый ErrNotFound); деактивированный
// пользователь тоже не проходит.
func (s *Store) Authenticate(ctx context.Context, login, password string) (domain.User, error) {
	var u domain.User
	var hash string
	err := s.Pool.QueryRow(ctx, `
		SELECT `+userCols+`, password_hash FROM users WHERE login=$1 AND NOT disabled`, login).
		Scan(&u.ID, &u.Login, &u.Name, &u.Admin, &u.Disabled, &u.Created, &u.MustChangePassword, &hash)
	if err != nil {
		EqualizeAuthCost(password)
		return domain.User{}, ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return domain.User{}, ErrNotFound
	}
	return u, nil
}

// SetUserState правит name/disabled/is_admin. Деактивация в той же транзакции
// удаляет сессии и PAT (реактивация их не воскрешает — design
// add-users-and-access, решение 11). Установка не остаётся без администратора:
// последнему активному админу нельзя ни снять права, ни деактивировать его.
// События аудита пишутся здесь же, в транзакции изменения: сравнение «до и
// после» вне транзакции приписало бы этому запросу чужую параллельную правку.
func (s *Store) SetUserState(ctx context.Context, id string, name *string, disabled, admin *bool, actor string) (domain.User, error) {
	var u domain.User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Операция, способная лишить установку админа, блокирует всех активных
		// админов в детерминированном порядке (ORDER BY id): параллельные
		// вызовы сериализуются без deadlock'а и не могут вдвоём пройти
		// проверку «остался другой активный админ».
		var activeAdmins []string
		losesAdmin := (disabled != nil && *disabled) || (admin != nil && !*admin)
		if losesAdmin {
			rows, err := tx.Query(ctx,
				`SELECT id FROM users WHERE is_admin AND NOT disabled ORDER BY id FOR UPDATE`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var a string
				if err := rows.Scan(&a); err != nil {
					rows.Close()
					return err
				}
				activeAdmins = append(activeAdmins, a)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return nf(err)
		}
		wasActiveAdmin := u.Admin && !u.Disabled
		wasDisabled := u.Disabled
		if name != nil {
			u.Name = *name
		}
		deactivating := disabled != nil && *disabled && !u.Disabled
		demoting := admin != nil && !*admin && u.Admin
		if wasActiveAdmin && (deactivating || demoting) {
			others := 0
			for _, a := range activeAdmins {
				if a != id {
					others++
				}
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		if deactivating {
			// Список проектов собирается уже под блокировкой строки users:
			// пути, делающие пользователя владельцем (создание проекта,
			// AddMember, повышение роли), берут эту же строку FOR SHARE, так
			// что новых проектов после этой точки не появится.
			rows, err := tx.Query(ctx,
				`SELECT project_id FROM project_members WHERE user_id=$1`, id)
			if err != nil {
				return err
			}
			var projects []string
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					rows.Close()
					return err
				}
				projects = append(projects, p)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			// Сериализация с правкой членства этих проектов: тот же
			// lockProjects берут SetMemberRole, RemoveMember и AddMember.
			if err := lockProjects(ctx, tx, projects); err != nil {
				return err
			}
			// Проект не должен остаться без владельца, который может войти:
			// деактивация единственного активного owner отклоняется так же,
			// как его исключение (спека domain-model).
			var orphaned int
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM project_members m
				WHERE m.user_id=$1 AND m.role='owner'
				  AND NOT EXISTS (
					SELECT 1 FROM project_members o JOIN users u ON u.id=o.user_id
					WHERE o.project_id=m.project_id AND o.role='owner'
					  AND o.user_id<>$1 AND NOT u.disabled)`, id).Scan(&orphaned); err != nil {
				return err
			}
			if orphaned > 0 {
				return ErrLastOwner
			}
			if _, err := tx.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM access_tokens WHERE user_id=$1`, id); err != nil {
				return err
			}
		}
		promoting := admin != nil && *admin && !u.Admin
		if disabled != nil {
			u.Disabled = *disabled
		}
		if admin != nil {
			u.Admin = *admin
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET name=$2, disabled=$3, is_admin=$4 WHERE id=$1`,
			id, u.Name, u.Disabled, u.Admin); err != nil {
			return err
		}
		if demoting || promoting {
			text := "сняты права администратора у " + u.Login
			if u.Admin {
				text = "выданы права администратора " + u.Login
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: actor, Type: "user.admin_changed",
				Text: text, Payload: map[string]any{"login": u.Login, "admin": u.Admin},
			}); err != nil {
				return err
			}
		}
		if disabled != nil && *disabled != wasDisabled {
			text := "включён пользователь " + u.Login
			if u.Disabled {
				text = "отключён пользователь " + u.Login
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: actor, Type: "user.state_changed",
				Text: text, Payload: map[string]any{"login": u.Login, "disabled": u.Disabled},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return u, err
}

// ChangePassword меняет пароль владельцем учётной записи: проверяет текущий,
// снимает признак обязательной смены и завершает остальные сессии, оставляя
// текущую (keepSession — секрет cookie вызывающего, пустой при Bearer).
// PAT сохраняются: смена пароля не должна гасить автоматику (design).
func (s *Store) ChangePassword(ctx context.Context, id, current, next, keepSession string) error {
	hash, err := hashPassword(next)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var stored string
		if err := tx.QueryRow(ctx,
			`SELECT password_hash FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&stored); err != nil {
			return nf(err)
		}
		if bcrypt.CompareHashAndPassword([]byte(stored), []byte(current)) != nil {
			return ErrBadPassword
		}
		if bcrypt.CompareHashAndPassword([]byte(stored), []byte(next)) == nil {
			return ErrSamePassword
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET password_hash=$2, must_change_password=false WHERE id=$1`, id, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM auth_sessions WHERE user_id=$1 AND token_hash <> $2`, id, hashSecret(keepSession))
		return err
	})
}

// ResetPassword выдаёт одноразовый пароль вместо текущего: поднимает признак
// обязательной смены и завершает сессии пользователя. PAT сохраняются —
// полный отзыв доступа делается деактивацией (design). Событие аудита пишется
// в той же транзакции: иначе сброс мог бы состояться, а ответ с единственной
// копией пароля — потеряться на ошибке записи события.
func (s *Store) ResetPassword(ctx context.Context, id, actor string) (login, password string, err error) {
	password = newPassword()
	hash, err := hashPassword(password)
	if err != nil {
		return "", "", err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			UPDATE users SET password_hash=$2, must_change_password=true
			WHERE id=$1 RETURNING login`, id, hash).Scan(&login); err != nil {
			return nf(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id=$1`, id); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actor, Type: "user.password_reset",
			Text: "сброшен пароль пользователя " + login, Payload: map[string]any{"login": login},
		})
		return err
	})
	if err != nil {
		return "", "", err
	}
	return login, password, nil
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

// newPassword — одноразовый пароль после сброса администратором: показывается
// один раз в ответе и нигде не хранится в открытом виде.
func newPassword() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand не отдаёт ошибок на поддерживаемых платформах
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
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
