package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Интерпретатор процесса (change add-process-model, спеки backend/process,
// orchestration): задача стоит на шаге снимка процесса, участники шага
// исполняются запусками, вердикты агрегируются правилом шага, переход
// берётся из документа. Переходы по-прежнему решает код, документ лишь
// описывает порядок и участников.

// processFor — снимок процесса задачи; у задачи без снимка — действующий
// процесс проекта (снимок запишет первый вход на шаг).
func (e *Engine) processFor(ctx context.Context, task domain.Task) (*policy.Resolved, string, error) {
	if p := store.TaskProcess(task); p != nil {
		return p, task.ProcessHash, nil
	}
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return nil, "", err
	}
	eff, err := e.St.EffectivePolicy(ctx, p.ID)
	if err != nil {
		return nil, "", err
	}
	r := eff.Presets.EffectiveProcess()
	return &r, eff.Hash, nil
}

// stageFor — стадия протокола по типу шага и входу: code с начала — CODING,
// code по changes — FIXING, test — TESTING, review — REVIEW.
func stageFor(kind, entry string) pb.StageResult_Stage {
	switch kind {
	case policy.StepTest:
		return pb.StageResult_TESTING
	case policy.StepReview:
		return pb.StageResult_REVIEW
	case policy.StepPrompt:
		return pb.StageResult_PROMPT
	}
	if entry == policy.OutcomeChanges {
		return pb.StageResult_FIXING
	}
	return pb.StageResult_CODING
}

// enterReady вводит ready-задачи на первый шаг процесса или возобновляет
// шаг, на котором задача стояла (потеря runner'а, решение человека).
func (e *Engine) enterReady(ctx context.Context, excluded, excludedEpics []string) error {
	tasks, err := e.St.ReadyToEnter(ctx, excluded, excludedEpics)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		proc, hash, err := e.processFor(ctx, task)
		if err != nil {
			slog.Error("process: enter ready", "task", task.ID, "err", err)
			continue
		}
		step, ok := proc.First()
		if !ok {
			continue
		}
		entry := policy.OutcomeOk
		if task.StepID != "" {
			if s, found := proc.Step(task.StepID); found {
				step = s
				if task.StepEntry != "" {
					entry = task.StepEntry
				}
			} else {
				// Шага нет в снимке (задача до процесса, чей процесс с тех пор
				// изменился): начинаем с первого шага и говорим об этом.
				slog.Warn("process: шаг задачи не найден в процессе, вход с первого шага",
					"task", task.ID, "step", task.StepID, "first", step.ID)
			}
		}
		if err := e.enterStep(ctx, task, proc, hash, step, entry, "", nil); err != nil {
			slog.Error("process: enter step", "task", task.ID, "step", step.ID, "err", err)
		}
	}
	return nil
}

// stepText — текст события входа на шаг.
func stepText(step policy.ResolvedStep, entry string, extraText string) string {
	if extraText != "" {
		return extraText
	}
	switch step.Kind {
	case policy.StepTest:
		return "реализация готова — запуск проверок"
	case policy.StepReview:
		return "ожидание review: " + step.Title
	case policy.StepCode:
		if entry == policy.OutcomeChanges {
			return "исправление: " + step.Title
		}
		return "переход к реализации: " + step.Title
	}
	return "шаг " + step.Title
}

// enterStep — вход задачи на шаг. reuseRunner — runner, уже занятый задачей
// (тот же worktree), которому единственный участник назначается сразу.
// Шаг merge исполняет control plane (подтверждение человеком или авто-merge),
// deploy достигается только из merge и обрабатывается там.
func (e *Engine) enterStep(ctx context.Context, task domain.Task, proc *policy.Resolved, hash string,
	step policy.ResolvedStep, entry, reuseRunner string, ev *store.EventInput) error {
	switch step.Kind {
	case policy.StepMerge:
		return e.enterMerge(ctx, task, proc, hash, ev)
	case policy.StepDeploy:
		// Достижимо только переходом ok с merge: публикацию ставит mergeTask.
		return nil
	}
	in := store.EnterStep{
		TaskID: task.ID, Step: step, Entry: entry,
		ReuseRunner: reuseRunner, ReleaseRunners: step.Kind == policy.StepReview,
		Roles:   e.roleBindings(ctx, task, step),
		Payload: map[string]any{"process_hash": hash},
	}
	if len(task.Process) == 0 {
		in.Process, in.ProcessHash = proc, hash
	}
	if ev != nil {
		in.Actor = *ev
		in.Text = ev.Text
		for k, v := range payloadMap(ev.Payload) {
			in.Payload[k] = v
		}
	}
	if task.Status != domain.TaskReady {
		in.Text = stepText(step, entry, in.Text)
	}
	runs, err := e.St.EnterStep(ctx, in)
	if err != nil {
		return err
	}
	// Store мог отказаться переиспользовать runner (его уже освободили):
	// тогда запуск ждёт планировщика.
	if reuseRunner != "" && len(runs) > 0 && runs[0].RunnerID == reuseRunner {
		task.StepID, task.StepEntry = step.ID, entry
		e.dispatchOrRelease(ctx, task, runs[0], domain.Runner{ID: reuseRunner}, step, entry, nil)
	}
	return nil
}

// startRetryPause и startRetryMax — пауза перед повторным назначением
// стадии, которая не смогла стартовать по причине до отправки (проект,
// сессия, учётные данные, шаг в снимке): 30 с, 60 с, 120 с и до 5 мин,
// пока стадия не будет доставлена.
const (
	startRetryPause = 30 * time.Second
	startRetryMax   = 5 * time.Minute
)

// connChecker — отправитель знает, подключён ли runner (реестр каналов).
type connChecker interface{ Connected(runnerID string) bool }

