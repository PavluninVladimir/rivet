package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Подключение модели агентской стадии (fix-agent-without-connection): стадия
// агента Rivet не назначается, пока у её модели нет рабочего подключения, а
// задача ждёт с причиной. Проверка идёт до назначения (screenConnections):
// ждущий запуск не занимает runner'а, агент команды без рабочей модели
// уступает коллеге. Назначение проверяет ещё раз по выбранному runner'у
// (stageEndpoint). Если стадия всё же упала из-за подключения (ключ не
// дошёл до runner'а, провайдер отклонил ключ), задача эскалируется кодом
// MODEL_CONNECTION, а не TEST_FAILED. Подключение переводит в invalid
// только control plane по своей проверке ключа (resolveConnectionFailure).

// connectionsPlace — где администратор чинит подключение.
const connectionsPlace = "Управление → Подключения"

// connRetryPause — пауза между проверками подключения у ждущего запуска:
// без роста, чтобы задача пошла вскоре после того, как подключение
// появится.
const connRetryPause = 15 * time.Second

// verdictModelConnection — вердикт runner'а: агент не запустился из-за
// подключения модели (нет ключа, адреса, неизвестный тип API) или
// провайдер отклонил ключ.
const verdictModelConnection = "connection"

// ModelConnectionError — у модели стадии нет рабочего подключения: стадия
// не назначается. Reason — причина ожидания для человека. Agent — агент
// команды, чья модель не работает: запуск может взять другой агент роли.
type ModelConnectionError struct {
	Reason       string
	ConnectionID string
	Model        string
	Agent        string
}

func (e *ModelConnectionError) Error() string { return e.Reason }

// stageModel — модель стадии до привязки к подключению: явная модель
// участника, модель агента команды (планировщик подобрал runner с ней) или
// модель шаблона и переопределения проекта, если runner её поддерживает.
func stageModel(run store.StepRun, perf performer, runner domain.Runner) string {
	switch {
	case run.Model != "":
		return run.Model
	case perf.agent != nil && perf.agent.Model != "":
		return perf.agent.Model
	case run.PrefModel != "" && (contains(runner.Models, run.PrefModel) || runner.Model == run.PrefModel):
		return run.PrefModel
	}
	return ""
}

// needsModel — стадии нужна модель: шаг test без роли только запускает
// проверки проекта и к модели не обращается (то же правило, что у runner'а).
func needsModel(stage pb.StageResult_Stage, perf performer) bool {
	return stage != pb.StageResult_TESTING || perf.role != nil
}

// nativeModel — модель стадии агента Rivet: профиль, привязка (подключение
// и модель) и признак доставки ключа в назначении.
type nativeModel struct {
	prof    domain.AgentProfile
	binding domain.AgentModelRef
	// ok — агент Rivet есть в каталоге.
	ok bool
	// include — ключ и секретные заголовки уходят runner'у в назначении:
	// режим always или secure на защищённом канале.
	include bool
}

// resolveNativeModel — привязка модели стадии: явная модель участника,
// пара агента команды, переопределение проекта, модель по умолчанию.
// ok=false — агента Rivet нет в каталоге.
func (e *Engine) resolveNativeModel(ctx context.Context, run store.StepRun, perf performer, runner domain.Runner,
	agentModels map[string]policy.AgentModel, model string) (nativeModel, error) {
	prof, err := e.St.GetAgent(ctx, store.AgentRivet)
	if errors.Is(err, store.ErrNotFound) {
		return nativeModel{}, nil
	}
	if err != nil {
		return nativeModel{}, err
	}
	return nativeModelIn(prof, run, perf, runner, agentModels, model), nil
}

// nativeModelIn — resolveNativeModel по уже прочитанному профилю агента
// Rivet (проход планировщика читает его один раз).
func nativeModelIn(prof domain.AgentProfile, run store.StepRun, perf performer, runner domain.Runner,
	agentModels map[string]policy.AgentModel, model string) nativeModel {
	var override *policy.AgentModel
	if am, ok := agentModels[store.AgentRivet]; ok {
		override = &am
	}
	explicit := model
	// Модель агента команды — пара с подключением: привязка ищется по паре,
	// а не по одному имени модели (одна модель может быть у двух
	// подключений).
	if perf.agent != nil && perf.agent.ConnectionID != "" && perf.agent.Model != "" && run.Model == "" {
		override = &policy.AgentModel{ConnectionID: perf.agent.ConnectionID, Model: perf.agent.Model}
		explicit = ""
	}
	binding := store.ResolveAgentModelIn(prof, explicit, override)
	if explicit == "" && perf.agent != nil && perf.agent.Model != "" && binding.Model != perf.agent.Model {
		// Пары нет в привязках профиля runner'а: модель — по имени.
		binding = store.ResolveAgentModelIn(prof, perf.agent.Model, nil)
	}
	return nativeModel{prof: prof, binding: binding, ok: true,
		include: prof.Secrets == "always" || (prof.Secrets == "secure" && runner.Secure)}
}

