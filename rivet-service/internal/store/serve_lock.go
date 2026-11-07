package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Блокировка «с базой работает rivetd» (change open-source, спека
// access-policy «Безопасные умолчания установки»). rivetd держит
// advisory-блокировку serveLockKey разделяемой всё время работы,
// перешифровка ключа (rivetd -rotate-key) берёт её исключительно и при
// работающем rivetd отказывает: он держал бы в памяти прежний ключ и писал
// бы им новые секреты. Разделяемые блокировки совместимы, поэтому
// несколько rivetd с одной базой друг другу не мешают.

// serveLockKey: ключ advisory-блокировки, «RIVET». В pg_locks он виден
// как classid 0x52, objid 0x49564554, objsubid 1.
const serveLockKey int64 = 0x5249564554

// ServeLockApp и RotateApp: application_name соединений с блокировкой у
// rivetd и у перешифровки. По ним видно, кто её держит (pg_stat_activity).
const (
	ServeLockApp = "rivetd serve-lock"
	RotateApp    = "rivetd rotate-key"
)

// serveLockPing: как часто rivetd проверяет соединение с блокировкой и как
// часто, пока ждёт конца перешифровки, пишет в журнал, кто её держит.
// Проверка заодно держит живым состояние NAT и балансировщиков.
// serveLockPoll: как часто rivetd пробует взять блокировку, пока ждёт.
var (
	serveLockPing = 30 * time.Second
	serveLockPoll = 2 * time.Second
)

// serveConnectTimeout: предел установки соединения с блокировкой, если
// DSN своего не задаёт. Без него попытка к пропавшему хосту (failover со
// сменой адреса) висела бы до системного таймаута SYN, минуты две.
const serveConnectTimeout = 10 * time.Second

// ErrServeRunning: с базой работает rivetd.
var ErrServeRunning = errors.New("с этой базой работает rivetd: остановите его (docker compose stop rivetd) и повторите перешифровку. " +
	"Кто держит блокировку: SELECT pid, client_addr, backend_start FROM pg_locks JOIN pg_stat_activity USING (pid) " +
	"WHERE locktype = 'advisory' AND application_name = '" + ServeLockApp + "' AND datname = current_database()")

// holdersSQL: держатели блокировки serveLockKey в этой базе.
const holdersSQL = `
	SELECT a.pid, COALESCE(host(a.client_addr), 'локально'), a.application_name, l.mode
	FROM pg_locks l JOIN pg_stat_activity a USING (pid)
	WHERE l.locktype = 'advisory' AND l.granted AND l.classid = 82 AND l.objid = 1230390612 AND l.objsubid = 1
	  AND a.datname = current_database()`

// HoldServeLock держит разделяемую блокировку на отдельном соединении,
// пока работает rivetd: соединение пула вернулось бы в пул и отпустило бы
// её. Раз в serveLockPing соединение проверяется, при обрыве (рестарт
// Postgres, failover, таймаут простоя) rivetd переподключается и берёт
// блокировку заново.
//
// После повторного захвата rivetd сверяет события secret_key.rotated с
// тем, что было при первом захвате. Если появилось новое, перешифровка
// прошла в окне без блокировки, и ключ в памяти rivetd устарел: вызывается
// onRotated (rivetd завершается, его перезапускают с новым ключом).
//
// release останавливает проверку и закрывает соединение, повторный вызов
// ничего не делает.
func (s *Store) HoldServeLock(ctx context.Context, onRotated func()) (release func(), err error) {
	cfg := s.Pool.Config().ConnConfig.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = ServeLockApp
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = serveConnectTimeout
	}
	conn, err := lockServe(ctx, cfg)
	if err != nil {
		return nil, err
	}
	base, err := lastRotation(ctx, conn)
	if err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	lctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(serveLockPing)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(lctx, 10*time.Second)
			_, perr := conn.Exec(pctx, `SELECT 1`)
			pcancel()
			if perr == nil || lctx.Err() != nil {
				continue
			}
			slog.Warn("соединение с блокировкой «работает rivetd» оборвалось, беру её заново", "err", perr)
			_ = conn.Close(context.Background())
			for {
				c, err := lockServe(lctx, cfg)
				if err == nil {
					conn = c
					slog.Info("блокировка «работает rivetd» снова взята")
					break
				}
				if lctx.Err() != nil {
					return
				}
				slog.Warn("не удалось снова взять блокировку «работает rivetd», повтор", "err", err)
				select {
				case <-lctx.Done():
					return
				case <-time.After(serveLockPing):
				}
			}
			cur, err := lastRotation(lctx, conn)
			if err != nil {
				slog.Warn("не удалось сверить перешифровки ключа после переподключения", "err", err)
				continue
			}
			if cur > base {
				slog.Error("перешифровка ключа прошла, пока rivetd работал без блокировки: ключ в памяти устарел, перезапустите rivetd с новым RIVET_SECRET_KEY")
				if onRotated != nil {
					onRotated()
				}
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			_ = conn.Close(context.Background())
		})
	}, nil
}

// lastRotation: id последнего события secret_key.rotated, 0 без них.
func lastRotation(ctx context.Context, conn *pgx.Conn) (int64, error) {
	var id int64
	err := conn.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM events WHERE type = 'secret_key.rotated'`).Scan(&id)
	return id, err
}

// lockServe открывает соединение и берёт на нём разделяемую блокировку.
// Пока идёт перешифровка ключа, пробует снова раз в serveLockPoll и раз в
// serveLockPing пишет в журнал, кто держит блокировку.
func lockServe(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var lastLog time.Time
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock_shared($1)`, serveLockKey).Scan(&ok); err != nil {
			_ = conn.Close(context.Background())
			return nil, err
		}
		if ok {
			return conn, nil
		}
		if time.Since(lastLog) >= serveLockPing {
			slog.Info("жду завершения перешифровки ключа (rivetd -rotate-key) с этой базой", "держит", lockHolders(ctx, conn))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(context.Background())
			return nil, ctx.Err()
		case <-time.After(serveLockPoll):
		}
	}
}

// lockHolders: кто держит блокировку, для журнала: pid, адрес клиента,
// application_name и режим.
func lockHolders(ctx context.Context, conn *pgx.Conn) string {
	rows, err := conn.Query(ctx, holdersSQL)
	if err != nil {
		return "неизвестно: " + err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid int
		var addr, app, mode string
		if err := rows.Scan(&pid, &addr, &app, &mode); err != nil {
			return "неизвестно: " + err.Error()
		}
		out = append(out, fmt.Sprintf("pid %d с %s (%s, %s)", pid, addr, app, mode))
	}
	if len(out) == 0 {
		return "никто (блокировка только что освободилась)"
	}
	return strings.Join(out, ", ")
}