// dispatchOrRelease — назначение стадии. Если стадия не стартовала, запуск
// отвязывается от runner'а и агента команды, а runner освобождается: иначе
// открытый запуск держал бы агента занятым навсегда. Причина до отправки
// откладывает повтор с причиной ожидания; недоставка на отключённый runner
// переводит его в offline (признак жизни вернёт). false — стадия не
// стартовала.
func (e *Engine) dispatchOrRelease(ctx context.Context, task domain.Task, run store.StepRun, runner domain.Runner,
	step policy.ResolvedStep, entry string, assigned *store.PendingAssign) bool {
	// Снимок last_seen до отправки: по нему недоставка отличает мёртвый
	// runner от переподключившегося. Назначение планировщика несёт снимок
	// из своей транзакции, переиспользование runner'а — только его id.
	if runner.LastSeen.IsZero() {
		if r, err := e.St.GetRunner(ctx, runner.ID); err == nil {
			runner = r
		}
	}
	sessionID, delivered, startErr := e.dispatchRun(ctx, task, run, runner, step, entry, nil)
	if sessionID != "" && delivered {
		if err := e.St.RunDelivered(ctx, run.ID, assigned); err != nil {
			slog.Error("dispatch: delivered", "run", run.ID, "err", err)
		}
		return true
	}
	if sessionID != "" {
		if _, err := e.St.EndSession(ctx, sessionID, "", "стадия не запущена: runner недоступен"); err != nil {
			slog.Error("dispatch: end undelivered session", "session", sessionID, "err", err)
		}
		e.dropSessionID(sessionID)
	}
	if err := e.St.UndoAssign(ctx, task.ID, assigned); err != nil {
		slog.Error("dispatch: undo assign", "task", task.ID, "err", err)
	}
	if startErr != nil {
		e.deferRun(ctx, task, run, runner.ID, "стадия не запустилась: "+startErr.Error())
		return false
	}
	if err := e.St.UnbindRun(ctx, run.ID); err != nil {
		slog.Error("dispatch: unbind run", "run", run.ID, "err", err)
	}
	if err := e.St.FreeRunnerFor(ctx, runner.ID, task.ID); err != nil {
		slog.Error("dispatch: free runner", "runner", runner.ID, "err", err)
	}
	if c, ok := e.Out.(connChecker); ok && sessionID != "" && !c.Connected(runner.ID) {
		if marked, err := e.St.MarkRunnerOffline(ctx, runner.ID, runner.LastSeen); err != nil {
			slog.Error("dispatch: runner offline", "runner", runner.ID, "err", err)
		} else if marked {
			slog.Warn("runner не подключён: назначения до признака жизни не идут", "runner", runner.ID)
		}
	}
	return false
}

// deferRun — запуск откладывается с причиной ожидания, runner свободен.
func (e *Engine) deferRun(ctx context.Context, task domain.Task, run store.StepRun, runnerID, reason string) {
	first, pause, err := e.St.DeferRun(ctx, run.ID, task.ID, startRetryPause, startRetryMax, reason)
	if err != nil {
		slog.Error("dispatch: defer run", "run", run.ID, "err", err)
	}
	if err := e.St.FreeRunnerFor(ctx, runnerID, task.ID); err != nil {
		slog.Error("dispatch: free runner", "runner", runnerID, "err", err)
	}
	if !first {
		return
	}
	projectID, epicID, err := e.St.TaskRefs(ctx, task.ID)
	if err != nil {
		slog.Error("dispatch: task refs", "task", task.ID, "err", err)
		return
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "task.wait",
		ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Text: "ожидание: " + reason + " (шаг " + run.StepID + ")",
		Payload: map[string]any{"step": run.StepID, "participant": run.Participant, "role": run.Role,
			"reason": reason, "deferred": true, "retry_in_s": int(pause.Seconds())},
	}); err != nil {
		slog.Error("dispatch: wait event", "task", task.ID, "err", err)
	}
}

// dispatchAssigned — запуск, назначенный планировщиком: стадия по шагу
// снимка. false — стадия не стартовала (запуск отвязан, runner свободен).
func (e *Engine) dispatchAssigned(ctx context.Context, a store.RunAssignment) bool {
	deferAssigned := func(reason string) bool {
		if err := e.St.UndoAssign(ctx, a.Task.ID, a.Pending); err != nil {
			slog.Error("dispatch: undo assign", "task", a.Task.ID, "err", err)
		}
		e.deferRun(ctx, a.Task, a.Run, a.Runner.ID, reason)
		return false
	}
	proc, _, err := e.processFor(ctx, a.Task)
	if err != nil {
		slog.Error("dispatch: process", "task", a.Task.ID, "err", err)
		return deferAssigned("стадия не запустилась: процесс задачи: " + err.Error())
	}
	step, ok := proc.Step(a.Run.StepID)
	if !ok {
		slog.Error("dispatch: шаг запуска не найден в снимке", "task", a.Task.ID, "step", a.Run.StepID)
		return deferAssigned("стадия не запустилась: шага " + a.Run.StepID + " нет в процессе задачи")
	}
	return e.dispatchOrRelease(ctx, a.Task, a.Run, a.Runner, step, a.Task.StepEntry, a.Pending)
}

// reviewContext — контекст ревьюера: diff PR и отчёт автопроверок.
func (e *Engine) reviewContext(ctx context.Context, task domain.Task) string {
	extra := e.peekStageContext(task.ID)
	if task.PRURL == "" {
		return extra
	}
	d, err := e.diffForTask(ctx, task)
	if errors.Is(err, scm.ErrDiffTruncated) {
		// Ревьюеру начало diff'а полезнее, чем ничего.
		d, err = d+"\n…[diff обрезан: превышен лимит чтения]\n", nil
	}
	if err != nil {
		slog.Error("diff for review", "task", task.ID, "err", err)
		return extra
	}
	if extra != "" {
		return d + "\n\n" + extra
	}
	return d
}

