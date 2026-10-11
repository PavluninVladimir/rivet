// Package orchestrator — политика конвейера: детерминированные реакции на
// результаты этапов и цикл диспетчеризации. Модель решает задачу,
// оркестратор управляет процессом (спеки backend/orchestration, task-pipeline).
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
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
	St *store.Store
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
	// транскрипты открытых сессий по session_id
	transcripts map[string][]byte
	// открытые сессии: session_id → task_id (у задачи может идти несколько
	// сессий сразу — параллельные участники шага)
	sessions map[string]string
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
	// Now — источник времени для бюджета (подменяется в тестах).
	Now func() time.Time
}

func New(st *store.Store, adapter scm.Adapter, bl *blob.Store, send Sender, heartbeat time.Duration) *Engine {
	return &Engine{
		St: st, SCM: adapter, Adapters: &scm.Factory{Fallback: adapter}, Blob: bl, Out: send,
		Policy:           policy.Default(),
		HeartbeatTimeout: heartbeat, BaseBranch: "main",
		stageContext:       map[string]string{},
		transcripts:        map[string][]byte{},
		sessions:           map[string]string{},
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
		if err := e.failDeployNow(ctx, depID, "", "deploy-runner потерян (heartbeat)"); err != nil {
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
	// Бюджет Epic (спека orchestration «Бюджет Epic»): исчерпанные Epic
	// исключаются из назначений до решения человека (поднять/снять бюджет).
	excludedEpics, err := e.epicBudgetExclusions(ctx)
	if err != nil {
		return fmt.Errorf("epic budget: %w", err)
	}
	// Ready-задачи входят на шаг процесса, ожидающие запуски участников
	// назначаются runner'ам по агенту, модели и capabilities.
	if err := e.enterReady(ctx, excluded, excludedEpics); err != nil {
		return err
	}
	for {
		a, ok, err := e.St.AssignRun(ctx, excluded, excludedEpics)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatchAssigned(ctx, a)
	}
	if err := e.markWaiting(ctx); err != nil {
		return err
	}
	if err := e.reconcileSteps(ctx); err != nil {
		return err
	}
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
func (e *Engine) SCMFor(ctx context.Context, p domain.Project) (scm.Adapter, error) {
	token, err := e.St.ProjectToken(ctx, p.ID, e.Box)
	if err != nil {
		return nil, fmt.Errorf("учётные данные проекта %s: %w", p.ID, err)
	}
	return e.Adapters.For(scm.Provider(p.Provider), p.BaseURL, token)
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

// dispatchRun открывает сессию запуска участника и отправляет Assignment
// runner'у. Возвращает id сессии (пусто, если стадию не удалось
// запустить) и признак доставки. spec — переопределение сессии (сессия
// доработки); nil — обычная scheduler-сессия со снимком задачи.
func (e *Engine) dispatchRun(ctx context.Context, task domain.Task, run store.StepRun, runner domain.Runner,
	step policy.ResolvedStep, entry string, spec *sessionSpec) (string, bool) {
	stage := stageFor(step.Kind, entry)
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		slog.Error("dispatch: project", "task", task.ID, "err", err)
		return "", false
	}
	if runner.Agent == "" {
		if r, err := e.St.GetRunner(ctx, runner.ID); err == nil {
			runner = r
		}
	}
	criteria := make([]string, 0, len(task.Criteria))
	for _, c := range task.Criteria {
		criteria = append(criteria, c.Text)
	}
	checks := make([]*pb.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, &pb.Check{Name: c.Name, Cmd: c.Cmd})
	}
	// Контекст стадии: замечания при исправлении, diff и отчёт проверок
	// ревьюеру. Читается без удаления: параллельным участникам нужен один
	// и тот же контекст, сбрасывает его исход шага.
	extra := ""
	switch {
	case step.Kind == policy.StepReview:
		extra = e.reviewContext(ctx, task)
	case step.Kind == policy.StepCode && entry == policy.OutcomeChanges:
		extra = e.peekStageContext(task.ID)
	}
	// Политика проекта едет вместе с назначением: у runner'а нет другого
	// источника, рабочая копия на исполнение не влияет (спека access-policy
	// «Доставка политик runner'ам»). Ошибка чтения не валит стадию — агент
	// работает как прежде, гейты на plane никуда не делись.
	var assignPolicy *pb.Policy
	var agentModels map[string]policy.AgentModel
	if eff, err := e.St.EffectivePolicy(ctx, p.ID); err != nil {
		slog.Error("dispatch: политика проекта", "project", p.ID, "err", err)
	} else {
		assignPolicy = &pb.Policy{
			Hash: eff.Hash, HumanReviewPaths: eff.Presets.HumanReviewPaths,
			PolicyDir: policy.PolicyDir,
		}
		agentModels = eff.Presets.AgentModels
	}
	// Профиль агента из каталога (add-agent-profiles, спека agents «Доставка
	// модели и учётных данных runner'у»): модель участника, иначе
	// переопределение проекта, иначе модель по умолчанию; окружение и
	// аргументы по шаблону, секреты по режиму профиля и каналу runner'а.
	model := run.Model
	var launch *store.AgentLaunch
	// Окружение профиля уезжает только runner'ам, объявившим протокол v12:
	// прежние полей не знают, а секреты без адресата отправлять незачем.
	if runner.Catalog && runner.Protocol == "12" {
		var override *policy.AgentModel
		if am, ok := agentModels[runner.Agent]; ok {
			override = &am
		}
		prof, binding, ok, rerr := e.St.ResolveAgentModel(ctx, runner.Agent, run.Model, override)
		if rerr != nil {
			slog.Error("dispatch: профиль агента", "agent", runner.Agent, "err", rerr)
		} else if ok {
			include := prof.Secrets == "always" || (prof.Secrets == "secure" && runner.Secure)
			l, berr := e.St.BuildAgentLaunch(ctx, prof, binding, e.Box, include)
			if berr != nil {
				slog.Error("dispatch: окружение агента", "agent", runner.Agent, "err", berr)
			} else {
				launch = &l
				if binding.Model != "" {
					model = binding.Model
				}
			}
		}
	}
	if model != "" && model != run.Model {
		if err := e.St.SetRunModel(ctx, run.ID, model); err != nil {
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
	// Сессия создаётся до отправки Assignment: runner повторяет session_id
	// во всех сообщениях стадии, сообщения без него отбрасываются (design,
	// решение 4). Без сессии стадию не запускаем — задачу вернёт
	// heartbeat-таймаут, как при недоступном runner'е.
	if spec == nil {
		// Запрос сессии — снимок задачи на момент запуска (история и поиск,
		// спека team-visibility): описание задачи меняется ответами человека.
		prompt := task.Title
		if task.Description != "" {
			prompt += "\n" + task.Description
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
		return "", false
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
		return "", false
	}
	e.mu.Lock()
	e.sessions[sessionID] = task.ID
	delete(e.transcripts, sessionID)
	e.mu.Unlock()
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
		return "", false
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
			Title: task.Title, Description: task.Description,
			Criteria: criteria, Repo: p.Repo(), Branch: task.Branch,
			Checks: checks, ExtraContext: extra, SessionId: sessionID,
			RepoUrl: repoURL, GitToken: gitToken, BaseBranch: p.DefaultBranch,
			UserPrompt: spec.userPrompt, Policy: assignPolicy,
			Model: model, StepId: step.ID, Participant: run.Participant,
			StepPrompt: step.Prompt,
		}},
	}
	if launch != nil {
		as := msg.GetAssign()
		as.AgentEnv, as.AgentArgs, as.AgentCommand, as.ConnectionId = launch.Env, launch.Args, launch.Command, launch.ConnectionID
		as.SecretsIncluded = len(launch.SecretNames) > 0
		as.AgentSecretNames = launch.SecretNames
	}
	if !e.Out.Send(runner.ID, msg) {
		slog.Warn("dispatch: runner недоступен, задачу вернёт heartbeat-таймаут",
			"runner", runner.ID, "task", task.ID)
		return sessionID, false
	}
	if launch != nil && len(launch.SecretNames) > 0 {
		// Факт доставки секретов в event log без значений (спека agents).
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorRunner, ActorID: runner.ID, Type: "runner.secrets_delivered",
			ProjectID: p.ID, TaskID: task.ID,
			Text:    "runner " + runner.ID + " получил учётные данные подключения " + launch.ConnectionID,
			Payload: map[string]any{"runner_id": runner.ID, "task_id": task.ID, "connection_id": launch.ConnectionID, "env_names": launch.SecretNames, "agent": runner.Agent},
		}); err != nil {
			slog.Error("dispatch: событие доставки секретов", "err", err)
		}
	}
	return sessionID, true
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
		}
	}
}

