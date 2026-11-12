package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/ops"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Задания агентов control plane (add-sdlc-cycle, design решение 3):
// Релиз-инженер пишет release notes и разбирает провал публикации,
// Инженер сопровождения описывает инцидент. Модель агента команды, без
// неё модель шаблона, затем модель планировщика из каталога. Без модели
// или при ошибке — детерминированный шаблон. У агента одно задание
// одновременно, задания роли делят её включённые агенты.

// Виды заданий.
const (
	JobReleaseNotes        = "release_notes"
	JobDeployAnalysis      = "deploy_analysis"
	JobIncidentDescription = "incident_description"
	JobGitHubIssues        = "github_issues"
	JobEpicAssessment      = "epic_architecture_assessment"
)

const (
	// opsJobTimeout — потолок одного вызова модели.
	opsJobTimeout = 3 * time.Minute
	// opsResumeEvery — как часто поднимаются залежавшиеся задания.
	opsResumeEvery = time.Minute
	// opsStale — задание без результата дольше этого считается потерянным
	// (сбой, рестарт rivetd) и ставится заново.
	opsStale = store.NotesStale
)

// OpsJob — задание агенту control plane.
type OpsJob struct {
	Kind      string
	RefID     string // релиз или инцидент
	ProjectID string
}

func (j OpsJob) role() string {
	if j.Kind == JobEpicAssessment {
		return domain.RoleArchitect
	}
	if j.Kind == JobIncidentDescription || j.Kind == JobGitHubIssues {
		return domain.RoleSRE
	}
	return domain.RoleDevops
}

func (j OpsJob) key() string { return j.Kind + ":" + j.RefID }

// opsQueue — очереди заданий по роли. Задание берёт свободный агент роли
// (без агентов — шаблон роли). Внутри роли проекты обслуживаются по
// очереди: шквал одного проекта не задерживает остальные.
type opsQueue struct {
	mu sync.Mutex
	// queues — ожидающие задания роли.
	queues map[string][]OpsJob
	// queued и active — ключи заданий в очереди и в работе.
	queued, active map[string]bool
	// workers — исполнители с запущенным циклом (агент или role:<id>).
	workers map[string]bool
	// lastProject — проект последнего взятого задания роли.
	lastProject map[string]string
	// current — текущая работа агента для агрегата цикла.
	current map[string]*cycle.Work
	resumed time.Time
	pruned  time.Time
	// monitorsStarted — первый проход наблюдения после старта выполнен.
	monitorsStarted bool
	wg              sync.WaitGroup
}

func (e *Engine) opsQ() *opsQueue {
	e.opsOnce.Do(func() {
		e.ops = &opsQueue{queues: map[string][]OpsJob{}, queued: map[string]bool{}, active: map[string]bool{},
			workers: map[string]bool{}, lastProject: map[string]string{}, current: map[string]*cycle.Work{}}
	})
	return e.ops
}

// OpsBusy — текущие задания агентов control plane (для агрегата цикла).
func (e *Engine) OpsBusy() store.CycleBusy {
	q := e.opsQ()
	q.mu.Lock()
	defer q.mu.Unlock()
	out := store.CycleBusy{}
	for k, w := range q.current {
		cp := *w
		out[k] = &cp
	}
	return out
}

// WaitOps — дождаться опустошения очередей (тесты).
func (e *Engine) WaitOps() { e.opsQ().wg.Wait() }

// EnqueueOps ставит задание роли. Задание, которое уже ждёт в очереди, не
// дублируется. Идущее задание не дублируется подъёмом залежавшихся, а
// явный запрос (перегенерация) встаёт после него.
func (e *Engine) EnqueueOps(ctx context.Context, job OpsJob) { e.enqueueOps(ctx, job, true) }