func (e *Engine) peekStageContext(taskID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stageContext[taskID]
}

func (e *Engine) setStageContext(taskID, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if text == "" {
		delete(e.stageContext, taskID)
		return
	}
	e.stageContext[taskID] = text
}

// verdictOf — вердикт участника по результату стадии: неуспех code —
// невосстановимая ошибка (fail), неуспех test и review — замечания
// (changes); у шага prompt вердикт называет runner (маркер агента), без
// него — по признаку успеха.
func verdictOf(kind string, ok bool, explicit string) string {
	if kind == policy.StepPrompt {
		switch explicit {
		case policy.OutcomeOk, policy.OutcomeChanges, policy.OutcomeFail:
			return explicit
		}
	}
	// Тестировщик (add-agent-roles) не может одобрить красные проверки и не
	// валит задачу: его «нет» — замечания разработчику.
	if kind == policy.StepTest && (explicit == policy.OutcomeChanges || explicit == policy.OutcomeFail) {
		return policy.OutcomeChanges
	}
	if ok {
		return policy.OutcomeOk
	}
	if kind == policy.StepCode {
		return policy.OutcomeFail
	}
	return policy.OutcomeChanges
}

// runnerFits — подходит ли runner участнику шага (тип агента, модель,
// capabilities шага и задачи).
func runnerFits(r domain.Runner, task domain.Task, step policy.ResolvedStep, p policy.ResolvedParticipant) bool {
	if p.IsUser() {
		return false
	}
	// Runner выкатки и снятый с работы агентских стадий не ведут: то же
	// правило, что у подбора в store (agentRunner).
	if r.Agent == "" || r.Retired {
		return false
	}
	if p.Agent.Kind != "" && r.Agent != p.Agent.Kind {
		return false
	}
	if p.Agent.Model != "" && !contains(r.Models, p.Agent.Model) && r.Model != p.Agent.Model {
		return false
	}
	if step.Kind == policy.StepPrompt && !contains(r.Stages, "PROMPT") {
		return false
	}
	need := append([]string{}, step.Capabilities...)
	if step.Kind == policy.StepCode || step.Kind == policy.StepTest || step.Kind == policy.StepPrompt {
		need = append(need, task.Capabilities...)
	}
	for _, c := range need {
		if !contains(r.Capabilities, c) {
			return false
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// reuseTarget — можно ли отдать следующий шаг тому же runner'у без
// переназначения (code → test, провал проверок → исправление): шаг типа
// code или test с единственным участником, которому runner подходит.
func (e *Engine) reuseTarget(ctx context.Context, task domain.Task, next policy.ResolvedStep, runnerID string) bool {
	if runnerID == "" || (next.Kind != policy.StepCode && next.Kind != policy.StepTest && next.Kind != policy.StepPrompt) || len(next.Participants) != 1 {
		return false
	}
	p := next.Participants[0]
	rb := e.roleBindings(ctx, task, next)[p.ID]
	r, err := e.St.GetRunner(ctx, runnerID)
	// Runner, который уже освободили (решение принималось раньше, а между
	// вызовами его могли отдать другой задаче), не переиспользуется.
	if err != nil || r.Draining || r.TaskID != task.ID {
		return false
	}
	if !runnerFits(r, task, next, p) || !roleFits(r, p, rb) {
		return false
	}
	// Роль с командой (add-agent-roles): стадию ведёт свободный агент роли,
	// его вместе с runner'ом подбирает планировщик.
	if rb.Team {
		return false
	}
	// Тестировщик (add-agent-roles) по возможности работает на другом
	// runner'е, чем разработчик: если сейчас свободен другой подходящий,
	// шаг уходит планировщику, он предпочтёт не писавший код runner. Иначе
	// тот же runner продолжает без ожидания.
	if next.Kind == policy.StepTest && rb.Role != "" && e.otherIdleRunner(ctx, task, next, p, rb, runnerID) {
		return false
	}
	return true
}

// otherIdleRunner — есть ли свободный подходящий участнику runner, кроме
// исключённого.
func (e *Engine) otherIdleRunner(ctx context.Context, task domain.Task, step policy.ResolvedStep,
	p policy.ResolvedParticipant, rb store.RoleBinding, exclude string) bool {
	list, err := e.St.ListRunners(ctx)
	if err != nil {
		slog.Error("свободные runner'ы", "task", task.ID, "err", err)
		return false
	}
	for _, r := range list {
		if r.ID == exclude || r.Status != domain.RunnerIdle || r.Draining {
			continue
		}
		if runnerFits(r, task, step, p) && roleFits(r, p, rb) {
			return true
		}
	}
	return false
}

// evaluateStep решает исход шага по вердиктам запусков прохода: sequential
// запускает следующего участника после ok, parallel ждёт всех при all и
// первого ok при any; замечания всех участников склеиваются с указанием
// автора (спека process «Агрегация вердиктов и переходы»).
func (e *Engine) evaluateStep(ctx context.Context, task domain.Task, proc *policy.Resolved, hash string,
	step policy.ResolvedStep, pass int, lastRunner, policyHash, prURL string) error {
	runs, err := e.St.StepRuns(ctx, task.ID, step.ID, pass)
	if err != nil {
		return err
	}
	var open, changes, fails int
	var remarks []string
	for _, r := range runs {
		switch r.Verdict {
		case "":
			open++
		case policy.OutcomeChanges:
			changes++
			remarks = append(remarks, remark(step, r))
		case policy.OutcomeFail:
			fails++
			remarks = append(remarks, remark(step, r))
		case policy.OutcomeBlocked, "cancelled":
			// Заблокированный или прерванный шаг решён другим путём.
			return nil
		}
	}
	outcome := ""
	switch {
	case step.Mode == policy.ModeSequential:
		last := runs[len(runs)-1]
		if last.Verdict == policy.OutcomeOk {
			if len(runs) < len(step.Participants) {
				next := step.Participants[len(runs)]
				_, err := e.St.AddSequentialRun(ctx, task.ID, step, pass, next, e.roleBindings(ctx, task, step)[next.ID])
				return err
			}
			outcome = policy.OutcomeOk
		} else if fails > 0 {
			outcome = policy.OutcomeFail
		} else {
			outcome = policy.OutcomeChanges
		}
	case step.Require == policy.RequireAny:
		if len(runs)-open-changes-fails > 0 {
			outcome = policy.OutcomeOk
		} else if open > 0 {
			return nil
		} else if fails > 0 {
			outcome = policy.OutcomeFail
		} else {
			outcome = policy.OutcomeChanges
		}
	default:
		if open > 0 {
			return nil
		}
		switch {
		case fails > 0:
			outcome = policy.OutcomeFail
		case changes > 0:
			outcome = policy.OutcomeChanges
		default:
			outcome = policy.OutcomeOk
		}
	}
	// Исход применяется один раз на вход: одновременные вердикты двух
	// участников не должны дважды расходовать попытку и входить на шаг.
	claimed, err := e.St.ClaimStepOutcome(ctx, task.ID, pass)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	// any: остальные участники прерываются только владельцем исхода, иначе
	// опоздавший обработчик отменил бы запуски уже следующего шага.
	if step.Require == policy.RequireAny && outcome == policy.OutcomeOk && open > 0 {
		e.cancelOpenRuns(ctx, task, 0)
	}
	verdicts := make([]map[string]any, 0, len(runs))
	titles, labels := e.roleTitles(ctx), e.runnerLabels(ctx)
	for _, r := range runs {
		v := map[string]any{
			"participant": r.Participant, "runner": r.RunnerID, "model": r.DisplayModel(), "verdict": r.Verdict,
		}
		if r.IsUser() {
			v["user"] = r.VerdictBy
		}
		// Агент, роль и имя runner'а исполнителя (спека agent-roles
		// «Агент-исполнитель в событиях и API»).
		if r.RunnerID != "" {
			v["runner_name"] = labels[r.RunnerID]
		}
		if r.Role != "" && titles[r.Role] != "" {
			v["role"] = map[string]any{"id": r.Role, "title": titles[r.Role]}
		}
		if r.AgentID != "" {
			v["agent"] = map[string]any{"id": r.AgentID, "name": r.AgentName}
		}
		if r.AgentVerdict != "" {
			v["agent_verdict"] = r.AgentVerdict
		}
		verdicts = append(verdicts, v)
	}
	// Автор блока замечаний — роль и имя исполнителя, если они есть.
	remarks = remarks[:0]
	for _, r := range runs {
		if r.Verdict == policy.OutcomeChanges || r.Verdict == policy.OutcomeFail {
			remarks = append(remarks, remarkBy(step, r, performerLabel(r, titles, labels)))
		}
	}
	detail := strings.Join(remarks, "\n\n")
	if outcome == policy.OutcomeOk && len(runs) == 1 {
		detail = runs[0].Detail
	}
	return e.applyOutcome(ctx, task, proc, hash, step, outcome, detail, verdicts, lastRunner, policyHash, prURL)
}

// remark — блок замечаний участника с указанием автора.
func remark(step policy.ResolvedStep, r store.StepRun) string {
	return remarkBy(step, r, "")
}

// remarkBy — блок замечаний участника; performer — роль и имя исполнителя
// (add-agent-roles), пусто у участника без роли.
func remarkBy(step policy.ResolvedStep, r store.StepRun, performer string) string {
	who := r.Participant
	switch {
	case r.VerdictBy != "":
		who += " (" + r.VerdictBy + ")"
	case performer != "":
		who += " (" + performer + ")"
	case r.AgentKind != "" || r.DisplayModel() != "":
		who += " (" + strings.TrimSuffix(r.AgentKind+"/"+r.DisplayModel(), "/") + ")"
	}
	if len(step.Participants) == 1 {
		return r.Detail
	}
	return "Замечания участника " + who + ":\n" + r.Detail
}

// performerLabel — «Роль Имя» исполнителя запуска (агент команды, иначе
// runner) или пусто без роли.
func performerLabel(r store.StepRun, titles, labels map[string]string) string {
	t := titles[r.Role]
	if r.Role == "" || t == "" {
		return ""
	}
	if r.AgentName != "" {
		return t + " " + r.AgentName
	}
	if name := labels[r.RunnerID]; name != "" {
		return t + " " + name
	}
	return t
}

// cancelOpenRuns прерывает остальные запуски шага (any удовлетворён,
// участник заблокирован): вердикт cancelled, runner'ам — CancelTask.
func (e *Engine) cancelOpenRuns(ctx context.Context, task domain.Task, except int64) {
	e.cancelRuns(ctx, task, except, store.RunsCancelledByStep)
}

// CancelRunsOfMerged прерывает стадии задачи, завершённой merge (кнопкой
// или на хостинге), пока они шли: иначе ревьюер продолжал бы работу на
// runner'е, уже свободном в базе, а его поздний результат записал бы шаг
// review у задачи в done и упал на переходе done → review.
func (e *Engine) CancelRunsOfMerged(ctx context.Context, task domain.Task) {
	e.cancelRuns(ctx, task, 0, store.RunsCancelledByMerge)
}

func (e *Engine) cancelRuns(ctx context.Context, task domain.Task, except int64, outcome string) {
	open, err := e.St.CancelOpenRunsOutcome(ctx, task.ID, except, outcome)
	if err != nil {
		slog.Error("cancel open runs", "task", task.ID, "err", err)
		return
	}
	for _, r := range open {
		if r.SessionID != "" {
			e.dropSessionID(r.SessionID)
		}
		if r.RunnerID != "" {
			e.Out.Send(r.RunnerID, &pb.PlaneMsg{
				MsgId: fmt.Sprintf("cancel-%s-%d", task.ID, r.ID),
				Kind:  &pb.PlaneMsg_Cancel{Cancel: &pb.CancelTask{TaskId: task.ID}},
			})
		}
	}
}

// applyOutcome — переход по исходу шага: событие task.step, затем ok →
// следующий шаг, changes → отказ шага с лимитами и исправление, fail →
// переход fail (по умолчанию эскалация).
func (e *Engine) applyOutcome(ctx context.Context, task domain.Task, proc *policy.Resolved, hash string,
	step policy.ResolvedStep, outcome, detail string, verdicts []map[string]any, lastRunner, policyHash, prURL string) error {
	p, epic, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	target := step.Target(outcome)
	payload := map[string]any{
		"step": step.ID, "kind": step.Kind, "outcome": outcome, "next": target,
		"verdicts": verdicts, "process_hash": hash,
	}
	if policyHash != "" {
		payload["policy_hash"] = policyHash
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "task.step",
		ProjectID: p.ID, EpicID: epic.ID, TaskID: task.ID,
		Text:    fmt.Sprintf("шаг %s: %s → %s", step.Title, outcome, target),
		Payload: payload,
	}); err != nil {
		return err
	}
	paused := epic.Status != domain.EpicRunning
	evPayload := map[string]any{}
	if policyHash != "" {
		evPayload["policy_hash"] = policyHash
	}
	switch outcome {
	case policy.OutcomeOk:
		if step.Kind == policy.StepTest {
			// Отчёт автопроверок — ревьюеру; PR создаётся после проверок.
			e.setStageContext(task.ID, "")
			if detail != "" {
				label := "Результаты автопроверок:\n"
				if testerRan(verdicts) {
					label = "Отчёт тестировщика и автопроверок:\n"
				}
				e.setStageContext(task.ID, label+detail)
			}
			if task.PRURL == "" {
				if err := e.createPR(ctx, task, p); err != nil {
					return err
				}
				if task, err = e.St.GetTask(ctx, task.ID); err != nil {
					return err
				}
			}
			if prURL == "" {
				prURL = task.PRURL
			}
		}
		if step.Kind == policy.StepReview {
			e.setStageContext(task.ID, "")
		}
		return e.gotoTarget(ctx, task, proc, hash, target, policy.OutcomeOk, lastRunner, paused, evPayload, prURL)

	case policy.OutcomeChanges:
		reason := domain.AttTestFailed
		prefix := "Вывод проверок:\n"
		tester := step.Kind == policy.StepTest && testerRan(verdicts)
		if tester {
			prefix = "Замечания тестировщика и вывод проверок:\n"
		}
		if step.Kind == policy.StepReview {
			reason, prefix = domain.AttReviewLimit, "Замечания review:\n"
		}
		// Контекст исправления кладётся до отказа: сразу после него resume
		// может назначить исправление, контекст уже должен лежать.
		e.setStageContext(task.ID, prefix+detail)
		failed, rej, err := e.St.RejectStep(ctx, task.ID, step, reason, detail, policyHash)
		if err != nil || failed {
			e.setStageContext(task.ID, "")
			e.cancelOpenRuns(ctx, task, 0)
			return err
		}
		if target == policy.TargetEscalate || target == "" {
			e.setStageContext(task.ID, "")
			return e.failTask(ctx, task, "Замечания без шага исправления: "+detail, lastRunner, evPayload)
		}
		var text string
		switch step.Kind {
		case policy.StepReview:
			text = fmt.Sprintf("review вернул замечания — исправление (попытка %d/%d)", rej, step.Attempts)
		case policy.StepTest:
			text = fmt.Sprintf("проверки упали — исправление (попытка %d/%d)", rej, step.Attempts)
			if tester && testerObjected(verdicts) {
				text = fmt.Sprintf("тестировщик вернул замечания — исправление (попытка %d/%d)", rej, step.Attempts)
			}
		default:
			text = fmt.Sprintf("%s — исправление (проход %d/%d)", strings.TrimSuffix(prefix, ":\n"), rej, step.Attempts)
		}
		evPayload["detail"] = detail
		ev := &store.EventInput{Text: text, Payload: evPayload}
		return e.gotoTarget(ctx, task, proc, hash, target, policy.OutcomeChanges, lastRunner, paused, evPayload, "", ev)

	case policy.OutcomeFail:
		e.setStageContext(task.ID, "")
		if target == policy.TargetEscalate || target == "" {
			detailText := detail
			// Текст ошибки приватной сессии не раскрывается в публичных
			// событии и эскалации; полный текст — в итоге сессии (автору).
			if private := e.lastRunPrivate(ctx, task, step); private {
				detailText = "ошибка приватной сессии — подробности доступны её автору в итоге сессии"
			}
			return e.failTask(ctx, task, "Невосстановимая ошибка этапа: "+detailText, lastRunner, evPayload)
		}
		return e.gotoTarget(ctx, task, proc, hash, target, policy.OutcomeOk, lastRunner, paused, evPayload, "")
	}
	return nil
}

// lastRunPrivate — была ли приватной сессия последнего запуска шага.
func (e *Engine) lastRunPrivate(ctx context.Context, task domain.Task, step policy.ResolvedStep) bool {
	runs, err := e.St.StepRuns(ctx, task.ID, step.ID, task.StepGen)
	if err != nil || len(runs) == 0 {
		return false
	}
	sid := runs[len(runs)-1].SessionID
	if sid == "" {
		return false
	}
	private, _, err := e.St.SessionPrivacy(ctx, sid)
	return err == nil && private
}

// gotoTarget исполняет переход: done → завершение, escalate → эскалация,
// шаг → вход с переиспользованием runner'а, если можно и Epic не на паузе.
func (e *Engine) gotoTarget(ctx context.Context, task domain.Task, proc *policy.Resolved, hash, target, entry, lastRunner string,
	paused bool, evPayload map[string]any, prURL string, ev ...*store.EventInput) error {
	switch target {
	case policy.TargetDone:
		return e.St.CompleteTask(ctx, task.ID, store.EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.status",
			Text: "процесс завершён — задача выполнена", Payload: map[string]any{"status": "done"},
		})
	case policy.TargetEscalate, "":
		return e.failTask(ctx, task, "процесс требует человека", lastRunner, evPayload)
	}
	next, ok := proc.Step(target)
	if !ok {
		return fmt.Errorf("шаг %q не найден в снимке процесса задачи %s", target, task.ID)
	}
	var event *store.EventInput
	if len(ev) > 0 && ev[0] != nil {
		event = ev[0]
	} else if next.Kind == policy.StepReview {
		pl := map[string]any{"pr": prURL}
		for k, v := range evPayload {
			pl[k] = v
		}
		event = &store.EventInput{Text: "проверки прошли, PR создан — ожидание review", Payload: pl}
	} else if next.Kind == policy.StepTest {
		event = &store.EventInput{Text: "реализация готова — запуск проверок", Payload: evPayload}
	}
	if event != nil && event.ActorKind == "" {
		// Переход по результату runner'а — от его имени; внешний (review с
		// хостинга, внешние проверки) — системный.
		event.ActorKind = domain.ActorSystem
		if lastRunner != "" {
			event.ActorKind, event.ActorID = domain.ActorRunner, lastRunner
		}
	}
	reuse := ""
	if !paused && e.reusableFrom(task) && e.reuseTarget(ctx, task, next, lastRunner) {
		reuse = lastRunner
	} else if lastRunner != "" && next.Kind != policy.StepReview {
		// Runner не подходит следующему шагу или Epic на паузе: он
		// освобождается, запуск подхватит планировщик (на паузе — после resume).
		// Тестировщику (add-agent-roles) исполнитель кода нужен и дальше:
		// планировщик предпочтёт другой runner, а ревьюер не совпадёт с ним.
		release := e.St.ReleaseTaskRunner
		if next.Kind == policy.StepTest && hasRoleParticipant(e.roleBindings(ctx, task, next)) {
			release = e.St.FreeTaskRunner
		}
		if err := release(ctx, task.ID); err != nil {
			return err
		}
	}
	return e.enterStep(ctx, task, proc, hash, next, entry, reuse, event)
}

// reusableFrom — с текущего шага задачи runner можно переиспользовать
// (code и test делят worktree; ревьюер исправлять не должен).
func (e *Engine) reusableFrom(task domain.Task) bool {
	proc := store.TaskProcess(task)
	if proc == nil {
		return true
	}
	s, ok := proc.Step(task.StepID)
	return !ok || s.Kind != policy.StepReview
}

// createPR открывает PR ветки задачи после успешных проверок.
func (e *Engine) createPR(ctx context.Context, task domain.Task, p domain.Project) error {
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return err
	}
	pr, err := adapter.CreatePR(ctx, p.RepoPath, task.Branch, p.DefaultBranch,
		fmt.Sprintf("task-%d: %s", task.Num, task.Title), task.Description)
	if err != nil {
		return fmt.Errorf("create PR: %w", err)
	}
	return e.St.SetTaskPR(ctx, task.ID, pr.URL)
}

