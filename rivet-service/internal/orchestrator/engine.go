// Package orchestrator — политика конвейера: детерминированные реакции на
// результаты этапов и цикл диспетчеризации. Модель решает задачу,
// оркестратор управляет процессом (спеки backend/orchestration, task-pipeline).
package orchestrator

import (
	"context"
	"errors"
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
	// контекст для следующего назначения стадии (вывод тестов, вердикт review)
	stageContext map[string]string
	// транскрипт текущей стадии задачи
	transcripts map[string][]byte
	// открытая сессия стадии задачи
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
	// Назначения: кодирование, исправления, review — до исчерпания кандидатов.
	for {
		a, ok, err := e.St.AssignNext(ctx, excluded, excludedEpics)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_CODING, "")
	}
	for {
		a, ok, err := e.St.AssignFixing(ctx, excluded, excludedEpics)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_FIXING, e.takeStageContext(a.Task.ID))
	}
	for {
		a, ok, err := e.St.AssignTesting(ctx, excluded, excludedEpics)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatch(ctx, a, pb.StageResult_TESTING, "")
	}
	for {
		a, ok, err := e.St.AssignReview(ctx, excluded, excludedEpics)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		// Контекст ревьюера: diff PR + отчёт автопроверок.
		extra := e.takeStageContext(a.Task.ID)
		if a.Task.PRURL != "" {
			d, err := e.diffForTask(ctx, a.Task)
			if errors.Is(err, scm.ErrDiffTruncated) {
				// Ревьюеру начало diff'а полезнее, чем ничего.
				d, err = d+"\n…[diff обрезан: превышен лимит чтения]\n", nil
			}
			if err != nil {
				slog.Error("diff for review", "task", a.Task.ID, "err", err)
			} else if extra != "" {
				extra = d + "\n\n" + extra
			} else {
				extra = d
			}
		}
		e.dispatch(ctx, a, pb.StageResult_REVIEW, extra)
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

func (e *Engine) takeStageContext(taskID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.stageContext[taskID]
	delete(e.stageContext, taskID)
	return c
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

// dispatch отправляет Assignment runner'у и открывает сессию стадии.
func (e *Engine) dispatch(ctx context.Context, a store.Assignment, stage pb.StageResult_Stage, extra string) {
	_, _ = e.dispatchWith(ctx, a, stage, extra, nil)
}

// dispatchWith возвращает id созданной сессии (пусто, если стадию не
// удалось запустить) и признак доставки Assignment runner'у.
func (e *Engine) dispatchWith(ctx context.Context, a store.Assignment, stage pb.StageResult_Stage, extra string, spec *sessionSpec) (string, bool) {
	p, _, err := e.projectOf(ctx, a.Task)
	if err != nil {
		slog.Error("dispatch: project", "task", a.Task.ID, "err", err)
		return "", false
	}
	criteria := make([]string, 0, len(a.Task.Criteria))
	for _, c := range a.Task.Criteria {
		criteria = append(criteria, c.Text)
	}
	checks := make([]*pb.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, &pb.Check{Name: c.Name, Cmd: c.Cmd})
	}
	runnerID := a.Runner.ID
	// Глубина сессии — глубина адаптера runner'а (спека agent-integration
	// «Глубина объявлена runner'ом»).
	depth, err := e.St.RunnerDepth(ctx, runnerID)
	if err != nil {
		slog.Error("dispatch: runner depth", "runner", runnerID, "err", err)
		depth = domain.DepthMinimal
	}
	// Сессия создаётся до отправки Assignment: runner повторяет session_id
	// во всех сообщениях стадии, сообщения без него отбрасываются (design,
	// решение 4). Без сессии стадию не запускаем — задачу вернёт
	// heartbeat-таймаут, как при недоступном runner'е.
	if spec == nil {
		// Запрос сессии — снимок задачи на момент запуска (история и поиск,
		// спека team-visibility): описание задачи меняется ответами человека.
		prompt := a.Task.Title
		if a.Task.Description != "" {
			prompt += "\n" + a.Task.Description
		}
		spec = &sessionSpec{driverKind: "scheduler", prompt: prompt}
	}
	sessionID, err := e.St.CreateSession(ctx, domain.Session{
		TaskID: a.Task.ID, Attempt: a.Task.AttemptUsed + 1,
		DriverKind: spec.driverKind, DriverID: spec.driverID,
		Agent: a.Runner.Agent, Model: a.Runner.Model,
		Depth: depth, Scope: stage.String(), Prompt: spec.prompt, Private: spec.private,
	})
	if err != nil {
		slog.Error("dispatch: session", "task", a.Task.ID, "err", err)
		return "", false
	}
	e.mu.Lock()
	e.sessions[a.Task.ID] = sessionID
	delete(e.transcripts, a.Task.ID)
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
		e.DropSession(a.Task.ID)
		return "", false
	}
	// У fake-провайдера (e2e-стенд) настоящего адреса нет: клонирование
	// идёт по RIVET_GIT_BASE, как до этого изменения.
	repoURL := p.WebURL()
	if p.Provider == string(scm.ProviderFake) {
		repoURL, gitToken = "", ""
	}
	msg := &pb.PlaneMsg{
		MsgId: fmt.Sprintf("assign-%s-%s-%d", a.Task.ID, stage, time.Now().UnixNano()),
		Kind: &pb.PlaneMsg_Assign{Assign: &pb.Assignment{
			TaskId: a.Task.ID, TaskNum: a.Task.Num, Stage: stage,
			Title: a.Task.Title, Description: a.Task.Description,
			Criteria: criteria, Repo: p.Repo(), Branch: a.Task.Branch,
			Checks: checks, ExtraContext: extra, SessionId: sessionID,
			RepoUrl: repoURL, GitToken: gitToken, BaseBranch: p.DefaultBranch,
			UserPrompt: spec.userPrompt,
		}},
	}
	if !e.Out.Send(runnerID, msg) {
		slog.Warn("dispatch: runner недоступен, задачу вернёт heartbeat-таймаут",
			"runner", runnerID, "task", a.Task.ID)
		return sessionID, false
	}
	return sessionID, true
}

