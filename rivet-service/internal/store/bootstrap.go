package store

import (
	"context"
	"errors"
	"log/slog"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Bootstrap готовит установку к работе (design add-users-and-access, решение 7):
// при пустой таблице пользователей создаёт первого администратора из
// конфигурации (без неё — отказ: установка не может остаться без владельца),
// затем чинит осиротевшие проекты, отдавая их первому активному админу.
func (s *Store) Bootstrap(ctx context.Context, adminLogin, adminPassword string) error {
	n, err := s.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		if adminLogin == "" || adminPassword == "" {
			return errors.New("нет ни одного пользователя: задайте RIVET_ADMIN_LOGIN и RIVET_ADMIN_PASSWORD для bootstrap первого администратора")
		}
		if !domain.ValidLogin(adminLogin) {
			return errors.New("RIVET_ADMIN_LOGIN: латиница, цифры, «._-», длина 1–64")
		}
		u, err := s.CreateUser(ctx, adminLogin, adminLogin, adminPassword, true)
		if err != nil {
			return err
		}
		if _, err := s.AppendEvent(ctx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: "bootstrap", Type: "user.bootstrap",
			Text: "создан первый администратор " + u.Login,
		}); err != nil {
			return err
		}
		slog.Info("bootstrap: создан первый администратор", "login", u.Login)
	}

	var orphans int
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM projects p
		WHERE NOT EXISTS (
			SELECT 1 FROM project_members m JOIN users u ON u.id=m.user_id
			WHERE m.project_id=p.id AND m.role='owner' AND NOT u.disabled)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans == 0 {
		return nil
	}
	ids, err := s.BackfillOrphanProjects(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.AppendEvent(ctx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: "bootstrap", Type: "project.member_backfill",
			ProjectID: id, Text: "проект без активного владельца передан администратору",
		}); err != nil {
			return err
		}
	}
	slog.Warn("bootstrap: осиротевшие проекты переданы администратору", "count", len(ids))
	return nil
}
