package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Планировщик наблюдения (add-sdlc-cycle, спека operations «Проверки
// наблюдения»): тиком выбираются окружения, которым пора на проверку.
// Команду и URL окружения с собственной доставкой проверяет deploy-runner с
// feature monitor (из сети окружения), URL внешней доставки — control
// plane с фильтром адресов. Идущая выкатка в окружение откладывает
// проверку.

const (
	// Дедлайны проверки: команда, логи и их завершение (WaitDelay runner'а
	// на каждую команду). Бюджет уходит runner'у в MonitorJob, и обе
	// стороны считают его одинаково.
	monitorCmdTimeout  = 60 * time.Second
	monitorLogsTimeout = 30 * time.Second
	monitorWaitDelay   = 3 * time.Second
	monitorBudget      = monitorCmdTimeout + monitorLogsTimeout + 2*monitorWaitDelay
	monitorURLTimeout  = 10 * time.Second
	// monitorResultGrace — запас на доставку результата сверх бюджета:
	// дольше результата нет — проверка потеряна (unknown), а не провалена.
	// Зависшее приложение runner сам сообщает провалом «таймаут». 45 с
	// покрывают и runner без поля budget_s (до 2×60+2×3 = 126 с) с тиком.
	monitorResultGrace = 45 * time.Second
	monitorPruneEvery  = time.Hour
	// monitorBodyBytes — тело ответа URL-проверки в выводе только при
	// провале и не больше 512 байт: успешный ответ не хранится.
	monitorBodyBytes = 512
)

// noMonitorRunner — причина unknown без подходящего runner'а.
const noMonitorRunner = "нет runner'а с поддержкой наблюдения"

func newCheckID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "mon-" + hex.EncodeToString(b)
}

// tickMonitors — проверки по расписанию и после публикаций, проверки без
// результата, а на первом проходе после старта — URL-проверки прошлого
// процесса rivetd.
func (e *Engine) tickMonitors(ctx context.Context) error {
	q := e.opsQ()
	q.mu.Lock()
	prune := time.Since(q.pruned) > monitorPruneEvery
	if prune {
		q.pruned = time.Now()
	}
	first := !q.monitorsStarted
	q.monitorsStarted = true
	q.mu.Unlock()
	if first {
		if err := e.St.AbandonControlPlaneChecks(ctx); err != nil {
			slog.Error("проверки прошлого процесса rivetd", "err", err)
		}
	}
	if prune {
		if err := e.St.PruneMonitorRuns(ctx); err != nil {
			slog.Error("ретенция истории проверок", "err", err)
		}
	}
	e.expireMonitorChecks(ctx)
	e.retryDeploymentIncidents(ctx)
	due, err := e.St.DueMonitors(ctx)
	if err != nil {
		return err
	}
	for _, d := range due {
		if err := e.startMonitor(ctx, d); err != nil {
			slog.Error("проверка наблюдения", "env", d.Env.ID, "err", err)
		}
	}
	return nil
}

// expireMonitorChecks — проверка без результата дольше бюджета с запасом
// потеряна: обрыв доставки, рестарт исполнителя. Это unknown без инцидента,
// следующая проверка сразу. Новую проверку окружения снятие не трогает.
func (e *Engine) expireMonitorChecks(ctx context.Context) {
	stale, err := e.St.StaleMonitorChecks(ctx, monitorBudget+monitorResultGrace)
	if err != nil {
		slog.Error("проверки наблюдения без результата", "err", err)
		return
	}
	for _, c := range stale {
		reason := "результат проверки не получен за " + (monitorBudget + monitorResultGrace).String()
		if !c.RunnerOnline && c.RunnerID != store.MonitorRunnerControlPlane {
			reason = "runner " + c.RunnerID + " отключился во время проверки"
		}
		if err := e.St.MonitorCheckUnknown(ctx, c.EnvID, c.CheckID, reason, true); err != nil {
			slog.Error("проверка наблюдения без результата", "env", c.EnvID, "err", err)
		}
	}
}

