// Package orchestrator — политика конвейера: детерминированные реакции на
// результаты этапов и цикл диспетчеризации. Модель решает задачу,
// оркестратор управляет процессом (спеки backend/orchestration, task-pipeline).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Sender доставляет сообщение подключённому runner'у (реализация — stream.Registry).
type Sender interface {
	Send(runnerID string, msg *pb.PlaneMsg) bool
}

type Engine struct {
	LoadLimits loadtest.Limits
	St         *store.Store
	// SCM — запасной адаптер установки (глобальный токен, fake на стендах);
	// у проекта с учётными данными адаптер берётся через SCMFor.
	SCM      scm.Adapter
	Adapters *scm.Factory
	// Box расшифровывает учётные данные проектов; nil — ключ не настроен.
	Box *secretbox.Box
	// Policy — движок политик: решения гейтов конвейера. Ошибка движка —
	// запрет для автоматики (спека access-policy «Точки принуждения»).
	Policy policy.Engine
	Blob   *blob.Store
	Out    Sender

	HeartbeatTimeout time.Duration
	BaseBranch       string

	mu sync.Mutex
	// контекст следующего шага задачи (вывод тестов, замечания review)
	stageContext map[string]string
	// prWait — когда снова пробовать создать PR задаче, которая вернулась
	// на review без PR (fix-pr-without-test-step).
	prWait map[string]prWaitState
	// prMergeRetryAt — когда снова пробовать создать PR задачам, которые
	// ждут на merge без PR (retryMergePRs).
	prMergeRetryAt time.Time
	// транскрипты открытых сессий по session_id
	transcripts map[string][]byte
	// открытые сессии: session_id → task_id (у задачи может идти несколько
	// сессий сразу — параллельные участники шага)
	sessions map[string]string
	// performers — исполнитель открытой сессии (роль и имя runner'а) для
	// событий её шагов (add-agent-roles)
	performers map[string]sessionPerformer
	// публикации: лог и владелец (runner); фаза отката — durable в БД
	deployLogs  map[string][]byte
	deployOwner map[string]string
	// budgetNotified — по каким проектам и за какие сутки (UTC, YYYY-MM-DD)
	// уже написано policy.budget_exceeded: событие пишется раз в сутки на
	// проект; после рестарта возможен повтор — допустимо (design).
	budgetNotified map[string]string
	// epicBudgetNotified — по каким Epic и значениям бюджета уже написано
	// epic.budget_exceeded: раз на факт превышения, смена бюджета — новый
	// факт; повтор после рестарта допустим (спека orchestration «Бюджет Epic»).
	epicBudgetNotified map[string]bool
	// depsWaiting — Epic'и, о чьём ожидании зависимостей объявлено событием
	// (add-product-discovery): снятие ожидания объявляется только им, в том
	// числе после паузы. depsInit — базовое множество снято: после рестарта
	// оно восстанавливается без событий.
	depsWaiting map[string]bool
	depsInit    bool
	// policySyncedAt — когда последний раз читалась политика проектов из
	// репозитория (git-провайдер); policyBroken — по каким проектам и с
	// какой причиной уже написано, что файл политики сломан (событие раз
	// на причину, а не каждую минуту; повтор после рестарта допустим).
	policySyncedAt time.Time
	policyBroken   map[string]string
	// externalPolled — когда в последний раз опрашивался прогон внешней
	// публикации: тик идёт секундами, пайплайны — минутами.
	externalPolled map[string]time.Time
	// policyDown — проекты, по которым уже написана эскалация «движок
	// политик недоступен»: при первом успешном решении она закрывается.
	// Свой mutex: переходы «упал/поднялся» ходят в базу и должны быть
	// сериализованы между тиком планировщика и публикациями.
	policyMu   sync.Mutex
	policyDown map[string]bool
	// mergeLocks сериализуют операцию merge задачи (вызов хостинга и
	// применение исхода): авто-merge и подтверждения человеком одной задачи
	// идут по очереди (change add-merge-conflict-recovery).
	mergeLocks taskLocks
	// archOnce и archSt — одиночное исполнение обновления снимков
	// архитектуры проектов (add-architecture-c4).
	archOnce sync.Once
	archSt   *archState
	// Now — источник времени для бюджета (подменяется в тестах).
	Now func() time.Time

	// Задания агентов control plane (add-sdlc-cycle): очередь по агенту.
	ops     *opsQueue
	opsOnce sync.Once
	// OpsComplete подменяет вызов модели в тестах (connID, model, prompt).
	OpsComplete func(ctx context.Context, connID, model, prompt string) (string, error)
	// IssueWatch is installed before Run and executes the deterministic SRE
	// intake job. Research execution remains owned by the Discovery service.
	IssueWatch func(ctx context.Context, projectID, agentID string) error
	// CheckKey подменяет в тестах проверку ключа подключения control
	// plane'ом после отказа провайдера во время стадии (список моделей).
	CheckKey func(ctx context.Context, cl llm.Client) error
	// URLPolicy — какие адреса доступны проверкам URL control plane'ом
	// (RIVET_ALLOW_PRIVATE_URLS). Задаётся до первой проверки.
	URLPolicy URLPolicy
	// Infra: провайдеры инфраструктуры хостов окружений
	// (add-environment-provisioning); nil: только встроенные с адресами
	// по умолчанию.
	Infra     *infra.Factory
	urlOnce   sync.Once
	urlClient *http.Client
	// credRecheckAt и credRecheckRunning — последний запуск и признак
	// идущей повторной проверки нерабочих учётных данных хостинга
	// (fix-credential-state); под mu.
	credRecheckAt      time.Time
	credRecheckRunning bool
	// credProbedOK — когда учётные данные (по id) последний раз прошли
	// проверку подключения после неоднозначного отказа; под mu.
	credProbedOK map[string]time.Time
}

func New(st *store.Store, adapter scm.Adapter, bl *blob.Store, send Sender, heartbeat time.Duration) *Engine {
	return &Engine{
		LoadLimits: loadtest.DefaultLimits(), St: st, SCM: adapter, Adapters: &scm.Factory{Fallback: adapter}, Blob: bl, Out: send,
		Policy:           policy.Default(),
		HeartbeatTimeout: heartbeat, BaseBranch: "main",
		stageContext:       map[string]string{},
		transcripts:        map[string][]byte{},
		sessions:           map[string]string{},
		performers:         map[string]sessionPerformer{},
		deployLogs:         map[string][]byte{},
		deployOwner:        map[string]string{},
		budgetNotified:     map[string]string{},
		epicBudgetNotified: map[string]bool{},
		policyDown:         map[string]bool{},
		externalPolled:     map[string]time.Time{},
		policyBroken:       map[string]string{},
		Now:                time.Now,
	}
}

// Run — цикл диспетчеризации.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil {
				slog.Error("orchestrator tick", "err", err)
			}
		}
	}
}