// connRead — подключение, прочитанное один раз: по нему и проверка, и
// сборка endpoint назначения.
type connRead struct {
	cl  llm.Client
	c   domain.ModelConnection
	err error
}

func (e *Engine) readConn(ctx context.Context, id string) connRead {
	cl, c, err := e.St.ConnectionClient(ctx, id, e.Box)
	return connRead{cl: cl, c: c, err: err}
}

// connProblem — почему у модели нет рабочего подключения; пусто —
// подключение рабочее. Без привязки причина называет, что с моделью в
// каталоге (нет, выключена, скрыта). include — ключ каталога уходит
// runner'у: без этого (режим never, secure на открытом канале) ключом
// занимается runner, и здесь проверяется только, что подключение есть и
// включено. Ключ нужен по тому же правилу, что у runner'а: без ключа он не
// идёт на нелокальный адрес. fallback — имя модели для текста, если
// привязки нет (модель участника или runner'а по умолчанию).
func (e *Engine) connProblem(ctx context.Context, b domain.AgentModelRef, fallback string, include bool, rd *connRead) (string, error) {
	model := firstNonEmpty(b.Model, fallback)
	if b.ConnectionID == "" {
		return e.modelAbsence(ctx, model)
	}
	if rd == nil {
		r := e.readConn(ctx, b.ConnectionID)
		rd = &r
	}
	switch {
	case errors.Is(rd.err, store.ErrNotFound):
		return fmt.Sprintf("нет подключения %s к модели %s: добавьте его в %s", b.ConnectionID, model, connectionsPlace), nil
	case errors.Is(rd.err, store.ErrSecret):
		if !include {
			break
		}
		if !e.Box.Enabled() {
			return fmt.Sprintf("у rivetd не задан ключ шифрования (RIVET_SECRET_KEY): ключ подключения %s к модели %s не расшифровать", b.ConnectionID, model), nil
		}
		return fmt.Sprintf("ключ подключения %s к модели %s не расшифровать: задайте его заново в %s", b.ConnectionID, model, connectionsPlace), nil
	case rd.err != nil:
		return "", rd.err
	}
	c, cl := rd.c, rd.cl
	switch {
	case !c.Enabled:
		return fmt.Sprintf("подключение %s к модели %s выключено: включите его в %s", b.ConnectionID, model, connectionsPlace), nil
	case !include:
		return "", nil
	case c.State == domain.LLMStateInvalid:
		return fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s: обновите ключ в %s", b.ConnectionID, model, connectionsPlace), nil
	case cl.Key == "" && !llm.IsLocalURL(c.BaseURL) && c.Kind == "local":
		// Без ключа runner идёт только на сервер своей машины (localhost).
		return fmt.Sprintf("у подключения %s к модели %s нет ключа, а без ключа runner обращается только к серверу на своей машине: добавьте ключ или локальный адрес в %s",
			b.ConnectionID, model, connectionsPlace), nil
	case cl.Key == "" && !llm.IsLocalURL(c.BaseURL):
		return fmt.Sprintf("у подключения %s к модели %s нет ключа: добавьте ключ в %s", b.ConnectionID, model, connectionsPlace), nil
	}
	return "", nil
}

