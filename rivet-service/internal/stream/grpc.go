package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Версия 10: рабочая копия репозитория в деплой-джобе — доставка
// манифестами и чартом (add-k8s-delivery); v9 — политика проекта в
// назначении; v8 — обратный канал контекста; v7 — промпт пользователя;
// v6 — адаптер и глубина данных; v5 — токены регистрации, v4 —
// репозиторий проекта, v3 — деплой-джобы, v2 — session_id. Runner'ы
// младших версий отклоняются при Register.
const protocolVersion = "12"

// compatProtocolVersion — прежняя версия, которую control plane принимает
// до следующего мажорного изменения (design add-process-model): runner'ы
// v10 регистрируются с одиночной моделью и игнорируют модель в назначении.
const compatProtocolVersion = "11"

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

	// ClientCertRequired: установка требует клиентский сертификат runner'а
	// (mTLS, RIVET_GRPC_CLIENT_CA): соединение без проверенного сертификата
	// не аутентифицируется, а его идентичность сверяется с runner_id.
	ClientCertRequired bool

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

	// Последнее событие отказа регистрации по runner'у и причине: забытый
	// runner прежней версии переподключается раз в несколько секунд, и без
	// окна event log и лента аудита росли бы на десятки тысяч записей в
	// сутки (ревью add-native-agent P2-2).
	rejMu   sync.Mutex
	rejSeen map[string]rejection
	// rejFullUntil: раньше этого момента о переполнении карты окна в
	// журнале не предупреждают повторно (одно предупреждение на окно).
	rejFullUntil time.Time
	// invalidLogAt: последняя строка журнала о недопустимом запросе
	// регистрации (logInvalidRequest).
	invalidLogAt time.Time
}

// rejectedEventWindow — не чаще одного события runner.rejected на runner и
// причину за это время. Повтор после рестарта rivetd допустим.
const rejectedEventWindow = time.Hour

// rejectedKeysMax: потолок карты окна. Держатель действующего токена
// может перебирать идентификаторы, и без потолка росли бы и карта, и event
// log. Сверх потолка новые отказы до истечения окна не пишутся (сами
// запросы по-прежнему отклоняются), а журнал rivetd один раз
// предупреждает, до какого момента события теряются.
const rejectedKeysMax = 1024

type rejection struct {
	at         time.Time
	suppressed int
}

