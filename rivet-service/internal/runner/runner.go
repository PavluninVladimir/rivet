// Package runner — агент на машине исполнителя: исходящее gRPC-соединение
// к control plane, PTY-обёртка CLI-агента (глубина minimal), файловый
// outbox-журнал для доставки at-least-once.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

const protocolVersion = "1"

type Config struct {
	PlaneAddr    string
	ID           string
	Agent        string
	Model        string
	AgentCmd     string
	Capabilities []string
	Workdir      string
	// GitBase — префикс URL клонирования; file://-префикс используют e2e-стенды.
	GitBase string
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return err
	}
	outbox, err := newOutbox(cfg.Workdir + "/outbox")
	if err != nil {
		return err
	}

	conn, err := grpc.NewClient(cfg.PlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewRunnerServiceClient(conn)

	r := &agent{cfg: cfg, client: client, outbox: outbox}
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

type agent struct {
	cfg    Config
	client pb.RunnerServiceClient
	outbox *outbox

	mu     sync.Mutex
	cancel map[string]context.CancelFunc // отмена стадий по task_id
}

func (a *agent) session(ctx context.Context) error {
	reg, err := a.client.Register(ctx, &pb.RegisterRequest{
		RunnerId: a.cfg.ID, Agent: a.cfg.Agent, Model: a.cfg.Model,
		Host: hostname(), Capabilities: a.cfg.Capabilities, ProtocolVersion: protocolVersion,
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
				sendCh <- &pb.RunnerMsg{MsgId: newMsgID(),
					Kind: &pb.RunnerMsg_Heartbeat{Heartbeat: &pb.Heartbeat{}}}
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
