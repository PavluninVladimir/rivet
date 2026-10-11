package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Членство в проектах — слой видимости данных (спека access-policy
// «Три слоя авторизации», domain-model «Пользователи и членство в проекте»).

// ErrLastOwner — проект не может остаться без активного владельца.
var ErrLastOwner = errors.New("нельзя оставить проект без владельца")

// lockProjects сериализует по проектам операции, меняющие число активных
// владельцев: правку членства (роль, исключение) и деактивацию пользователя.
// Advisory-блокировка транзакции берётся в порядке возрастания id, поэтому
// deadlock невозможен, а операции не зависят от того, какие строки каких
// таблиц каждая из них читает: конкурент дожидается коммита и видит уже
// новые данные (иначе снимок users в READ COMMITTED мог бы остаться старым).
func lockProjects(ctx context.Context, tx pgx.Tx, ids []string) error {
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, id); err != nil {
			return err
		}
	}
	return nil
}

// lockUserByLogin блокирует учётную запись на чтение: параллельная
// деактивация (FOR UPDATE той же строки) дождётся конца этой транзакции, а
// значит пользователь не станет владельцем проекта в момент отключения.
func lockUserByLogin(ctx context.Context, tx pgx.Tx, login string) (id string, disabled bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT id, disabled FROM users WHERE login=$1 FOR SHARE`, login).Scan(&id, &disabled)
	return id, disabled, nf(err)
}

// IsMember — принадлежит ли пользователь проекту.
func (s *Store) IsMember(ctx context.Context, projectID, userID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM project_members WHERE project_id=$1 AND user_id=$2)`,
		projectID, userID).Scan(&ok)
	return ok, err
}

// MemberRole — роль пользователя в проекте; не участник — ErrNotFound.
func (s *Store) MemberRole(ctx context.Context, projectID, userID string) (string, error) {
	var role string
	err := s.Pool.QueryRow(ctx,
		`SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2`,
		projectID, userID).Scan(&role)
	return role, nf(err)
}

// EpicForViewer — epic, только если viewer участник его проекта. Один запрос:
// «нет объекта» и «объект чужой» неотличимы ни по ответу, ни по таймингу.
func (s *Store) EpicForViewer(ctx context.Context, epicID, viewerID string) (domain.Epic, error) {
	var e domain.Epic
	err := s.Pool.QueryRow(ctx, `
		SELECT e.id, e.project_id, e.title, e.goal, e.status, e.token_budget, COALESCE(e.source_key,''), e.created_at FROM epics e
		JOIN project_members m ON m.project_id = e.project_id AND m.user_id = $2
		WHERE e.id = $1`, epicID, viewerID).
		Scan(&e.ID, &e.ProjectID, &e.Title, &e.Goal, &e.Status, &e.TokenBudget, &e.SourceKey, &e.Created)
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
		SELECT u.login, u.name, m.role, m.added_at
		FROM project_members m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=$1 ORDER BY m.added_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Member
	for rows.Next() {
		var m domain.Member
		if err := rows.Scan(&m.Login, &m.Name, &m.Role, &m.Added); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember добавляет участника по login с ролью. Неизвестный login —
// ErrNotFound, повторное добавление — ErrConflict. Отключённого пользователя
// в проект не берём: иначе проект мог бы получить владельца, который не может
// войти. Сериализуется с деактивацией и правкой ролей через lockProjects.
func (s *Store) AddMember(ctx context.Context, projectID, login, role string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Порядок блокировок общий для всех путей: сначала строка users
		// (FOR SHARE — деактивация ждёт на FOR UPDATE), затем проект. Иначе
		// участник мог бы попасть в проект в момент собственной деактивации.
		userID, disabled, err := lockUserByLogin(ctx, tx, login)
		if err != nil {
			return err
		}
		if disabled {
			return fmt.Errorf("%s отключён: %w", login, ErrConflict)
		}
		if err := lockProjects(ctx, tx, []string{projectID}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,$3)
			ON CONFLICT DO NOTHING`, projectID, userID, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%s уже участник: %w", login, ErrConflict)
		}
		return nil
	})
}

// SetMemberRole меняет роль участника; понижение последнего активного
// владельца — отказ (спека domain-model «Проект не остаётся без owner»).
func (s *Store) SetMemberRole(ctx context.Context, projectID, login, role string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		userID, owners, member, err := lockMembers(ctx, tx, projectID, login)
		if err != nil {
			return err
		}
		if member.role == role {
			return nil
		}
		// Отключённый владелец не может войти: повышать его бессмысленно и
		// опасно (проект формально с владельцем, фактически без него).
		if role == domain.RoleOwner && member.disabled {
			return fmt.Errorf("%s отключён: %w", login, ErrConflict)
		}
		if role != domain.RoleOwner && member.activeOwner && owners <= 1 {
			return ErrLastOwner
		}
		_, err = tx.Exec(ctx,
			`UPDATE project_members SET role=$3 WHERE project_id=$1 AND user_id=$2`,
			projectID, userID, role)
		return err
	})
}

// RemoveMember удаляет участника; последнего участника и последнего активного
// владельца — отказ (design add-users-and-access решение 6, спека
// domain-model). Сериализуется по проекту: параллельные удаления не могут
// вдвоём пройти проверку.
func (s *Store) RemoveMember(ctx context.Context, projectID, login string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		userID, owners, member, err := lockMembers(ctx, tx, projectID, login)
		if err != nil {
			return err
		}
		if member.total <= 1 {
			return ErrLastMember
		}
		if member.activeOwner && owners <= 1 {
			return ErrLastOwner
		}
		_, err = tx.Exec(ctx,
			`DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID)
		return err
	})
}