// Tick — один проход планировщика: протухшие runner'ы, пересчёт DAG, назначения.
func (e *Engine) Tick(ctx context.Context) error {
	// В проходе PR создаётся одной попыткой без пауз (tickContext).
	ctx = tickContext(ctx)
	// Наблюдение и задания control plane идут и при сбое тика конвейера.
	defer e.tickLoadTests(ctx)
	defer e.tickOps(ctx)
	if err := e.replayLocalTerminals(ctx); err != nil {
		return fmt.Errorf("replay local terminal: %w", err)
	}
	lost, lostDeps, err := e.St.MarkStaleRunnersOffline(ctx, int(e.HeartbeatTimeout.Seconds()))
	if err != nil {
		return fmt.Errorf("stale runners: %w", err)
	}
	// Сессии задач потерянных runner'ов закрыты в БД — кеш обязан забыть их,
	// иначе replay стадии пройдёт SessionMatches по памяти.
	for _, taskID := range lost {
		e.DropSession(taskID)
	}
	// Публикации потерянных runner'ов проваливаются полной цепочкой.
	for _, depID := range lostDeps {
		if err := e.failDeployInfra(ctx, depID, "", "deploy-runner потерян (heartbeat)"); err != nil {
			slog.Error("deploy runner lost", "deployment", depID, "err", err)
		}
	}
	// Политика проектов, которая живёт в репозитории: свежие правки
	// доверенной ветки становятся версиями политики (спека access-policy).
	if err := e.syncGitPolicies(ctx); err != nil {
		slog.Error("синхронизация политики из репозитория", "err", err)
	}
	epics, err := e.St.RunningEpics(ctx)
	if err != nil {
		return err
	}
	for _, id := range epics {
		if err := e.St.RecomputeEpic(ctx, id); err != nil {
			return fmt.Errorf("recompute %s: %w", id, err)
		}
	}
	// Дневной бюджет токенов (спека orchestration): проекты, исчерпавшие
	// бюджет, исключаются из назначений до следующих суток. Выполняющиеся
	// стадии не трогаются — граница стадии, как у паузы Epic.
	excluded, err := e.budgetExclusions(ctx)
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	// Нерабочие учётные данные хостинга (fix-credential-state): проекту
	// ничего не назначается до восстановления подключения — стадия упала
	// бы на clone или push и потратила попытку.
	credExcluded, err := e.credentialExclusions(ctx)
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	excluded = append(excluded, credExcluded...)
	e.recheckCredentials(ctx)
	// Бюджет Epic (спека orchestration «Бюджет Epic»): исчерпанные Epic
	// исключаются из назначений до решения человека (поднять/снять бюджет).
	excludedEpics, err := e.epicBudgetExclusions(ctx)
	if err != nil {
		return fmt.Errorf("epic budget: %w", err)
	}
	// Зависимости Epic'ов (спека orchestration «Ожидание зависимостей
	// Epic'а»): задачи Epic'а с незавершёнными зависимостями не назначаются,
	// сам Epic остаётся running.
	waitingEpics, err := e.depsExclusions(ctx, epics)
	if err != nil {
		return fmt.Errorf("epic deps: %w", err)
	}
	excludedEpics = append(excludedEpics, waitingEpics...)
	// Ready-задачи входят на шаг процесса, ожидающие запуски участников
	// назначаются runner'ам по агенту, модели и capabilities.
	if err := e.enterReady(ctx, excluded, excludedEpics); err != nil {
		return err
	}
	e.retryMergePRs(ctx)
	// Runner'ы и запуски, чья стадия в этом тике не стартовала, исключаются
	// до конца тика: остальные назначения идут дальше, а цикл не крутится
	// на недоступном runner'е.
	// Подключения моделей до назначений (fix-agent-without-connection):
	// запуски без рабочего подключения ждут, не занимая runner'ов, агенты
	// команды без рабочей модели в этом проходе не назначаются.
	bad := e.screenConnections(ctx, excluded, excludedEpics)
	if bad == nil {
		bad = map[string]string{}
	}
	badAgents := make([]string, 0, len(bad))
	for id := range bad {
		badAgents = append(badAgents, id)
	}
	var badRunners []string
	var badRuns []int64
	misses := map[int64]agentMiss{}
	for {
		a, ok, err := e.St.AssignRunExcept(ctx, excluded, excludedEpics, badRunners, badRuns, badAgents)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		started, mc := e.dispatchAssigned(ctx, a)
		// Отказ по модели агента откладывается после прохода, только если
		// он последний исход запуска: стартовавший запуск или запуск, не
		// стартовавший по другой причине, свою причину уже получил.
		delete(misses, a.Run.ID)
		switch {
		case started:
		case mc != nil && mc.Agent != "":
			// Модель агента не работает: запуск возьмёт другой агент роли,
			// а если не возьмёт — подождёт с причиной после прохода.
			badAgents = append(badAgents, mc.Agent)
			bad[mc.Agent] = mc.Reason
			misses[a.Run.ID] = agentMiss{run: a.Run, err: mc}
		case mc != nil:
			// Запуск ждёт подключения, runner свободен для других задач.
			badRuns = append(badRuns, a.Run.ID)
		default:
			badRunners = append(badRunners, a.Runner.ID)
			badRuns = append(badRuns, a.Run.ID)
		}
	}
	e.deferAgentMisses(ctx, misses)
	if err := e.markWaiting(ctx, excluded, excludedEpics, bad); err != nil {
		return err
	}
	if err := e.reconcileSteps(ctx); err != nil {
		return err
	}
	e.resumeAwaitingCredentials(ctx)
	return e.tickDeployments(ctx)
}

// budgetExclusions считает токены за текущие сутки по UTC и возвращает
// проекты, которым в этот проход ничего не назначается: превысившие свой
// бюджет и все проекты при превышении бюджета установки. Событие
// policy.budget_exceeded пишется раз в сутки на проект.
func (e *Engine) budgetExclusions(ctx context.Context) ([]string, error) {
	projects, err := e.St.ProjectsWithRunningEpics(ctx)
	if err != nil || len(projects) == 0 {
		return nil, err
	}
	now := e.Now()
	dayStart := store.DayStartUTC(now)
	day := dayStart.Format("2006-01-02")
	pausedUntil := dayStart.Add(24 * time.Hour)
	inst, _, err := e.St.InstallationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	byProject, total, err := e.St.DailyTokenUsage(ctx, dayStart)
	if err != nil {
		return nil, err
	}
	var excluded []string
	for _, pid := range projects {
		eff, err := e.St.EffectivePolicy(ctx, pid)
		if err != nil {
			return nil, err
		}
		// Решение о назначении принимает движок: бюджеты установки и
		// проекта — правила стандартной политики (спека access-policy
		// «Точки принуждения»).
		in := budgetInput{
			Installation: budgetOf(total, inst.DailyTokenBudget),
			Project:      budgetOf(byProject[pid], eff.Presets.DailyTokenBudget),
		}
		d, err := e.decide(ctx, policy.PointAssign, in)
		if err != nil {
			// Решения нет — проекту в этот проход не назначается ничего.
			excluded = append(excluded, pid)
			if err := e.policyEngineDown(ctx, policy.PointAssign, pid, eff, err); err != nil {
				return nil, err
			}
			continue
		}
		if err := e.policyEngineUp(ctx, pid); err != nil {
			return nil, err
		}
		if d.Allow {
			continue
		}
		var used, limit int64
		scope := d.Reason
		switch d.Reason {
		case "installation":
			used, limit = in.Installation.Used, in.Installation.Budget
		default:
			used, limit = in.Project.Used, in.Project.Budget
		}
		excluded = append(excluded, pid)
		e.mu.Lock()
		notified := e.budgetNotified[pid] == day
		if !notified {
			e.budgetNotified[pid] = day
		}
		e.mu.Unlock()
		if notified {
			continue
		}
		payload := eff.Ref()
		payload["scope"], payload["used"], payload["limit"] = scope, used, limit
		payload["point"] = policy.PointAssign
		payload["paused_until"] = pausedUntil.Format(time.RFC3339)
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "policy.budget_exceeded", ProjectID: pid,
			Text: fmt.Sprintf("планирование приостановлено: дневной бюджет токенов исчерпан (%d из %d), до %s",
				used, limit, pausedUntil.Format(time.RFC3339)),
			Payload: payload,
		}); err != nil {
			return nil, err
		}
	}
	return excluded, nil
}

// epicBudgetExclusions — работающие Epic с исчерпанным бюджетом: список
// для запросов назначений и событие epic.budget_exceeded раз на факт.
func (e *Engine) epicBudgetExclusions(ctx context.Context) ([]string, error) {
	exceeded, err := e.St.ExceededEpicBudgets(ctx)
	if err != nil || len(exceeded) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(exceeded))
	for _, x := range exceeded {
		// Факт превышения даёт запрос, решение — движок: бюджет Epic такое
		// же правило стандартной политики, как дневные бюджеты.
		in := budgetInput{Epic: budgetSide{Used: x.Used, Budget: x.Budget}}
		d, err := e.decide(ctx, policy.PointAssign, in)
		if err != nil {
			ids = append(ids, x.EpicID)
			eff, effErr := e.St.EffectivePolicy(ctx, x.ProjectID)
			if effErr != nil {
				return nil, effErr
			}
			if err := e.policyEngineDown(ctx, policy.PointAssign, x.ProjectID, eff, err); err != nil {
				return nil, err
			}
			continue
		}
		if d.Allow {
			continue
		}
		ids = append(ids, x.EpicID)
		key := fmt.Sprintf("%s|%d", x.EpicID, x.Budget)
		e.mu.Lock()
		seen := e.epicBudgetNotified[key]
		e.epicBudgetNotified[key] = true
		e.mu.Unlock()
		if seen {
			continue
		}
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "epic.budget_exceeded",
			ProjectID: x.ProjectID, EpicID: x.EpicID,
			Text: fmt.Sprintf("бюджет Epic исчерпан (%d из %d токенов) — новые стадии не назначаются, поднимите или снимите бюджет",
				x.Used, x.Budget),
			Payload: map[string]any{"used": x.Used, "budget": x.Budget, "point": policy.PointAssign},
		}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// SCMFor — адаптер хостинга проекта: провайдер, инстанс и учётные данные