// enterMerge — шаг merge: задача ждёт подтверждения человеком, авто-merge
// решает гейт политики (спека task-pipeline «Merge после успешной проверки»).
func (e *Engine) enterMerge(ctx context.Context, task domain.Task, proc *policy.Resolved, hash string, ev *store.EventInput) error {
	p, _, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	in := store.EnterStep{TaskID: task.ID, Entry: policy.OutcomeOk, ReleaseRunners: true,
		Payload: map[string]any{"process_hash": hash}, Silent: true}
	mergeStep, ok := proc.Step(mergeStepID(proc))
	if !ok {
		return fmt.Errorf("в снимке процесса задачи %s нет шага merge", task.ID)
	}
	in.Step = mergeStep
	if len(task.Process) == 0 {
		in.Process, in.ProcessHash = proc, hash
	}
	if _, err := e.St.EnterStep(ctx, in); err != nil {
		return err
	}
	actor := domain.ActorScheduler
	actorID := ""
	payload := map[string]any{"process_hash": hash}
	if ev != nil {
		if ev.ActorKind != "" {
			actor, actorID = ev.ActorKind, ev.ActorID
		}
		for k, v := range payloadMap(ev.Payload) {
			payload[k] = v
		}
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: actor, ActorID: actorID, Type: "task.review_passed",
		ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
		Text:    "review пройден — ожидание подтверждения merge",
		Payload: payload,
	}); err != nil {
		return err
	}
	eff, err := e.St.EffectivePolicy(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if task, err = e.St.GetTask(ctx, task.ID); err != nil {
		return err
	}
	return e.autoMerge(ctx, task, p, eff)
}

