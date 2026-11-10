package runner

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Задание хоста окружения (add-environment-provisioning, design решения 4
// и 9): первичная настройка нового сервера или снятие локального окружения.
// Скрипт идёт тем же путём, что команды выкатки. У нового сервера ключ
// хоста принимается в пустой known_hosts задания и возвращается control
// plane, дальше ssh проверяет хост по закреплённому ключу.

// featureHostJobs: runner исполняет HostJob (объявляется при deploy).
const featureHostJobs = "host_jobs"

const (
	defaultHostJobTimeout = 20 * time.Minute
	// sshProbeInterval: пауза между попытками достучаться до нового сервера.
	sshProbeInterval  = 10 * time.Second
	sshConnectTimeout = "15"
)

// knownHostRe: строка known_hosts, которую runner возвращает control plane.
var knownHostRe = regexp.MustCompile(`^[A-Za-z0-9.:\[\],_-]+ (ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+={0,3}$`)

func (a *agent) executeHostJob(ctx context.Context, job *pb.HostJob, emit func(*pb.RunnerMsg)) {
	timeout := defaultHostJobTimeout
	if job.TimeoutS > 0 {
		timeout = time.Duration(job.TimeoutS) * time.Second
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &maskedTail{}
	res := &pb.HostJobResult{JobId: job.JobId}
	hostKey, err := a.runHostJob(jctx, job, out.feed)
	// Сессия с control plane закрылась: итог не отправляется, control plane
	// посчитает задание потерянным по своему дедлайну.
	if ctx.Err() != nil {
		return
	}
	res.Output = out.String()
	res.HostKey = hostKey
	if err != nil {
		if jctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("таймаут задания %s", timeout)
		}
		res.Output += fmt.Sprintf("\n[rivet] задание хоста: %v\n", err)
	} else {
		res.Ok, res.HostKey = true, hostKey
	}
	res.Output = validText(res.Output)
	emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_HostJobResult{HostJobResult: res}})
}

// runHostJob ждёт SSH нового сервера, исполняет скрипт и возвращает
// принятый ключ хоста (только при первом подключении).
func (a *agent) runHostJob(ctx context.Context, job *pb.HostJob, feed func([]byte)) (key string, resultErr error) {
	run := scriptRun{Host: job.Host, HostKey: job.HostKey, Dir: a.cfg.Workdir, Script: job.Script, Env: job.Env}
	if job.Host != "" && job.AcceptNewHostKey {
		f, err := os.CreateTemp("", "rivet-known-hosts-*")
		if err != nil {
			return "", err
		}
		_ = f.Close()
		defer os.Remove(f.Name())
		run.KnownHosts, run.HostKey = f.Name(), ""
		defer func() {
			accepted, err := acceptedHostKey(run.KnownHosts)
			if accepted != "" {
				key = accepted
			}
			if resultErr == nil && err != nil {
				resultErr = err
			}
		}()
	}
	if job.Host != "" && job.SshWaitS > 0 {
		if err := waitSSH(ctx, run, time.Duration(job.SshWaitS)*time.Second, feed); err != nil {
			return "", err
		}
	}
	feed([]byte(fmt.Sprintf("== %s: задание хоста ==\n", job.EnvName)))
	if _, err := runScript(ctx, run, feed); err != nil {
		return "", err
	}
	if run.KnownHosts == "" {
		return "", nil
	}
	return acceptedHostKey(run.KnownHosts)
}

// waitSSH: новый сервер поднимает sshd не сразу: пробное подключение
// повторяется, пока не пройдёт или не выйдет время.
func waitSSH(ctx context.Context, run scriptRun, wait time.Duration, feed func([]byte)) error {
	deadline := time.Now().Add(wait)
	probe := run
	probe.Script, probe.Env = "true", nil
	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := runScript(pctx, probe, nil)
		cancel()
		if err == nil {
			feed([]byte(fmt.Sprintf("SSH доступен (попытка %d)\n", attempt)))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SSH недоступен за %s: %v", wait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sshProbeInterval):
		}
	}
}

// acceptedHostKey: строки known_hosts, которые ssh записал при первом
// подключении (без хэширования имён и маркеров).
func acceptedHostKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var lines []string
	seen := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if knownHostRe.MatchString(l) && !seen[l] {
			seen[l] = true
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("ключ хоста не получен при первом подключении")
	}
	return strings.Join(lines, "\n"), nil
}

// sshOptions: опции ssh для проверки ключа хоста. Закреплённый ключ:
// временный known_hosts из него и строгая проверка, общий known_hosts
// runner'а не участвует. Первое подключение: пустой known_hosts задания и
// accept-new. Без ключа: прежнее поведение (окружение runner'а решает).
func sshOptions(hostKey, knownHosts string) ([]string, func(), error) {
	noop := func() {}
	switch {
	case knownHosts != "":
		return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=" + sshConnectTimeout,
			"-o", "StrictHostKeyChecking=accept-new", "-o", "HashKnownHosts=no",
			"-o", "UserKnownHostsFile=" + knownHosts, "-o", "GlobalKnownHostsFile=/dev/null"}, noop, nil
	case hostKey != "":
		f, err := os.CreateTemp("", "rivet-known-hosts-*")
		if err != nil {
			return nil, noop, err
		}
		cleanup := func() { _ = os.Remove(f.Name()) }
		if _, err := f.WriteString(strings.TrimSpace(hostKey) + "\n"); err != nil {
			_ = f.Close()
			cleanup()
			return nil, noop, err
		}
		if err := f.Close(); err != nil {
			cleanup()
			return nil, noop, err
		}
		return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=" + sshConnectTimeout,
			"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + f.Name(),
			"-o", "GlobalKnownHostsFile=/dev/null"}, cleanup, nil
	default:
		return nil, noop, nil
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
