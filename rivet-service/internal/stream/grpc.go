package stream

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Версия 3: деплой-джобы (implement-deployment); версия 2 добавила
// обязательный session_id. Runner'ы младших версий отклоняются при Register.
const protocolVersion = "3"

// Server — реализация RunnerService: приём соединений runner'ов.
type Server struct {
	pb.UnimplementedRunnerServiceServer
	St     *store.Store
	Engine *orchestrator.Engine
	Reg    *Registry
	Hub    *Hub

	HeartbeatInterval time.Duration

	// Построчные редакторы секретов по задачам: наружу (Hub, Engine) уходит
	// только замаскированный текст (спека team-visibility, design решение 3).
	redMu sync.Mutex
	reds  map[string]*taskRedactor
}

// taskRedactor — состояние редактора текущей сессии задачи; смена сессии
// сбрасывает недосброшенный хвост прошлой стадии. Собственный mu: replay
// после reconnect даёт два Channel-goroutine с одним session_id, а
// redact.Stream не потокобезопасен.
type taskRedactor struct {
	mu      sync.Mutex
	session string
	stream  redact.Stream
}

// redactor возвращает редактор сессии задачи, создавая или сбрасывая его
// при смене сессии.
func (s *Server) redactor(taskID, sessionID string) *taskRedactor {
	s.redMu.Lock()
	defer s.redMu.Unlock()
	if s.reds == nil {
		s.reds = map[string]*taskRedactor{}
	}
	r := s.reds[taskID]
	if r == nil || r.session != sessionID {
		r = &taskRedactor{session: sessionID}
		s.reds[taskID] = r
	}
	return r
}

// emitTranscript доводит замаскированный кусок до буфера транскрипта и
// live-подписчиков.
func (s *Server) emitTranscript(ctx context.Context, taskID, sessionID string, data []byte) {
	if len(data) == 0 {
		return
	}
	s.Engine.OnTranscript(taskID, sessionID, data)
	if projectID, _, err := s.St.TaskRefs(ctx, taskID); err == nil {
		s.Hub.Publish(LogChunk{ProjectID: projectID, TaskID: taskID, Data: data})
	}
}

// emitDeployLog доводит замаскированный кусок лога публикации до буфера
// Engine и live-подписчиков (SSE deploy.log).
func (s *Server) emitDeployLog(ctx context.Context, depID string, data []byte) {
	if len(data) == 0 {
		return
	}
	s.Engine.OnDeployTranscript(depID, data)
	if projectID, _, _, _, err := s.St.DeploymentRefs(ctx, depID); err == nil {
		s.Hub.Publish(LogChunk{ProjectID: projectID, DeployID: depID, Data: data})
	}
}

// flushDeployRedactor сбрасывает хвост лога публикации перед реакцией на
// DeployResult (финал уводит буфер в blob). Промежуточный результат
// (deploy ok перед verify) хвост не теряет: сбрасывать безопасно, чанки
// следующего этапа продолжат тем же редактором.
func (s *Server) flushDeployRedactor(ctx context.Context, depID string) {
	s.redMu.Lock()
	r := s.reds["deploy:"+depID]
	delete(s.reds, "deploy:"+depID)
	s.redMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	rest := r.stream.Flush()
	r.mu.Unlock()
	s.emitDeployLog(ctx, depID, rest)
}

// flushRedactor сбрасывает хвост неполной строки перед обработкой конца
// стадии: OnStageResult/OnBlocked закрывают сессию и уводят буфер в blob,
// хвост должен успеть в него попасть (design, решение 3).
func (s *Server) flushRedactor(ctx context.Context, taskID, sessionID string) {
	s.redMu.Lock()
	r := s.reds[taskID]
	if r == nil || r.session != sessionID {
		// Чужая сессия (stale-сообщение) не трогает редактор текущей.
		s.redMu.Unlock()
		return
	}
	delete(s.reds, taskID)
	s.redMu.Unlock()
	r.mu.Lock()
	rest := r.stream.Flush()
	r.mu.Unlock()
	s.emitTranscript(ctx, taskID, sessionID, rest)
}

