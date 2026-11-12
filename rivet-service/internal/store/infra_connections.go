package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Подключения к провайдерам инфраструктуры (add-environment-provisioning,
// спека deployment «Подключения к провайдерам инфраструктуры»): токен в
// secretbox, наружу только префикс, аудит установки без значения токена.

// InfraConnInput: что сохраняет администратор. Token: nil: не менять.
type InfraConnInput struct {
	ID       string
	Provider string
	Name     string
	Token    *string
	Enabled  *bool
}

const infraConnCols = `c.id, c.provider, c.name, c.token_prefix, c.token_enc IS NOT NULL, c.enabled, c.state,
	c.check_detail, c.checked_at, c.balance_amount, c.balance_currency, c.updated_at, u.login`

func scanInfraConn(row pgx.Row) (domain.InfraConnection, error) {
	var c domain.InfraConnection
	err := row.Scan(&c.ID, &c.Provider, &c.Name, &c.TokenPrefix, &c.HasToken, &c.Enabled, &c.State,
		&c.CheckDetail, &c.CheckedAt, &c.BalanceAmount, &c.BalanceCurrency, &c.UpdatedAt, &c.UpdatedBy)
	return c, err
}

// UpsertInfraConnection создаёт или меняет подключение. Проверку у
// провайдера делает вызывающий после сохранения.
func (s *Store) UpsertInfraConnection(ctx context.Context, in InfraConnInput, box *secretbox.Box, actorID, actorLogin string) (domain.InfraConnection, bool, error) {
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case !domain.ValidInfraConnID(in.ID):
		return domain.InfraConnection{}, false, &FieldError{Field: "id", Msg: "идентификатор: строчная латиница, цифры и дефис"}
	case in.Provider != domain.ProvisionRUVDS:
		return domain.InfraConnection{}, false, &FieldError{Field: "provider", Msg: "провайдер: ruvds"}
	case in.Name == "" || len(in.Name) > 200:
		return domain.InfraConnection{}, false, &FieldError{Field: "name", Msg: "нужно название до 200 символов"}
	}
	if in.Token != nil {
		t := strings.TrimSpace(*in.Token)
		if t == "" || len(t) > 4096 || strings.ContainsAny(t, " \t\r\n") {
			return domain.InfraConnection{}, false, &FieldError{Field: "token", Msg: "нужен токен API без пробелов"}
		}
		in.Token = &t
		if !box.Enabled() {
			return domain.InfraConnection{}, false, secretbox.ErrNoKey
		}
	}
	var out domain.InfraConnection
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var exists bool
		var provider string
		err := tx.QueryRow(ctx, `SELECT true, provider FROM infra_connections WHERE id=$1 FOR UPDATE`, in.ID).Scan(&exists, &provider)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !exists {
			if in.Token == nil {
				return &FieldError{Field: "token", Msg: "нужен токен API провайдера"}
			}
			created = true
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO infra_connections (id, provider, name, enabled, updated_by) VALUES ($1,$2,$3,$4,$5)`,
				in.ID, in.Provider, in.Name, enabled, actorID); err != nil {
				return err
			}
		} else {
			if provider != in.Provider {
				return &FieldError{Field: "provider", Msg: "провайдера подключения не сменить: создайте новое"}
			}
			if in.Enabled != nil && !*in.Enabled {
				refs, err := infraConnRefs(ctx, tx, in.ID)
				if err != nil {
					return err
				}
				if len(refs) > 0 {
					return &ErrInUse{Refs: refs}
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE infra_connections SET name=$2, enabled=COALESCE($3, enabled), updated_at=now(), updated_by=$4
				WHERE id=$1`, in.ID, in.Name, in.Enabled, actorID); err != nil {
				return err
			}
		}
		replaced := false
		if in.Token != nil {
			enc, err := box.Seal(*in.Token)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE infra_connections SET token_prefix=$2, token_enc=$3, state='unchecked', check_detail='',
					balance_amount=NULL, balance_currency='' WHERE id=$1`,
				in.ID, tokenPrefix(*in.Token), enc); err != nil {
				return err
			}
			replaced = true
		}
		out, err = scanInfraConn(tx.QueryRow(ctx, `SELECT `+infraConnCols+`
			FROM infra_connections c JOIN users u ON u.id=c.updated_by WHERE c.id=$1`, in.ID))
		if err != nil {
			return err
		}
		typ, text := "infra_connection.updated", "изменено подключение инфраструктуры "+in.ID
		if created {
			typ, text = "infra_connection.created", "создано подключение инфраструктуры "+in.ID
		}
		if _, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin, Type: typ, Text: text,
			Payload: map[string]any{"connection_id": in.ID, "provider": in.Provider, "enabled": out.Enabled}}); err != nil {
			return err
		}
		if replaced {
			_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin,
				Type: "infra_connection.token_replaced", Text: "заменён токен подключения инфраструктуры " + in.ID,
				Payload: map[string]any{"connection_id": in.ID, "token_prefix": out.TokenPrefix}})
		}
		return err
	})
	return out, created, err
}

// ListInfraConnections: подключения по названию.
func (s *Store) ListInfraConnections(ctx context.Context) ([]domain.InfraConnection, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+infraConnCols+`
		FROM infra_connections c JOIN users u ON u.id=c.updated_by ORDER BY c.name, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.InfraConnection{}
	for rows.Next() {
		c, err := scanInfraConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetInfraConnection: подключение по идентификатору.
func (s *Store) GetInfraConnection(ctx context.Context, id string) (domain.InfraConnection, error) {
	c, err := scanInfraConn(s.Pool.QueryRow(ctx, `SELECT `+infraConnCols+`
		FROM infra_connections c JOIN users u ON u.id=c.updated_by WHERE c.id=$1`, id))
	return c, nf(err)
}

// SetInfraConnectionCheck: итог проверки подключения у провайдера.
func (s *Store) SetInfraConnectionCheck(ctx context.Context, id string, state domain.LLMProviderState, detail string,
	balance *float64, currency, actorLogin string) (domain.InfraConnection, error) {
	var out domain.InfraConnection
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanInfraConn(tx.QueryRow(ctx, `
			WITH up AS (UPDATE infra_connections SET state=$2, check_detail=$3, checked_at=now(),
				balance_amount=COALESCE($4, balance_amount), balance_currency=CASE WHEN $4::float8 IS NULL THEN balance_currency ELSE $5 END
				WHERE id=$1 RETURNING *)
			SELECT `+infraConnCols+` FROM up c JOIN users u ON u.id=c.updated_by`, id, state, detail, balance, currency))
		if err != nil {
			return nf(err)
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin,
			Type: "infra_connection.checked", Text: "проверено подключение инфраструктуры " + id + ": " + string(state),
			Payload: map[string]any{"connection_id": id, "state": state, "detail": detail}})
		return err
	})
	return out, err
}

