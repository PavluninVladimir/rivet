package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Исполнение деплой-джобы (спека backend/deployment, тип «Linux-хост»).
// Команды окружения — shell-скрипт с переменными RIVET_*; по ssh на host
// или локально при пустом host (e2e-стенд, деплой «на себя»).

const (
	defaultDeployTimeout = 30 * time.Minute
	verifyURLAttempts    = 10
	verifyURLInterval    = 3 * time.Second
	verifyURLTimeout     = 10 * time.Second
)

func (a *agent) executeDeploy(ctx context.Context, job *pb.DeployJob, emit func(*pb.RunnerMsg)) {
	timeout := defaultDeployTimeout
	if job.TimeoutS > 0 {
		timeout = time.Duration(job.TimeoutS) * time.Second
	}
	// Жёсткий дедлайн джобы: зависший ssh/docker не держит runner вечно.
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transcript := func(data []byte) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Transcript{
			Transcript: &pb.TranscriptChunk{DeployId: job.DeploymentId, Data: data}}})
	}
	result := func(stage pb.DeployResult_Stage, ok bool, detail string) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_DeployResult{
			DeployResult: &pb.DeployResult{DeploymentId: job.DeploymentId,
				Stage: stage, Ok: ok, Detail: tail(detail, 8000), Rollback: job.Rollback}}})
	}

	step := "деплой версии " + job.Version
	if job.Rollback {
		step = "откат к версии " + job.Version
	}
	transcript([]byte(fmt.Sprintf("== %s: %s ==\n", job.EnvName, step)))

	// Доставка файлами репозитория (манифесты Kubernetes, helm-чарт)
	// исполняется в рабочей копии на версии публикации: пути в командах
	// относительные от корня репозитория (протокол v10).
	dir := a.cfg.Workdir
	if job.Checkout {
		var err error
		dir, err = a.deployWorkspace(dctx, job, transcript)
		if err != nil {
			result(pb.DeployResult_DEPLOY, false, "рабочая копия: "+err.Error())
			return
		}
	}

	out, err := a.runDeployCmd(dctx, job, dir, job.DeployCmd, transcript)
	if err != nil {
		result(pb.DeployResult_DEPLOY, false, fmt.Sprintf("deploy: %v\n%s", err, out))
		return
	}
	result(pb.DeployResult_DEPLOY, true, "")

	if job.VerifyCmd != "" {
		transcript([]byte("== verify: команда ==\n"))
		if out, err := a.runDeployCmd(dctx, job, dir, job.VerifyCmd, transcript); err != nil {
			result(pb.DeployResult_VERIFY, false, fmt.Sprintf("verify: %v\n%s", err, out))
			return
		}
	}
	if job.VerifyUrl != "" {
		transcript([]byte("== verify: " + job.VerifyUrl + " ==\n"))
		if err := verifyURL(dctx, job.VerifyUrl, transcript); err != nil {
			result(pb.DeployResult_VERIFY, false, "verify: "+err.Error())
			return
		}
	}
	result(pb.DeployResult_VERIFY, true, "")
}

// runDeployCmd исполняет скрипт с переменными RIVET_* локально или по ssh.
// Host передаётся отдельным аргументом exec (никакой shell-склейки; формат
// провалидирован на control plane, здесь — последний рубеж).
func (a *agent) runDeployCmd(ctx context.Context, job *pb.DeployJob, dir, script string, transcript func([]byte)) (string, error) {
	rivetEnv := map[string]string{
		"RIVET_REPO":         job.Repo,
		"RIVET_VERSION":      job.Version,
		"RIVET_PREV_VERSION": job.PrevVersion,
		"RIVET_ENV":          job.EnvName,
	}
	var cmd *exec.Cmd
	if job.Host == "" {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", script)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for k, v := range rivetEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	} else {
		if strings.HasPrefix(job.Host, "-") {
			return "", fmt.Errorf("недопустимый host %q", job.Host)
		}
		// env-переменные уезжают внутри скрипта: ssh чужое окружение не передаёт.
		var b strings.Builder
		for k, v := range rivetEnv {
			fmt.Fprintf(&b, "export %s=%s; ", k, shq(v))
		}
		b.WriteString(script)
		cmd = exec.CommandContext(ctx, "ssh", job.Host, b.String())
	}
	// Вывод стримится чанками по мере появления (live-прогресс, никакого
	// накопления всего вывода в памяти); для detail хранится только хвост.
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return "", err
	}
	done := make(chan struct{})
	var tailBuf []byte
	go func() {
		defer close(done)
		buf := make([]byte, 16<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				if transcript != nil {
					transcript(append([]byte(nil), buf[:n]...))
				}
				tailBuf = append(tailBuf, buf[:n]...)
				if len(tailBuf) > 8000 {
					tailBuf = tailBuf[len(tailBuf)-8000:]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	err := cmd.Wait()
	_ = pw.Close()
	<-done
	_ = pr.Close()
	return string(tailBuf), err
}

// shq — одинарные shell-кавычки для значения переменной.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// verifyURL — health-check по URL: 2xx = успех, ретраи с интервалом,
// редиректы не следуются, тело читается с лимитом (только для лога).
func verifyURL(ctx context.Context, url string, transcript func([]byte)) error {
	client := &http.Client{
		Timeout: verifyURLTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var lastErr error
	for i := 0; i < verifyURLAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(verifyURLInterval):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			transcript([]byte(fmt.Sprintf("health-check %d за %d попыток\n", resp.StatusCode, i+1)))
			return nil
		}
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, tail(string(body), 500))
	}
	return fmt.Errorf("health-check не прошёл за %d попыток: %w", verifyURLAttempts, lastErr)
}