func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if req.ProtocolVersion != protocolVersion {
		// Несовместимая версия — понятная ошибка, не разрыв (api-contract).
		return &pb.RegisterResponse{
			Accepted: false,
			Message:  "несовместимая версия протокола: ожидается " + protocolVersion,
		}, nil
	}
	// Reconnect убивает деплой-goroutine прежней сессии runner'а: его
	// активная публикация проваливается сразу, не дожидаясь watchdog
	// (UpsertRunner ниже сбрасывает занятость).
	if depID, err := s.St.RunnerActiveDeployment(ctx, req.RunnerId); err == nil && depID != "" {
		if err := s.Engine.FailDeploymentNow(ctx, depID, "runner переподключился — джоба потеряна"); err != nil {
			return nil, err
		}
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

// ctxPct переводит optional-поле протокола в *int store'а (nil = неизвестно).
// Значение вне 0–100 не доверяем: runner свой ввод уже фильтрует, но gRPC
// открыт любому клиенту.
func ctxPct(v *int32) *int {
	if v == nil || *v < 0 || *v > 100 {
		return nil
	}
	p := int(*v)
	return &p
}

// nonNegative отбрасывает отрицательные и не-конечные (NaN, ±Inf) значения
// отчёта: они испортили бы агрегаты метеринга (nil = данных нет).
func nonNegative[T int64 | float64](v *T) *T {
	if v == nil || *v < 0 || math.IsNaN(float64(*v)) || math.IsInf(float64(*v), 0) {
		return nil
	}
	return v
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
		return s.St.TouchRunner(ctx, runnerID, ctxPct(k.Heartbeat.CtxPct))

	case *pb.RunnerMsg_Event:
		if !s.Engine.SessionMatches(ctx, k.Event.TaskId, k.Event.SessionId) {
			return nil // stale-шаг не загрязняет timeline текущей сессии
		}
		projectID, epicID, err := s.St.TaskRefs(ctx, k.Event.TaskId)
		if err != nil {
			return err
		}
		// Текст шага — runner-controlled, виден участникам в event log.
		_, err = s.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "session.step",
			ProjectID: projectID, EpicID: epicID, TaskID: k.Event.TaskId,
			Text: redact.String(k.Event.Text),
		})
		return err

	case *pb.RunnerMsg_Transcript:
		t := k.Transcript
		if t.DeployId != "" {
			// Лог деплой-джобы: свой буфер и live-событие deploy.log.
			if !s.Engine.DeployMatches(ctx, t.DeployId, runnerID) {
				return nil
			}
			r := s.redactor("deploy:"+t.DeployId, t.DeployId)
			r.mu.Lock()
			masked := r.stream.Feed(t.Data)
			r.mu.Unlock()
			s.emitDeployLog(ctx, t.DeployId, masked)
			return nil
		}
		if !s.Engine.SessionMatches(ctx, t.TaskId, t.SessionId) {
			return nil // чужая сессия (replay) — отбрасываем, но ack'аем
		}
		r := s.redactor(t.TaskId, t.SessionId)
		r.mu.Lock()
		masked := r.stream.Feed(t.Data)
		r.mu.Unlock()
		s.emitTranscript(ctx, t.TaskId, t.SessionId, masked)
		return nil

	case *pb.RunnerMsg_Usage:
		if !s.Engine.SessionMatches(ctx, k.Usage.TaskId, k.Usage.SessionId) {
			return nil // usage чужой сессии исказил бы итог текущей (EndSession)
		}
		projectID, epicID, err := s.St.TaskRefs(ctx, k.Usage.TaskId)
		if err != nil {
			return err
		}
		// Свежий ctx_pct из отчёта агента не ждёт следующего heartbeat.
		if k.Usage.CtxPct != nil {
			if err := s.St.TouchRunner(ctx, runnerID, ctxPct(k.Usage.CtxPct)); err != nil {
				return err
			}
		}
		return s.St.RecordUsage(ctx, store.UsageInput{
			SourceMsgID: msg.MsgId, ProjectID: projectID, EpicID: epicID,
			TaskID: k.Usage.TaskId, RunnerID: runnerID, Model: k.Usage.Model,
			TokensIn: nonNegative(k.Usage.TokensIn), TokensOut: nonNegative(k.Usage.TokensOut),
			CostUSD: nonNegative(k.Usage.CostUsd), DurationS: max(int(k.Usage.DurationS), 0),
		})

	case *pb.RunnerMsg_StageResult:
		// Хвост неполной строки — до OnStageResult: тот закрывает сессию
		// и уводит буфер транскрипта в blob.
		s.flushRedactor(ctx, k.StageResult.TaskId, k.StageResult.SessionId)
		return s.Engine.OnStageResult(ctx, runnerID, k.StageResult)

	case *pb.RunnerMsg_Blocked:
		s.flushRedactor(ctx, k.Blocked.TaskId, k.Blocked.SessionId)
		return s.Engine.OnBlocked(ctx, runnerID, k.Blocked)

	case *pb.RunnerMsg_DeployResult:
		dr := k.DeployResult
		// Хвост лога — до реакции (финал уводит буфер в blob), но только
		// для владельца: чужой/stale результат не трогает редактор.
		if s.Engine.DeployMatches(ctx, dr.DeploymentId, runnerID) {
			s.flushDeployRedactor(ctx, dr.DeploymentId)
		}
		return s.Engine.OnDeployResult(ctx, runnerID, dr)
	}
	return nil
}

var errMissingHello = errHello{}

type errHello struct{}

func (errHello) Error() string {
	return "первым сообщением Channel должен быть Hello"
}