// modelAbsence — причина для модели без привязки к доступному
// подключению: подключение выключено, модель скрыта или пропала, или
// подключения к модели нет вовсе.
func (e *Engine) modelAbsence(ctx context.Context, model string) (string, error) {
	if model == "" {
		return "у агента Rivet нет доступной модели: добавьте подключение к модели в " + connectionsPlace, nil
	}
	conns, err := e.St.ListConnections(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range conns {
		for _, m := range c.Models {
			if m.ID != model {
				continue
			}
			switch {
			case !c.Enabled:
				return fmt.Sprintf("подключение %s к модели %s выключено: включите его в %s", c.ID, model, connectionsPlace), nil
			case m.Hidden:
				return fmt.Sprintf("модель %s скрыта в подключении %s: откройте её в %s", model, c.ID, connectionsPlace), nil
			case m.Missing:
				return fmt.Sprintf("модель %s пропала у провайдера подключения %s: обновите список моделей в %s", model, c.ID, connectionsPlace), nil
			}
		}
	}
	return fmt.Sprintf("нет подключения к модели %s: добавьте его в %s", model, connectionsPlace), nil
}

// stageEndpoint — endpoint назначения агенту Rivet по одному чтению
// подключения. Стадии, которой модель не нужна (шаг test без роли), и
// runner'у без агентского цикла endpoint не нужен. ModelConnectionError —
// подключение не рабочее или endpoint не собрался: назначение без endpoint
// не уходит, иначе runner взял бы адрес и ключ из своего окружения.
func (e *Engine) stageEndpoint(ctx context.Context, run store.StepRun, perf performer, runner domain.Runner,
	agentModels map[string]policy.AgentModel, model string, stage pb.StageResult_Stage) (*store.ModelEndpoint, string, error) {
	if !slices.Contains(runner.Features, featureNative) || !needsModel(stage, perf) {
		return nil, model, nil
	}
	nm, err := e.resolveNativeModel(ctx, run, perf, runner, agentModels, model)
	if err != nil {
		slog.Error("dispatch: профиль агента", "agent", store.AgentRivet, "err", err)
		return nil, model, &ModelConnectionError{Reason: "модель стадии не определена: профиль агента Rivet не прочитан", Model: model}
	}
	if !nm.ok {
		return nil, model, &ModelConnectionError{Reason: "агента Rivet нет в каталоге агентов: модель стадии не определить", Model: model}
	}
	wait := func(reason string) error {
		mc := &ModelConnectionError{Reason: reason, ConnectionID: nm.binding.ConnectionID,
			Model: firstNonEmpty(nm.binding.Model, model, runner.Model)}
		// Модель агента команды не работает: запуск возьмёт другой агент роли.
		if perf.agent != nil && perf.agent.Model != "" && run.Model == "" {
			mc.Agent = perf.agent.ID
		}
		return mc
	}
	var rd *connRead
	if nm.binding.ConnectionID != "" {
		r := e.readConn(ctx, nm.binding.ConnectionID)
		rd = &r
	}
	problem, err := e.connProblem(ctx, nm.binding, firstNonEmpty(model, runner.Model), nm.include, rd)
	if err != nil {
		return nil, model, wait("состояние подключения модели не прочитано: " + err.Error())
	}
	if problem != "" {
		return nil, model, wait(problem)
	}
	var ep store.ModelEndpoint
	if rd == nil {
		ep = store.ModelEndpointFrom(nm.prof, nm.binding, llm.Client{}, domain.ModelConnection{}, false, false)
	} else {
		ep = store.ModelEndpointFrom(nm.prof, nm.binding, rd.cl, rd.c, rd.err == nil, nm.include)
	}
	if nm.binding.Model != "" {
		model = nm.binding.Model
	}
	return &ep, model, nil
}

// screen — проверка подключений на один проход планировщика: модели, роли,
// политики проектов и подключения читаются один раз.
type screen struct {
	e        *Engine
	prof     domain.AgentProfile
	runner   domain.Runner // типичный runner агента Rivet: модели каталога, канал
	projects map[string]string
	policies map[string]map[string]policy.AgentModel
	roles    map[string]performer
	models   map[string]nativeModel
	conns    map[string]*connRead
	absent   map[string]string
}

func newScreen(e *Engine, prof domain.AgentProfile, runner domain.Runner) *screen {
	return &screen{e: e, prof: prof, runner: runner, projects: map[string]string{},
		policies: map[string]map[string]policy.AgentModel{}, roles: map[string]performer{},
		models: map[string]nativeModel{}, conns: map[string]*connRead{}, absent: map[string]string{}}
}

func (sc *screen) project(ctx context.Context, taskID string) string {
	if id, ok := sc.projects[taskID]; ok {
		return id
	}
	projectID, _, err := sc.e.St.TaskRefs(ctx, taskID)
	if err != nil {
		return ""
	}
	sc.projects[taskID] = projectID
	return projectID
}

// resolve — привязка модели запуска с кэшем прохода: модель зависит от
// модели участника, предпочтения роли и переопределения проекта.
func (sc *screen) resolve(ctx context.Context, run store.StepRun, perf performer, model string) nativeModel {
	projectID := sc.project(ctx, run.TaskID)
	key := run.Model + "\x00" + run.PrefModel + "\x00" + model + "\x00" + projectID
	if nm, ok := sc.models[key]; ok {
		return nm
	}
	nm := nativeModelIn(sc.prof, run, perf, sc.runner, sc.agentModels(ctx, run.TaskID), model)
	sc.models[key] = nm
	return nm
}

func (sc *screen) agentModels(ctx context.Context, taskID string) map[string]policy.AgentModel {
	projectID := sc.project(ctx, taskID)
	if projectID == "" {
		return nil
	}
	if m, ok := sc.policies[projectID]; ok {
		return m
	}
	var m map[string]policy.AgentModel
	if eff, err := sc.e.St.EffectivePolicy(ctx, projectID); err == nil {
		m = eff.Presets.AgentModels
	}
	sc.policies[projectID] = m
	return m
}

func (sc *screen) performer(ctx context.Context, run store.StepRun) performer {
	if p, ok := sc.roles[run.Role]; ok {
		return p
	}
	p := sc.e.runPerformer(ctx, store.StepRun{ID: run.ID, Role: run.Role})
	sc.roles[run.Role] = p
	return p
}

// problem — проверка привязки с кэшем подключений прохода.
func (sc *screen) problem(ctx context.Context, nm nativeModel, fallback string) (string, error) {
	b := nm.binding
	if b.ConnectionID == "" {
		model := firstNonEmpty(b.Model, fallback)
		if r, ok := sc.absent[model]; ok {
			return r, nil
		}
		r, err := sc.e.modelAbsence(ctx, model)
		if err == nil {
			sc.absent[model] = r
		}
		return r, err
	}
	rd, ok := sc.conns[b.ConnectionID]
	if !ok {
		r := sc.e.readConn(ctx, b.ConnectionID)
		rd = &r
		sc.conns[b.ConnectionID] = rd
	}
	include := nm.prof.Secrets == "always" || (nm.prof.Secrets == "secure" && sc.runner.Secure)
	return sc.e.connProblem(ctx, b, fallback, include, rd)
}

// screenConnections — до назначений прохода: запуски агента Rivet, у модели
// которых нет рабочего подключения, откладываются с причиной без runner'а
// и агента команды, а агенты команды без рабочей модели исключаются из
// назначений прохода (их запуск возьмёт другой агент роли). Ключ каталога
// проверяется строго, если все свободные runner'ы агента Rivet на
// защищённом канале (ключ уходит в назначении); решения, которые зависят
// от выбранного runner'а, остаются проверке назначения. Возвращает агентов
// без рабочей модели с причиной.
func (e *Engine) screenConnections(ctx context.Context, excluded, excludedEpics []string) map[string]string {
	runners, err := e.St.ListRunners(ctx)
	if err != nil {
		slog.Error("проверка подключений: runner'ы", "err", err)
		return nil
	}
	idle, secure := false, true
	for _, r := range runners {
		if r.Agent == "" || r.Retired || r.Status != domain.RunnerIdle || r.Draining || r.TaskID != "" ||
			!slices.Contains(r.Features, featureNative) {
			continue
		}
		idle = true
		secure = secure && r.Secure
	}
	if !idle {
		return nil
	}
	runs, err := e.St.PendingAgentRuns(ctx, excluded, excludedEpics)
	if err != nil {
		slog.Error("проверка подключений: запуски", "err", err)
		return nil
	}
	if len(runs) == 0 {
		return nil
	}
	prof, err := e.St.GetAgent(ctx, store.AgentRivet)
	if err != nil {
		return nil
	}
	models := make([]string, 0, len(prof.Models))
	for _, m := range prof.Models {
		models = append(models, m.Model)
	}
	sc := newScreen(e, prof, domain.Runner{Features: []string{featureNative}, Models: models, Secure: secure})

	// Агенты команды со своей моделью: её подключение не зависит от задачи.
	team, err := e.St.ListTeam(ctx)
	if err != nil {
		slog.Error("проверка подключений: команда", "err", err)
		return nil
	}
	badAgents := map[string]string{}
	byRole := map[string][]domain.TeamAgent{}
	for _, a := range team {
		if !a.Enabled {
			continue
		}
		byRole[a.RoleID] = append(byRole[a.RoleID], a)
		if a.Model == "" {
			continue
		}
		agent := a
		nm := nativeModelIn(prof, store.StepRun{}, performer{agent: &agent}, sc.runner, nil, a.Model)
		problem, err := sc.problem(ctx, nm, a.Model)
		if err != nil {
			slog.Error("проверка подключений: агент команды", "agent", a.ID, "err", err)
			continue
		}
		if problem != "" {
			badAgents[a.ID] = problem
		}
	}
	for _, run := range runs {
		// Модель агента команды важна, только если у участника нет своей.
		if agents := byRole[run.Role]; run.Role != "" && run.Model == "" && len(agents) > 0 {
			healthy, modelless := false, false
			firstProblem := ""
			for _, a := range agents {
				switch {
				case a.Model == "":
					modelless = true
				case badAgents[a.ID] == "":
					healthy = true
				case firstProblem == "":
					firstProblem = badAgents[a.ID]
				}
			}
			if healthy {
				continue
			}
			if !modelless {
				e.deferPendingRun(ctx, run, "нет агента роли «"+sc.roleTitle(ctx, run)+"» с рабочим подключением к модели: "+firstProblem, "", "")
				continue
			}
		}
		perf := sc.performer(ctx, run)
		if run.StepKind == policy.StepTest && perf.role == nil {
			continue
		}
		model := stageModel(run, perf, sc.runner)
		nm := sc.resolve(ctx, run, perf, model)
		// Без модели вовсе решает модель runner'а по умолчанию: это
		// проверка назначения.
		if nm.binding.ConnectionID == "" && firstNonEmpty(nm.binding.Model, model) == "" {
			continue
		}
		problem, err := sc.problem(ctx, nm, model)
		if err != nil {
			slog.Error("проверка подключений: запуск", "run", run.ID, "err", err)
			continue
		}
		if problem != "" {
			e.deferPendingRun(ctx, run, problem, nm.binding.ConnectionID, firstNonEmpty(nm.binding.Model, model))
		}
	}
	return badAgents
}

// roleTitle — название роли запуска для причины ожидания.
func (sc *screen) roleTitle(ctx context.Context, run store.StepRun) string {
	if p := sc.performer(ctx, run); p.role != nil && p.role.Title != "" {
		return p.role.Title
	}
	return run.Role
}

// deferAgentMisses — запуски, которые в проходе получили отказ по модели
// агента команды и так и не назначились (смешанные каналы runner'ов, сбой
// чтения подключения), ждут с причиной: иначе их назначали бы и снимали
// каждый проход.
func (e *Engine) deferAgentMisses(ctx context.Context, misses map[int64]agentMiss) {
	for _, m := range misses {
		e.deferPendingRun(ctx, m.run, "нет свободного агента роли с рабочим подключением к модели: "+m.err.Reason, m.err.ConnectionID, m.err.Model)
	}
}

// agentMiss — отказ назначения по модели агента команды.
type agentMiss struct {
	run store.StepRun
	err *ModelConnectionError
}

// deferPendingRun — запуск без runner'а ждёт подключения: пауза без роста,
// причина на задаче, событие task.wait одно на серию с той же причиной.
func (e *Engine) deferPendingRun(ctx context.Context, run store.StepRun, reason, connID, model string) {
	deferred, first, pause, err := e.St.DeferPendingRun(ctx, run.ID, run.TaskID, connRetryPause, connRetryPause, reason)
	if err != nil {
		slog.Error("ожидание подключения: откладывание", "run", run.ID, "err", err)
		return
	}
	if !deferred || !first {
		return
	}
	e.waitEvent(ctx, run.TaskID, run, reason, pause, map[string]any{"wait": "model_connection", "connection": connID, "model": model})
}

// sessionRunnerCheck — проверка подключения модели сессии доработки по
// runner'у, которого выбрал St.StartUserSession, до любых изменений задачи:
// сессия идёт на шаге code без роли, модель — участника шага или по
// умолчанию. Отказ — ModelConnectionError, транзакция сессии откатывается.
func (e *Engine) sessionRunnerCheck(ctx context.Context, task domain.Task) func(runnerID string) error {
	return func(runnerID string) error {
		proc, _, err := e.processFor(ctx, task)
		if err != nil {
			return nil
		}
		codeID := nearestCodeBefore(proc, task.StepID)
		if codeID == policy.TargetEscalate {
			codeID = nearestCodeBefore(proc, mergeStepID(proc))
		}
		step, ok := proc.Step(codeID)
		if !ok || len(step.Participants) == 0 {
			return nil
		}
		runner, err := e.St.GetRunner(ctx, runnerID)
		if err != nil {
			return nil
		}
		var agentModels map[string]policy.AgentModel
		if projectID, _, err := e.St.TaskRefs(ctx, task.ID); err == nil {
			if eff, err := e.St.EffectivePolicy(ctx, projectID); err == nil {
				agentModels = eff.Presets.AgentModels
			}
		}
		run := store.StepRun{TaskID: task.ID, StepKind: policy.StepCode, Model: step.Participants[0].Agent.Model}
		_, _, err = e.stageEndpoint(ctx, run, performer{}, runner, agentModels, stageModel(run, performer{}, runner), pb.StageResult_FIXING)
		var mc *ModelConnectionError
		if errors.As(err, &mc) {
			return &ModelConnectionError{Reason: "сессию не удалось запустить: " + mc.Reason, ConnectionID: mc.ConnectionID, Model: mc.Model}
		}
		return nil
	}
}

// modelConnectionFailed — участник шага не запустился из-за подключения
// модели (вердикт runner'а).
func modelConnectionFailed(verdicts []map[string]any) bool {
	for _, v := range verdicts {
		if av, _ := v["agent_verdict"].(string); av == verdictModelConnection {
			return true
		}
	}
	return false
}

// Итоги разбора отказа стадии по подключению.
const (
	connFailEscalate = iota // эскалация MODEL_CONNECTION
	connFailWait            // подключение уже invalid: задача возвращается на шаг и ждёт его
)

// connFailure — разбор отказа стадии по подключению.
type connFailure struct {
	reason string
	kind   int
}

// target — фактический переход задачи: тот же шаг или эскалация.
func (f connFailure) target(step string) string {
	if f.kind == connFailWait {
		return step
	}
	return policy.TargetEscalate
}

// connCheckTimeout — предел проверки ключа control plane'ом после отказа
// провайдера во время стадии.
const connCheckTimeout = 15 * time.Second

// checkKey — проверка ключа подключения control plane'ом: список моделей
// у провайдера, как у кнопки «Проверить» в консоли.
func (e *Engine) checkKey(ctx context.Context, cl llm.Client) error {
	ctx, cancel := context.WithTimeout(ctx, connCheckTimeout)
	defer cancel()
	if e.CheckKey != nil {
		return e.CheckKey(ctx, cl)
	}
	_, err := cl.ListModels(ctx)
	return err
}

// onModelConnectionFailure — стадия упала из-за подключения модели (разбор
// уже сделан resolveConnectionFailure). Подключение уже invalid — задача
// без эскалации возвращается на тот же шаг с тем же входом и ждёт
// подключения, как остальные. Иначе эскалация MODEL_CONNECTION. Попытка не
// расходуется, переход fail не применяется, контекст стадии (замечания
// review, отчёт проверок) остаётся: стадия не шла, повтор шага получит тот
// же контекст.
func (e *Engine) onModelConnectionFailure(ctx context.Context, task domain.Task, proc *policy.Resolved, hash string,
	step policy.ResolvedStep, f connFailure, lastRunner string, paused bool, evPayload map[string]any) error {
	payload := map[string]any{"reason": "model_connection"}
	for k, v := range evPayload {
		payload[k] = v
	}
	if f.kind == connFailWait {
		payload["detail"] = f.reason
		return e.gotoTarget(ctx, task, proc, hash, step.ID, task.StepEntry, lastRunner, paused, payload, "",
			&store.EventInput{Text: "агент не запустился: " + f.reason + " — задача ждёт подключения", Payload: payload})
	}
	return e.failTaskWith(ctx, task, domain.AttModelConnection, domain.ActorRunner, lastRunner,
		"агент не запустился: "+f.reason, payload)
}

// resolveConnectionFailure — почему агент не запустился по подключению:
// разбор по тому, с чем стадия ушла runner'у (запуск хранит подключение,
// модель и признак, что ключ каталога передан), а не по текущим
// настройкам: модель проекта и канал runner'а могли смениться, пока
// стадия шла.
//
// Провайдер отклонил ключ каталога (HTTP 401): control plane сам проверяет
// подключение списком моделей и переводит его в invalid только по отказу
// 401 своей проверки. Перевёл этот разбор — эскалация, одна на перевод.
// Подключение уже invalid — задача ждёт его без эскалации. Отказ 403
// (права ключа, регион, блокировка сети) и отказ ключа runner'а (ключ
// каталога не передавался) подключение каталога не трогают: эскалация с
// причиной.
func (e *Engine) resolveConnectionFailure(ctx context.Context, task domain.Task, step policy.ResolvedStep) connFailure {
	generic := connFailure{reason: "у модели стадии нет рабочего подключения или ключа: проверьте подключение в " + connectionsPlace}
	runs, err := e.St.StepRuns(ctx, task.ID, step.ID, task.StepGen)
	if err != nil {
		slog.Error("подключение модели: запуски шага", "task", task.ID, "err", err)
		return generic
	}
	var run *store.StepRun
	for i := range runs {
		if runs[i].AgentVerdict == verdictModelConnection {
			run = &runs[i]
		}
	}
	if run == nil || run.RunnerID == "" || run.AssignedConnection == "" {
		return generic
	}
	name := run.RunnerID
	if r, err := e.St.GetRunner(ctx, run.RunnerID); err == nil {
		name = r.Label()
	}
	connID := run.AssignedConnection
	model := firstNonEmpty(run.AssignedModel, run.Model)
	status := llm.RejectionStatus(run.Detail)
	env := "RIVET_PROVIDER_KEY_" + envSuffix(connID)
	if !run.CatalogKey {
		// Ключ каталога runner'у не передавался: runner шёл со своим ключом
		// или без ключа. Подключение каталога тут ни при чём, кроме случая,
		// когда ключа в каталоге нет вовсе (локальный сервер, которому
		// включили ключ).
		if status != 0 {
			if cl, _, err := e.St.ConnectionClient(ctx, connID, e.Box); err == nil && cl.Key == "" {
				return connFailure{reason: fmt.Sprintf("сервер модели подключения %s отклонил запрос runner'а %s (HTTP %d): у подключения в каталоге нет ключа, добавьте ключ в %s или задайте runner'у переменную %s",
					connID, name, status, connectionsPlace, env)}
			}
			return connFailure{reason: fmt.Sprintf("провайдер отклонил ключ runner'а %s для подключения %s к модели %s (HTTP %d): ключ каталога runner'у не передавался, обновите переменную %s на runner'е",
				name, connID, model, status, env)}
		}
		if prof, err := e.St.GetAgent(ctx, store.AgentRivet); err == nil && prof.Secrets == "never" {
			return connFailure{reason: fmt.Sprintf("runner %s не получил ключ подключения %s: ключи runner'ам не передаются (режим секретов агента Rivet never), задайте runner'у переменную %s",
				name, connID, env)}
		}
		return connFailure{reason: fmt.Sprintf("runner %s не получил ключ подключения %s: ключ передаётся только runner'ам с защищённым каналом (TLS или та же машина), подключите runner по TLS или задайте ему переменную %s",
			name, connID, env)}
	}
	switch status {
	case http.StatusUnauthorized:
		return e.verifyRejectedKey(ctx, task, connID, model, name)
	case http.StatusForbidden:
		return connFailure{reason: fmt.Sprintf("провайдер отказал ключу подключения %s в доступе к модели %s (HTTP 403): проверьте права ключа на модель и доступ из сети runner'а %s (регион, блокировки)",
			connID, model, name)}
	}
	// Ключ каталога ушёл, а агент не собрал подключение: если подключение
	// с тех пор сломалось (удалили ключ, выключили), причина называет это.
	if problem, err := e.connProblem(ctx, domain.AgentModelRef{ConnectionID: connID, Model: model}, model, true, nil); err == nil && problem != "" {
		return connFailure{reason: problem}
	}
	return connFailure{reason: fmt.Sprintf("агент на runner'е %s не смог обратиться к подключению %s к модели %s: проверьте адрес и тип API подключения в %s",
		name, connID, model, connectionsPlace)}
}

// verifyRejectedKey — провайдер отклонил ключ каталога во время стадии
// (HTTP 401): control plane проверяет ключ сам. Одного отказа на одном
// runner'е мало, чтобы остановить все стадии на подключении.
func (e *Engine) verifyRejectedKey(ctx context.Context, task domain.Task, connID, model, runner string) connFailure {
	cl, c, err := e.St.ConnectionClient(ctx, connID, e.Box)
	if err != nil {
		return connFailure{reason: fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s на runner'е %s (HTTP 401), а проверить ключ control plane не смог (%v): проверьте подключение в %s",
			connID, model, runner, err, connectionsPlace)}
	}
	invalid := connFailure{kind: connFailWait,
		reason: fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s: обновите ключ в %s", connID, model, connectionsPlace)}
	if c.State == domain.LLMStateInvalid {
		return invalid
	}
	if llm.IsLocalURL(c.BaseURL) {
		// Локальный адрес — машина runner'а: control plane проверил бы свой
		// localhost, а не сервер, который отклонил ключ.
		return connFailure{reason: fmt.Sprintf("сервер модели подключения %s на машине runner'а %s отклонил ключ (HTTP 401): control plane такой сервер не проверяет, сверьте ключ сервера модели на runner'е и ключ подключения в %s",
			connID, runner, connectionsPlace)}
	}
	err = e.checkKey(ctx, cl)
	switch {
	case llm.KeyRejected(err):
		marked, merr := e.St.MarkConnectionRejected(ctx, connID,
			fmt.Sprintf("стадия задачи task-%d и проверка control plane: %v", task.Num, err))
		if merr != nil {
			slog.Error("подключение модели: отметка invalid", "connection", connID, "err", merr)
		}
		if merr == nil && !marked {
			// Подключение перевёл в invalid разбор другой стадии: эскалация
			// уже есть, эта задача ждёт подключения.
			return invalid
		}
		return connFailure{reason: fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s, проверка control plane подтвердила отказ (HTTP 401): подключение переведено в invalid, обновите ключ в %s",
			connID, model, connectionsPlace)}
	case err == nil:
		// Список моделей у части провайдеров открыт без ключа, а ключ могли
		// заменить, пока шла стадия: отказ во время стадии всё равно был.
		return connFailure{reason: fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s во время стадии на runner'е %s (HTTP 401), а список моделей control plane получил: провайдер мог не проверять ключ на списке моделей, или ключ заменили, пока шла стадия. Подключение не переведено в invalid, проверьте ключ в %s и повторите задачу",
			connID, model, runner, connectionsPlace)}
	}
	return connFailure{reason: fmt.Sprintf("провайдер отклонил ключ подключения %s к модели %s на runner'е %s (HTTP 401), а проверка control plane ответа не дала (%v): подключение не переведено в invalid, проверьте ключ в %s",
		connID, model, runner, err, connectionsPlace)}
}