func (e *Engine) enqueueOps(ctx context.Context, job OpsJob, explicit bool) {
	role := job.role()
	workers := []string{"role:" + role}
	if team, err := e.St.ListTeam(ctx); err == nil {
		var ids []string
		for _, a := range team {
			if a.RoleID == role && a.Enabled {
				ids = append(ids, a.ID)
			}
		}
		if len(ids) > 0 {
			workers = ids
		}
	}
	q := e.opsQ()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[job.key()] || (!explicit && q.active[job.key()]) {
		return
	}
	q.queued[job.key()] = true
	q.queues[role] = append(q.queues[role], job)
	// Свободные исполнители роли разбирают очередь; занятые возьмут
	// следующее задание сами.
	waiting := len(q.queues[role])
	for _, w := range workers {
		if waiting == 0 {
			break
		}
		if q.workers[w] {
			continue
		}
		q.workers[w] = true
		waiting--
		q.wg.Add(1)
		go e.opsWorker(role, w)
	}
}

// nextJob — следующее задание роли: сначала другого проекта, чем прошлое.
// Задание, которое уже идёт у другого агента (перегенерация во время
// работы), ждёт его окончания: одно задание не идёт у двух агентов сразу.
func (q *opsQueue) nextJob(role string) (OpsJob, bool) {
	jobs := q.queues[role]
	idx := -1
	for i, j := range jobs {
		if q.active[j.key()] {
			continue
		}
		if idx < 0 {
			idx = i
		}
		if j.ProjectID != q.lastProject[role] {
			idx = i
			break
		}
	}
	if idx < 0 {
		return OpsJob{}, false
	}
	job := jobs[idx]
	q.queues[role] = append(jobs[:idx:idx], jobs[idx+1:]...)
	q.lastProject[role] = job.ProjectID
	delete(q.queued, job.key())
	q.active[job.key()] = true
	return job, true
}

func (e *Engine) opsWorker(role, worker string) {
	q := e.opsQ()
	defer q.wg.Done()
	for {
		q.mu.Lock()
		job, ok := q.nextJob(role)
		if !ok {
			delete(q.workers, worker)
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), opsJobTimeout+30*time.Second)
		if err := e.runOpsJob(ctx, job, strings.TrimPrefix(worker, "role:"+role)); err != nil {
			slog.Error("задание агента control plane", "kind", job.Kind, "ref", job.RefID, "err", err)
		}
		cancel()

		q.mu.Lock()
		delete(q.active, job.key())
		q.mu.Unlock()
	}
}

// opsAgent — исполнитель задания: агент команды и шаблон роли.
type opsAgent struct {
	role    domain.AgentRole
	agent   *domain.TeamAgent
	persona ops.Persona
	connID  string
	model   string
}

func (a opsAgent) id() string {
	if a.agent != nil {
		return a.agent.ID
	}
	return ""
}

// resolveOpsAgent — шаблон роли и агент-исполнитель (пусто — по шаблону) с
// моделью: агента, шаблона, затем планировщика из каталога. Модель
// планировщика из переменных окружения rivetd сюда не входит (README).
func (e *Engine) resolveOpsAgent(ctx context.Context, roleID, agentID string) (opsAgent, error) {
	var a opsAgent
	role, err := e.St.GetRole(ctx, roleID)
	if err != nil {
		return a, err
	}
	a.role = role
	a.persona = ops.Persona{RoleTitle: role.Title, Instruction: role.Instruction}
	if !role.Enabled {
		return a, nil // отключённый шаблон: только запасной вариант
	}
	if agentID != "" {
		if ag, err := e.St.GetTeamAgent(ctx, agentID); err == nil && ag.Enabled && ag.RoleID == roleID {
			a.agent = &ag
			a.persona.AgentName, a.persona.Extra = ag.Name, ag.InstructionExtra
			if ag.ConnectionID != "" && ag.Model != "" {
				a.connID, a.model = ag.ConnectionID, ag.Model
			}
		}
	}
	if a.model == "" && role.DefaultModel != nil && !role.DefaultModel.Unavailable && role.DefaultModel.Model != "" {
		a.connID, a.model = role.DefaultModel.ConnectionID, role.DefaultModel.Model
	}
	if a.model == "" {
		if pm, ok, err := e.St.PlannerModel(ctx); err == nil && ok {
			a.connID, a.model = pm.ConnectionID, pm.Model
		}
	}
	return a, nil
}

// errNoModel — у исполнителя нет модели: запасной вариант по шаблону.
var errNoModel = errors.New("модель агента не настроена")