func mergeStepID(proc *policy.Resolved) string {
	for _, s := range proc.Steps {
		if s.Kind == policy.StepMerge {
			return s.ID
		}
	}
	return policy.StepMerge
}

// externalChanges — замечания снаружи конвейера (review человека на
// хостинге, провал внешних проверок) для задачи на шаге review или merge:
// идущие review-сессии прерываются, отказ считается по текущему шагу,
// исправление идёт ближайшим шагом code.
func (e *Engine) externalChanges(ctx context.Context, task domain.Task, reason domain.AttentionReason, detail string) error {
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	step, ok := proc.Step(task.StepID)
	if !ok {
		// Задача из-до процесса: текущий шаг по статусу.
		step, ok = proc.Step(policy.StepReview)
		if !ok {
			return nil
		}
	}
	target := step.On.Changes
	if step.Kind == policy.StepMerge || target == "" {
		target = nearestCodeBefore(proc, step.ID)
	}
	e.cancelOpenRuns(ctx, task, 0)
	e.setStageContext(task.ID, detail)
	failed, rej, err := e.St.RejectStep(ctx, task.ID, step, reason, detail, "")
	if err != nil || failed {
		e.setStageContext(task.ID, "")
		return err
	}
	if target == policy.TargetEscalate || target == "" {
		e.setStageContext(task.ID, "")
		return e.failTask(ctx, task, "Замечания без шага исправления: "+detail, "")
	}
	text := fmt.Sprintf("review вернул замечания — исправление (попытка %d/%d)", rej, step.Attempts)
	if reason == domain.AttTestFailed {
		text = fmt.Sprintf("проверки упали — исправление (попытка %d/%d)", rej, step.Attempts)
	}
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return err
	}
	return e.gotoTarget(ctx, task, proc, hash, target, policy.OutcomeChanges, "", epic.Status != domain.EpicRunning,
		map[string]any{"detail": detail}, "", &store.EventInput{Text: text, Payload: map[string]any{"detail": detail}})
}

