package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	if reuseRunner != "" && len(runs) > 0 {
		task.StepID, task.StepEntry = step.ID, entry
		e.dispatchRun(ctx, task, runs[0], domain.Runner{ID: reuseRunner}, step, entry, nil)
	}
	return nil
}

// dispatchAssigned — запуск, назначенный планировщиком: стадия по шагу снимка.
func (e *Engine) dispatchAssigned(ctx context.Context, a store.RunAssignment) {
	proc, _, err := e.processFor(ctx, a.Task)
	if err != nil {
		slog.Error("dispatch: process", "task", a.Task.ID, "err", err)
		return
	}
	step, ok := proc.Step(a.Run.StepID)
	if !ok {
		slog.Error("dispatch: шаг запуска не найден в снимке", "task", a.Task.ID, "step", a.Run.StepID)
		return
	}
	e.dispatchRun(ctx, a.Task, a.Run, a.Runner, step, a.Task.StepEntry, nil)
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
// невосстановимая ошибка (fail), неуспех test и review — замечания (changes).
func verdictOf(kind string, ok bool) string {
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
	if p.Agent.Kind != "" && r.Agent != p.Agent.Kind {
		return false
	}
	if p.Agent.Model != "" && !contains(r.Models, p.Agent.Model) && r.Model != p.Agent.Model {
		return false
	}
	need := append([]string{}, step.Capabilities...)
	if step.Kind == policy.StepCode || step.Kind == policy.StepTest {
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
	if runnerID == "" || (next.Kind != policy.StepCode && next.Kind != policy.StepTest) || len(next.Participants) != 1 {
		return false
	}
	r, err := e.St.GetRunner(ctx, runnerID)
	if err != nil || r.Draining {
		return false
	}
	return runnerFits(r, task, next, next.Participants[0])
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
				_, err := e.St.AddSequentialRun(ctx, task.ID, step, pass, step.Participants[len(runs)])
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
	for _, r := range runs {
		v := map[string]any{
			"participant": r.Participant, "runner": r.RunnerID, "model": r.Model, "verdict": r.Verdict,
		}
		if r.IsUser() {
			v["user"] = r.VerdictBy
		}
		verdicts = append(verdicts, v)
	}
	detail := strings.Join(remarks, "\n\n")
	if outcome == policy.OutcomeOk && len(runs) == 1 {
		detail = runs[0].Detail
	}
	return e.applyOutcome(ctx, task, proc, hash, step, outcome, detail, verdicts, lastRunner, policyHash, prURL)
}

// remark — блок замечаний участника с указанием автора.
func remark(step policy.ResolvedStep, r store.StepRun) string {
	who := r.Participant
	switch {
	case r.VerdictBy != "":
		who += " (" + r.VerdictBy + ")"
	case r.AgentKind != "" || r.Model != "":
		who += " (" + strings.TrimSuffix(r.AgentKind+"/"+r.Model, "/") + ")"
	}
	if len(step.Participants) == 1 {
		return r.Detail
	}
	return "Замечания участника " + who + ":\n" + r.Detail
}

// cancelOpenRuns прерывает остальные запуски шага (any удовлетворён,
// участник заблокирован): вердикт cancelled, runner'ам — CancelTask.
func (e *Engine) cancelOpenRuns(ctx context.Context, task domain.Task, except int64) {
	open, err := e.St.CancelOpenRuns(ctx, task.ID, except)
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
				e.setStageContext(task.ID, "Результаты автопроверок:\n"+detail)
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
		if err := e.St.ReleaseTaskRunner(ctx, task.ID); err != nil {
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
// нужным агентом и моделью»).
func (e *Engine) markWaiting(ctx context.Context) error {
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