// retryDeploymentIncidents — инциденты провалов публикаций, которые не
// записались вместе с провалом: по одному на проект за тик, с растущей
// паузой и лимитом попыток. Отметку, захваченную другим rivetd, store
// пропускает.
func (e *Engine) retryDeploymentIncidents(ctx context.Context) {
	ids, err := e.St.PendingDeploymentIncidents(ctx)
	if err != nil {
		slog.Error("инциденты провалов публикаций", "err", err)
		return
	}
	for _, depID := range ids {
		dep, err := e.St.GetDeployment(ctx, depID)
		if err != nil {
			continue
		}
		rf := e.releaseFailure(ctx, depID, nil, dep.Detail)
		res, claimed, err := e.St.RecordReleaseFailure(ctx, depID, rf.Stage, rf.Log)
		if err != nil {
			slog.Error("инцидент провала публикации", "deployment", depID, "err", err)
			continue
		}
		if claimed {
			e.afterFailure(ctx, res, JobDeployAnalysis)
		}
	}
}

// externalDelivery — окружение без своей доставки: команды исполнять негде.
func externalDelivery(execType string) bool {
	return execType == domain.ExecPipeline || execType == domain.ExecGitOps
}

func (e *Engine) startMonitor(ctx context.Context, d store.MonitorDue) error {
	m := d.Env.Config.Monitor
	if m == nil || !m.Enabled {
		return nil
	}
	if d.Deploying {
		return e.St.PostponeMonitor(ctx, d.Env.ID, store.MonitorPostpone)
	}
	checkID := newCheckID()
	if externalDelivery(d.Env.ExecType) {
		if m.CheckURL == "" {
			return e.St.MonitorUnknown(ctx, d.Env.ID, "не задан URL проверки")
		}
		ok, err := e.St.StartMonitorCheck(ctx, d.Env.ID, checkID, store.MonitorRunnerControlPlane)
		if err != nil || !ok {
			return err
		}
		go e.urlCheck(d.Env.ID, checkID, m.CheckURL)
		return nil
	}
	if m.CheckCmd == "" && m.CheckURL == "" {
		return e.St.MonitorUnknown(ctx, d.Env.ID, "не задана проверка")
	}
	// Хост от провайдера ещё не готов (add-environment-provisioning): адреса
	// нет, и команда проверки ушла бы на машину runner'а.
	prefix := ""
	if d.Env.Config.Provision != nil {
		h, live, err := e.St.LiveHost(ctx, d.Env.ID)
		if err != nil {
			return err
		}
		if !live || h.State != domain.HostReady {
			return e.St.PostponeMonitor(ctx, d.Env.ID, store.MonitorPostpone)
		}
		prefix = exportPrefix(hostEnv(d.Env, hostAddress(d.Env.Config.Host)))
	}
	urlOnly := m.CheckCmd == ""
	runnerID, ok, err := e.St.MonitorRunner(ctx, d.Env, urlOnly)
	if err != nil {
		return err
	}
	if !ok {
		return e.St.MonitorUnknown(ctx, d.Env.ID, noMonitorRunner)
	}
	started, err := e.St.StartMonitorCheck(ctx, d.Env.ID, checkID, runnerID)
	if err != nil || !started {
		return err
	}
	checkCmd, logsCmd := m.CheckCmd, m.LogsCmd
	if prefix != "" && checkCmd != "" {
		checkCmd = prefix + checkCmd
	}
	if prefix != "" && logsCmd != "" {
		logsCmd = prefix + logsCmd
	}
	job := &pb.MonitorJob{
		CheckId: checkID, EnvName: d.Env.Name, Host: d.Env.Config.Host, Repo: d.Repo,
		CheckCmd: checkCmd, LogsCmd: logsCmd, HostKey: d.Env.Config.HostKey,
		TimeoutS:     int32(monitorCmdTimeout.Seconds()),
		LogsTimeoutS: int32(monitorLogsTimeout.Seconds()),
		BudgetS:      int32(monitorBudget.Seconds()),
	}
	if urlOnly {
		// URL окружения с собственной доставкой проверяется из сети
		// runner'а, а не control plane.
		job.CheckUrl, job.LogsCmd = m.CheckURL, ""
	}
	msg := &pb.PlaneMsg{MsgId: checkID, Kind: &pb.PlaneMsg_Monitor{Monitor: job}}
	if !e.Out.Send(runnerID, msg) {
		// Канала нет или он полон: следующая попытка через интервал, а не
		// на каждом тике.
		return e.St.MonitorCheckUnknown(ctx, d.Env.ID, checkID, "runner "+runnerID+" недоступен", false)
	}
	return nil
}