// complete — вызов модели исполнителя с учётом usage проекта по роли.
func (e *Engine) complete(ctx context.Context, a opsAgent, projectID, prompt string) (string, error) {
	return e.completeBounded(ctx, a, projectID, prompt, 0)
}

func (e *Engine) completeBounded(ctx context.Context, a opsAgent, projectID, prompt string, maxTokens int) (string, error) {
	if a.model == "" {
		return "", errNoModel
	}
	ctx, cancel := context.WithTimeout(ctx, opsJobTimeout)
	defer cancel()
	start := time.Now()
	var out string
	var use llm.Usage
	var err error
	if e.OpsComplete != nil {
		out, err = e.OpsComplete(ctx, a.connID, a.model, prompt)
	} else {
		cl, c, cerr := e.St.ConnectionClient(ctx, a.connID, e.Box)
		switch {
		case cerr != nil:
			return "", cerr
		case !c.Enabled:
			return "", fmt.Errorf("подключение %s отключено", c.ID)
		}
		out, use, err = cl.CompleteWithUsage(ctx, a.model, prompt, maxTokens)
	}
	// Usage проекта с ролью шаблона: runner'а у задания нет, токены — как
	// сообщил провайдер (nil — не сообщил).
	if uerr := e.St.RecordUsage(context.WithoutCancel(ctx), store.UsageInput{
		ProjectID: projectID, Role: a.role.ID, Model: a.model, TokensIn: use.InputTokens, TokensOut: use.OutputTokens,
		DurationS: int(time.Since(start).Seconds()),
	}); uerr != nil {
		slog.Error("usage задания control plane", "role", a.role.ID, "err", uerr)
	}
	return out, err
}

// opsEvent — событие ops.job для состояния working у агентов фазы.
func (e *Engine) opsEvent(ctx context.Context, job OpsJob, a opsAgent, state, text string) {
	agentID := a.id()
	if agentID == "" {
		agentID = "role:" + a.role.ID
	}
	payload := map[string]any{"agent_id": agentID, "kind": job.Kind, "ref_id": job.RefID, "state": state}
	if job.Kind == JobEpicAssessment {
		payload["phase_from"], payload["phase_to"] = "architecture", "architecture"
	}
	if _, err := e.St.AppendEvent(context.WithoutCancel(ctx), store.EventInput{
		ActorKind: domain.ActorSystem, ActorID: a.id(), Type: "ops.job", ProjectID: job.ProjectID,
		Text:    text,
		Payload: payload,
	}); err != nil {
		slog.Error("событие ops.job", "kind", job.Kind, "err", err)
	}
}

func (e *Engine) runOpsJob(ctx context.Context, job OpsJob, agentID string) error {
	if job.Kind == JobEpicAssessment {
		return e.runEpicAssessment(ctx, job, agentID)
	}
	a, err := e.resolveOpsAgent(ctx, job.role(), agentID)
	if err != nil {
		return err
	}
	who := a.persona.RoleTitle
	if a.persona.AgentName != "" {
		who = a.persona.AgentName
	}
	// Задание, которое уже не ждут, закрывается до события started: агент
	// не мелькает занятым, пары ops.job нет.
	var st opsStart
	var ok bool
	switch job.Kind {
	case JobGitHubIssues:
		ok = e.IssueWatch != nil
	case JobReleaseNotes:
		st.requestedAt, st.attempt, ok, err = e.St.StartNotesJob(ctx, job.RefID)
	default:
		st.requestedAt, st.attempt, ok, err = e.St.StartDescriptionJob(ctx, job.RefID)
	}
	if err != nil || !ok {
		return err
	}
	work := &cycle.Work{ID: job.RefID, Phase: cycle.Release, ProjectID: job.ProjectID}
	switch job.Kind {
	case JobGitHubIssues:
		work.Kind, work.Title, work.Action, work.Phase = "issue_watch", "GitHub Issues", "проверяет Issues и передаёт обратную связь в исследование", cycle.Operations
	case JobReleaseNotes:
		work.Kind, work.Action = "release", "пишет release notes"
	case JobDeployAnalysis:
		work.Kind, work.Action = "incident", "разбирает провал публикации"
	default:
		work.Kind, work.Action, work.Phase = "incident", "описывает инцидент", cycle.Operations
	}
	if id := a.id(); id != "" {
		q := e.opsQ()
		q.mu.Lock()
		q.current[id] = work
		q.mu.Unlock()
		defer func() {
			q.mu.Lock()
			delete(q.current, id)
			q.mu.Unlock()
		}()
	}
	e.opsEvent(ctx, job, a, "started", who+": "+work.Action)
	var runErr error
	switch job.Kind {
	case JobGitHubIssues:
		runErr = e.IssueWatch(ctx, job.ProjectID, a.id())
	case JobReleaseNotes:
		runErr = e.releaseNotesJob(ctx, job, a, work, st)
	case JobDeployAnalysis:
		runErr = e.deployAnalysisJob(ctx, job, a, work, st)
	default:
		runErr = e.incidentDescriptionJob(ctx, job, a, work, st)
	}
	if runErr != nil {
		e.opsEvent(ctx, job, a, "failed", who+": задание не выполнено")
		return runErr
	}
	e.opsEvent(ctx, job, a, "done", who+": готово")
	return nil
}