// envSuffix — суффикс переменной ключа подключения в окружении runner'а
// (то же правило, что у runner'а: латиница и цифры, остальное — _).
func envSuffix(connection string) string {
	out := make([]byte, 0, len(connection))
	for _, r := range connection {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, byte(r-'a'+'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// modelAvailability — доступность моделей каталога для причин ожидания
// (markWaiting): модели и runner'ы агента Rivet читаются один раз.
type modelAvailability struct {
	e       *Engine
	loaded  bool
	native  bool
	catalog map[string]bool
	team    []domain.TeamAgent
}

func (a *modelAvailability) load(ctx context.Context) {
	if a.loaded {
		return
	}
	a.loaded = true
	runners, err := a.e.St.ListRunners(ctx)
	if err != nil {
		return
	}
	for _, r := range runners {
		if r.Agent != "" && !r.Retired && r.Status != domain.RunnerOffline && slices.Contains(r.Features, featureNative) {
			a.native = true
		}
	}
	prof, err := a.e.St.GetAgent(ctx, store.AgentRivet)
	if err != nil {
		a.native = false
		return
	}
	a.catalog = map[string]bool{}
	for _, m := range prof.Models {
		a.catalog[m.Model] = true
	}
	a.team, _ = a.e.St.ListTeam(ctx)
}

// reason — причина про подключение, если runner'ы агента Rivet есть, а
// модели нет среди доступных моделей каталога; иначе пусто.
func (a *modelAvailability) reason(ctx context.Context, model string) string {
	a.load(ctx)
	if !a.native || a.catalog[model] {
		return ""
	}
	why, err := a.e.modelAbsence(ctx, model)
	if err != nil {
		return ""
	}
	return why
}

// teamReason — у всех включённых агентов роли своя модель, и ни одной нет
// среди доступных моделей каталога: причина по первому агенту.
func (a *modelAvailability) teamReason(ctx context.Context, role string) string {
	a.load(ctx)
	first := ""
	for _, ag := range a.team {
		if !ag.Enabled || ag.RoleID != role {
			continue
		}
		if ag.Model == "" || a.catalog[ag.Model] {
			return ""
		}
		if first == "" {
			first = ag.Model
		}
	}
	if first == "" {
		return ""
	}
	return a.reason(ctx, first)
}

// badTeamReason — причина первого агента роли, исключённого в проходе за
// неработающую модель (только для запусков без явной модели); пусто, если
// таких нет.
func (a *modelAvailability) badTeamReason(ctx context.Context, run store.StepRun, bad map[string]string) string {
	if run.Model != "" || len(bad) == 0 {
		return ""
	}
	a.load(ctx)
	for _, ag := range a.team {
		if ag.Enabled && ag.RoleID == run.Role && bad[ag.ID] != "" {
			return bad[ag.ID]
		}
	}
	return ""
}
