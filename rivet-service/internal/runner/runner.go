// Package runner — агент на машине исполнителя: исходящее gRPC-соединение
// к control plane, PTY-обёртка CLI-агента (глубина minimal), файловый
// outbox-журнал для доставки at-least-once.
package runner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Версия 6: адаптер и глубина данных в Register, структурированные шаги
// в AgentEvent (add-claude-code-adapter); v5 — токены регистрации, v4 —
// репозиторий проекта в Assignment, v3 — деплой-джобы, v2 — session_id.
// Runner'ы младших версий отклоняются при Register.
const protocolVersion = "6"

type Config struct {
	PlaneAddr string
	// Token — токен регистрации, выданный администратором (спека runners);
	// без него runner не подключается.
	Token string
	// TLS — подключаться к control plane по TLS; TLSCA — корневой сертификат
	// (пусто — системные корни). Без TLS токен идёт открытым текстом и порт
	// протокола обязан быть закрыт периметром.
	TLS          bool
	TLSCA        string
	ID           string
	Agent        string
	Model        string
	AgentCmd     string
	Capabilities []string
	Workdir      string
	// GitBase — префикс URL клонирования; file://-префикс используют e2e-стенды.
	GitBase string
	// Adapter — способ подключения агента: claude-code (нативный, полная
	// глубина) или wrap (универсальная PTY-обёртка, минимальная).
	Adapter string
	// ClaudeBin — бинарник Claude Code для нативного адаптера (подмена в
	// тестах и на стендах); пусто — «claude» из PATH.
	ClaudeBin string
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return err
	}
	// Остатки хуков прошлых запусков (сокеты, настройки) не нужны никому.
	_ = os.RemoveAll(cfg.Workdir + "/hooks")
	outbox, err := newOutbox(cfg.Workdir + "/outbox")
	if err != nil {
		return err
	}

	if cfg.Token == "" {
		return errors.New("не задан токен регистрации: укажите -token или RIVET_RUNNER_TOKEN (выпускается администратором в разделе «Управление приложением»)")
	}
	creds := insecure.NewCredentials()
	if cfg.TLS {
		if cfg.TLSCA != "" {
			if creds, err = credentials.NewClientTLSFromFile(cfg.TLSCA, ""); err != nil {
				return fmt.Errorf("tls-ca: %w", err)
			}
		} else {
			creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
		}
	}
	conn, err := grpc.NewClient(cfg.PlaneAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewRunnerServiceClient(conn)

	r := &agent{cfg: cfg, client: client, outbox: outbox, adapter: newAdapter(cfg)}
	r.ctxPct.Store(ctxUnknown)
	// Переподключение с бэкоффом: соединение всегда исходящее от runner'а.
	for {
		if err := r.session(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("сессия оборвалась — переподключение", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// ctxUnknown — заполненность контекста неизвестна (агент не отчитался).
const ctxUnknown = -1

type agent struct {
	cfg     Config
	client  pb.RunnerServiceClient
	outbox  *outbox
	adapter adapter

	// Последний ctx_pct из USAGE:-отчёта агента; ctxUnknown до первого отчёта
	// и после нового Assignment (design add-usage-metering, решение 5).
	ctxPct atomic.Int32

	mu     sync.Mutex
	cancel map[string]context.CancelFunc // отмена стадий по task_id
}

func (a *agent) session(ctx context.Context) error {
	// Токен регистрации — на обоих RPC (api-contract, протокол v5).
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.Token)
	reg, err := a.client.Register(ctx, &pb.RegisterRequest{
		RunnerId: a.cfg.ID, Agent: a.cfg.Agent, Model: a.cfg.Model,
		Host: hostname(), Capabilities: a.cfg.Capabilities, ProtocolVersion: protocolVersion,
		Adapter: a.cfg.Adapter, Depth: depthOf(a.cfg.Adapter),
	})
	if err != nil {
		return err
	}
	if !reg.Accepted {
		return fmt.Errorf("регистрация отклонена: %s", reg.Message)
	}
	heartbeat := time.Duration(reg.HeartbeatIntervalS) * time.Second
	if heartbeat <= 0 {
		heartbeat = 30 * time.Second
	}

	stream, err := a.client.Channel(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.RunnerMsg{
		MsgId: newMsgID(),
		Kind:  &pb.RunnerMsg_Hello{Hello: &pb.Hello{RunnerId: a.cfg.ID}},
	}); err != nil {
		return err
	}
	slog.Info("подключён к control plane", "plane", a.cfg.PlaneAddr, "runner", a.cfg.ID)

	sendCh := make(chan *pb.RunnerMsg, 256)
	sctx, stop := context.WithCancel(ctx)
	defer stop()

	// Отправка: сначала недоставленное из журнала, дальше — из канала.
	go func() {
		for _, m := range a.outbox.pending() {
			if err := stream.Send(m); err != nil {
				return
			}
		}
		for {
			select {
			case <-sctx.Done():
				return
			case m := <-sendCh:
				if err := stream.Send(m); err != nil {
					return
				}
			}
		}
	}()

	// Heartbeat.
	go func() {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				hb := &pb.Heartbeat{}
				if v := a.ctxPct.Load(); v != ctxUnknown {
					hb.CtxPct = &v
				}
				sendCh <- &pb.RunnerMsg{MsgId: newMsgID(),
					Kind: &pb.RunnerMsg_Heartbeat{Heartbeat: hb}}
			}
		}
	}()

	emit := func(m *pb.RunnerMsg) {
		a.outbox.put(m)
		select {
		case sendCh <- m:
		case <-sctx.Done():
		}
	}

	a.mu.Lock()
	a.cancel = map[string]context.CancelFunc{}
	a.mu.Unlock()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch k := msg.Kind.(type) {
		case *pb.PlaneMsg_Ack:
			a.outbox.ack(k.Ack.AckedMsgId)
		case *pb.PlaneMsg_Assign:
			go a.executeStage(sctx, k.Assign, emit)
		case *pb.PlaneMsg_Deploy:
			go a.executeDeploy(sctx, k.Deploy, emit)
		case *pb.PlaneMsg_Cancel:
			a.mu.Lock()
			if c := a.cancel[k.Cancel.TaskId]; c != nil {
				c()
			}
			a.mu.Unlock()
		case *pb.PlaneMsg_Answer:
			// Ответ человека приходит новой стадией Assign — отдельной обработки нет.
		case *pb.PlaneMsg_Pause:
			// drain управляется control plane'ом (назначения прекращаются там).
		}
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