// opsStart — начатое задание: поколение запроса (запись сверяет его) и
// номер попытки.
type opsStart struct {
	requestedAt time.Time
	attempt     int
}

func (e *Engine) releaseNotesJob(ctx context.Context, job OpsJob, a opsAgent, work *cycle.Work, st opsStart) error {
	requestedAt, attempt := st.requestedAt, st.attempt
	rel, err := e.St.GetRelease(ctx, job.RefID)
	if err != nil {
		return err
	}
	work.Title = "релиз " + rel.EnvName + " " + shortVersion(rel.Version)
	comp, err := e.St.ReleaseComposition(ctx, rel)
	if err != nil {
		return err
	}
	var incIDs []string
	for _, m := range comp {
		if m.IncidentID != "" {
			incIDs = append(incIDs, m.IncidentID)
		}
	}
	titles, err := e.St.IncidentTitles(ctx, incIDs)
	if err != nil {
		return err
	}
	status, dropped := "ready", 0
	var notes ops.Notes
	var cerr error
	switch {
	case len(comp) == 0:
		cerr = errors.New("пустой состав")
	case attempt > store.MaxOpsAttempts:
		cerr = errAttempts
	default:
		raw, err := e.complete(ctx, a, job.ProjectID, ops.ReleaseNotesPrompt(ops.ReleaseNotesInput{
			Persona: a.persona, Env: rel.EnvName, Version: rel.Version, PrevVersion: rel.PrevVersion,
			Composition: comp, Incidents: titles,
		}))
		cerr = err
		if err == nil {
			notes, dropped, cerr = ops.ParseNotes(raw, comp)
		}
	}
	model := a.model
	if cerr != nil {
		notes, status, dropped, model = ops.TemplateNotes(comp), "template", 0, ""
		if !errors.Is(cerr, errNoModel) {
			slog.Warn("release notes по шаблону", "release", rel.ID, "err", cerr)
		}
	}
	md := ops.RenderNotes(notes, comp, rel.EnvName, rel.Version)
	agentID := ""
	if status == "ready" {
		agentID = a.id()
	}
	written, err := e.St.SetReleaseNotesJob(ctx, rel.ID, requestedAt, md, status, agentID, model, dropped)
	if err == nil && !written {
		slog.Info("release notes устарели: запрошены заново во время задания", "release", rel.ID)
	}
	return err
}

// errAttempts — попытки с моделью исчерпаны (ответ не записывается или не
// разбирается): текст по шаблону, задание закрывается.
var errAttempts = errors.New("попытки задания с моделью исчерпаны")

// errSkippedDaily — инцидент сверх суточного лимита описывается по
// шаблону, без модели: стоимость шквала не растёт.
var errSkippedDaily = errors.New("инцидент сверх суточного лимита")

