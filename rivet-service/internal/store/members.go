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

// EpicForViewer — epic, только если viewer участник его проекта. Один запрос:
// «нет объекта» и «объект чужой» неотличимы ни по ответу, ни по таймингу.
func (s *Store) EpicForViewer(ctx context.Context, epicID, viewerID string) (domain.Epic, error) {
	var e domain.Epic
	err := s.Pool.QueryRow(ctx, `
		SELECT e.id, e.project_id, e.title, e.goal, e.status, e.created_at FROM epics e
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE e.id = $1`, epicID, viewerID).
		Scan(&e.ID, &e.ProjectID, &e.Title, &e.Goal, &e.Status, &e.Created)
	return e, nf(err)
}

// TaskProjectForViewer — проект задачи, только если viewer его участник.
func (s *Store) TaskProjectForViewer(ctx context.Context, taskID, viewerID string) (string, error) {
	var projectID string
	err := s.Pool.QueryRow(ctx, `
		SELECT e.project_id FROM tasks t
		JOIN epics e ON e.id = t.epic_id
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE t.id = $1`, taskID, viewerID).Scan(&projectID)
	return projectID, nf(err)
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
// остаться без владельца, design решение 6). Строки участников блокируются
// FOR UPDATE: параллельные удаления сериализуются и не могут вдвоём пройти
// проверку «не последний».
func (s *Store) RemoveMember(ctx context.Context, projectID, login string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var userID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE login=$1`, login).Scan(&userID); err != nil {
			return nf(err)
		}
		rows, err := tx.Query(ctx,
			`SELECT user_id FROM project_members WHERE project_id=$1 FOR UPDATE`, projectID)
		if err != nil {
			return err
		}
		members := 0
		found := false
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			members++
			found = found || id == userID
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if members <= 1 {
			return ErrLastMember
		}
		_, err = tx.Exec(ctx,
			`DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID)
		return err
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
