package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Перешифровка секретов установки новым ключом (change open-source, спека
// access-policy «Безопасные умолчания установки»). Секреты в базе:
// токены хостингов (scm_credentials.token_enc), ключи подключений к
// моделям (model_connections.key_enc) и значения секретных заголовков
// подключений (model_connections.headers[].value_enc). Новый вид
// шифрованного секрета добавляется сюда в той же правке, иначе после смены
// ключа он перестанет читаться.

// RotateResult: сколько секретов перешифровано. Значений и ключей здесь
// нет: результат пишется в event log и журнал.
type RotateResult struct {
	Credentials       int `json:"scm_credentials"`
	ConnectionKeys    int `json:"connection_keys"`
	ConnectionHeaders int `json:"connection_headers"`
	// AlreadyNew: секреты, которые уже читаются новым ключом (их
	// сохранили после смены ключа): остаются как есть.
	AlreadyNew int `json:"already_new"`
}

// UndecryptableError: секреты, которые не читаются ни прежним, ни новым
// ключом. Перешифровка в этом случае не делается вовсе: неверный прежний
// ключ иначе оставил бы все секреты нечитаемыми.
type UndecryptableError struct {
	Items []string
}

func (e *UndecryptableError) Error() string {
	return fmt.Sprintf("секреты не расшифровываются ни прежним, ни новым ключом (%d): %s. "+
		"Проверьте RIVET_SECRET_KEY_OLD. Если ключ верный, введите эти секреты заново в консоли или удалите их и повторите перешифровку",
		len(e.Items), "«"+strings.Join(e.Items, "», «")+"»")
}

// rotateLockTimeout: сколько перешифровка ждёт таблицы секретов за чужой
// транзакцией (lock_timeout). Переменная ради тестов.
var rotateLockTimeout = "30s"

// ErrSecretsBusy: таблицы секретов 30 секунд заняты другой транзакцией.
var ErrSecretsBusy = errors.New("таблицы секретов заняты другой транзакцией дольше 30 секунд (возможно, с базой работает rivetd прежней версии, " +
	"который блокировку не держит): остановите его и повторите перешифровку")

// RotateSecrets перешифровывает все секреты установки одной транзакцией и
// пишет в event log событие secret_key.rotated без значений. Любая ошибка
// откатывает всё: в базе остаются секреты под прежним ключом. При
// работающем rivetd возвращает ErrServeRunning.
func (s *Store) RotateSecrets(ctx context.Context, oldBox, newBox *secretbox.Box) (RotateResult, error) {
	var res RotateResult
	if !oldBox.Enabled() || !newBox.Enabled() {
		return res, secretbox.ErrNoKey
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		res = RotateResult{}
		// Зависшая перешифровка не должна держать старт rivetd без предела:
		// ожидание таблиц за чужой транзакцией ограничено, а простаивающая
		// транзакция (процесс остановлен, клиент пропал из сети) сама
		// откатится и отпустит блокировку.
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, rotateLockTimeout); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '60s'`); err != nil {
			return err
		}
		var free bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, serveLockKey).Scan(&free); err != nil {
			return err
		}
		if !free {
			return ErrServeRunning
		}
		// Запись секретов работающим rivetd ждёт конца транзакции: иначе
		// секрет, сохранённый прежним ключом посреди перешифровки, остался
		// бы под ним.
		if _, err := tx.Exec(ctx, `LOCK TABLE scm_credentials, model_connections IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				return ErrSecretsBusy
			}
			return err
		}
		var bad []string
		// recrypt: шифротекст под новым ключом; changed=false значит, что секрет
		// уже под новым ключом или не читается (тогда он в bad).
		recrypt := func(enc []byte, what string) ([]byte, bool, error) {
			plain, err := oldBox.Open(enc)
			if err == nil {
				out, err := newBox.Seal(plain)
				return out, err == nil, err
			}
			if _, nerr := newBox.Open(enc); nerr == nil {
				res.AlreadyNew++
				return enc, false, nil
			}
			bad = append(bad, what)
			return enc, false, nil
		}

		// Токен хостинга в ошибке называется так, как его видно в консоли:
		// хостинг, владелец токена и проекты (UUID записи там не показан).
		type credRow struct {
			id, label string
			enc       []byte
		}
		var creds []credRow
		rows, err := tx.Query(ctx, `
			SELECT c.id::text, c.token_enc, c.provider, c.base_url, c.owner,
			       COALESCE((SELECT string_agg(p.name, ', ' ORDER BY p.name) FROM projects p WHERE p.credential_id = c.id), '')
			FROM scm_credentials c ORDER BY c.id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r credRow
			var provider, baseURL, owner, projects string
			if err := rows.Scan(&r.id, &r.enc, &provider, &baseURL, &owner, &projects); err != nil {
				rows.Close()
				return err
			}
			r.label = "токен хостинга " + provider + " " + baseURL
			if owner != "" {
				r.label += " (владелец " + owner + ")"
			}
			if projects != "" {
				r.label += ", проекты: " + projects
			} else {
				r.label += ", без проекта, запись " + r.id
			}
			creds = append(creds, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range creds {
			enc, changed, err := recrypt(c.enc, c.label)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE scm_credentials SET token_enc=$2 WHERE id=$1::uuid`, c.id, enc); err != nil {
				return err
			}
			res.Credentials++
		}

		type connRow struct {
			id      string
			key     []byte
			headers []byte
		}
		var conns []connRow
		rows, err = tx.Query(ctx, `SELECT id, key_enc, headers FROM model_connections ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r connRow
			if err := rows.Scan(&r.id, &r.key, &r.headers); err != nil {
				rows.Close()
				return err
			}
			conns = append(conns, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range conns {
			key := c.key
			keyChanged := false
			if key != nil {
				if key, keyChanged, err = recrypt(c.key, "ключ подключения "+c.id); err != nil {
					return err
				}
			}
			headers, n, err := recryptHeaders(c.headers, func(enc []byte, name string) ([]byte, bool, error) {
				return recrypt(enc, "заголовок "+name+" подключения "+c.id)
			})
			if err != nil {
				return fmt.Errorf("заголовки подключения %s: %w", c.id, err)
			}
			if !keyChanged && n == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE model_connections SET key_enc=$2, headers=$3 WHERE id=$1`, c.id, key, headers); err != nil {
				return err
			}
			if keyChanged {
				res.ConnectionKeys++
			}
			res.ConnectionHeaders += n
		}

		if len(bad) > 0 {
			return &UndecryptableError{Items: bad}
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, ActorID: "rotate-key", Type: "secret_key.rotated",
			Text:    fmt.Sprintf("ключ шифрования секретов сменён: перешифровано %d", res.Credentials+res.ConnectionKeys+res.ConnectionHeaders),
			Payload: res,
		})
		return err
	})
	return res, err
}

// recryptHeaders перешифровывает value_enc секретных заголовков. Заголовки
// разбираются как сырой JSON: поля, которых не знает этот код, переносятся
// без изменений. Возвращает новый JSON и число перешифрованных значений.
func recryptHeaders(raw []byte, recrypt func(enc []byte, name string) ([]byte, bool, error)) ([]byte, int, error) {
	var hs []map[string]json.RawMessage
	if len(raw) == 0 {
		return raw, 0, nil
	}
	if err := json.Unmarshal(raw, &hs); err != nil {
		return nil, 0, err
	}
	n := 0
	for _, h := range hs {
		v, ok := h["value_enc"]
		if !ok {
			continue
		}
		var enc []byte
		if err := json.Unmarshal(v, &enc); err != nil {
			return nil, 0, err
		}
		if enc == nil {
			continue
		}
		var name string
		_ = json.Unmarshal(h["name"], &name)
		out, changed, err := recrypt(enc, name)
		if err != nil {
			return nil, 0, err
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, 0, err
		}
		h["value_enc"] = b
		n++
	}
	if n == 0 {
		return raw, 0, nil
	}
	out, err := json.Marshal(hs)
	return out, n, err
}