func (e *Engine) incidentDescriptionJob(ctx context.Context, job OpsJob, a opsAgent, work *cycle.Work, st opsStart) error {
	requestedAt, attempt := st.requestedAt, st.attempt
	inc, err := e.St.IncidentForJob(ctx, job.RefID)
	if err != nil {
		return err
	}
	work.Title = "инцидент " + inc.EnvName
	envDesc := ""
	if env, err := e.St.GetEnvironment(ctx, inc.EnvID); err == nil {
		envDesc = execTypeTitle(env.ExecType)
	}
	var raw string
	cerr := errSkippedDaily
	if attempt > store.MaxOpsAttempts {
		cerr = errAttempts
	} else if !inc.SkippedDaily {
		raw, cerr = e.complete(ctx, a, job.ProjectID, ops.IncidentDescriptionPrompt(ops.IncidentInput{
			Persona: a.persona, Env: inc.EnvName, EnvDescription: envDesc, Source: inc.Source,
			ErrorLine: inc.ErrorLine, Occurrences: inc.Occurrences,
			CheckOutput: inc.Evidence.CheckOutput, Logs: inc.Evidence.Logs,
		}))
	}
	status := "ready"
	var d ops.Description
	if cerr == nil {
		d, cerr = ops.ParseDescription(raw)
	}
	if cerr != nil {
		d, status = ops.TemplateDescription(inc.EnvName, inc.ErrorLine, inc.EvidenceText()), "template"
		if !errors.Is(cerr, errNoModel) && !errors.Is(cerr, errSkippedDaily) {
			slog.Warn("описание инцидента по шаблону", "incident", inc.ID, "err", cerr)
		}
	}
	return e.saveDescription(ctx, inc.ID, requestedAt, d, status, a)
}

func (e *Engine) deployAnalysisJob(ctx context.Context, job OpsJob, a opsAgent, work *cycle.Work, st opsStart) error {
	requestedAt, attempt := st.requestedAt, st.attempt
	inc, err := e.St.IncidentForJob(ctx, job.RefID)
	if err != nil {
		return err
	}
	work.Title = "провал публикации " + inc.EnvName
	info := store.DeploymentInfo{EnvName: inc.EnvName}
	if inc.DeploymentID != "" {
		if info, err = e.St.DeploymentComposition(ctx, inc.DeploymentID); err != nil {
			return err
		}
	}
	stage := inc.Evidence.Stage
	logTail := inc.Evidence.DeployLog
	var raw string
	cerr := errSkippedDaily
	if attempt > store.MaxOpsAttempts {
		cerr = errAttempts
	} else if !inc.SkippedDaily {
		raw, cerr = e.complete(ctx, a, job.ProjectID, ops.DeployAnalysisPrompt(ops.DeployAnalysisInput{
			Persona: a.persona, Env: info.EnvName, ExecType: execTypeTitle(info.ExecType), Version: info.Version,
			Stage: stage, LogTail: logTail, Composition: info.Composition,
		}))
	}
	status := "ready"
	var analysis *ops.Analysis
	if cerr == nil {
		var an ops.Analysis
		if an, cerr = ops.ParseAnalysis(raw, info.Composition); cerr == nil {
			analysis = &an
		}
	}
	if cerr != nil {
		status = "template"
		if !errors.Is(cerr, errNoModel) && !errors.Is(cerr, errSkippedDaily) {
			slog.Warn("разбор провала публикации по шаблону", "incident", inc.ID, "err", cerr)
		}
	}
	d := ops.ReleaseIncident(analysis, info.EnvName, info.Version, stage, logTail)
	return e.saveDescription(ctx, inc.ID, requestedAt, d, status, a)
}

// saveDescription — описание в инцидент (только для того запроса, с
// которого начато задание); ждущая его задача-баг создаётся и сразу
// пересчитывается готовность Epic'а «Сопровождение».
func (e *Engine) saveDescription(ctx context.Context, incID string, requestedAt time.Time, d ops.Description, status string, a opsAgent) error {
	agentID := ""
	if status == "ready" {
		agentID = a.id()
	}
	written, _, epicID, err := e.St.SetIncidentDescriptionJob(ctx, incID, requestedAt, d, status, agentID)
	if err != nil {
		return err
	}
	if !written {
		slog.Info("описание инцидента устарело: запрошено заново во время задания", "incident", incID)
	}
	if epicID != "" {
		return e.St.RecomputeEpic(ctx, epicID)
	}
	return nil
}

