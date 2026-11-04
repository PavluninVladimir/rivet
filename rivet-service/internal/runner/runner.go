// Package runner — агент на машине исполнителя: исходящее gRPC-соединение
// к control plane, PTY-обёртка CLI-агента (глубина minimal), файловый
// outbox-журнал для доставки at-least-once.
package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Версия 10: рабочая копия репозитория в деплой-джобе — доставка
// манифестами и чартом (add-k8s-delivery); v9 — политика проекта в
// назначении; v8 — обратный канал контекста; v7 — промпт пользователя;
// v6 — адаптер и глубина данных; v5 — токены регистрации, v4 —
// репозиторий проекта, v3 — деплой-джобы, v2 — session_id. Runner'ы
// младших версий отклоняются при Register.
const protocolVersion = "12"

// Идентификатор поколения стадий (RegisterRequest.boot_id) новый на каждом
// подключении: стадии привязаны к каналу и отменяются при его обрыве и при
// остановке процесса, поэтому control plane по смене boot_id снимает
// открытые запуски runner'а (кроме сессий из журнала) и назначает их заново
// без расхода попытки.

// featureRoles — runner передаёт инструкцию роли агенту и выполняет сессию
// тестировщика на TESTING (add-agent-roles). Аддитивно к версии протокола.
const featureRoles = "roles"

type Config struct {
	PlaneAddr string
	// Token — токен регистрации, выданный администратором (спека runners);
	// без него runner не подключается.
	Token string
	// TLS — подключаться к control plane по TLS; TLSCA — корневой сертификат
	// (пусто — системные корни). Без TLS токен идёт открытым текстом и порт
	// протокола обязан быть закрыт периметром.
	TLS   bool
	TLSCA string
	ID    string
	// Name — отображаемое имя runner'а (add-agent-roles): «Разработчик Макс».
	Name  string
	Agent string
	// Model — модель по умолчанию; Models — все поддерживаемые (протокол
	// v11): модель сессии приходит в Assignment.model и передаётся агенту.
	Model        string
	Models       []string
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
	// AdapterCmd — команда внешнего адаптера (спека agent-integration
	// «Открытый SDK адаптеров»); AdapterDepth — глубина данных, которую он
	// даёт; AdapterContext — принимает ли он контекст от Rivet.
	AdapterCmd     string
	AdapterDepth   string
	AdapterContext bool
	// ExtraEnv и ExtraArgs — окружение и аргументы агента из назначения
	// (профиль каталога, протокол v12): накладываются поверх окружения
	// runner'а и аргументов адаптера. SecretValues — значения для
	// маскирования в транскрипте.
	ExtraEnv     []string
	ExtraArgs    []string
	SecretValues []string
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

	r := &agent{cfg: cfg, client: client, outbox: outbox,
		adapter: newAdapter(cfg), contexts: newContextHub()}
	r.ctxPct.Store(ctxUnknown)
	if err := checkGitEnvConfig(); err != nil {
		slog.Warn("сессии тестировщика не запустятся", "err", err)
	}
	// Сигнал завершения отменяет стадии: runner ждёт их откат и запись
	// результата в журнал, иначе рабочая копия осталась бы в середине сессии.
	defer r.waitStages(r.stopGrace())
	// Переподключение с бэкоффом: соединение всегда исходящее от runner'а.
	for {
		if err := r.session(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errStagesStuck) {
				return err
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

	// contexts — очереди контекста выполняющихся стадий (обратный канал).
	contexts *contextHub

	mu     sync.Mutex
	cancel map[string]*stageCancel // отмена стадий по task_id

	// gen — стадии и публикации текущего подключения (поколения): новое
	// подключение регистрируется только после отката стадий прошлого, а
	// завершение runner'а ждёт их не дольше grace.
	genMu sync.Mutex
	gen   *stageGroup
	// grace — сколько завершение runner'а ждёт стадии (0 — stopGrace),
	// stuck — сколько новое подключение ждёт откат прошлого (0 — stuckLimit).
	grace, stuck time.Duration
}

// stageGroup — выполняющиеся стадии одного подключения. Ожидание без
// горутины и без повторного использования WaitGroup: канал zero закрыт,
// пока стадий нет.
type stageGroup struct {
	mu     sync.Mutex
	seq    int
	active map[int]stageInfo
	zero   chan struct{}
}

// stageInfo — что за стадия выполняется: для журнала ожидания.
type stageInfo struct {
	Task, Session, Stage string
}

func newStageGroup() *stageGroup {
	z := make(chan struct{})
	close(z)
	return &stageGroup{zero: z, active: map[int]stageInfo{}}
}

func (g *stageGroup) add(info stageInfo) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.active) == 0 {
		g.zero = make(chan struct{})
	}
	g.seq++
	g.active[g.seq] = info
	return g.seq
}