// nearestCodeBefore — ближайший шаг code перед указанным (или сам он).
func nearestCodeBefore(proc *policy.Resolved, stepID string) string {
	last := ""
	for _, s := range proc.Steps {
		if s.Kind == policy.StepCode {
			last = s.ID
		}
		if s.ID == stepID {
			break
		}
	}
	if last == "" {
		return policy.TargetEscalate
	}
	return last
}

// markWaiting пишет причину ожидания задачам, чьим запускам не подходит ни
// один зарегистрированный runner (спека orchestration «Нет runner'а с
// нужным агентом и моделью»). excluded и excludedEpics — исключённые из
// назначений этого прохода (бюджет, зависимости Epic'а): причину про
// агентов им не пишем, а записанную раньше снимаем.
func (e *Engine) markWaiting(ctx context.Context, excluded, excludedEpics []string) error {
	runs, err := e.St.WaitingRuns(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range runs {
		if seen[r.TaskID] {
			continue
		}
		seen[r.TaskID] = true
		want := strings.TrimSuffix(r.AgentKind+"/"+r.Model, "/")
		reason := "нет runner'а с capabilities " + strings.Join(r.Capabilities, ", ")
		if want != "" {
			reason = "нет runner'а " + want
		}
		if r.StepKind == policy.StepPrompt {
			reason += " со стадией PROMPT"
		}
		reason = e.architectureWaitReason(ctx, r.Capabilities, reason)
		if err := e.St.SetTaskWaitReason(ctx, r.TaskID, reason); err != nil {
			return err
		}
		projectID, epicID, err := e.St.TaskRefs(ctx, r.TaskID)
		if err != nil {
			return err
		}
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.wait",
			ProjectID: projectID, EpicID: epicID, TaskID: r.TaskID,
			Text:    "ожидание: " + reason + " (шаг " + r.StepID + ")",
			Payload: map[string]any{"step": r.StepID, "participant": r.Participant, "agent": r.AgentKind, "model": r.Model, "reason": reason},
		}); err != nil {
			return err
		}
	}
	return e.markTeamWaiting(ctx, seen, excluded, excludedEpics)
}

