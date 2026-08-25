package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Публикация через внешнюю систему доставки (change add-external-delivery,
// спека deployment «Дирижирование внешними системами доставки»): триггер и
// наблюдение идут из control plane — учётные данные хостинга живут здесь, и
// отдавать их runner'у ради запуска пайплайна незачем. Verify такого
// окружения — проверка URL плоскостью: машины у неё нет, команду выполнять
// негде.

// externalPollInterval — как часто опрашивается состояние одного прогона:
// тик идёт каждые секунды, а пайплайны идут минуты, и упираться в лимиты
// хостинга незачем.
const externalPollInterval = 10 * time.Second

// externalTimeout — дедлайн внешней публикации: пайплайны идут дольше
// собственной доставки, и watchdog у них свой.
const externalTimeout = time.Hour

// tickExternalDeployments запускает и доводит публикации внешних окружений.
func (e *Engine) tickExternalDeployments(ctx context.Context) error {
	timedOut, err := e.St.TimedOutExternalDeployments(ctx, externalTimeout)
	if err != nil {
		return err
	}
	for _, depID := range timedOut {
		if err := e.failDeployNow(ctx, depID, "", "таймаут внешней публикации: пайплайн хостинга не завершился"); err != nil {
			slog.Error("внешняя публикация: watchdog", "deployment", depID, "err", err)
		}
	}
	for {
		a, ok, err := e.St.StartNextExternalDeployment(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.triggerPipeline(ctx, a)
	}
	active, err := e.St.ActiveExternalDeployments(ctx)
	if err != nil {
		return err
	}
	for _, a := range active {
		if !e.pollDue(a.Deployment.ID) {
			continue
		}
		// Пустой идентификатор прогона означает, что запуск не выполнялся:
		// рестарт rivetd между захватом публикации (или переходом в откат)
		// и запуском пайплайна. Повторяем — иначе публикация зависла бы.
		// Запущенный, но ещё не найденный прогон помечен pending и сюда
		// не попадает: второй workflow_dispatch не нужен.
		if a.Deployment.ExternalRunID == "" {
			e.triggerPipeline(ctx, a)
			continue
		}
		if err := e.pollPipeline(ctx, a); err != nil {
			slog.Error("внешняя публикация", "deployment", a.Deployment.ID, "err", err)
		}
	}
	return nil
}

// pollDue — не чаще одного опроса на публикацию в externalPollInterval.
func (e *Engine) pollDue(depID string) bool {
	now := e.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.externalPolled[depID]; ok && now.Sub(last) < externalPollInterval {
		return false
	}
	e.externalPolled[depID] = now
	return true
}

// triggerPipeline запускает пайплайн хостинга для захваченной публикации.
// Публикация в фазе отката везёт предыдущую успешную версию: в самой
// строке публикации остаётся версия, которая провалилась.
func (e *Engine) triggerPipeline(ctx context.Context, a store.DeployAssignment) {
	// Право на запуск захватывается до похода к хостингу: два
	// workflow_dispatch — это две доставки в прод, а зависшую после
	// падения публикацию добьёт watchdog.
	claimed, err := e.St.ClaimExternalTrigger(ctx, a.Deployment.ID)
	if err != nil {
		slog.Error("внешняя публикация: захват запуска", "deployment", a.Deployment.ID, "err", err)
		return
	}
	if !claimed {
		return
	}
	adapter, ref, err := e.pipelineTarget(ctx, a)
	if err != nil {
		e.failExternal(ctx, a, "запуск пайплайна: "+err.Error())
		return
	}
	version := a.Deployment.Version
	if a.Deployment.Rollback {
		prev, err := e.St.LastSuccessfulVersion(ctx, a.Env.ID, a.Deployment.ID)
		if err != nil {
			slog.Error("внешняя публикация: предыдущая версия", "deployment", a.Deployment.ID, "err", err)
			return
		}
		if prev == "" {
			if err := e.failDeployNow(ctx, a.Deployment.ID, "", "откат невозможен: успешных версий не было"); err != nil {
				slog.Error("внешняя публикация: провал отката", "deployment", a.Deployment.ID, "err", err)
			}
			return
		}
		version = prev
	}
	run, err := adapter.TriggerPipeline(ctx, a.Repo, a.Env.Config.Pipeline, ref,
		pipelineVars(a, version, a.Deployment.Rollback))
	if err != nil {
		e.failExternal(ctx, a, "запуск пайплайна: "+err.Error())
		return
	}
	// Прогон запущен: даже без идентификатора (GitHub его не возвращает)
	// публикация помечается pending, чтобы следующий тик искал прогон, а
	// не запускал пайплайн второй раз.
	if run.RunID != "" {
		if _, err := e.St.SetDeploymentExternalRun(ctx, a.Deployment.ID,
			store.ExternalRunPending, run.RunID, run.URL); err != nil {
			slog.Error("внешняя публикация: запись прогона", "deployment", a.Deployment.ID, "err", err)
			return
		}
	}
	text := "пайплайн хостинга запущен"
	if run.URL != "" {
		text += ": " + run.URL
	}
	if err := e.deployEventWithRun(ctx, a, "deploying", text, run.URL); err != nil {
		slog.Error("внешняя публикация: событие запуска", "deployment", a.Deployment.ID, "err", err)
	}
}

// pollPipeline читает состояние прогона и доводит публикацию до финала.
func (e *Engine) pollPipeline(ctx context.Context, a store.DeployAssignment) error {
	adapter, ref, err := e.pipelineTarget(ctx, a)
	if err != nil {
		e.failExternal(ctx, a, "состояние пайплайна: "+err.Error())
		return nil
	}
	since := a.Deployment.Created
	if a.Deployment.Started != nil {
		since = *a.Deployment.Started
	}
	// pending — прогон запущен, но не найден: адаптер ищет его сам.
	runID := a.Deployment.ExternalRunID
	if runID == store.ExternalRunPending {
		runID = ""
	}
	run, err := adapter.PipelineRun(ctx, a.Repo, a.Env.Config.Pipeline, ref, runID, since)
	if err != nil {
		// Разовая ошибка опроса не проваливает публикацию: её доведёт
		// следующий тик или watchdog по таймауту.
		slog.Warn("внешняя публикация: опрос прогона", "deployment", a.Deployment.ID, "err", err)
		return nil
	}
	if run.RunID != "" && run.RunID != runID {
		// CAS от того значения, которое видел этот опрос: параллельный
		// переход в откат уже мог записать свой прогон.
		ok, err := e.St.SetDeploymentExternalRun(ctx, a.Deployment.ID,
			a.Deployment.ExternalRunID, run.RunID, run.URL)
		if err != nil {
			return err
		}
		if !ok {
			return nil // публикацию увёл другой путь — решает он
		}
		a.Deployment.ExternalRunID, a.Deployment.ExternalURL = run.RunID, run.URL
	}
	switch run.State {
	case scm.PipelineSuccess:
		return e.verifyExternal(ctx, a)
	case scm.PipelineFailed:
		e.failExternal(ctx, a, "пайплайн хостинга завершился с ошибкой"+urlSuffix(run.URL))
		return nil
	default:
		return nil // прогон идёт — ждём следующего опроса
	}
}

// verifyExternal — этап Verify внешнего окружения: проверка URL из
// конфигурации, дальше обычный финал публикации.
func (e *Engine) verifyExternal(ctx context.Context, a store.DeployAssignment) error {
	ok, err := e.St.DeployStageVerifying(ctx, a.Deployment.ID, "")
	if err != nil {
		return err
	}
	if !ok {
		// Переход сделал другой путь (или публикация уже финализирована):
		// Verify выполняет тот, кто перевёл публикацию в verifying.
		dep, err := e.St.GetDeployment(ctx, a.Deployment.ID)
		if err != nil {
			return err
		}
		if dep.Ended != nil || dep.Status != "verifying" {
			return nil
		}
		a.Deployment = dep
	} else if err := e.deployEventWithRun(ctx, a, "verifying",
		"пайплайн выполнен — проверка окружения", a.Deployment.ExternalURL); err != nil {
		return err
	}
	if err := verifyDeployURL(ctx, a.Env.Config.VerifyURL); err != nil {
		e.failExternal(ctx, a, "verify: "+err.Error())
		return nil
	}
	if a.Deployment.Rollback {
		return e.finishDeploy(ctx, a.Deployment.ID, "", "rolled_back",
			"откат к предыдущей версии выполнен")
	}
	return e.finishDeploy(ctx, a.Deployment.ID, "", "done", "")
}

// failExternal — провал внешней публикации: одна попытка отката тем же
// пайплайном с предыдущей версией, иначе обычная цепочка провала.
func (e *Engine) failExternal(ctx context.Context, a store.DeployAssignment, detail string) {
	var err error
	if a.Deployment.Rollback {
		err = e.failDeployNow(ctx, a.Deployment.ID, "", "откат тоже провалился: "+detail)
	} else {
		err = e.rollbackExternal(ctx, a, detail)
	}
	if err != nil {
		slog.Error("внешняя публикация: провал", "deployment", a.Deployment.ID, "err", err)
	}
}

// rollbackExternal — откат внешнего окружения: тот же пайплайн с
// предыдущей успешной версией; некуда откатываться — обычный провал.
func (e *Engine) rollbackExternal(ctx context.Context, a store.DeployAssignment, detail string) error {
	prev, err := e.St.LastSuccessfulVersion(ctx, a.Env.ID, a.Deployment.ID)
	if err != nil {
		return err
	}
	if prev == "" {
		return e.failDeployNow(ctx, a.Deployment.ID, "", detail)
	}
	claimed, err := e.St.MarkDeploymentRollingBack(ctx, a.Deployment.ID, "", "провал: "+detail)
	if err != nil {
		return err
	}
	dep, err := e.St.GetDeployment(ctx, a.Deployment.ID)
	if err != nil {
		return err
	}
	if !claimed && (dep.Ended != nil || !dep.Rollback) {
		return nil // публикацию уже финализировали другим путём
	}
	if err := e.deployEventWithRun(ctx, a, "rolling_back",
		"пайплайн провалился — откат к версии "+prev, a.Deployment.ExternalURL); err != nil {
		return err
	}
	// Прогон провалившейся версии сбросил сам переход в откат (одной
	// транзакцией), поэтому запуск отката идёт общим путём: он и
	// восстановит публикацию, если rivetd упадёт прямо сейчас.
	a.Deployment = dep
	a.Deployment.ExternalRunID, a.Deployment.ExternalURL = "", ""
	e.triggerPipeline(ctx, a)
	return nil
}

// pipelineTarget — адаптер хостинга проекта и ветка запуска пайплайна.
func (e *Engine) pipelineTarget(ctx context.Context, a store.DeployAssignment) (scm.Adapter, string, error) {
	p, err := e.St.GetProject(ctx, a.ProjectID)
	if err != nil {
		return nil, "", err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return nil, "", err
	}
	ref := a.Env.Config.Ref
	if ref == "" {
		ref = p.DefaultBranch
	}
	if ref == "" {
		ref = e.BaseBranch
	}
	return adapter, ref, nil
}

// pipelineVars — переменные прогона: версия и окружение от Rivet плюс то,
// что задал администратор в конфигурации окружения.
func pipelineVars(a store.DeployAssignment, version string, rollback bool) map[string]string {
	vars := map[string]string{}
	for k, v := range a.Env.Config.Vars {
		vars[k] = v
	}
	// Служебные переменные пишутся последними: версию и режим прогона
	// решает Rivet, конфигурация окружения их не переопределяет.
	vars["RIVET_VERSION"] = version
	vars["RIVET_ENV"] = a.Env.Name
	vars["RIVET_ROLLBACK"] = fmt.Sprintf("%v", rollback)
	vars["RIVET_DEPLOYMENT"] = a.Deployment.ID
	return vars
}

// deployEventWithRun — событие публикации со ссылкой на внешний прогон.
func (e *Engine) deployEventWithRun(ctx context.Context, a store.DeployAssignment, status, text, runURL string) error {
	payload := map[string]any{
		"environment_id": a.Env.ID, "deployment_id": a.Deployment.ID,
		"status": status, "version": a.Deployment.Version,
	}
	if runURL != "" {
		payload["external_url"] = runURL
	}
	_, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "deploy.status",
		ProjectID: a.ProjectID,
		Text:      fmt.Sprintf("публикация %s: %s", a.Env.Name, text),
		Payload:   payload,
	})
	return err
}

func urlSuffix(u string) string {
	if u == "" {
		return ""
	}
	return ": " + u
}

// verifyClient — клиент health-check'а: редиректы не выполняются (адрес
// проверки задаёт администратор, и уводить запрос плоскости в другое место
// он не должен), дедлайн короткий.
var verifyClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// verifyDeployURL — health-check окружения силами control plane: 2xx на
// GET считается успехом, как и у проверки runner'ом. Адрес приходит из
// конфигурации окружения, а её меняет только администратор установки — тот
// же уровень доверия, что у команд деплоя.
func verifyDeployURL(ctx context.Context, rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("у окружения не настроен verify_url")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := verifyClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s ответил %d", rawURL, resp.StatusCode)
	}
	return nil
}