func (g *stageGroup) done(id int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, id)
	if len(g.active) == 0 {
		close(g.zero)
	}
}

// running — выполняющиеся стадии.
func (g *stageGroup) running() []stageInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]stageInfo, 0, len(g.active))
	for _, s := range g.active {
		out = append(out, s)
	}
	return out
}

// idle — канал, закрытый, когда стадий поколения нет.
func (g *stageGroup) idle() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.zero
}

// generation — стадии текущего подключения.
func (a *agent) generation() *stageGroup {
	a.genMu.Lock()
	defer a.genMu.Unlock()
	if a.gen == nil {
		a.gen = newStageGroup()
	}
	return a.gen
}

func (a *agent) stopGrace() time.Duration {
	if a.grace > 0 {
		return a.grace
	}
	return stopGrace
}

// stuckLimit — сколько новое подключение ждёт откат стадий прошлого.
// Дольше — стадия зависла: runner завершается с ошибкой, супервизор
// (launchd, systemd, compose) перезапустит процесс, а следующие стадии
// откатят рабочие копии при подготовке.
const stuckLimit = 10 * time.Minute

// errStagesStuck — стадии прошлого подключения не завершились за stuckLimit.
var errStagesStuck = errors.New("стадии прошлого подключения не завершились")

func (a *agent) stuckLimit() time.Duration {
	if a.stuck > 0 {
		return a.stuck
	}
	return stuckLimit
}

// stageCancel — отмена выполняющейся стадии. byPlane — отмену прислал
// control plane (CancelTask): результат такой стадии отправляется. Стадия,
// отменённая остановкой runner'а или обрывом канала, результата не шлёт и в
// журнал его не пишет: control plane назначит её заново без расхода попытки.
type stageCancel struct {
	cancel  context.CancelFunc
	byPlane atomic.Bool
}

// stopGrace — сколько завершение runner'а ждёт отменённые стадии: откат
// рабочей копии и запись результата в журнал, который проиграется после
// следующей регистрации.
const stopGrace = 30 * time.Second

// waitStages ждёт стадии текущего подключения не дольше grace (завершение
// runner'а). false — не дождались.
func (a *agent) waitStages(grace time.Duration) bool {
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-a.generation().idle():
		return true
	case <-t.C:
		slog.Warn("стадии не завершились за отведённое время — выход", "grace", grace)
		return false
	}
}