// dropSessionID забывает одну сессию (прерван один участник шага).
func (e *Engine) dropSessionID(sessionID string) {
	e.mu.Lock()
	delete(e.sessions, sessionID)
	delete(e.transcripts, sessionID)
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
	verdict := verdictOf(step.Kind, sr.Ok, sr.Verdict)
	// Runner остаётся за задачей, если следующий шаг можно отдать ему же
	// (тот же worktree: code → test, провал проверок → исправление).
	// С шага review runner не переиспользуется: ревьюер не должен
	// исправлять по собственным замечаниям (спека task-pipeline
	// «Независимое review»).
	keep := false
	if next, found := proc.Step(step.Target(verdict)); found && step.Kind != policy.StepReview && !e.epicPaused(ctx, task) {
		keep = e.reuseTarget(ctx, task, next, runnerID)
	}
	claimed, err := e.St.RecordVerdict(ctx, run.ID, verdict, sr.Detail, keep)
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
	failPayload := map[string]any{"status": "failed"}
	for _, m := range extra {
		for k, v := range m {
			failPayload[k] = v
		}
	}
	return e.St.TransitionTask(ctx, task.ID, domain.TaskFailed,
		store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.status",
			Text: msg, Payload: failPayload},
		func(tx pgx.Tx) error {
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
				VALUES ($1,$2,'TEST_FAILED',$3)`, epic.ProjectID, task.ID, msg)
			return err
		})
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
	a, err := e.St.StartUserSession(ctx, taskID, login, private)
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
	sessionID, delivered := e.dispatchRun(ctx, a.Task, runs[0], a.Runner, step, policy.OutcomeChanges, &sessionSpec{
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
		return "", fmt.Errorf("сессию не удалось запустить: runner недоступен или стадия не собралась, подробности в логе rivetd")
	}
	return sessionID, nil
}

// MergeTask — подтверждение человека: merge PR → done → пересчёт DAG.
// Доступно при любой политике, в том числе когда авто-merge отложен.
func (e *Engine) MergeTask(ctx context.Context, taskID, userID string) error {
	task, err := e.St.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	return e.mergeTask(ctx, task, p, store.EventInput{
		ActorKind: domain.ActorUser, ActorID: userID, Type: "task.status",
		Text: "PR смержен — задача выполнена", Payload: map[string]any{"status": "done"},
	})
}

// autoMerge — решение точки принуждения после одобренного review при
// включённом пресете: пути PR из diff адаптера, метаправило про файлы
// политики (.rivet/) с эскалацией POLICY_CHANGE, защищённые пути с
// отложенным merge, fail-closed без списка путей; иначе merge от системного
// актора с версией политики в payload.
func (e *Engine) autoMerge(ctx context.Context, task domain.Task, p domain.Project, eff store.EffectivePolicy) error {
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
	filesUnknown := false
	if task.PRURL != "" {
		diff, err := e.diffForTask(ctx, task)
		if err != nil {
			slog.Error("auto-merge: diff", "task", task.ID, "err", err)
			filesUnknown = true
		} else {
			paths = policy.PathsFromDiff(diff)
			filesUnknown = len(paths) == 0 && strings.TrimSpace(diff) != ""
		}
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
	err = e.mergeTask(ctx, task, p, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "task.status",
		Text: "PR смержен автоматически по политике " + shortHash(eff.Hash) + " — задача выполнена", Payload: payload,
	})
	if err == nil {
		return nil
	}
	// Хостинг отказал (конфликт, защита ветки): как при ручном merge задача
	// остаётся в review, а причина видна человеку — событием, не только логом.
	slog.Error("auto-merge", "task", task.ID, "err", err)
	failPayload := eff.Ref()
	failPayload["error"] = err.Error()
	_, evErr := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "task.merge_failed",
		ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
		Text: "авто-merge не выполнен: " + err.Error() + " — ожидание подтверждения человеком", Payload: failPayload,
	})
	return evErr
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
// перехода (человек или система).
func (e *Engine) mergeTask(ctx context.Context, task domain.Task, p domain.Project, ev store.EventInput) error {
	if task.Status != domain.TaskReview {
		return domain.ErrBadTransition{Entity: "task", From: string(task.Status), To: string(domain.TaskDone)}
	}
	taskID := task.ID
	mergeSHA := ""
	if task.PRURL != "" {
		num, err := prNumber(task.PRURL)
		if err != nil {
			return err
		}
		adapter, aerr := e.SCMFor(ctx, p)
		if aerr != nil {
			return aerr
		}
		if mergeSHA, err = adapter.Merge(ctx, p.RepoPath, num); err != nil {
			return fmt.Errorf("merge PR: %w", err)
		}
	}
	if err := e.St.CompleteTask(ctx, taskID, ev); err != nil {
		return err
	}
	if err := e.St.RecomputeEpic(ctx, task.EpicID); err != nil {
		return err
	}
	// Шаг deploy отключён в процессе проекта — автопубликация не ставится.
	if proc, _, err := e.processFor(ctx, task); err == nil && !proc.HasKind(policy.StepDeploy) {
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorSystem, Type: "deploy.deferred", ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
			Text:    "публикация не запускается: шаг deploy отключён в процессе проекта",
			Payload: map[string]any{"reason": "step_disabled"},
		}); err != nil {
			slog.Error("deploy step disabled event", "task", task.ID, "err", err)
		}
		return nil
	}
	e.enqueueAutoDeploys(ctx, p.ID, mergeSHA)
	return nil
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