// markTeamWaiting — причина ожидания запусков ролей с командой (add-agent-roles):
// все агенты роли заняты или ни одному агенту роли не подходит ни один
// подключённый runner. Задачи проектов и Epic'ов, исключённых из
// назначений (бюджет, ожидание зависимостей Epic'а), пропускаются: они
// стоят не из-за агентов, и «нет свободного агента» ввело бы в
// заблуждение. Свою причину показывают Epic и проект. Командную
// причину, записанную до исключения, планировщик снимает: иначе она
// держалась бы и после того, как агенты освободились. После снятия
// исключения причина пишется заново, если задача всё ещё ждёт агента.
func (e *Engine) markTeamWaiting(ctx context.Context, seen map[string]bool, excluded, excludedEpics []string) error {
	if _, err := e.St.ClearTeamWaits(ctx, excluded, excludedEpics); err != nil {
		return err
	}
	waits, err := e.St.WaitingTeamRuns(ctx, excluded, excludedEpics)
	if err != nil {
		return err
	}
	for _, w := range waits {
		if seen[w.Run.TaskID] {
			continue
		}
		seen[w.Run.TaskID] = true
		reason := e.architectureWaitReason(ctx, w.Run.Capabilities, "нет runner'а для свободных агентов роли «"+w.RoleTitle+"»")
		if w.Busy {
			reason = "нет свободного агента роли «" + w.RoleTitle + "»: все заняты"
		}
		if err := e.St.SetTaskTeamWait(ctx, w.Run.TaskID, reason); err != nil {
			return err
		}
		projectID, epicID, err := e.St.TaskRefs(ctx, w.Run.TaskID)
		if err != nil {
			return err
		}
		if _, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: domain.ActorScheduler, Type: "task.wait",
			ProjectID: projectID, EpicID: epicID, TaskID: w.Run.TaskID,
			Text:    "ожидание: " + reason + " (шаг " + w.Run.StepID + ")",
			Payload: map[string]any{"step": w.Run.StepID, "participant": w.Run.Participant, "role": w.Run.Role, "reason": reason, "busy": w.Busy},
		}); err != nil {
			return err
		}
	}
	return nil
}

