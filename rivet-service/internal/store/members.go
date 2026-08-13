package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Членство в проектах — слой видимости данных (спека access-policy
// «Три слоя авторизации», domain-model «Пользователи и членство в проекте»).

// IsMember — принадлежит ли пользователь проекту.
func (s *Store) IsMember(ctx context.Context, projectID, userID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM project_members WHERE project_id=$1 AND user_id=$2)`,
		projectID, userID).Scan(&ok)
	return ok, err
}

func (s *Store) ListMembers(ctx context.Context, projectID string) ([]domain.Member, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT u.login, u.name, m.added_at
		FROM project_members m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=$1 ORDER BY m.added_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Member
	for rows.Next() {
		var m domain.Member
		if err := rows.Scan(&m.Login, &m.Name, &m.Added); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember добавляет участника по login. Неизвестный login — ErrNotFound,
// повторное добавление — ErrConflict.
func (s *Store) AddMember(ctx context.Context, projectID, login string) error {
	var userID string
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM users WHERE login=$1`, login).Scan(&userID); err != nil {
		return nf(err)
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO project_members (project_id, user_id) VALUES ($1,$2)
		ON CONFLICT DO NOTHING`, projectID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s уже участник: %w", login, ErrConflict)
	}
	return nil
}

// RemoveMember удаляет участника; последнего — отказ (проект не может
// остаться без владельца, design решение 6).
func (s *Store) RemoveMember(ctx context.Context, projectID, login string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var userID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE login=$1`, login).Scan(&userID); err != nil {
			return nf(err)
		}
		var members int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM project_members WHERE project_id=$1`, projectID).Scan(&members); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if members <= 1 {
			return ErrLastMember // откатывает транзакцию вместе с DELETE
		}
		return nil
	})
}

// BackfillOrphanProjects даёт проектам без участников первого активного
// админа (переход существующих установок на видимость по членству,
// design решение 7). Возвращает id вылеченных проектов.
func (s *Store) BackfillOrphanProjects(ctx context.Context) ([]string, error) {
	var admin string
	err := s.Pool.QueryRow(ctx, `
		SELECT id FROM users WHERE is_admin AND NOT disabled ORDER BY created_at LIMIT 1`).Scan(&admin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("нет активного администратора для backfill осиротевших проектов")
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		INSERT INTO project_members (project_id, user_id)
		SELECT p.id, $1 FROM projects p
		WHERE NOT EXISTS (SELECT 1 FROM project_members m WHERE m.project_id=p.id)
		RETURNING project_id`, admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