// живут у проекта (design add-repo-onboarding, решение 5). Проект без
// собственных учётных данных работает на запасном адаптере установки.
//
// Итог каждой операции отмечается в состоянии учётных данных проекта: отказ
// по авторизации делает их нерабочими (ошибка обёрнута в
// ErrSCMCredentials), успех возвращает в работу (fix-credential-state).
func (e *Engine) SCMFor(ctx context.Context, p domain.Project) (scm.Adapter, error) {
	cred, err := e.St.ProjectTokenState(ctx, p.ID, e.Box)
	if err != nil {
		return nil, fmt.Errorf("учётные данные проекта %s: %w", p.ID, err)
	}
	a, err := e.Adapters.For(scm.Provider(p.Provider), p.BaseURL, cred.Token)
	if err != nil {
		return nil, err
	}
	return e.track(a, p, cred), nil
}

func (e *Engine) projectOf(ctx context.Context, task domain.Task) (domain.Project, domain.Epic, error) {
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return domain.Project{}, epic, err
	}
	p, err := e.St.GetProject(ctx, epic.ProjectID)
	return p, epic, err
}

// sessionSpec — переопределение сессии стадии: сессия доработки
// (водитель-пользователь, его промпт, приватность). nil — обычная
// scheduler-сессия со снимком задачи.
type sessionSpec struct {
	driverKind string
	driverID   string
	prompt     string
	userPrompt string
	private    bool
}

// featureNative — runner ведёт агентский цикл Rivet (add-native-agent).
const featureNative = "native"

// dispatchRun открывает сессию запуска участника и отправляет Assignment
// runner'у. Возвращает id сессии (пусто, если стадию не удалось
// запустить) и признак доставки. spec — переопределение сессии (сессия
// доработки); nil — обычная scheduler-сессия со снимком задачи.
func (e *Engine) dispatchRun(ctx context.Context, task domain.Task, run store.StepRun, runner domain.Runner,
	step policy.ResolvedStep, entry string, spec *sessionSpec) (string, bool, error) {
	stage := stageFor(step.Kind, entry)
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		slog.Error("dispatch: project", "task", task.ID, "err", err)
		return "", false, fmt.Errorf("проект задачи: %w", err)
	}
	if e.credentialsInvalid(ctx, p.ID) {
		return "", false, ErrSCMCredentials
	}
	if runner.Agent == "" {
		if r, err := e.St.GetRunner(ctx, runner.ID); err == nil {
			runner = r
		}
	}
	// Политика проекта едет вместе с назначением: у runner'а нет другого
	// источника, рабочая копия на исполнение не влияет (спека access-policy
	// «Доставка политик runner'ам»). Ошибка чтения не валит стадию — агент
	// работает как прежде, гейты на plane никуда не делись.
	var assignPolicy *pb.Policy
	var agentModels map[string]policy.AgentModel
	testPaths := append([]string{}, policy.DefaultTestPaths...)
	if eff, err := e.St.EffectivePolicy(ctx, p.ID); err != nil {
		slog.Error("dispatch: политика проекта", "project", p.ID, "err", err)
	} else {
		assignPolicy = &pb.Policy{
			Hash: eff.Hash, HumanReviewPaths: eff.Presets.HumanReviewPaths,
			PolicyDir: policy.PolicyDir,
		}
		agentModels = eff.Presets.AgentModels
		testPaths = eff.Presets.EffectiveTestPaths()
	}
	// Исполнитель (add-agent-roles): шаблон роли и агент команды —
	// инструкция и имя; отключённый или пропавший шаблон — без роли.
	perf := e.runPerformer(ctx, run)
	// Профиль агента из каталога (add-agent-profiles, спека agents «Доставка
	// модели и учётных данных runner'у»): модель участника, иначе
	// переопределение проекта, иначе модель по умолчанию; окружение и
	// аргументы по шаблону, секреты по режиму профиля и каналу runner'а.
	model := stageModel(run, perf, runner)
	// Агенту Rivet (feature native) уходит endpoint модели: подключение,
	// окно, пределы ответа, цены, лимиты, ключ по режиму секретов и каналу
	// runner'а. Без рабочего подключения стадия не назначается: сессии ещё
	// нет, задача ждёт с причиной (fix-agent-without-connection). Проверка
	// идёт раньше обращений к хостингу (задача описания архитектуры).
	endpoint, model, err := e.stageEndpoint(ctx, run, perf, runner, agentModels, model, stage)
	if err != nil {
		return "", false, err
	}
	criteria := make([]string, 0, len(task.Criteria))
	for _, c := range task.Criteria {
		criteria = append(criteria, c.Text)
	}
	checks := make([]*pb.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, &pb.Check{Name: c.Name, Cmd: c.Cmd})
	}
	// Проверка архитектуры (add-architecture-c4): у задачи описания только
	// она, у остальных добавляется, если у проекта есть описание.
	checks = e.architectureChecks(ctx, task, p, checks)
	if task.Kind == domain.KindArchitecture && step.Kind == policy.StepTest {
		// Сессии ещё нет: отказ откладывает шаг без «зависшей» сессии.
		if err := e.recordCheckedSHA(ctx, task, p); err != nil {
			slog.Warn("dispatch: проверенная вершина задачи описания", "task", task.ID, "err", err)
			return "", false, err
		}
	}
	// Контекст стадии: замечания при исправлении, diff и отчёт проверок
	// ревьюеру. Читается без удаления: параллельным участникам нужен один
	// и тот же контекст, сбрасывает его исход шага.
	extra := ""
	switch {
	case step.Kind == policy.StepReview:
		if extra, err = e.reviewContext(ctx, task); err != nil {
			return "", false, err
		}
	case step.Kind == policy.StepCode && entry == policy.OutcomeChanges:
		extra = e.peekStageContext(task.ID)
	}
	// Модель назначения пишется отдельно от явной модели участника: при
	// повторном назначении она не станет требованием к runner'у. Вместе с
	// ней — подключение и признак передачи ключа каталога: по ним
	// разбирается отказ провайдера во время стадии.
	var connID string
	var catalogKey bool
	if endpoint != nil {
		connID, catalogKey = endpoint.ConnectionID, endpoint.SecretsIncluded
	}
	if model != "" || connID != "" {
		if err := e.St.SetRunAssignment(ctx, run.ID, model, connID, catalogKey); err != nil {
			slog.Error("dispatch: модель запуска", "run", run.ID, "err", err)
		}
	}
	// Глубина сессии — глубина адаптера runner'а (спека agent-integration
	// «Глубина объявлена runner'ом»).
	depth, err := e.St.RunnerDepth(ctx, runner.ID)
	if err != nil {
		slog.Error("dispatch: runner depth", "runner", runner.ID, "err", err)
		depth = domain.DepthMinimal
	}
	// Модель сессии — из профиля или участника, иначе модель runner'а по умолчанию.
	if model == "" {
		model = runner.Model
	}
	description, err := e.assessedTaskDescription(ctx, task)
	if err != nil {
		return "", false, err
	}
	// Сессия создаётся до отправки Assignment: runner повторяет session_id
	// во всех сообщениях стадии, сообщения без него отбрасываются (design,
	// решение 4). Без сессии стадию не запускаем — задачу вернёт
	// heartbeat-таймаут, как при недоступном runner'е.
	if spec == nil {
		// Запрос сессии — снимок задачи на момент запуска (история и поиск,
		// спека team-visibility): описание задачи меняется ответами человека.
		prompt := task.Title
		if description != "" {
			prompt += "\n" + description
		}
		spec = &sessionSpec{driverKind: "scheduler", prompt: prompt}
	}
	sessionID, err := e.St.CreateSession(ctx, domain.Session{
		TaskID: task.ID, Attempt: task.AttemptUsed + 1,
		DriverKind: spec.driverKind, DriverID: spec.driverID,
		Agent: runner.Agent, Model: model,
		Depth: depth, Scope: stage.String(), Prompt: spec.prompt, Private: spec.private,
		PolicyHash: assignPolicy.GetHash(), StepID: step.ID, Participant: run.Participant,
	})
	if err != nil {
		slog.Error("dispatch: session", "task", task.ID, "err", err)
		return "", false, fmt.Errorf("сессия стадии: %w", err)
	}
	// Запуск мог быть отменён другим участником шага (any, blocked) между
	// назначением и отправкой: тогда сессия закрывается, стадия не стартует.
	bound, err := e.St.SetRunSession(ctx, run.ID, sessionID)
	if err != nil || !bound {
		if err != nil {
			slog.Error("dispatch: run session", "task", task.ID, "err", err)
		} else {
			slog.Info("dispatch: запуск отменён до отправки, стадия не запускается", "task", task.ID, "run", run.ID)
		}
		if _, cerr := e.St.EndSession(ctx, sessionID, "", "стадия не запущена: запуск отменён"); cerr != nil {
			slog.Error("dispatch: end unbound session", "session", sessionID, "err", cerr)
		}
		return "", false, nil
	}
	e.mu.Lock()
	e.sessions[sessionID] = task.ID
	delete(e.transcripts, sessionID)
	e.mu.Unlock()
	e.rememberPerformer(sessionID, perf, runner)
	// Репозиторий проекта: адрес клонирования и токен доступа (design
	// решение 8). Токен уедет в git через askpass, не в аргументы команд.
	gitToken, err := e.St.ProjectToken(ctx, p.ID, e.Box)
	if err != nil {
		slog.Error("dispatch: учётные данные проекта", "project", p.ID, "err", err)
		// Сессия уже создана — закрывается, иначе осталась бы «идущей»
		// навсегда (ghost) и держала бы кеш.
		if _, cerr := e.St.EndSession(ctx, sessionID, "", "стадия не запущена: учётные данные проекта недоступны"); cerr != nil {
			slog.Error("dispatch: end ghost session", "session", sessionID, "err", cerr)
		}
		e.dropSessionID(sessionID)
		return "", false, fmt.Errorf("учётные данные проекта: %w", err)
	}
	// У fake-провайдера (e2e-стенд) настоящего адреса нет: клонирование
	// идёт по RIVET_GIT_BASE, как до этого изменения.
	repoURL := p.WebURL()
	if p.Provider == string(scm.ProviderFake) {
		repoURL, gitToken = "", ""
	}
	msg := &pb.PlaneMsg{
		MsgId: fmt.Sprintf("assign-%s-%s-%d", task.ID, stage, time.Now().UnixNano()),
		Kind: &pb.PlaneMsg_Assign{Assign: &pb.Assignment{
			TaskId: task.ID, TaskNum: task.Num, Stage: stage,
			Title: task.Title, Description: description,
			Criteria: criteria, Repo: p.Repo(), Branch: task.Branch,
			Checks: checks, ExtraContext: extra, SessionId: sessionID,
			RepoUrl: repoURL, GitToken: gitToken, BaseBranch: p.DefaultBranch,
			UserPrompt: spec.userPrompt, Policy: assignPolicy,
			Model: model, StepId: step.ID, Participant: run.Participant,
			StepPrompt: step.Prompt,
			Role:       assignmentRole(perf),
		}},
	}
	if perf.role != nil && stage == pb.StageResult_TESTING {
		msg.GetAssign().TestPaths = testPaths
	}
	if endpoint != nil {
		as := msg.GetAssign()
		as.ConnectionId = endpoint.ConnectionID
		as.SecretsIncluded = endpoint.SecretsIncluded
		as.Endpoint = &pb.ModelEndpoint{
			Api: endpoint.API, BaseUrl: endpoint.BaseURL, Model: endpoint.Model,
			ContextWindow:   int32(min(endpoint.ContextWindow, 1<<31-1)),
			MaxOutputTokens: int32(min(endpoint.MaxOutputTokens, 1<<31-1)),
			ModelMaxOutput:  int32(min(endpoint.ModelMaxOutput, 1<<31-1)),
			HasPrices:       endpoint.HasPrices, InputPrice: endpoint.InputPrice, OutputPrice: endpoint.OutputPrice,
			CachedInputPrice: endpoint.CachedInputPrice,
			ConnectionId:     endpoint.ConnectionID, Key: endpoint.Key, Headers: endpoint.Headers,
			Limits: &pb.SessionLimits{MaxTurns: int32(endpoint.Limits.MaxTurns), MaxTokens: endpoint.Limits.MaxTokens,
				MaxMinutes: int32(endpoint.Limits.MaxMinutes)},
		}
	}
	if !e.Out.Send(runner.ID, msg) {
		slog.Warn("dispatch: runner недоступен, задачу вернёт heartbeat-таймаут",
			"runner", runner.ID, "task", task.ID)
		return sessionID, false, nil
	}
	if endpoint != nil && endpoint.SecretsIncluded {
		// Факт доставки секретов в event log без значений (спека agents).
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorRunner, ActorID: runner.ID, Type: "runner.secrets_delivered",
			ProjectID: p.ID, TaskID: task.ID,
			Text:    "runner " + runner.ID + " получил учётные данные подключения " + endpoint.ConnectionID,
			Payload: map[string]any{"runner_id": runner.ID, "task_id": task.ID, "connection_id": endpoint.ConnectionID, "env_names": endpoint.SecretNames, "agent": runner.Agent},
		}); err != nil {
			slog.Error("dispatch: событие доставки секретов", "err", err)
		}
	}
	return sessionID, true, nil
}