// nextGeneration — новое подключение: стадии прошлого отменены вместе с его
// каналом, и до их отката (рабочая копия, журнал) runner не регистрируется,
// иначе новые назначения пришли бы в те же каталоги. Откат ограничен своими
// таймаутами (settleTimeout, убийство группы процессов агента), а до
// регистрации control plane новых стадий этому runner'у не даёт. Стадия,
// которая не завершилась за stuckLimit, зависла: errStagesStuck, процесс
// runner'а завершается.
func (a *agent) nextGeneration(ctx context.Context) error {
	prev := a.generation()
	start := time.Now()
	for {
		t := time.NewTimer(a.stopGrace())
		select {
		case <-prev.idle():
			t.Stop()
			a.genMu.Lock()
			a.gen = newStageGroup()
			a.genMu.Unlock()
			return nil
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		waited := time.Since(start).Round(time.Second)
		stuck := prev.running()
		if time.Since(start) >= a.stuckLimit() {
			var names []string
			for _, s := range stuck {
				names = append(names, fmt.Sprintf("task %s, сессия %s, стадия %s", s.Task, s.Session, s.Stage))
			}
			return fmt.Errorf("%w за %v (%s): runner завершается, супервизор перезапустит процесс",
				errStagesStuck, a.stuckLimit(), strings.Join(names, "; "))
		}
		for _, s := range stuck {
			slog.Warn("стадия прошлого подключения ещё откатывается: регистрация ждёт",
				"task", s.Task, "session", s.Session, "stage", s.Stage, "waited", waited, "limit", a.stuckLimit())
		}
	}
}

// goStage запускает стадию или публикацию в текущем подключении.
func (a *agent) goStage(info stageInfo, f func()) {
	g := a.generation()
	id := g.add(info)
	go func() {
		defer g.done(id)
		f()
	}()
}

// supportedStages — стадии, которые исполняет этот runner (спека
// agent-integration «Стадия PROMPT»).
var supportedStages = []string{"CODING", "TESTING", "REVIEW", "FIXING", "PROMPT"}

// models — объявляемый список моделей: явный список, иначе модель по
// умолчанию как список из одного элемента.
func (c Config) models() []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	if c.Model != "" {
		return []string{c.Model}
	}
	return nil
}

// forModel — конфигурация адаптера под модель назначения: пустая модель
// в назначении означает модель runner'а по умолчанию.
func (c Config) forModel(model string) Config {
	if model != "" {
		c.Model = model
	}
	return c
}

// forAssignment — конфигурация адаптера под назначение: модель, окружение и
// аргументы профиля агента (add-agent-profiles), команда обёртки из
// профиля. Секреты назначения маскируются в транскрипте.
func (c Config) forAssignment(as *pb.Assignment) Config {
	c = c.forModel(as.Model)
	if len(as.AgentEnv) > 0 {
		keys := make([]string, 0, len(as.AgentEnv))
		for k := range as.AgentEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		secret := map[string]bool{}
		for _, n := range as.AgentSecretNames {
			secret[n] = true
		}
		for _, k := range keys {
			c.ExtraEnv = append(c.ExtraEnv, k+"="+as.AgentEnv[k])
			if secret[k] && as.AgentEnv[k] != "" {
				c.SecretValues = append(c.SecretValues, as.AgentEnv[k])
			}
		}
	}
	c.ExtraArgs = append([]string{}, as.AgentArgs...)
	if as.AgentCommand != "" && c.Adapter == AdapterWrap {
		c.AgentCmd = as.AgentCommand
	}
	return c
}

// secretMasker — подмена значений секретов в потоке транскрипта: вывод
// агента не должен унести ключ подключения в хранилище. Хвост чанка длиной
// до самого длинного секрета придерживается до следующего чанка или
// flush: секрет, разрезанный границей чанков, всё равно маскируется.
type secretMasker struct {
	values [][]byte
	keep   int
	tail   []byte
	sink   func([]byte)
}

func newSecretMasker(values []string, sink func([]byte)) *secretMasker {
	m := &secretMasker{sink: sink}
	for _, v := range values {
		if v == "" {
			continue
		}
		m.values = append(m.values, []byte(v))
		if len(v) > m.keep {
			m.keep = len(v)
		}
	}
	if m.keep > 0 {
		m.keep--
	}
	return m
}

func (m *secretMasker) mask(b []byte) []byte {
	for _, v := range m.values {
		b = bytes.ReplaceAll(b, v, []byte("***"))
	}
	return b
}

