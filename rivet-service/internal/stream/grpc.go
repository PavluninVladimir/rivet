package stream

import (
	"context"
	"log/slog"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

const protocolVersion = "1"

// Server — реализация RunnerService: приём соединений runner'ов.
type Server struct {
	pb.UnimplementedRunnerServiceServer
	St     *store.Store
	Engine *orchestrator.Engine
	Reg    *Registry
	Hub    *Hub

	HeartbeatInterval time.Duration
}

func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if req.ProtocolVersion != protocolVersion {
		// Несовместимая версия — понятная ошибка, не разрыв (api-contract).
		return &pb.RegisterResponse{
			Accepted: false,
			Message:  "несовместимая версия протокола: ожидается " + protocolVersion,
		}, nil
	}
	err := s.St.UpsertRunner(ctx, domain.Runner{
		ID: req.RunnerId, Agent: req.Agent, Model: req.Model,
		Host: req.Host, Capabilities: req.Capabilities,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("runner registered", "runner", req.RunnerId, "agent", req.Agent, "caps", req.Capabilities)
	return &pb.RegisterResponse{
		Accepted:           true,
		HeartbeatIntervalS: int32(s.HeartbeatInterval.Seconds()),
	}, nil
}

func (s *Server) Channel(streamSrv pb.RunnerService_ChannelServer) error {
	ctx := streamSrv.Context()

	first, err := streamSrv.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errMissingHello
	}
	runnerID := hello.RunnerId
	sendCh := s.Reg.Attach(runnerID)
	defer s.Reg.Detach(runnerID, sendCh)
	slog.Info("runner connected", "runner", runnerID)

	// Отправка plane → runner.
	go func() {
		for msg := range sendCh {
			if err := streamSrv.Send(msg); err != nil {
				slog.Warn("send to runner", "runner", runnerID, "err", err)
				return
			}
		}
	}()

	// Приём runner → plane с дедупликацией по msg_id.
	seen := map[string]struct{}{}
	for {
		msg, err := streamSrv.Recv()
		if err != nil {
			slog.Info("runner disconnected", "runner", runnerID, "err", err)
			return nil
		}
		if _, dup := seen[msg.MsgId]; dup {
			s.ack(sendCh, msg.MsgId)
			continue
		}
		if len(seen) > 65536 {
			seen = map[string]struct{}{}
		}
		seen[msg.MsgId] = struct{}{}

		if err := s.handle(ctx, runnerID, msg); err != nil {
			slog.Error("runner msg", "runner", runnerID, "err", err)
			continue // без Ack — runner повторит
		}
		s.ack(sendCh, msg.MsgId)
	}
}

func (s *Server) ack(sendCh chan *pb.PlaneMsg, msgID string) {
	select {
	case sendCh <- &pb.PlaneMsg{Kind: &pb.PlaneMsg_Ack{Ack: &pb.Ack{AckedMsgId: msgID}}}:
	default:
	}
}

func (s *Server) handle(ctx context.Context, runnerID string, msg *pb.RunnerMsg) error {
	switch k := msg.Kind.(type) {
	case *pb.RunnerMsg_Heartbeat:
		return s.St.TouchRunner(ctx, runnerID, int(k.Heartbeat.CtxPct))

	case *pb.RunnerMsg_Event:
		projectID, epicID, err := s.St.TaskRefs(ctx, k.Event.TaskId)
		if err != nil {
			return err
		}
		_, err = s.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "session.step",
			ProjectID: projectID, EpicID: epicID, TaskID: k.Event.TaskId,
			Text: k.Event.Text,
		})
		return err

	case *pb.RunnerMsg_Transcript:
		s.Engine.OnTranscript(k.Transcript.TaskId, k.Transcript.Data)
		if projectID, _, err := s.St.TaskRefs(ctx, k.Transcript.TaskId); err == nil {
			s.Hub.Publish(LogChunk{ProjectID: projectID, TaskID: k.Transcript.TaskId, Data: k.Transcript.Data})
		}
		return nil

	case *pb.RunnerMsg_Usage:
		projectID, epicID, err := s.St.TaskRefs(ctx, k.Usage.TaskId)
		if err != nil {
			return err
		}
		return s.St.RecordUsage(ctx, store.UsageInput{
			SourceMsgID: msg.MsgId, ProjectID: projectID, EpicID: epicID,
			TaskID: k.Usage.TaskId, RunnerID: runnerID, Model: k.Usage.Model,
			TokensIn: k.Usage.TokensIn, TokensOut: k.Usage.TokensOut,
			CostUSD: k.Usage.CostUsd, DurationS: int(k.Usage.DurationS),
		})

	case *pb.RunnerMsg_StageResult:
		return s.Engine.OnStageResult(ctx, runnerID, k.StageResult)

	case *pb.RunnerMsg_Blocked:
		return s.Engine.OnBlocked(ctx, runnerID, k.Blocked)
	}
	return nil
}

var errMissingHello = errHello{}

type errHello struct{}

func (errHello) Error() string {
	return "первым сообщением Channel должен быть Hello"
}