// execTypeTitle — описание окружения для задания без секретов.
func execTypeTitle(t string) string {
	switch t {
	case domain.ExecSSH:
		return "Linux-хост, доставка командами deploy-runner'а"
	case domain.ExecK8s:
		return "Kubernetes"
	case domain.ExecPipeline:
		return "внешний пайплайн хостинга"
	case domain.ExecGitOps:
		return "GitOps"
	}
	return t
}

func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// ─── публикации ─────────────────────────────────────────────────────────

// afterRelease — успешная публикация: release notes в очередь Релиз-инженеру.
func (e *Engine) afterRelease(ctx context.Context, depID string) {
	rel, err := e.St.ReleaseByDeployment(ctx, depID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("релиз публикации", "deployment", depID, "err", err)
		}
		return
	}
	e.EnqueueOps(ctx, OpsJob{Kind: JobReleaseNotes, RefID: rel.ID, ProjectID: rel.ProjectID})
}

// releaseFailure — данные инцидента провала публикации: этап и полный
// замаскированный лог (отпечаток по нему, доказательства обрежет store).
// Без лога в памяти — лог из blob, иначе причина провала.
func (e *Engine) releaseFailure(ctx context.Context, depID string, logBuf []byte, detail string) *store.ReleaseFailure {
	logText := string(logBuf)
	if logText == "" {
		if dep, err := e.St.GetDeployment(ctx, depID); err == nil && dep.LogRef != "" && e.Blob != nil {
			if b, err := e.Blob.Get(ctx, dep.LogRef); err == nil {
				logText = string(b)
			}
		}
	}
	if strings.TrimSpace(logText) == "" {
		logText = detail
	} else if detail != "" {
		logText += "\n" + detail
	}
	return &store.ReleaseFailure{Stage: failStage(detail), Log: logText}
}

// afterFailure — задание описания при открытии инцидента и пересчёт
// Epic'а с новым багом.
func (e *Engine) afterFailure(ctx context.Context, res store.FailureResult, describe string) {
	projectID, err := e.St.IncidentProject(ctx, res.IncidentID)
	if err != nil {
		slog.Error("проект инцидента", "incident", res.IncidentID, "err", err)
		return
	}
	if res.Describe {
		e.EnqueueOps(ctx, OpsJob{Kind: describe, RefID: res.IncidentID, ProjectID: projectID})
	}
	if res.EpicID != "" {
		if err := e.St.RecomputeEpic(ctx, res.EpicID); err != nil {
			slog.Error("пересчёт Epic'а сопровождения", "epic", res.EpicID, "err", err)
		}
	}
}

// failStage — этап провала по тексту причины.
func failStage(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "verify"):
		return "verify"
	case strings.Contains(d, "таймаут"):
		return "timeout"
	}
	return "deploy"
}

// resumeOps — задания, которые ждут результата дольше opsStale (сбой
// задания, рестарт rivetd), ставятся заново. Первый проход после старта
// поднимает все ожидающие.
func (e *Engine) resumeOps(ctx context.Context) {
	q := e.opsQ()
	q.mu.Lock()
	first := q.resumed.IsZero()
	due := first || time.Since(q.resumed) >= opsResumeEvery
	if due {
		q.resumed = time.Now()
	}
	q.mu.Unlock()
	if !due {
		return
	}
	age := opsStale
	if first {
		age = 0
	}
	if ids, err := e.St.PendingReleaseNotes(ctx, age); err == nil {
		for _, id := range ids {
			if rel, err := e.St.GetRelease(ctx, id); err == nil {
				e.enqueueOps(ctx, OpsJob{Kind: JobReleaseNotes, RefID: id, ProjectID: rel.ProjectID}, false)
			}
		}
	} else {
		slog.Error("залежавшиеся release notes", "err", err)
	}
	if ids, err := e.St.PendingIncidentDescriptions(ctx, age); err == nil {
		for _, id := range ids {
			inc, err := e.St.IncidentForJob(ctx, id)
			if err != nil {
				continue
			}
			kind := JobIncidentDescription
			if inc.Source == ops.SourceRelease {
				kind = JobDeployAnalysis
			}
			e.enqueueOps(ctx, OpsJob{Kind: kind, RefID: id, ProjectID: inc.ProjectID}, false)
		}
	} else {
		slog.Error("залежавшиеся описания инцидентов", "err", err)
	}
}
