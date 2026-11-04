package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/redact"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Проверка наблюдения окружения (add-sdlc-cycle, спека operations
// «Проверки наблюдения»): check_cmd исполняется так же, как команды
// выкатки, при провале исполняется logs_cmd. Окружение без команды
// проверяется GET по check_url с сети runner'а. Вывод маскируется
// построчно до отправки, control plane маскирует его ещё раз.

// featureMonitor — runner исполняет MonitorJob (объявляется при deploy),
// featureMonitorURL — и URL-проверки (check_url): runner без неё принял бы
// URL-задание за пустую команду.
const (
	featureMonitor    = "monitor"
	featureMonitorURL = "monitor_url"
)

const (
	defaultMonitorTimeout = 60 * time.Second
	defaultLogsTimeout    = 30 * time.Second
	// monitorTailBytes — предел хвоста вывода одной команды (64 КиБ).
	monitorTailBytes = 64 << 10
	// monitorBodyBytes — сколько тела ответа URL-проверки попадает в вывод
	// при провале; при успехе тело не передаётся.
	monitorBodyBytes  = 512
	monitorURLTimeout = 10 * time.Second
)

// maskedTail — замаскированный хвост вывода команды.
type maskedTail struct {
	st  redact.Stream
	buf []byte
}

func (m *maskedTail) feed(b []byte) { m.add(m.st.Feed(b)) }

func (m *maskedTail) add(b []byte) {
	m.buf = append(m.buf, b...)
	if len(m.buf) > monitorTailBytes {
		m.buf = m.buf[len(m.buf)-monitorTailBytes:]
		// Хвост начинается с границы руны: обрезанный UTF-8 нельзя
		// отправить строкой protobuf.
		for len(m.buf) > 0 && !utf8.RuneStart(m.buf[0]) {
			m.buf = m.buf[1:]
		}
	}
}

func (m *maskedTail) String() string {
	m.add(m.st.Flush())
	return string(m.buf)
}

// validText — строка, которую можно отправить полем protobuf.
func validText(s string) string { return strings.ToValidUTF8(s, "�") }

// runMonitorCmd исполняет одну команду наблюдения со своим дедлайном.
func (a *agent) runMonitorCmd(ctx context.Context, job *pb.MonitorJob, script string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dj := &pb.DeployJob{Repo: job.Repo, EnvName: job.EnvName, Host: job.Host}
	out := &maskedTail{}
	_, err := a.runDeployCmd(cctx, dj, a.cfg.Workdir, script, out.feed)
	text := out.String()
	if err != nil && cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		err = fmt.Errorf("таймаут %s", timeout)
	}
	return text, err
}

// probeMonitorURL — GET по URL проверки: 2xx — успех. Тело ответа идёт в
// вывод только при провале и не больше 512 байт.
func probeMonitorURL(ctx context.Context, rawURL string) (bool, string) {
	client := &http.Client{Timeout: monitorURLTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	shown := shownURL(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, "error: " + shownErr(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, "error: GET " + shown + ": " + shownErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, fmt.Sprintf("GET %s → %d", shown, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, monitorBodyBytes))
	return false, fmt.Sprintf("HTTP/1.1 %d: GET %s → %d\n%s", resp.StatusCode, shown, resp.StatusCode, body)
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

// monitorDeadlines — дедлайны проверки: команда, логи и общий бюджет. Без
// бюджета от control plane (старый CP) — сумма таймаутов.
func monitorDeadlines(job *pb.MonitorJob) (check, logs, budget time.Duration) {
	check, logs = defaultMonitorTimeout, defaultLogsTimeout
	if job.TimeoutS > 0 {
		check = time.Duration(job.TimeoutS) * time.Second
	}
	if job.LogsTimeoutS > 0 {
		logs = time.Duration(job.LogsTimeoutS) * time.Second
	}
	budget = check + logs + 2*deployWaitDelay
	if job.BudgetS > 0 {
		budget = time.Duration(job.BudgetS) * time.Second
	}
	return check, logs, budget
}

func (a *agent) executeMonitor(ctx context.Context, job *pb.MonitorJob, emit func(*pb.RunnerMsg)) {
	checkTimeout, logsTimeout, budget := monitorDeadlines(job)
	// Общий дедлайн проверки и логов: результат приходит до того, как
	// control plane перестанет его ждать.
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	res := &pb.MonitorResult{CheckId: job.CheckId, Ok: true}
	switch {
	case job.CheckCmd == "" && job.CheckUrl != "":
		uctx, ucancel := context.WithTimeout(bctx, checkTimeout)
		ok, out := probeMonitorURL(uctx, job.CheckUrl)
		ucancel()
		res.Ok, res.Output = ok, redact.String(out)
	case strings.TrimSpace(job.CheckCmd) == "":
		// Пустое задание — не успех: иначе окружение показывало бы
		// зелёное состояние при лежащем приложении.
		res.Ok, res.Output = false, "[rivet] пустая проверка: ни команды, ни URL\n"
	default:
		out, err := a.runMonitorCmd(bctx, job, job.CheckCmd, checkTimeout)
		res.Output = out
		if err != nil {
			res.Ok = false
			res.Output = out + fmt.Sprintf("\n[rivet] проверка: %v\n", err)
			if job.LogsCmd != "" && bctx.Err() == nil {
				logs, lerr := a.runMonitorCmd(bctx, job, job.LogsCmd, logsTimeout)
				if lerr != nil {
					logs += fmt.Sprintf("\n[rivet] команда логов: %v\n", lerr)
				}
				res.Logs = logs
			}
		}
	}
	// Сессия с control plane закрылась (обрыв, переподключение, остановка
	// runner'а): проверка прервана, а не провалена. Результат не
	// отправляется, control plane снимет ожидание сам.
	if ctx.Err() != nil {
		return
	}
	res.DurationMs = time.Since(start).Milliseconds()
	res.Output, res.Logs = validText(res.Output), validText(res.Logs)
	emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_MonitorResult{MonitorResult: res}})
}

// features — возможности runner'а сверх версии протокола: роли всегда,
// наблюдение у runner'а с capability deploy (он исполняет команды
// окружений).
func (c Config) features() []string {
	out := []string{featureRoles}
	for _, cp := range c.Capabilities {
		if cp == "deploy" {
			return append(out, featureMonitor, featureMonitorURL)
		}
	}
	return out
}