// memberState — состояние изменяемого участника внутри транзакции.
type memberState struct {
	role        string
	activeOwner bool
	disabled    bool
	total       int
}

// lockMembers сериализует правку членства по проекту (lockProjects) и
// возвращает id участника, число активных владельцев и его собственное
// состояние. Отключённые пользователи владельцами не считаются: проект не
// должен остаться с владельцем, который не может войти.
func lockMembers(ctx context.Context, tx pgx.Tx, projectID, login string) (string, int, memberState, error) {
	// Тот же порядок, что и в AddMember: строка users, затем проект.
	userID, _, err := lockUserByLogin(ctx, tx, login)
	if err != nil {
		return "", 0, memberState{}, err
	}
	if err := lockProjects(ctx, tx, []string{projectID}); err != nil {
		return "", 0, memberState{}, err
	}
	rows, err := tx.Query(ctx, `
		SELECT m.user_id, m.role, u.disabled
		FROM project_members m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=$1 ORDER BY m.user_id`, projectID)
	if err != nil {
		return "", 0, memberState{}, err
	}
	var owners int
	var st memberState
	found := false
	for rows.Next() {
		var id, role string
		var disabled bool
		if err := rows.Scan(&id, &role, &disabled); err != nil {
			rows.Close()
			return "", 0, memberState{}, err
		}
		st.total++
		if role == domain.RoleOwner && !disabled {
			owners++
		}
		if id == userID {
			found = true
			st.role = role
			st.disabled = disabled
			st.activeOwner = role == domain.RoleOwner && !disabled
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", 0, memberState{}, err
	}
	if !found {
		return "", 0, memberState{}, ErrNotFound
	}
	return userID, owners, st, nil
}

// BackfillOrphanProjects даёт первого активного админа проектам, которыми
// некому управлять: без участников вовсе (переход установок на видимость по
// членству, design add-users-and-access решение 7) и без активного владельца
// (например, все владельцы отключены на данных до появления ролей).
// Возвращает id вылеченных проектов.
func (s *Store) BackfillOrphanProjects(ctx context.Context) ([]string, error) {
	var ids []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Общий ключ сериализации на транзакцию: два инстанса rivetd на одной
		// базе не чинят одни и те же проекты одновременно (сессионный
		// pg_advisory_lock на пуле брался бы и снимался на разных соединениях).
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('rivet:backfill-owners', 0))`); err != nil {
			return err
		}
		// FOR SHARE, как и остальные пути, делающие пользователя владельцем:
		// иначе параллельная деактивация могла бы отключить выбранного админа
		// уже после выбора, и проект получил бы владельца, который не войдёт.
		var admin string
		err := tx.QueryRow(ctx, `
			SELECT id FROM users WHERE is_admin AND NOT disabled
			ORDER BY created_at LIMIT 1 FOR SHARE`).Scan(&admin)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("нет активного администратора для backfill осиротевших проектов")
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			INSERT INTO project_members (project_id, user_id, role)
			SELECT p.id, $1, 'owner' FROM projects p
			WHERE NOT EXISTS (
				SELECT 1 FROM project_members m JOIN users u ON u.id=m.user_id
				WHERE m.project_id=p.id AND m.role='owner' AND NOT u.disabled)
			ON CONFLICT (project_id, user_id) DO UPDATE SET role='owner'
			RETURNING project_id`, admin)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}