func (m *secretMasker) feed(b []byte) {
	if len(m.values) == 0 {
		m.sink(b)
		return
	}
	buf := m.mask(append(m.tail, b...))
	if len(buf) > m.keep {
		m.sink(buf[:len(buf)-m.keep])
		m.tail = append([]byte{}, buf[len(buf)-m.keep:]...)
	} else {
		m.tail = buf
	}
}

func (m *secretMasker) flush() {
	if len(m.tail) > 0 {
		m.sink(m.mask(m.tail))
		m.tail = nil
	}
}

// maskString — маскирование секретов в тексте итога и шагов.
func (m *secretMasker) maskString(s string) string {
	if len(m.values) == 0 {
		return s
	}
	return string(m.mask([]byte(s)))
}

// maskSecrets — маскирование без переноса (тесты и одиночные строки).
func maskSecrets(values []string, sink func([]byte)) func([]byte) {
	m := newSecretMasker(values, sink)
	return func(b []byte) { m.feed(b); m.flush() }
}

func (a *agent) session(ctx context.Context) error {
	// Стадии прошлого подключения отменены вместе с его каналом: их откат
	// должен закончиться до новых назначений в те же каталоги.
	if err := a.nextGeneration(ctx); err != nil {
		return err
	}
	bootID := newMsgID()
	// Токен регистрации — на обоих RPC (api-contract, протокол v5).
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.Token)
	reg, err := a.client.Register(ctx, &pb.RegisterRequest{
		RunnerId: a.cfg.ID, Agent: a.cfg.Agent, Model: a.cfg.Model, Models: a.cfg.models(),
		Stages: supportedStages,
		Host:   hostname(), Capabilities: a.cfg.Capabilities, ProtocolVersion: protocolVersion,
		Adapter: a.cfg.Adapter, Depth: a.cfg.depth(),
		ContextChannel:  a.cfg.contextChannel(),
		DisplayName:     a.cfg.Name,
		Features:        []string{featureRoles},
		BootId:          bootID,
		PendingSessions: a.pendingSessions(),
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
	if a.cancel == nil {
		a.cancel = map[string]*stageCancel{}
	}
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
			as := k.Assign
			a.goStage(stageInfo{Task: as.TaskId, Session: as.SessionId, Stage: as.Stage.String()},
				func() { a.executeStage(sctx, as, emit) })
		case *pb.PlaneMsg_Deploy:
			dep := k.Deploy
			a.goStage(stageInfo{Session: dep.DeploymentId, Stage: "DEPLOY"},
				func() { a.executeDeploy(sctx, dep, emit) })
		case *pb.PlaneMsg_Cancel:
			a.mu.Lock()
			if c := a.cancel[k.Cancel.TaskId]; c != nil {
				c.byPlane.Store(true)
				c.cancel()
			}
			a.mu.Unlock()
		case *pb.PlaneMsg_Context:
			// Контекст работающему агенту: очередь заберёт адаптер на
			// ближайшем хуке. Нет очереди — стадия уже закончилась.
			if !a.contexts.push(k.Context.SessionId, k.Context.Text) {
				slog.Debug("контекст без активной стадии — отброшен",
					"session", k.Context.SessionId, "kind", k.Context.Kind)
			}
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

// pendingSessions — сессии, результаты которых лежат в журнале и ещё не
// доставлены (прошлый процесс runner'а): control plane их не снимает при
// перезапуске, журнал проиграется после регистрации.
func (a *agent) pendingSessions() []string {
	if a.outbox == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range a.outbox.pending() {
		sid := ""
		switch k := m.Kind.(type) {
		case *pb.RunnerMsg_StageResult:
			sid = k.StageResult.GetSessionId()
		case *pb.RunnerMsg_Blocked:
			sid = k.Blocked.GetSessionId()
		}
		if sid != "" && !seen[sid] {
			seen[sid] = true
			out = append(out, sid)
		}
	}
	return out
}