// transcriptCap — жёсткий предел буфера транскрипта стадии: после append
// буфер не превышает кап, лишнее отбрасывается с маркером обрезки
// (api-contract: транскрипт отдаётся целиком одним ответом).
const transcriptCap = 4 << 20

var truncMarker = []byte("\n…[транскрипт обрезан: превышен лимит 4 МБ]\n")

// SessionMatches — открыта ли сессия задачи. После рестарта rivetd карта
// сессий пуста: открытая сессия поднимается из БД, чтобы не терять
// результаты стадий, назначенных до рестарта (доставка at-least-once
// переживает рестарт plane).
func (e *Engine) SessionMatches(ctx context.Context, taskID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	e.mu.Lock()
	owner, known := e.sessions[sessionID]
	e.mu.Unlock()
	if known {
		return owner == taskID
	}
	open, err := e.St.IsSessionOpen(ctx, taskID, sessionID)
	if err != nil || !open {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if owner, known := e.sessions[sessionID]; known {
		return owner == taskID
	}
	e.sessions[sessionID] = taskID
	return true
}

// DropSession инвалидирует память обо всех сессиях задачи. Вызывается там,
// где сессии закрываются в БД мимо StageResult/Blocked (отмена, потеря
// runner'а): иначе SessionMatches продолжил бы принимать поздние сообщения
// закрытой сессии из кеша (например, CreatePR после отмены).
func (e *Engine) DropSession(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for sid, tid := range e.sessions {
		if tid == taskID {
			delete(e.sessions, sid)
			delete(e.transcripts, sid)
			delete(e.performers, sid)
		}
	}
}

// dropSessionID забывает одну сессию (прерван один участник шага).
func (e *Engine) dropSessionID(sessionID string) {
	e.mu.Lock()
	delete(e.sessions, sessionID)
	delete(e.transcripts, sessionID)
	delete(e.performers, sessionID)
	e.mu.Unlock()
}

// OnTranscript накапливает чанки транскрипта сессии. Чанки чужой сессии
// отбрасываются; принадлежность проверяет вызывающий через SessionMatches
// (он же поднимает сессию из БД после рестарта), здесь — только сверка с
// картой под общим замком.
func (e *Engine) OnTranscript(taskID, sessionID string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sessionID == "" || e.sessions[sessionID] != taskID {
		return
	}
	buf := e.transcripts[sessionID]
	if len(buf) >= transcriptCap {
		return
	}
	if len(buf)+len(data) > transcriptCap {
		// Маркер обрезки входит в кап: итоговый буфер ровно transcriptCap.
		keep := transcriptCap - len(truncMarker)
		if len(buf) > keep {
			buf = buf[:keep]
		} else {
			buf = append(buf, data[:keep-len(buf)]...)
		}
		e.transcripts[sessionID] = append(buf, truncMarker...)
		return
	}
	e.transcripts[sessionID] = append(buf, data...)
}

// flushTranscript закрывает сессию стадии и уводит транскрипт в blob.
// Сохраняемый буфер маскируется целиком: страховка от секретов,
// разрезанных между чанками (спека team-visibility «Секрет в транскрипте»).
//
// Возвращает, удалось ли атомарно закрыть сессию: false — её уже закрыл
// другой путь (отмена, потеря runner'а) в окне до инвалидации кеша, и
// вызывающий обязан отбросить сообщение стадии без реакций конвейера.
// Объект, записанный в blob до неудавшегося захвата, остаётся без ссылки —
// это безвредный мусор.
func (e *Engine) flushTranscript(ctx context.Context, task domain.Task, stage, sessionID, outcome string) bool {
	e.mu.Lock()
	if sessionID == "" || e.sessions[sessionID] != task.ID {
		e.mu.Unlock()
		return false
	}
	buf := e.transcripts[sessionID]
	delete(e.transcripts, sessionID)
	delete(e.sessions, sessionID)
	delete(e.performers, sessionID)
	e.mu.Unlock()
	ref := ""
	if len(buf) > 0 && e.Blob != nil {
		// Ключ уникален по сессии: user-сессии не расходуют попытку, и ключ
		// без session_id перезаписывал бы объект прежней сессии (а старая
		// ссылка открыла бы чужой — в т.ч. приватный — транскрипт).
		key := fmt.Sprintf("tasks/%d/attempt-%d-%s-%s.log", task.Num, task.AttemptUsed+1, stage, shortID(sessionID))
		var err error
		if ref, err = e.Blob.Put(ctx, key, redact.Bytes(buf)); err != nil {
			slog.Error("transcript flush", "task", task.ID, "err", err)
			ref = ""
		}
	}
	claimed, err := e.St.EndSession(ctx, sessionID, ref, outcome)
	if err != nil {
		slog.Error("end session", "session", sessionID, "err", err)
		return false
	}
	return claimed
}

// OnStageResult — результат стадии как вердикт участника шага (спека
// process «Агрегация вердиктов и переходы»). Результат чужой сессии (replay
// после reconnect) отбрасывается; Detail — runner-controlled текст, идущий
// в event log и контекст следующего шага, поэтому маскируется на входе.
func (e *Engine) OnStageResult(ctx context.Context, runnerID string, sr *pb.StageResult) error {
	if !e.SessionMatches(ctx, sr.TaskId, sr.SessionId) {
		slog.Warn("stage result чужой сессии отброшен",
			"task", sr.TaskId, "session", sr.SessionId, "stage", sr.Stage)
		return nil
	}
	sr.Detail = redact.String(sr.Detail)
	task, err := e.St.GetTask(ctx, sr.TaskId)
	if err != nil {
		return err
	}
	// Итог сессии для истории: текст результата стадии (уже маскирован).
	outcome := sr.Detail
	if sr.Ok && outcome == "" {
		outcome = "стадия завершена успешно"
	}
	if !e.flushTranscript(ctx, task, sr.Stage.String(), sr.SessionId, outcome) {
		// Сессию уже закрыл другой путь (отмена, потеря runner'а):
		// результат стадии опоздал, реакции конвейера не выполняются.
		slog.Warn("stage result закрытой сессии отброшен", "task", sr.TaskId, "session", sr.SessionId)
		return nil
	}
	run, err := e.St.RunBySession(ctx, sr.SessionId)
	if err != nil {
		return fmt.Errorf("запуск сессии %s: %w", sr.SessionId, err)
	}
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	step, ok := proc.Step(run.StepID)
	if !ok {
		return fmt.Errorf("шаг %q запуска не найден в снимке процесса задачи %s", run.StepID, task.ID)
	}
	// Git на runner'е отклонён хостингом по авторизации (fix-credential-state):
	// если проверка подключения подтверждает нерабочие учётные данные,
	// запуск ждёт их восстановления без вердикта, попытка не расходуется.
	if sr.Verdict == verdictSCMAuth {
		handled, err := e.runnerAuthFailure(ctx, task, run, runnerID, sr.Detail)
		if err != nil || handled {
			return err
		}
		// Отзыв не подтвердился: сбой git разбирается, как раньше. На шаге
		// test это сбой инфраструктуры (повтор без расхода попытки), на
		// остальных — обычный неуспех стадии.
		sr.Verdict = ""
		if step.Kind == policy.StepTest {
			sr.Verdict = verdictInfraError
		}
	}
	// Сбой инфраструктуры шага test (add-agent-roles, add-native-agent):
	// git, push, падение тестировщика, проверки без песочницы или упавшие
	// из-за её окружения — не замечания разработчику. Шаг повторяется без
	// расхода попытки, повторный сбой эскалирует.
	infraExhausted := false
	if step.Kind == policy.StepTest && sr.Verdict == verdictInfraError {
		retried, err := e.retryInfra(ctx, task, step, run, runnerID, sr.Detail)
		if err != nil || retried {
			return err
		}
		infraExhausted = true
		sr.Detail = "повторный сбой инфраструктуры шага проверки: " + sr.Detail
	}
	// Вердикт самого агента (тестировщик, шаг prompt): по нему подписывается
	// исход шага.
	if sr.Verdict != "" {
		if err := e.St.SetRunAgentVerdict(ctx, run.ID, sr.Verdict); err != nil {
			return err
		}
	}
	verdict := verdictOf(step.Kind, sr.Ok, sr.Verdict)
	var reports []store.CriteriaReport
	if step.Kind == policy.StepReview && sr.Stage == pb.StageResult_REVIEW && sr.Verdict == "" {
		report := store.CriteriaReport{Pass: run.Pass, By: runnerID}
		var failed []string
		indexed := map[int]*pb.CriterionResult{}
		for _, c := range sr.Criteria {
			if c != nil && c.Index > 0 && int(c.Index) <= len(task.Criteria) {
				indexed[int(c.Index)] = c
			}
		}
		for i := 1; i <= len(task.Criteria); i++ {
			c := indexed[i]
			if c == nil || c.Index < 1 || int(c.Index) > len(task.Criteria) {
				continue
			}
			note := redact.String(c.Note)
			if len([]rune(note)) > 1000 {
				note = string([]rune(note)[:1000])
			}
			report.Results = append(report.Results, store.CriterionResult{Index: int(c.Index), OK: c.Ok, Note: note})
			if !c.Ok {
				failed = append(failed, fmt.Sprintf("%d — %s", c.Index, note))
			}
		}
		reports = append(reports, report)
		if verdict == policy.OutcomeOk && len(failed) > 0 {
			verdict = policy.OutcomeChanges
			detail := "Не выполнены критерии: " + strings.Join(failed, "; ")
			if sr.Detail != "" && sr.Detail != "review пройден" {
				detail += "\n\n" + sr.Detail
			}
			sr.Detail = detail
		}
	}
	if infraExhausted {
		// Повторный сбой — не замечания разработчику, а эскалация (переход fail).
		verdict = policy.OutcomeFail
	}
	// Runner остаётся за задачей, если следующий шаг можно отдать ему же
	// (тот же worktree: code → test, провал проверок → исправление).
	// С шага review runner не переиспользуется: ревьюер не должен
	// исправлять по собственным замечаниям (спека task-pipeline
	// «Независимое review»).
	keep := false
	if next, found := proc.Step(step.Target(verdict)); found && step.Kind != policy.StepReview && !e.epicPaused(ctx, task) {
		keep = e.reuseTarget(ctx, task, next, runnerID)
	}
	claimed, err := e.St.RecordVerdict(ctx, run.ID, verdict, sr.Detail, keep, reports...)
	if err != nil {
		return err
	}
	if !claimed {
		slog.Warn("stage result закрытого запуска отброшен", "task", sr.TaskId, "run", run.ID)
		return nil
	}
	return e.evaluateStep(ctx, task, proc, hash, step, run.Pass, runnerID, sr.PolicyHash, sr.PrUrl)
}

// epicPaused — Epic задачи не выполняется (пауза и т.п.): конвейер задачи
// останавливается на границе стадии.
func (e *Engine) epicPaused(ctx context.Context, task domain.Task) bool {
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		slog.Error("epicPaused: epic", "task", task.ID, "err", err)
		return false
	}
	return epic.Status != domain.EpicRunning
}