// noteRejection — писать ли событие отказа сейчас и сколько отказов с той
// же причиной пропущено с прошлого события. Чужие записи старше окна
// чистятся: карта не растёт от runner'ов, которые давно не появлялись.
func (s *Server) noteRejection(runnerID, reason string, now time.Time) (bool, int) {
	s.rejMu.Lock()
	defer s.rejMu.Unlock()
	if s.rejSeen == nil {
		s.rejSeen = map[string]rejection{}
	}
	key := runnerID + "\x00" + reason
	r, seen := s.rejSeen[key]
	if seen && now.Sub(r.at) < rejectedEventWindow {
		r.suppressed++
		s.rejSeen[key] = r
		return false, 0
	}
	for k, o := range s.rejSeen {
		if k != key && now.Sub(o.at) >= rejectedEventWindow {
			delete(s.rejSeen, k)
		}
	}
	if !seen && len(s.rejSeen) >= rejectedKeysMax {
		if !now.Before(s.rejFullUntil) {
			// Под затяжным перебором слоты освобождаются по одному, поэтому
			// следующее предупреждение не раньше чем через окно, а не к
			// истечению самой старой записи.
			s.rejFullUntil = now.Add(rejectedEventWindow)
			slog.Warn("окно отказов регистрации переполнено: часть новых отказов не пишется в event log и журнал, следующее предупреждение не раньше "+
				s.rejFullUntil.Format("15:04"), "ключей", len(s.rejSeen), "runner", clip(runnerID, 128))
		}
		return false, 0
	}
	s.rejSeen[key] = rejection{at: now}
	return true, r.suppressed
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
	// Идентичность проверяется первой (add-runner-identity): чужой идентификатор не
	// должен ни провалить публикацию runner'а, ни снять его проверки и
	// запуски, ни переписать его запись. Токен без привязки здесь же
	// привязывается к этому runner'у.
	if err := s.checkIdentity(ctx, identityCheck{runnerID: req.RunnerId, host: req.Host, agent: req.Agent, register: true}); err != nil {
		return nil, err
	}
	if req.ProtocolVersion != protocolVersion && req.ProtocolVersion != compatProtocolVersion {
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
	// Агентом может быть только агент Rivet (add-native-agent, design
	// решение 7): сторонние CLI-агенты не регистрируются. Runner без агента
	// допускается только как runner выкатки (единственная capability deploy).
	// Событие runner.rejected — не чаще раза в час на runner и причину:
	// отказанный runner переподключается каждые несколько секунд.
	if msg, ok := agentAllowed(req); !ok {
		if write, suppressed := s.noteRejection(req.RunnerId, msg, time.Now()); write {
			token := tokenFromContext(ctx)
			payload := map[string]any{"runner_id": req.RunnerId, "agent": req.Agent, "host": req.Host, "token_name": token.Name, "reason": msg}
			if suppressed > 0 {
				payload["repeated"] = suppressed
			}
			if _, err := s.St.AppendEvent(ctx, store.EventInput{
				ActorKind: domain.ActorRunner, ActorID: req.RunnerId, Type: "runner.rejected", Text: "регистрация runner'а " + req.RunnerId + " отклонена: " + msg,
				Payload: payload,
			}); err != nil {
				slog.Error("register: событие отказа", "err", err)
			}
		}
		return nil, status.Error(codes.FailedPrecondition, msg)
	}
	// Проверки наблюдения прежней сессии прерваны: ожидание снимается, а
	// проверка повторяется сразу, без ложного провала по таймауту.
	if err := s.St.ReleaseMonitorChecks(ctx, req.RunnerId); err != nil {
		slog.Warn("проверки наблюдения runner'а", "runner", req.RunnerId, "err", err)
	}
	// Задания хоста прежней сессии тоже прерваны: повторяются сразу.
	if err := s.St.ReleaseHostJobs(ctx, req.RunnerId); err != nil {
		slog.Warn("задания хоста runner'а", "runner", req.RunnerId, "err", err)
	}
	// Глубина данных — часть контракта v6: неизвестное значение — это
	// несовместимый runner, а не повод молча записать minimal.
	switch domain.SessionDepth(req.Depth) {
	case domain.DepthFull, domain.DepthPartial, domain.DepthMinimal:
	default:
		return &pb.RegisterResponse{Accepted: false,
			Message: "неизвестная глубина данных " + req.Depth + ": ожидается full, partial или minimal"}, nil
	}
	// Перезапуск runner'а (новый идентификатор процесса): его открытые
	// стадии потеряны — запуски отвязываются и назначаются заново, иначе
	// они держали бы агентов команды занятыми навсегда (add-agent-roles).
	if req.BootId != "" {
		prev, err := s.St.RunnerBootID(ctx, req.RunnerId)
		if err != nil {
			return nil, err
		}
		if prev != "" && prev != req.BootId {
			if err := s.Engine.ReleaseRunnerRuns(ctx, req.RunnerId, req.PendingSessions); err != nil {
				return nil, err
			}
		}
	}
	// Регистрация фиксирует токен и пишет событие установки (спека runners
	// «Регистрация фиксируется»).
	token := tokenFromContext(ctx)
	secure := secureChannel(ctx)
	err := s.St.RegisterRunner(ctx, domain.Runner{
		ID: req.RunnerId, Agent: req.Agent, Model: req.Model, Models: req.Models, Stages: req.Stages,
		Host: req.Host, Capabilities: req.Capabilities,
		Adapter: req.Adapter, Depth: domain.SessionDepth(req.Depth),
		ContextChannel: req.ContextChannel, Secure: secure, Protocol: req.ProtocolVersion,
		DisplayName: displayName(req.DisplayName), Features: req.Features, BootID: req.BootId,
		NetworkIsolated: req.NetworkIsolated,
	}, token)
	if err != nil {
		return nil, err
	}
	rn, err := s.St.GetRunner(ctx, req.RunnerId)
	if err != nil {
		return nil, err
	}
	slog.Info("runner registered", "runner", req.RunnerId, "agent", req.Agent, "caps", rn.Capabilities, "token", token.Name, "catalog", rn.Catalog, "secure", secure)
	return &pb.RegisterResponse{
		Accepted:           true,
		HeartbeatIntervalS: int32(s.HeartbeatInterval.Seconds()),
		Catalog:            rn.Catalog,
		Secure:             secure,
	}, nil
}

// agentAllowed — агент runner'а допустим: rivet с feature native, либо без
// агента с единственной capability deploy (runner выкатки).
func agentAllowed(req *pb.RegisterRequest) (string, bool) {
	const reject = "агентом может быть только агент Rivet: запустите rivet-runner -agent rivet"
	switch req.Agent {
	case "rivet":
		// Агент уже rivet, но бинарник прежней версии без агентского цикла:
		// совет «запустите -agent rivet» тут не поможет.
		if !slices.Contains(req.Features, "native") {
			return "runner прежней версии без агентского цикла Rivet: обновите rivet-runner", false
		}
		return "", true
	case "":
		if len(req.Capabilities) == 2 && slices.Contains(req.Capabilities, "deploy") && slices.Contains(req.Capabilities, "docker") && slices.Contains(req.Features, "local_docker_v1") {
			return "", true
		}
		if len(req.Capabilities) == 1 && req.Capabilities[0] == "deploy" {
			return "", true
		}
		return "runner без агента допускается только с единственной capability deploy (runner выкатки)", false
	}
	return reject, false
}

// secureChannel — защищён ли канал runner'а: TLS у соединения или
// loopback-адрес (runner на той же машине). Секреты подключений в режиме
// профиля secure уходят только по такому каналу.
func secureChannel(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	if _, tls := p.AuthInfo.(credentials.TLSInfo); tls {
		return true
	}
	host := p.Addr.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	// Hello верить нельзя (add-runner-identity): канал подключается к
	// очереди назначений runner'а, только если токен и сертификат
	// соединения выданы для него.
	if err := s.checkIdentity(ctx, identityCheck{runnerID: runnerID}); err != nil {
		return err
	}
	sendCh := s.Reg.Attach(runnerID)
	defer s.Reg.Detach(runnerID, sendCh)
	slog.Info("runner connected", "runner", runnerID)
	// Канал открыт — признак жизни: отметка offline после недоставки в окне
	// между Register и Hello снимается (add-agent-roles, ревью круг 3).
	if err := s.St.RunnerAttached(ctx, runnerID); err != nil {
		slog.Warn("runner attached", "runner", runnerID, "err", err)
	}

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
			// Снимаются проверки только этой сессии: если runner уже
			// переподключился, его новые проверки не трогаются.
			if s.Reg.Current(runnerID, sendCh) {
				if rerr := s.St.ReleaseMonitorChecks(context.WithoutCancel(ctx), runnerID); rerr != nil {
					slog.Warn("проверки наблюдения runner'а", "runner", runnerID, "err", rerr)
				}
				if rerr := s.St.ReleaseHostJobs(context.WithoutCancel(ctx), runnerID); rerr != nil {
					slog.Warn("задания хоста runner'а", "runner", runnerID, "err", rerr)
				}
			}
			return nil
		}
		if _, dup := seen[msg.MsgId]; dup {
			s.ack(runnerID, msg.MsgId)
			continue
		}
		if len(seen) > 65536 {
			seen = map[string]struct{}{}
		}
		seen[msg.MsgId] = struct{}{}

		if err := s.handle(ctx, runnerID, msg); err != nil {
			if errors.Is(err, errStaleRunner) {
				// Стадии runner'а сняты по таймауту и назначены другим, а
				// его процесс ещё может их выполнять: канал закрывается,
				// runner переподключится и отменит прежние стадии.
				slog.Warn("heartbeat от runner'а, потерянного по таймауту: канал закрыт", "runner", runnerID)
				return status.Error(codes.FailedPrecondition, "runner потерян по таймауту heartbeat: переподключитесь")
			}
			slog.Error("runner msg", "runner", runnerID, "err", err)
			continue // без Ack — runner повторит
		}
		s.ack(runnerID, msg.MsgId)
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

// Причины недоставки контекста агенту (api-contract: delivery_reason).
const (
	reasonNoChannel     = "no_channel"     // адаптер runner'а без обратного канала
	reasonRunnerOffline = "runner_offline" // runner не подключён или канал переполнен
	reasonNoRunner      = "no_runner"      // стадия завершилась — доставлять некому
)

// overlapContext — текст предупреждения для самого агента. Формулировка
// явно отделяет предупреждение от ошибки инструмента: нативный адаптер
// отдаёт его через stderr хука, и агент не должен принять его за отказ
// вызова (design, решение о коде возврата 2).
func overlapContext(otherNum int64, files []string) string {
	// Пути приходят от runner'а и уезжают в промпт другого агента: перевод
	// строки в имени файла превратил бы предупреждение в чужую инструкцию.
	safe := make([]string, 0, len(files))
	for _, f := range files {
		safe = append(safe, clip(sanitizeLine(f), 300))
	}
	return "Предупреждение Rivet (не ошибка инструмента): параллельно с тобой над task-" +
		strconv.FormatInt(otherNum, 10) + " идёт работа в общих файлах: " + strings.Join(safe, ", ") +
		". Продолжай задачу, но учитывай пересечение: не переписывай чужие изменения целиком и держи правки минимальными."
}

// sanitizeLine заменяет управляющие символы и разделители строк пробелом:
// текст контекста должен остаться одной строкой без разметки, которую агент
// примет за границу сообщения (U+2028/U+2029 — не control, но перевод
// строки для многих потребителей).
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

// deliverContext доставляет контекст работающему агенту сессии: сообщение
// уходит только runner'у, объявившему обратный канал (спека
// agent-integration «Адаптер без обратного канала»). Недоставка — не
// ошибка: она фиксируется причиной и не влияет на выполнение стадии.
func (s *Server) deliverContext(ctx context.Context, taskID, sessionID, kind, text string) (bool, string) {
	runnerID, channel, err := s.St.SessionRunner(ctx, sessionID)
	if err != nil {
		slog.Warn("контекст: runner сессии не определён", "session", sessionID, "err", err)
		return false, reasonNoRunner
	}
	if runnerID == "" {
		return false, reasonNoRunner
	}
	if !channel {
		return false, reasonNoChannel
	}
	if s.Reg == nil {
		return false, reasonRunnerOffline
	}
	sent := s.Reg.Send(runnerID, &pb.PlaneMsg{
		MsgId: fmt.Sprintf("ctx-%s-%s-%d", sessionID, kind, time.Now().UnixNano()),
		Kind: &pb.PlaneMsg_Context{Context: &pb.Context{
			TaskId: taskID, SessionId: sessionID, Kind: kind, Text: text}},
	})
	if !sent {
		return false, reasonRunnerOffline
	}
	return true, ""
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
			session             string
			otherNum            int64
			payload             map[string]any
			text                string
		}{
			{projectID, epicID, self.TaskID, self.SessionID, h.TaskNum, map[string]any{
				"session_id": self.SessionID, "other_task_id": h.TaskID,
				"other_task_num": h.TaskNum, "other_session_id": h.SessionID, "files": h.Files,
			}, text(h.TaskNum, h.Files)},
			{otherProject, otherEpic, otherTask, h.SessionID, self.TaskNum, map[string]any{
				"session_id": h.SessionID, "other_task_id": self.TaskID,
				"other_task_num": self.TaskNum, "other_session_id": self.SessionID, "files": h.Files,
			}, text(self.TaskNum, h.Files)},
		}
		evs := make([]store.EventInput, 0, len(pairs))
		for _, p := range pairs {
			// Предупреждение доезжает до самого агента обратным каналом, а
			// результат доставки — в payload события (спека team-visibility
			// «Агент предупреждён о пересечении»). Контекст уходит до записи
			// события: агент должен узнать о пересечении как можно раньше,
			// а событие несёт уже фактический результат.
			delivered, reason := s.deliverContext(ctx, p.task, p.session, "overlap",
				overlapContext(p.otherNum, h.Files))
			p.payload["delivered"] = delivered
			if !delivered {
				p.payload["delivery_reason"] = reason
			}
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

// ack подтверждает приём сообщения runner'у. Отправка идёт через реестр, а
// не прямо в канал соединения: reconnect закрывает прежний канал, и прямая
// запись в него уронила бы plane. Потерянный ack не страшен — доставка
// at-least-once, runner повторит.
func (s *Server) ack(runnerID, msgID string) {
	s.Reg.Send(runnerID, &pb.PlaneMsg{
		MsgId: "ack-" + msgID,
		Kind:  &pb.PlaneMsg_Ack{Ack: &pb.Ack{AckedMsgId: msgID}}})
}

func (s *Server) handle(ctx context.Context, runnerID string, msg *pb.RunnerMsg) error {
	switch k := msg.Kind.(type) {
	case *pb.RunnerMsg_Heartbeat:
		stale, err := s.St.TouchRunner(ctx, runnerID, ctxPct(k.Heartbeat.CtxPct))
		if err != nil {
			return err
		}
		if stale {
			return errStaleRunner
		}
		return nil

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
		// Роль и имя исполнителя (спека agent-roles «Роль исполнителя в
		// событиях и API»).
		payload = s.Engine.PerformerPayload(ctx, k.Event.SessionId, payload)
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
			if _, err := s.St.TouchRunner(ctx, runnerID, ctxPct(k.Usage.CtxPct)); err != nil {
				return err
			}
		}
		return s.St.RecordUsage(ctx, store.UsageInput{
			SourceMsgID: msg.MsgId, SessionID: k.Usage.SessionId, ProjectID: projectID, EpicID: epicID,
			TaskID: k.Usage.TaskId, RunnerID: runnerID, Model: k.Usage.Model,
			TokensIn: nonNegative(k.Usage.TokensIn), TokensOut: nonNegative(k.Usage.TokensOut),
			TokensCached: nonNegative(k.Usage.TokensCached), TokensCacheWrite: nonNegative(k.Usage.TokensCacheWrite),
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

	case *pb.RunnerMsg_MonitorResult:
		return s.Engine.OnMonitorResult(ctx, runnerID, k.MonitorResult)

	case *pb.RunnerMsg_HostJobResult:
		return s.Engine.OnHostJobResult(ctx, runnerID, k.HostJobResult)
	}
	return nil
}

var errMissingHello = errHello{}

// errStaleRunner — heartbeat от runner'а, потерянного по таймауту.
var errStaleRunner = errors.New("runner потерян по таймауту heartbeat")

type errHello struct{}

func (errHello) Error() string {
	return "первым сообщением Channel должен быть Hello"
}

// displayName — имя runner'а из регистрации: одна строка до 40 символов,
// иначе без имени (показывается идентификатор).
func displayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return name
}