// payloadMap — payload события как карта (EventInput.Payload — any).
func payloadMap(p any) map[string]any {
	m, _ := p.(map[string]any)
	return m
}

// ApplyVerdict — вердикт человека по запуску (очередь «мои шаги», review с
// хостинга): запуск закрывается от имени пользователя, событие task.verdict,
// исход шага теми же правилами, что у агентов. ErrRunClosed — запуск уже
// закрыт (второй владелец при участнике по роли).
func (e *Engine) ApplyVerdict(ctx context.Context, task domain.Task, run store.StepRun, verdict, detail, login string) error {
	claimed, err := e.St.RecordUserVerdict(ctx, run.ID, login, verdict, detail)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrRunClosed
	}
	p, epic, err := e.projectOf(ctx, task)
	if err != nil {
		return err
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorUser, ActorID: login, Type: "task.verdict",
		ProjectID: p.ID, EpicID: epic.ID, TaskID: task.ID,
		Text: fmt.Sprintf("вердикт участника %s на шаге %s: %s", login, run.StepID, verdict),
		Payload: map[string]any{"run": run.ID, "step": run.StepID, "participant": run.Participant,
			"verdict": verdict, "detail": detail},
	}); err != nil {
		return err
	}
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return err
	}
	step, ok := proc.Step(run.StepID)
	if !ok {
		return fmt.Errorf("шаг %q запуска не найден в снимке процесса задачи %s", run.StepID, task.ID)
	}
	return e.evaluateStep(ctx, task, proc, hash, step, run.Pass, "", "", "")
}

// ErrRunClosed — запуск уже закрыт другим вердиктом.
var ErrRunClosed = errors.New("запуск уже закрыт")

// reconcileSteps дожимает шаги, у которых все запуски входа закрыты, а
// исход не применён: вердикт записался, а продвижение упало (БД, хостинг).
// Идемпотентно благодаря ClaimStepOutcome.
func (e *Engine) reconcileSteps(ctx context.Context) error {
	tasks, err := e.St.StepsToReconcile(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		proc, hash, err := e.processFor(ctx, task)
		if err != nil {
			slog.Error("reconcile: process", "task", task.ID, "err", err)
			continue
		}
		step, ok := proc.Step(task.StepID)
		if !ok {
			continue
		}
		// Заблокированные и прерванные входы решаются другим путём.
		runs, err := e.St.StepRuns(ctx, task.ID, step.ID, task.StepGen)
		if err != nil {
			return err
		}
		skip := false
		for _, r := range runs {
			if r.Verdict == policy.OutcomeBlocked || r.Verdict == "cancelled" {
				skip = true
			}
		}
		if skip {
			continue
		}
		slog.Warn("reconcile: дожимаем исход шага", "task", task.ID, "step", step.ID)
		if err := e.evaluateStep(ctx, task, proc, hash, step, task.StepGen, "", "", ""); err != nil {
			slog.Error("reconcile: evaluate", "task", task.ID, "step", step.ID, "err", err)
		}
	}
	return nil
}

// testerRan — на шаге работал тестировщик: агент вернул свой вердикт
// (runner без поддержки ролей гонит только проверки и вердикта не даёт).
func testerRan(verdicts []map[string]any) bool {
	for _, v := range verdicts {
		if av, _ := v["agent_verdict"].(string); av != "" {
			return true
		}
	}
	return false
}

// testerObjected — тестировщик сам вернул замечания (а не только упали
// проверки при его одобрении).
func testerObjected(verdicts []map[string]any) bool {
	for _, v := range verdicts {
		if av, _ := v["agent_verdict"].(string); av == policy.OutcomeChanges || av == policy.OutcomeFail {
			return true
		}
	}
	return false
}

// verdictInfraError — вердикт runner'а о сбое инфраструктуры шага test
// (git, push, падение тестировщика, проверки без песочницы или упавшие из-за
// её окружения): не замечания разработчику.
const verdictInfraError = "error"

// infraRetryLimit — сколько раз шаг test повторяется после сбоя
// инфраструктуры без расхода попытки; следующий сбой эскалирует.
const infraRetryLimit = 1

// retryInfra — повтор шага после сбоя инфраструктуры: запуск отвязывается
// от runner'а и агента и ждёт нового назначения в том же проходе. false —
// повторы исчерпаны, вызывающий эскалирует.
func (e *Engine) retryInfra(ctx context.Context, task domain.Task, step policy.ResolvedStep, run store.StepRun, runnerID, detail string) (bool, error) {
	retry, n, err := e.St.RecordInfraRetry(ctx, task.ID, step.ID, infraRetryLimit)
	if err != nil || !retry {
		return false, err
	}
	if err := e.St.UnbindRun(ctx, run.ID); err != nil {
		return false, err
	}
	if err := e.St.FreeRunnerFor(ctx, runnerID, task.ID); err != nil {
		return false, err
	}
	p, epic, err := e.projectOf(ctx, task)
	if err != nil {
		return false, err
	}
	_, err = e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "task.assign",
		ProjectID: p.ID, EpicID: epic.ID, TaskID: task.ID,
		Text:    fmt.Sprintf("сбой инфраструктуры шага проверки — повтор шага без расхода попытки (%d/%d)", n, infraRetryLimit),
		Payload: map[string]any{"step": step.ID, "infra_error": detail, "retry": n},
	})
	return true, err
}

// hasRoleParticipant — у шага есть участник с ролью.
func hasRoleParticipant(rb map[string]store.RoleBinding) bool {
	for _, b := range rb {
		if b.Role != "" {
			return true
		}
	}
	return false
}