// deployWorkspace — клон репозитория проекта на версии публикации: у
// доставки манифестами и чартом пути относительные от корня репозитория,
// и применять надо ровно ту версию, которую публикуем.
func (a *agent) deployWorkspace(ctx context.Context, job *pb.DeployJob, transcript func([]byte)) (string, error) {
	dir := a.cfg.Workdir + "/deploy/" + deployKey(job)
	env, cleanup, err := deployGitCredentials(a.cfg.Workdir, job)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := os.Stat(dir + "/.git"); err != nil {
		url := job.RepoUrl
		if url == "" {
			url = cloneBase(a.cfg.GitBase) + job.Repo + ".git"
		} else {
			url += ".git"
		}
		transcript([]byte("== клонирование " + job.Repo + " ==\n"))
		cmd := exec.CommandContext(ctx, "git", "clone", "--", url, dir)
		cmd.Dir = a.cfg.Workdir
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone: %v: %s", err, out)
		}
	}
	// Версия публикации — sha коммита: fetch и жёсткий checkout на него.
	if err := validVersion(job.Version); err != nil {
		return "", err
	}
	// Дерево приводится к версии публикации целиком: клон переиспользуется
	// между публикациями, и остатки прошлой (в том числе неотслеживаемые
	// файлы) не должны уехать в kubectl apply.
	script := "git fetch origin && git checkout --detach --force " + shq(job.Version) +
		" && git reset --hard " + shq(job.Version) + " && git clean -fdx"
	out, err := runShellEnv(ctx, dir, script, env, nil)
	if err != nil {
		return "", fmt.Errorf("checkout %s: %v: %s", job.Version, err, out)
	}
	return dir, nil
}

// deployKey — каталог клона по полной идентичности репозитория и
// окружению: один и тот же owner/name встречается на разных инстансах, и
// общий каталог применил бы чужие манифесты.
func deployKey(job *pb.DeployJob) string {
	id := job.Repo
	if job.RepoUrl != "" {
		id = strings.TrimPrefix(strings.TrimPrefix(job.RepoUrl, "https://"), "http://")
	}
	var b strings.Builder
	for _, r := range id + "-" + job.EnvName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// validVersion — версия публикации как безопасный аргумент git.
func validVersion(v string) error {
	if v == "" {
		return fmt.Errorf("пустая версия публикации")
	}
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("версия %q начинается с дефиса", v)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/':
		default:
			return fmt.Errorf("недопустимый символ %q в версии %q", r, v)
		}
	}
	return nil
}

// cloneBase — префикс адреса клонирования (e2e-стенды передают file://).
func cloneBase(gitBase string) string {
	if gitBase == "" {
		return "https://github.com/"
	}
	return gitBase
}

// deployGitCredentials — askpass-хелпер для клона приватного репозитория:
// токен уходит в git через окружение, а не в аргументы команды.
func deployGitCredentials(workdir string, job *pb.DeployJob) ([]string, func(), error) {
	if job.GitToken == "" {
		return nil, func() {}, nil
	}
	f, err := os.CreateTemp(workdir, "deploy-askpass-*.sh")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	script := "#!/bin/sh\n" +
		`case "$1" in *Username*) echo "$RIVET_GIT_USER";; *) echo "$RIVET_GIT_TOKEN";; esac` + "\n"
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		cleanup()
		return nil, func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return []string{
		"GIT_ASKPASS=" + f.Name(),
		"GIT_TERMINAL_PROMPT=0",
		"RIVET_GIT_USER=rivet",
		"RIVET_GIT_TOKEN=" + job.GitToken,
	}, cleanup, nil
}