// failTask — невосстановимая ошибка: failed + эскалация TEST_FAILED.
func (e *Engine) failTask(ctx context.Context, task domain.Task, msg, runnerID string, extra ...map[string]any) error {
	return e.failTaskBy(ctx, task, domain.ActorRunner, runnerID, msg, extra...)
}

// failTaskBy — failTask с явным автором события (система для отказов
// control plane, например границы задачи описания архитектуры).
func (e *Engine) failTaskBy(ctx context.Context, task domain.Task, actor domain.ActorKind, actorID, msg string, extra ...map[string]any) error {
	return e.failTaskWith(ctx, task, domain.AttTestFailed, actor, actorID, msg, extra...)
}

// failTaskWith — failed + эскалация с указанной причиной (ошибка настройки
// подключения модели — MODEL_CONNECTION, остальное — TEST_FAILED).
func (e *Engine) failTaskWith(ctx context.Context, task domain.Task, reason domain.AttentionReason, actor domain.ActorKind, actorID, msg string, extra ...map[string]any) error {
	ev, mutate := e.failParts(ctx, task, reason, actor, actorID, msg, extra...)
	return e.St.TransitionTask(ctx, task.ID, domain.TaskFailed, ev, mutate)
}

// failParts — событие перехода в failed и изменения в той же транзакции:
// runner'ы задачи свободны, эскалация с причиной reason.
func (e *Engine) failParts(ctx context.Context, task domain.Task, reason domain.AttentionReason, actor domain.ActorKind, actorID, msg string,
	extra ...map[string]any) (store.EventInput, func(pgx.Tx) error) {
	failPayload := map[string]any{"status": "failed"}
	for _, m := range extra {
		for k, v := range m {
			failPayload[k] = v
		}
	}
	ev := store.EventInput{ActorKind: actor, ActorID: actorID, Type: "task.status", Text: msg, Payload: failPayload}
	return ev, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET wait_kind='', wait_reason='', wait_claimed_at=NULL, wait_failures=0 WHERE id=$1`, task.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runners SET status='idle', task_id=NULL WHERE task_id=$1`, task.ID); err != nil {
			return err
		}
		epic, err := e.St.GetEpic(ctx, task.EpicID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO attention (project_id, task_id, reason, message)
			VALUES ($1,$2,$3,$4)`, epic.ProjectID, task.ID, string(reason), msg)
		return err
	}
}

// OnBlocked — вопрос агента: blocked + эскалация. Вопрос — runner-controlled
// текст (event log, эскалация), маскируется; чужая сессия отбрасывается.
func (e *Engine) OnBlocked(ctx context.Context, runnerID string, b *pb.BlockedQuestion) error {
	if !e.SessionMatches(ctx, b.TaskId, b.SessionId) {
		slog.Warn("blocked чужой сессии отброшен", "task", b.TaskId, "session", b.SessionId)
		return nil
	}
	b.Question = redact.String(b.Question)
	task, err := e.St.GetTask(ctx, b.TaskId)
	if err != nil {
		return err
	}
	if !e.flushTranscript(ctx, task, "blocked", b.SessionId, "заблокирована вопросом: "+b.Question) {
		slog.Warn("blocked закрытой сессии отброшен", "task", b.TaskId, "session", b.SessionId)
		return nil
	}
	// Вопрос одного участника блокирует задачу целиком: остальные запуски
	// шага прерываются, ответ человека вернёт задачу на тот же шаг.
	if run, err := e.St.RunBySession(ctx, b.SessionId); err == nil {
		if _, err := e.St.RecordVerdict(ctx, run.ID, policy.OutcomeBlocked, b.Question, false); err != nil {
			return err
		}
		e.cancelOpenRuns(ctx, task, run.ID)
	} else {
		// Сессия без запуска (до процесса): остальные запуски всё равно
		// прерываются, задача блокируется целиком.
		e.cancelOpenRuns(ctx, task, 0)
	}
	e.setStageContext(task.ID, "")
	// Вопрос приватной сессии — её содержимое: в публичные block_reason,
	// событие и эскалацию идёт заглушка, полный вопрос остаётся в итоге
	// сессии (виден автору). Спека team-visibility «Видимость и приватность».
	question := b.Question
	if private, _, err := e.St.SessionPrivacy(ctx, b.SessionId); err == nil && private {
		question = "вопрос приватной сессии — содержимое доступно её автору в итоге сессии"
	}
	return e.St.BlockTask(ctx, b.TaskId, question,
		store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID})
}

// StartUserSession — сессия доработки задачи по промпту участника (спека
// agent-integration «Сессия из интерфейса Rivet»): задача blocked/failed/
// review переводится в fixing на свободном runner'е, агент получает промпт
// человека, результат идёт обычным конвейером. Попытка не расходуется.
func (e *Engine) StartUserSession(ctx context.Context, taskID, prompt, login string, private bool) (string, error) {
	// Сессия на нерабочих учётных данных хостинга упала бы на clone или
	// push, потратив работу агента (fix-credential-state).
	if projectID, _, err := e.St.TaskRefs(ctx, taskID); err == nil && e.credentialsInvalid(ctx, projectID) {
		return "", fmt.Errorf("%w: замените токен в настройках проекта, потом запускайте сессию", ErrSCMCredentials)
	}
	// Подключение модели проверяется по выбранному runner'у до изменений
	// задачи: отказ по нему не закрывает эскалацию и не переводит задачу в
	// fixing (fix-agent-without-connection).
	var check func(string) error
	if task, err := e.St.GetTask(ctx, taskID); err == nil {
		check = e.sessionRunnerCheck(ctx, task)
	}
	a, err := e.St.StartUserSession(ctx, taskID, login, private, check)
	if err != nil {
		return "", err
	}
	// Кеш сессий обязан забыть прежнюю: StartUserSession закрыл её в БД
	// (иначе поздний StageResult прежней стадии прошёл бы по памяти).
	e.DropSession(taskID)
	// Сессия доработки — исправление на ближайшем шаге code процесса.
	proc, hash, err := e.processFor(ctx, a.Task)
	if err != nil {
		return "", err
	}
	codeID := nearestCodeBefore(proc, a.Task.StepID)
	if codeID == policy.TargetEscalate {
		codeID = nearestCodeBefore(proc, mergeStepID(proc))
	}
	step, ok := proc.Step(codeID)
	if !ok {
		return "", fmt.Errorf("в процессе проекта нет шага code для доработки")
	}
	in := store.EnterStep{TaskID: taskID, Step: step, Entry: policy.OutcomeChanges,
		ReuseRunner: a.Runner.ID, Silent: true}
	if len(a.Task.Process) == 0 {
		in.Process, in.ProcessHash = proc, hash
	}
	runs, err := e.St.EnterStep(ctx, in)
	if err != nil || len(runs) == 0 {
		return "", fmt.Errorf("сессию не удалось запустить: %v", err)
	}
	a.Task.StepID, a.Task.StepEntry = step.ID, policy.OutcomeChanges
	sessionID, delivered, startErr := e.dispatchRun(ctx, a.Task, runs[0], a.Runner, step, policy.OutcomeChanges, &sessionSpec{
		driverKind: "user", driverID: login, prompt: prompt,
		userPrompt: prompt, private: private,
	})
	if sessionID == "" || !delivered {
		// Стадию не удалось запустить или Assignment не доставлен: сессия
		// закрывается, runner освобождается, задача остаётся в fixing без
		// runner'а — её подхватит планировщик обычным промптом.
		// Пользователь видит отказ, а не 200 без запуска.
		if sessionID != "" {
			if _, cerr := e.St.EndSession(ctx, sessionID, "", "сессия не запущена: runner недоступен"); cerr != nil {
				slog.Error("user session: end undelivered session", "session", sessionID, "err", cerr)
			}
			e.DropSession(taskID)
		}
		// Запуск остаётся ожидающим без runner'а: его подхватит планировщик
		// обычным промптом исправления.
		if rerr := e.St.UnbindRun(ctx, runs[0].ID); rerr != nil {
			slog.Error("user session: unbind run", "task", taskID, "err", rerr)
		}
		if rerr := e.St.ReleaseTaskRunner(ctx, taskID); rerr != nil {
			slog.Error("user session: release after dispatch failure", "task", taskID, "err", rerr)
		}
		// Нет рабочего подключения к модели: человеку — причина и где её
		// исправить (fix-agent-without-connection).
		var mc *ModelConnectionError
		if errors.As(startErr, &mc) {
			return "", &ModelConnectionError{Reason: "сессию не удалось запустить: " + mc.Reason, ConnectionID: mc.ConnectionID, Model: mc.Model}
		}
		return "", fmt.Errorf("сессию не удалось запустить: runner недоступен или стадия не собралась, подробности в логе rivetd")
	}
	return sessionID, nil
}

// MergeTask — подтверждение человека: merge PR → done → пересчёт DAG.
// Доступно при любой политике, в том числе когда авто-merge отложен.
func (e *Engine) MergeTask(ctx context.Context, taskID, userID string) error {
	// Операция merge задачи сериализована с авто-merge и другими
	// подтверждениями: id берётся из базы (неизвестная задача — 404),
	// а сама задача перечитывается уже под блокировкой.
	known, err := e.St.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	return e.withMergeLock(known.ID, func() error {
		task, err := e.St.GetTask(ctx, known.ID)
		if err != nil {
			return err
		}
		p, _, err := e.projectOf(ctx, task)
		if err != nil {
			return err
		}
		err = e.mergeTask(ctx, task, p, store.EventInput{
			ActorKind: domain.ActorUser, ActorID: userID, Type: "task.status",
			Text: "PR смержен — задача выполнена", Payload: map[string]any{"status": "done"},
		})
		if !errors.Is(err, scm.ErrMergeConflict) {
			return err
		}
		// Конфликт при подтверждении человеком: счётчик возвратов
		// сбрасывается, задача уходит агенту, кнопке — 409 с пояснением.
		returned, cerr := e.mergeConflict(ctx, task, p, userID)
		if cerr != nil {
			return cerr
		}
		base := p.DefaultBranch
		if base == "" {
			base = "main"
		}
		return &MergeConflictError{Base: base, Returned: returned}
	})
}

// autoMerge — решение точки принуждения после одобренного review при
// включённом пресете: пути PR из diff адаптера, метаправило про файлы
// политики (.rivet/) с эскалацией POLICY_CHANGE, защищённые пути с
// отложенным merge, fail-closed без списка путей; иначе merge от системного
// актора с версией политики в payload.
func (e *Engine) autoMerge(ctx context.Context, task domain.Task, p domain.Project, eff store.EffectivePolicy) error {
	// Без PR подтверждать и мержить нечего: PR создаётся при любом значении
	// пресета, отказ хостинга даёт событие о неудачном merge и эскалацию.
	if task.PRURL == "" {
		withPR, err := e.ensurePR(ctx, task)
		if err != nil {
			return e.prMissingAtMerge(ctx, task, p, err)
		}
		task = withPR
	}
	deferMerge := func(reason, text string, paths []string) error {
		payload := eff.Ref()
		payload["reason"] = reason
		payload["paths"] = paths
		payload["point"] = policy.PointMerge
		_, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "task.merge_deferred",
			ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
			Text: text + " — merge отложен политикой " + shortHash(eff.Hash), Payload: payload,
		})
		return err
	}
	decide := func(filesUnknown bool, policyFiles, protected []string) (policy.Decision, error) {
		// Списки идут непустыми: nil в JSON — null, а count(null) в Rego
		// не определён, и правило молча не срабатывало бы.
		return e.decide(ctx, policy.PointMerge, map[string]any{
			"presets":       eff.Presets,
			"files_unknown": filesUnknown,
			"policy_files":  orEmpty(policyFiles),
			"protected":     orEmpty(protected),
		})
	}
	// Первый вопрос движку — без фактов PR: «пропускает ли гейт merge в
	// принципе». Отказ auto_merge_off от файлов не зависит, и diff тогда
	// читать незачем — иначе на каждый reviewed PR уходил бы лишний вызов
	// хостинга при выключенном авто-merge.
	if d, err := decide(false, nil, nil); err != nil {
		if err := e.policyEngineDown(ctx, policy.PointMerge, p.ID, eff, err); err != nil {
			return err
		}
		return deferMerge("engine_error", "движок политик не дал решения", nil)
	} else if !d.Allow && d.Reason == "auto_merge_off" {
		// Авто-merge выключен: задача штатно ждёт человека, отдельного
		// события об этом нет — иначе оно писалось бы на каждую задачу.
		return e.policyEngineUp(ctx, p.ID)
	}
	var paths []string
	// Список файлов PR неизвестен — авто-merge не выполняется (fail-closed).
	// Пустой diff тоже неизвестный список: хостинг готовит diff только что
	// созданного PR асинхронно, а PR без изменений создать нельзя.
	filesUnknown := true
	if diff, err := e.diffForTask(ctx, task); err != nil {
		slog.Error("auto-merge: diff", "task", task.ID, "err", err)
	} else {
		paths = policy.PathsFromDiff(diff)
		filesUnknown = len(paths) == 0
	}
	var policyFiles, protected []string
	for _, path := range paths {
		switch {
		case policy.IsPolicyPath(path):
			policyFiles = append(policyFiles, path)
		case policy.MatchAny(eff.Presets.HumanReviewPaths, path):
			protected = append(protected, path)
		}
	}
	// Окончательное решение — с фактами PR.
	d, err := decide(filesUnknown, policyFiles, protected)
	if err != nil {
		if err := e.policyEngineDown(ctx, policy.PointMerge, p.ID, eff, err); err != nil {
			return err
		}
		return deferMerge("engine_error", "движок политик не дал решения", nil)
	}
	if err := e.policyEngineUp(ctx, p.ID); err != nil {
		return err
	}
	switch {
	case !d.Allow && d.Reason == "auto_merge_off":
		return nil
	case len(policyFiles) > 0:
		// Метаправило в коде и поверх движка: PR с файлами политики никогда
		// не мержится автоматически, нужен человек, и разрешающий ответ
		// движка этого не отменяет (спека access-policy «Защита от
		// самоослабления»).
		if err := deferMerge("policy_file", "PR изменяет файлы политики ("+strings.Join(policyFiles, ", ")+")", policyFiles); err != nil {
			return err
		}
		return e.St.Escalate(ctx, p.ID, task.ID, domain.AttPolicyChange,
			"изменение политики требует человека: PR меняет "+strings.Join(policyFiles, ", "))
	case !d.Allow:
		switch d.Reason {
		case "files_unknown":
			return deferMerge(d.Reason, "список изменённых файлов PR получить не удалось", nil)
		case "human_review_path":
			return deferMerge(d.Reason, "PR меняет пути, требующие человека ("+strings.Join(protected, ", ")+")", protected)
		default:
			return deferMerge(d.Reason, "авто-merge запрещён политикой", nil)
		}
	}
	payload := eff.Ref()
	payload["status"], payload["auto_merge"] = "done", true
	payload["point"] = policy.PointMerge
	// Операция merge сериализована с подтверждениями человека: под
	// блокировкой задача перечитывается, и если её уже увели с этого входа
	// в merge (смержил или вернул агенту человек), авто-merge ничего не делает.
	return e.withMergeLock(task.ID, func() error {
		fresh, err := e.St.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		if fresh.Status != domain.TaskReview || fresh.StepID != task.StepID || fresh.StepGen != task.StepGen {
			return nil
		}
		err = e.mergeTask(ctx, fresh, p, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "task.status",
			Text: "PR смержен автоматически по политике " + shortHash(eff.Hash) + " — задача выполнена", Payload: payload,
		})
		if err == nil {
			return nil
		}
		// Конфликт с базовой веткой разрешает агент (change
		// add-merge-conflict-recovery): задача уходит на шаг code.
		if errors.Is(err, scm.ErrMergeConflict) {
			_, cerr := e.mergeConflict(ctx, fresh, p, "")
			return cerr
		}
		// PR задачи описания: отказ (задача в failed), сдвиг ветки (задача
		// на проверке) или сбой хостинга (событие и эскалация уже записаны) —
		// ничего больше делать не нужно.
		if errors.Is(err, ErrArchitecturePRRejected) || errors.Is(err, ErrArchitectureHeadMoved) ||
			errors.Is(err, ErrArchitectureFilesUnavailable) {
			return nil
		}
		// Хостинг отказал по другой причине (защита ветки, обязательные
		// проверки): задача остаётся в review, причина видна человеку событием.
		slog.Error("auto-merge", "task", task.ID, "err", err)
		failPayload := eff.Ref()
		failPayload["error"] = err.Error()
		_, evErr := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "task.merge_failed",
			ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
			Text: "авто-merge не выполнен: " + err.Error() + " — ожидание подтверждения человеком", Payload: failPayload,
		})
		return evErr
	})
}

// shortID — первые 8 символов id для ключей и текстов.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// mergeTask — merge PR → done → пересчёт DAG → автопубликации; ev — событие
// перехода (человек или система). Задача без PR в done не переходит: PR
// создаётся здесь, иначе изменения ветки потерялись бы молча.
func (e *Engine) mergeTask(ctx context.Context, task domain.Task, p domain.Project, ev store.EventInput) error {
	if task.Status != domain.TaskReview {
		return domain.ErrBadTransition{Entity: "task", From: string(task.Status), To: string(domain.TaskDone)}
	}
	taskID := task.ID
	if task.PRURL == "" {
		// Обычно PR создаёт вход на review или merge. Сюда задача без PR
		// попадает, если человек подтвердил merge раньше (задача на шаге
		// prompt до review) или дошла до merge до этого исправления.
		withPR, err := e.ensurePR(ctx, task)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrPRNotCreated, err)
		}
		task = withPR
	}
	num, err := prNumber(task.PRURL)
	if err != nil {
		return err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return err
	}
	mergeSHA := ""
	// Граница задачи описания по данным хостинга, а не только гейтом в
	// рабочей копии, и merge проверенной вершины (add-architecture-c4).
	if task.Kind == domain.KindArchitecture {
		if mergeSHA, err = e.mergeArchitecturePR(ctx, task, p, adapter, num); err != nil {
			if errors.Is(err, ErrArchitecturePRRejected) || errors.Is(err, ErrArchitectureHeadMoved) ||
				errors.Is(err, ErrArchitectureFilesUnavailable) {
				return err
			}
			return fmt.Errorf("merge PR: %w", err)
		}
	} else if mergeSHA, err = adapter.Merge(ctx, p.RepoPath, num); err != nil {
		return fmt.Errorf("merge PR: %w", err)
	}
	// Точка merge задаёт состав релиза (add-sdlc-cycle): у задачи описания
	// архитектуры её нет, документация в релиз не входит и не публикуется.
	complete := e.St.CompleteMergedTask
	if task.Kind == domain.KindArchitecture {
		complete = e.St.CompleteTask
	}
	if err := complete(ctx, taskID, ev); err != nil {
		return err
	}
	// Человек мог подтвердить merge, пока шёл review: запуски ревьюеров
	// прерываются, их поздний результат отбрасывается.
	e.CancelRunsOfMerged(ctx, task)
	if err := e.St.RecomputeEpic(ctx, task.EpicID); err != nil {
		return err
	}
	// Основная ветка изменилась: снимок архитектуры перечитывается в фоне,
	// его сбой на merge не влияет (add-architecture-c4).
	e.RefreshArchitecture(p.ID, false)
	e.DeployAfterMerge(ctx, p.ID, task, mergeSHA)
	return nil
}

// DeployAfterMerge — автопубликации после merge задачи: общий путь для
// merge в Rivet и внешнего merge по webhook. У задачи описания архитектуры
// шага deploy нет, публикация документации не запускается
// (add-architecture-c4). Шаг deploy, отключённый в процессе проекта, даёт
// deploy.deferred.
func (e *Engine) DeployAfterMerge(ctx context.Context, projectID string, task domain.Task, mergeSHA string) {
	if task.Kind == domain.KindArchitecture {
		return
	}
	if proc, _, err := e.processFor(ctx, task); err == nil && !proc.HasKind(policy.StepDeploy) {
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "deploy.deferred", ProjectID: projectID, EpicID: task.EpicID, TaskID: task.ID,
			Text:    "публикация не запускается: шаг deploy отключён в процессе проекта",
			Payload: map[string]any{"reason": "step_disabled"},
		}); err != nil {
			slog.Error("deploy step disabled event", "task", task.ID, "err", err)
		}
		return
	}
	e.enqueueAutoDeploys(ctx, projectID, mergeSHA)
}

// enqueueAutoDeploys ставит автопубликации проекта после merge (спека
// deployment «Режимы запуска»); ошибка не валит merge — публикация догонит
// со следующим merge, а проблему видно в логе. Политика проекта может
// запретить автопубликацию: тогда пишется deploy.deferred и ничего не
// ставится (спека task-pipeline «Автопубликация запрещена политикой»).
func (e *Engine) enqueueAutoDeploys(ctx context.Context, projectID, version string) {
	if version == "" {
		return
	}
	if err := e.EnqueueAutoDeploys(ctx, projectID, version); err != nil {
		slog.Error("enqueue auto deployments", "project", projectID, "err", err)
	}
}

// EnqueueAutoDeploys — автопубликации после merge с проверкой пресета
// auto_publish; общий путь для merge в Rivet и внешнего merge по webhook.
func (e *Engine) EnqueueAutoDeploys(ctx context.Context, projectID, version string) error {
	eff, err := e.St.EffectivePolicy(ctx, projectID)
	if err != nil {
		return err
	}
	d, decErr := e.decide(ctx, policy.PointPublish, map[string]any{"presets": eff.Presets})
	if decErr != nil {
		// Решения нет — автопубликация не запускается (fail-closed).
		if err := e.policyEngineDown(ctx, policy.PointPublish, projectID, eff, decErr); err != nil {
			return err
		}
		return deferDeploy(ctx, e.St, projectID, eff, "engine_error", "движок политик не дал решения")
	}
	if err := e.policyEngineUp(ctx, projectID); err != nil {
		return err
	}
	if d.Allow {
		return e.St.EnqueueAutoDeployments(ctx, projectID, version)
	}
	return deferDeploy(ctx, e.St, projectID, eff, d.Reason, "автопубликация запрещена")
}

// deferDeploy пишет deploy.deferred по автоматическим окружениям проекта;
// без таких окружений событие не нужно.
func deferDeploy(ctx context.Context, st *store.Store, projectID string, eff store.EffectivePolicy, reason, text string) error {
	envs, err := st.AutoEnvironmentNames(ctx, projectID)
	if err != nil {
		return err
	}
	if len(envs) == 0 {
		return nil
	}
	payload := eff.Ref()
	payload["environments"] = envs
	payload["reason"], payload["point"] = reason, policy.PointPublish
	_, err = st.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "deploy.deferred", ProjectID: projectID,
		Text:    "публикация отложена политикой " + shortHash(eff.Hash) + ": " + text + " (" + strings.Join(envs, ", ") + ")",
		Payload: payload,
	})
	return err
}

func (e *Engine) diffForTask(ctx context.Context, task domain.Task) (string, error) {
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return "", err
	}
	num, err := prNumber(task.PRURL)
	if err != nil {
		return "", err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return "", err
	}
	return adapter.Diff(ctx, p.RepoPath, num)
}

// prNumber извлекает номер PR из https://github.com/owner/repo/pull/N.
// Разбор строгий: хвост после последнего слэша — ровно каноническая запись
// положительного числа (без ведущих нулей, знака и мусора).
func prNumber(url string) (int, error) {
	seg := url[lastSlash(url)+1:]
	n, err := strconv.Atoi(seg)
	if err != nil || n <= 0 || strconv.Itoa(n) != seg {
		return 0, fmt.Errorf("не разобрать номер PR из %q", url)
	}
	return n, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