// transcriptCap — жёсткий предел буфера транскрипта стадии: после append
// буфер не превышает кап, лишнее отбрасывается с маркером обрезки
// (api-contract: транскрипт отдаётся целиком одним ответом).
const transcriptCap = 4 << 20

var truncMarker = []byte("\n…[транскрипт обрезан: превышен лимит 4 МБ]\n")

// SessionMatches — принадлежит ли сообщение текущей сессии задачи.
// После рестарта rivetd карта сессий пуста: открытая сессия задачи
// поднимается из БД, чтобы не терять результаты стадий, назначенных
// до рестарта (доставка at-least-once переживает рестарт plane).
func (e *Engine) SessionMatches(ctx context.Context, taskID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	e.mu.Lock()
	cur, known := e.sessions[taskID]
	e.mu.Unlock()
	if known {
		return cur == sessionID
	}
	open, err := e.St.OpenSession(ctx, taskID)
	if err != nil || open == "" || open != sessionID {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, known := e.sessions[taskID]; known {
		return cur == sessionID
	}
	e.sessions[taskID] = sessionID
	return true
}

// DropSession инвалидирует память о сессии задачи. Вызывается там, где
// сессия закрывается в БД мимо StageResult/Blocked (отмена, потеря
// runner'а): иначе SessionMatches продолжил бы принимать поздние сообщения
// закрытой сессии из кеша (например, CreatePR после отмены).
func (e *Engine) DropSession(taskID string) {
	e.mu.Lock()
	delete(e.sessions, taskID)
	delete(e.transcripts, taskID)
	e.mu.Unlock()
}

// OnTranscript накапливает чанки транскрипта текущей стадии. Чанки чужой
// сессии отбрасываются; принадлежность проверяет вызывающий через
// SessionMatches (он же поднимает сессию из БД после рестарта), здесь —
// только сверка с картой под общим замком.
func (e *Engine) OnTranscript(taskID, sessionID string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sessionID == "" || e.sessions[taskID] != sessionID {
		return
	}
	buf := e.transcripts[taskID]
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
		e.transcripts[taskID] = append(buf, truncMarker...)
		return
	}
	e.transcripts[taskID] = append(buf, data...)
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
	if sessionID == "" || e.sessions[task.ID] != sessionID {
		e.mu.Unlock()
		return false
	}
	buf := e.transcripts[task.ID]
	delete(e.transcripts, task.ID)
	delete(e.sessions, task.ID)
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

// OnStageResult — детерминированные реакции конвейера (спека task-pipeline).
// Результат чужой сессии (replay после reconnect) отбрасывается; Detail —
// runner-controlled текст, идущий в event log и контекст следующей стадии,
// поэтому маскируется на входе.
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
	ev := store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.status"}

	switch sr.Stage {
	case pb.StageResult_CODING, pb.StageResult_FIXING:
		if !sr.Ok {
			// Текст ошибки приватной сессии не раскрывается в публичных
			// событии и эскалации; полный текст — в итоге сессии (автору).
			detail := sr.Detail
			if private, _, perr := e.St.SessionPrivacy(ctx, sr.SessionId); perr == nil && private {
				detail = "ошибка приватной сессии — подробности доступны её автору в итоге сессии"
			}
			return e.failTask(ctx, task, "Невосстановимая ошибка этапа: "+detail, runnerID)
		}
		ev.Text = "реализация готова — запуск проверок"
		ev.Payload = map[string]any{"status": "testing"}
		if err := e.St.TransitionTask(ctx, task.ID, domain.TaskTesting, ev, nil); err != nil {
			return err
		}
		if e.epicPaused(ctx, task) {
			// Пауза Epic: безопасная точка — граница стадии. Проверки не
			// запускаем, runner освобождаем; resume подхватит AssignTesting.
			return e.St.ReleaseTaskRunner(ctx, task.ID)
		}
		a := store.Assignment{Task: task, Runner: domain.Runner{ID: runnerID}}
		e.dispatch(ctx, a, pb.StageResult_TESTING, "")
		return nil

	case pb.StageResult_TESTING:
		if !sr.Ok {
			// Провал проверок расходует попытку (спека orchestration).
			// Вне паузы исправление идёт тем же runner'ом в том же worktree;
			// на паузе runner освобождается, fixing подхватит AssignFixing.
			// Вывод проверок кладётся до перехода в fixing: сразу после
			// коммита resume может назначить FIXING, контекст уже должен лежать.
			e.mu.Lock()
			e.stageContext[task.ID] = "Вывод проверок:\n" + sr.Detail
			e.mu.Unlock()
			paused := e.epicPaused(ctx, task)
			failed, err := e.St.ConsumeAttempt(ctx, task.ID, domain.AttTestFailed, sr.Detail, !paused, 0)
			if err != nil || failed {
				e.takeStageContext(task.ID)
				return err
			}
			if paused {
				return nil
			}
			a := store.Assignment{Task: task, Runner: domain.Runner{ID: runnerID}}
			e.dispatch(ctx, a, pb.StageResult_FIXING, e.takeStageContext(task.ID))
			return nil
		}
		// Отчёт автопроверок сохраняется для ревьюера (спека task-pipeline:
		// ревьюер получает diff, критерии и результаты проверок).
		if sr.Detail != "" {
			e.mu.Lock()
			e.stageContext[task.ID] = "Результаты автопроверок:\n" + sr.Detail
			e.mu.Unlock()
		}
		// Тесты прошли: PR (если ещё нет) → review, исполнитель освобождается.
		if task.PRURL == "" {
			p, _, err := e.projectOf(ctx, task)
			if err != nil {
				return err
			}
			adapter, err := e.SCMFor(ctx, p)
			if err != nil {
				return err
			}
			pr, err := adapter.CreatePR(ctx, p.RepoPath, task.Branch, p.DefaultBranch,
				fmt.Sprintf("task-%d: %s", task.Num, task.Title), task.Description)
			if err != nil {
				return fmt.Errorf("create PR: %w", err)
			}
			if err := e.St.SetTaskPR(ctx, task.ID, pr.URL); err != nil {
				return err
			}
			if sr.PrUrl == "" {
				sr.PrUrl = pr.URL
			}
		}
		ev.Text = "проверки прошли, PR создан — ожидание review"
		ev.Payload = map[string]any{"status": "review", "pr": sr.PrUrl}
		return e.St.TransitionWithRunnerRelease(ctx, task.ID, domain.TaskReview, ev)

	case pb.StageResult_REVIEW:
		p, _, err := e.projectOf(ctx, task)
		if err != nil {
			return err
		}
		eff, err := e.St.EffectivePolicy(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("policy: %w", err)
		}
		if sr.Ok {
			// Освобождаем runner-ревьюера, но reviewer_id на задаче сохраняем:
			// он же признак «review выполнен» — иначе планировщик назначит review заново.
			if err := e.St.FreeReviewerRunner(ctx, task.ID); err != nil {
				return err
			}
			if _, err := e.St.AppendEvent(ctx, store.EventInput{
				ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.review_passed",
				ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
				Text: "review пройден — ожидание подтверждения merge",
			}); err != nil {
				return err
			}
			// Авто-merge — гейт политики (спека task-pipeline «Merge после
			// успешной проверки»). Решает движок, а не пресет напрямую:
			// во внешнем режиме локальные пресеты вообще не главные.
			return e.autoMerge(ctx, task, p, eff)
		}
		// Ревьюера освобождает ConsumeAttempt в той же транзакции, что и
		// переход в fixing: без окна, где Tick назначил бы review повторно.
		e.mu.Lock()
		e.stageContext[task.ID] = "Замечания review:\n" + sr.Detail
		e.mu.Unlock()
		_, err = e.St.ConsumeAttempt(ctx, task.ID, domain.AttReviewLimit, sr.Detail, false, eff.Presets.ReviewLimit)
		return err

	default:
		return fmt.Errorf("неизвестная стадия %v", sr.Stage)
	}
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
func (e *Engine) failTask(ctx context.Context, task domain.Task, msg, runnerID string) error {
	return e.St.TransitionTask(ctx, task.ID, domain.TaskFailed,
		store.EventInput{ActorKind: domain.ActorRunner, ActorID: runnerID, Type: "task.status",
			Text: msg, Payload: map[string]any{"status": "failed"}},
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
	sessionID, delivered := e.dispatchWith(ctx, a, pb.StageResult_FIXING, "", &sessionSpec{
		driverKind: "user", driverID: login, prompt: prompt,
		userPrompt: prompt, private: private,
	})
	if sessionID == "" || !delivered {
		// Стадию не удалось запустить или Assignment не доставлен: сессия
		// закрывается, runner освобождается, задача остаётся в fixing без
		// runner'а — её подхватит AssignFixing обычным промптом.
		// Пользователь видит отказ, а не 200 без запуска.
		if sessionID != "" {
			if _, cerr := e.St.EndSession(ctx, sessionID, "", "сессия не запущена: runner недоступен"); cerr != nil {
				slog.Error("user session: end undelivered session", "session", sessionID, "err", cerr)
			}
			e.DropSession(taskID)
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