// urlCheck — проверка URL control plane'ом: GET, 2xx, 10 секунд, без
// редиректов, с фильтром адресов. Адрес, закрытый политикой установки, —
// ошибка настройки: unknown с причиной, без инцидента, следующая проверка
// через интервал окружения.
func (e *Engine) urlCheck(envID, checkID, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), monitorURLTimeout+5*time.Second)
	defer cancel()
	start := time.Now()
	ok, out, err := e.probeURL(ctx, url)
	if isPrivateAddress(err) {
		if uerr := e.St.MonitorCheckUnknown(ctx, envID, checkID, errPrivateAddress.Error(), false); uerr != nil {
			slog.Error("проверка наблюдения: адрес закрыт", "env", envID, "err", uerr)
		}
		return
	}
	// Ошибку записи recordMonitor уже залогировал: проверка повторится,
	// когда ожидание истечёт.
	_ = e.recordMonitor(ctx, store.MonitorResultInput{EnvID: envID, CheckID: checkID, OK: ok, Output: out,
		DurationMs: time.Since(start).Milliseconds()})
}

// probeURL — GET по URL проверки. Успех без тела ответа; при провале —
// статус и не больше 512 байт тела (store приведёт его к UTF-8). err —
// ошибка запроса (для отличия политики адресов от сбоя приложения).
func (e *Engine) probeURL(ctx context.Context, rawURL string) (bool, string, error) {
	shown := shownURL(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, "error: " + shownErr(err), err
	}
	rctx, cancel := context.WithTimeout(ctx, monitorURLTimeout)
	defer cancel()
	resp, err := e.urlHTTP().Do(req.WithContext(rctx))
	if err != nil {
		return false, "error: GET " + shown + ": " + shownErr(err), err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, fmt.Sprintf("GET %s → %d", shown, resp.StatusCode), nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, monitorBodyBytes))
	return false, fmt.Sprintf("HTTP/1.1 %d: GET %s → %d\n%s", resp.StatusCode, shown, resp.StatusCode, body), nil
}

// shownURL — адрес проверки в выводе без query и userinfo: в параметрах
// бывают подписи и токены, а вывод видят все участники проекта.
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "URL проверки"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// shownErr — ошибка запроса без полного URL (*url.Error его повторяет).
func shownErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// OnMonitorResult — результат проверки от runner'а: только исполнителя
// текущей проверки окружения.
func (e *Engine) OnMonitorResult(ctx context.Context, runnerID string, r *pb.MonitorResult) error {
	envID, owner, err := e.St.MonitorCheckEnv(ctx, r.CheckId)
	if err != nil {
		slog.Warn("результат чужой или устаревшей проверки отброшен", "check", r.CheckId, "runner", runnerID)
		return nil
	}
	if owner != runnerID {
		slog.Warn("результат проверки от другого runner'а отброшен", "check", r.CheckId, "runner", runnerID, "owner", owner)
		return nil
	}
	return e.recordMonitor(ctx, store.MonitorResultInput{EnvID: envID, CheckID: r.CheckId, OK: r.Ok,
		Output: r.Output, Logs: r.Logs, DurationMs: r.DurationMs})
}

func (e *Engine) recordMonitor(ctx context.Context, in store.MonitorResultInput) error {
	out, err := e.St.RecordMonitorResult(ctx, in)
	if err != nil {
		slog.Error("запись результата проверки", "env", in.EnvID, "err", err)
		return err
	}
	if out.Failure != nil {
		e.afterFailure(ctx, *out.Failure, JobIncidentDescription)
	}
	return nil
}

// tickOps — наблюдение и подъём заданий control plane. Идёт после тика
// конвейера независимо от его исхода: сбой назначения задач не
// останавливает наблюдение.
func (e *Engine) tickOps(ctx context.Context) {
	e.resumeOps(ctx)
	if err := e.tickMonitors(ctx); err != nil {
		slog.Error("планировщик наблюдения", "err", err)
	}
	e.tickHosts(ctx)
}
