package stream

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Версия 7: промпт пользователя в Assignment — сессия доработки
// (add-user-sessions); v6 — адаптер и глубина данных, структурированные
// шаги; v5 — токены регистрации, v4 — репозиторий проекта, v3 —
// деплой-джобы, v2 — session_id. Runner'ы младших версий отклоняются
// при Register.
const protocolVersion = "7"

// ProtocolVersion — версия протокола для состояния установки.
const ProtocolVersion = protocolVersion

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

	// Пары сессий, о пересечении которых уже написано (спека
	// team-visibility «Предупреждение о пересечениях работ»): событие
	// информационное, повтор после рестарта допустим.
	overlapMu   sync.Mutex
	overlapSeen map[string]bool

	// Кэш приватности сессий (шаги и live-вывод приватной не публикуются,
	// спека team-visibility): приватность неизменна после создания.
	privMu    sync.Mutex
	privCache map[string]bool
}

// sessionPrivate — приватность сессии с кэшем; при ошибке чтения считается
// приватной (fail-closed: лучше не показать, чем раскрыть).
func (s *Server) sessionPrivate(ctx context.Context, sessionID string) bool {
	s.privMu.Lock()
	if v, ok := s.privCache[sessionID]; ok {
		s.privMu.Unlock()
		return v
	}
	s.privMu.Unlock()
	private, _, err := s.St.SessionPrivacy(ctx, sessionID)
	if err != nil {
		return true
	}
	s.privMu.Lock()
	if s.privCache == nil {
		s.privCache = map[string]bool{}
	}
	s.privCache[sessionID] = private
	s.privMu.Unlock()
	return private
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
	// Буфер транскрипта копится всегда (автор получит сохранённый
	// транскрипт), live-трансляция приватной сессии не публикуется.
	s.Engine.OnTranscript(taskID, sessionID, data)
	if s.sessionPrivate(ctx, sessionID) {
		return
	}
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
	// Глубина данных — часть контракта v6: неизвестное значение — это
	// несовместимый runner, а не повод молча записать minimal.
	switch domain.SessionDepth(req.Depth) {
	case domain.DepthFull, domain.DepthPartial, domain.DepthMinimal:
	default:
		return &pb.RegisterResponse{Accepted: false,
			Message: "неизвестная глубина данных " + req.Depth + ": ожидается full, partial или minimal"}, nil
	}
	// Регистрация фиксирует токен и пишет событие установки (спека runners
	// «Регистрация фиксируется»).
	token := tokenFromContext(ctx)
	err := s.St.RegisterRunner(ctx, domain.Runner{
		ID: req.RunnerId, Agent: req.Agent, Model: req.Model,
		Host: req.Host, Capabilities: req.Capabilities,
		Adapter: req.Adapter, Depth: domain.SessionDepth(req.Depth),
	}, token)
	if err != nil {
		return nil, err
	}
	slog.Info("runner registered", "runner", req.RunnerId, "agent", req.Agent, "caps", req.Capabilities, "token", token.Name)
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

// stepPayload — структурированные поля session.step из AgentEvent: строки
// маскируются, размеры ограничиваются. Пустой kind — простой текстовый шаг
// (обёртка), payload несёт только session_id.
func stepPayload(ev *pb.AgentEvent) map[string]any {
	payload := map[string]any{"session_id": ev.SessionId}
	if ev.Kind == "" {
		return payload
	}
	payload["kind"] = ev.Kind
	payload["ok"] = ev.Ok
	if ev.Tool != "" {
		payload["tool"] = redact.String(clip(ev.Tool, 100))
	}
	if ev.Detail != "" {
		payload["detail"] = redact.String(clip(ev.Detail, 500))
	}
	if files := maskedFiles(ev.Files, 50); len(files) > 0 {
		payload["files"] = files
	}
	return payload
}

// maskedFiles — пути файлов шага для payload и сессии: маскирование,
// обрезка длины, не больше limit штук.
func maskedFiles(files []string, limit int) []string {
	if len(files) == 0 {
		return nil
	}
	if len(files) > limit {
		files = files[:limit]
	}
	masked := make([]string, 0, len(files))
	for _, f := range files {
		masked = append(masked, redact.String(clip(f, 300)))
	}
	return masked
}

// clip обрезает строку до n байт по границе руны.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// emitOverlaps — пересечения работ: общие затронутые файлы у активных
// сессий разных задач проекта дают событие session.overlap в timeline
// обеих задач; работа не блокируется (спека team-visibility).
func (s *Server) emitOverlaps(ctx context.Context, sessionID, projectID, epicID string, files []string) error {
	self, hits, err := s.St.OverlappingSessions(ctx, sessionID, files)
	if err != nil || len(hits) == 0 {
		return err
	}
	for _, h := range hits {
		key := self.SessionID + "|" + h.SessionID
		if h.SessionID < self.SessionID {
			key = h.SessionID + "|" + self.SessionID
		}
		// Пара помечается «в работе» атомарно с проверкой: параллельные
		// шаги одной пары не запишут дубль; при ошибке отметка снимается,
		// и следующий шаг повторит запись.
		s.overlapMu.Lock()
		if s.overlapSeen == nil {
			s.overlapSeen = map[string]bool{}
		}
		if s.overlapSeen[key] {
			s.overlapMu.Unlock()
			continue
		}
		s.overlapSeen[key] = true
		s.overlapMu.Unlock()
		release := func() {
			s.overlapMu.Lock()
			delete(s.overlapSeen, key)
			s.overlapMu.Unlock()
		}
		otherProject, otherEpic, otherTask, err := s.St.SessionProjectEpic(ctx, h.SessionID)
		if err != nil {
			release()
			return err
		}
		text := func(other int64, files []string) string {
			return "пересечение работ с task-" + strconv.FormatInt(other, 10) + ": общие файлы " + strings.Join(files, ", ")
		}
		pairs := []struct {
			project, epic, task string
			payload             map[string]any
			text                string
		}{
			{projectID, epicID, self.TaskID, map[string]any{
				"session_id": self.SessionID, "other_task_id": h.TaskID,
				"other_task_num": h.TaskNum, "other_session_id": h.SessionID, "files": h.Files,
			}, text(h.TaskNum, h.Files)},
			{otherProject, otherEpic, otherTask, map[string]any{
				"session_id": h.SessionID, "other_task_id": self.TaskID,
				"other_task_num": self.TaskNum, "other_session_id": self.SessionID, "files": h.Files,
			}, text(self.TaskNum, h.Files)},
		}
		evs := make([]store.EventInput, 0, len(pairs))
		for _, p := range pairs {
			evs = append(evs, store.EventInput{
				ActorKind: domain.ActorSystem, Type: "session.overlap",
				ProjectID: p.project, EpicID: p.epic, TaskID: p.task,
				Text: p.text, Payload: p.payload,
			})
		}
		// Обе стороны одной транзакцией; при ошибке отметка пары снимается,
		// чтобы следующий шаг повторил запись.
		if err := s.St.AppendEvents(ctx, evs); err != nil {
			release()
			return err
		}
	}
	return nil
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
		private := s.sessionPrivate(ctx, k.Event.SessionId)
		// Текст и структура шага — runner-controlled: всё маскируется и
		// ограничивается (api-contract: detail ≤500, files ≤50 в payload).
		payload := stepPayload(k.Event)
		// Затронутые файлы сессии копятся из полного списка события, а не из
		// урезанного payload: кап сессии — 500 путей (AppendSessionFiles);
		// files IS NULL у минимальной глубины — запрос её не тронет.
		if files := maskedFiles(k.Event.Files, len(k.Event.Files)); len(files) > 0 {
			if err := s.St.AppendSessionFiles(ctx, k.Event.SessionId, files); err != nil {
				return err
			}
			// Для поиска пересечений хватает первых путей шага: гигантский
			// шаг не должен тормозить обработку канала runner'а. Приватная
			// сессия в пересечениях не участвует (design add-user-sessions).
			if len(files) > 100 {
				files = files[:100]
			}
			if !private {
				if err := s.emitOverlaps(ctx, k.Event.SessionId, projectID, epicID, files); err != nil {
					return err
				}
			}
		}
		// Последний шаг — в реестр активных сессий (спека team-visibility).
		text := redact.String(k.Event.Text)
		if err := s.St.SetSessionLastStep(ctx, k.Event.SessionId, clip(text, 300)); err != nil {
			return err
		}
		// Шаги приватной сессии не публикуются в event log и SSE: команде —
		// факт сессии, содержимое — автору (спека team-visibility).
		if private {
			return nil
		}
		_, err = s.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "session.step",
			ProjectID: projectID, EpicID: epicID, TaskID: k.Event.TaskId,
			Text:    text,
			Payload: payload,
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
