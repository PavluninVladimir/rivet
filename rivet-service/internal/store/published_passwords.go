package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/domain"
)

// FlagPublishedPasswords находит активных администраторов с паролем из
// документации и примеров Rivet (спека access-policy «Безопасные умолчания
// установки»). Так бывает у установки, поднятой по старому
// docker-compose.yml с admin-local. Им ставится обязательная смена пароля
// при следующем входе, в event log пишется событие без пароля. Возвращает
// логины, которым признак поставлен сейчас: уже отмеченных не трогает.
func (s *Store) FlagPublishedPasswords(ctx context.Context) ([]string, error) {
	type admin struct{ id, login, hash string }
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, login, password_hash FROM users
		WHERE is_admin AND NOT disabled AND NOT must_change_password ORDER BY login`)
	if err != nil {
		return nil, err
	}
	var admins []admin
	for rows.Next() {
		var a admin
		if err := rows.Scan(&a.id, &a.login, &a.hash); err != nil {
			rows.Close()
			return nil, err
		}
		admins = append(admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	published := config.PublishedAdminPasswords()
	var flagged []string
	for _, a := range admins {
		match := false
		for _, pw := range published {
			if bcrypt.CompareHashAndPassword([]byte(a.hash), []byte(pw)) == nil {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE users SET must_change_password=true WHERE id=$1 AND NOT must_change_password`, a.id)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorSystem, ActorID: "bootstrap", Type: "user.password_published",
				Text:    "у администратора " + a.login + " пароль из документации Rivet: при следующем входе нужна смена пароля",
				Payload: map[string]any{"login": a.login},
			}); err != nil {
				return err
			}
			flagged = append(flagged, a.login)
			return nil
		})
		if err != nil {
			return flagged, err
		}
	}
	return flagged, nil
}