// InfraConnectionToken: провайдер и расшифрованный токен подключения;
// выключенное подключение: ErrInvalid.
func (s *Store) InfraConnectionToken(ctx context.Context, id string, box *secretbox.Box) (string, string, error) {
	var provider string
	var enc []byte
	var enabled bool
	err := s.Pool.QueryRow(ctx, `SELECT provider, token_enc, enabled FROM infra_connections WHERE id=$1`, id).
		Scan(&provider, &enc, &enabled)
	if err != nil {
		return "", "", nf(err)
	}
	if !enabled {
		return "", "", errors.Join(ErrInvalid, errors.New("подключение "+id+" выключено"))
	}
	if enc == nil {
		return "", "", errors.Join(ErrInvalid, errors.New("у подключения "+id+" нет токена"))
	}
	token, err := box.Open(enc)
	if err != nil {
		return "", "", errors.Join(ErrSecret, err)
	}
	return provider, token, nil
}

// infraConnRefs: окружения, привязанные к подключению, и живые хосты.
func infraConnRefs(ctx context.Context, q rowsQuerier, id string) ([]Ref, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT p.name || '/' || e.name FROM environments e JOIN projects p ON p.id = e.project_id
		WHERE e.config->'provision'->>'connection' = $1
		   OR EXISTS (SELECT 1 FROM env_hosts h WHERE h.env_id = e.id AND h.connection_id = $1
		              AND h.state NOT IN ('deleted','rejected','failed'))
		ORDER BY 1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		refs = append(refs, Ref{Kind: "environment", ID: name})
	}
	return refs, rows.Err()
}

// DeleteInfraConnection: удаление; используемое подключение: ErrInUse.
func (s *Store) DeleteInfraConnection(ctx context.Context, id, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM infra_connections WHERE id=$1 FOR UPDATE`, id); err != nil {
			return err
		}
		refs, err := infraConnRefs(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return &ErrInUse{Refs: refs}
		}
		// Конечные хосты хранят ссылку для истории: она снимается.
		if _, err := tx.Exec(ctx, `UPDATE env_hosts SET connection_id=NULL WHERE connection_id=$1`, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM infra_connections WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin,
			Type: "infra_connection.deleted", Text: "удалено подключение инфраструктуры " + id,
			Payload: map[string]any{"connection_id": id}})
		return err
	})
}
