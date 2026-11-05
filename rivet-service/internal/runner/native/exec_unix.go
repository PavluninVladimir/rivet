//go:build unix

package native

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Инструмент run (design, решение 6): sh -c в корне рабочей копии,
// окружение собирается заново без ключей и токенов, HOME — временный
// каталог сессии. После команды не остаётся её процессов: таймаут бьёт
// по всей группе, а по завершении добиваются и фоновые потомки, в том
// числе ушедшие в свою сессию.
//
// Режимы запуска (launchMode):
//   - sandbox (Linux с user namespaces): команда в своих user, mount, net
//     и pid namespace — без сети, loopback поднят, кеши зависимостей
//     смонтированы только на чтение, привилегии сброшены; при выходе
//     команды ядро завершает всё её дерево процессов;
//   - reaper (Linux без user namespaces): помощник-subreaper, к которому
//     переходят осиротевшие потомки, после команды завершает всё дерево;
//     сеть у команды есть;
//   - direct (macOS и прочие): группа процессов плюс опрос потомков; сеть
//     у команды есть.

type commandSpec struct {
	dir, command string
	timeout      time.Duration
	home, path   string
	isolate      bool
	maxOut       int
	killGrace    time.Duration
	// readOnly — кеши зависимостей runner'а для команды (только чтение
	// в режиме sandbox).
	readOnly []CacheDir
	// hidePaths — каталоги и файлы runner'а, скрытые от команды в песочнице
	// (его $HOME и файлы секретов).
	hidePaths []string
	// extraEnv — переменные runner'а сверх белого списка.
	extraEnv []string
	// requireSandbox — без песочницы команду не запускать (ErrNoSandbox),
	// а не откатываться в режим reaper или direct.
	requireSandbox bool
	// onOutput — поток вывода по мере поступления (транскрипт проверок).
	onOutput func([]byte)
	// network — в песочнице оставить сеть хоста: без net namespace, всё
	// остальное (скрытые $HOME и файлы секретов, кеши только на чтение,
	// свой pid namespace) как обычно. Только проверки проекта у runner'а
	// с -checks-network; команды агента сеть не получают.
	network bool
}

type commandResult struct {
	output   string
	exitCode int
	timedOut bool
	// leftovers — сколько фоновых процессов команды пришлось завершить
	// после её выхода.
	leftovers int
	// isolated — команда выполнялась без сети (режим sandbox без network).
	isolated bool
	// sandboxed — команда выполнялась в песочнице (режим sandbox, с сетью
	// или без): $HOME runner'а и файлы секретов ей не видны.
	sandboxed bool
	err       error
}

type launchMode int

const (
	modeDirect launchMode = iota
	modeReaper
	modeSandbox
)

// commandEnv — окружение команды: только разрешённые переменные, кеши
// зависимостей runner'а и бинарь runner'а (CacheDir с файлом).
func commandEnv(home, path string, caches []CacheDir) []string {
	if path == "" {
		path = os.Getenv("PATH")
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"TMPDIR=" + home,
		"LANG=C.UTF-8",
		"TERM=dumb",
		"CI=1",
		"NO_COLOR=1",
		"GIT_TERMINAL_PROMPT=0",
		// Кеш сборки Go пишется в HOME сессии, кеш модулей — общий.
		"GOCACHE=" + filepath.Join(home, ".cache", "go-build"),
	}
	for _, c := range caches {
		if c.Env == "" || c.Dir == "" {
			continue
		}
		if st, err := os.Stat(c.Dir); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
			env = append(env, c.Env+"="+c.Dir)
		}
	}
	return env
}

// launch — платформенная сборка процесса команды (exec_linux.go,
// exec_bsd.go). report — канал отчёта помощника (nil в режиме direct).
type launched struct {
	cmd    *exec.Cmd
	mode   launchMode
	report *os.File
	close  func()
}

func runCommand(ctx context.Context, spec commandSpec) commandResult {
	env := append(commandEnv(spec.home, spec.path, spec.readOnly), spec.extraEnv...)
	out := newHeadTail(spec.maxOut)
	l, err := launch(spec, env)
	if err != nil {
		return commandResult{err: err}
	}
	defer l.close()
	cmd := l.cmd
	// Вывод — через свой pipe, а не через копирование внутри exec.Cmd:
	// Wait возвращается сразу по выходу команды, даже если фоновый потомок
	// держит pipe, и фоновые процессы добиваются, пока они ещё живы.
	pr, pw, err := os.Pipe()
	if err != nil {
		return commandResult{err: err}
	}
	defer func() { _ = pr.Close() }()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return commandResult{err: fmt.Errorf("запуск команды: %w", err)}
	}
	_ = pw.Close()
	if l.report != nil {
		// Конец записи отчёта нужен только помощнику.
		for _, f := range cmd.ExtraFiles {
			_ = f.Close()
		}
	}
	var sink io.Writer = out
	if spec.onOutput != nil {
		sink = io.MultiWriter(out, outputFunc(spec.onOutput))
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(sink, pr)
		close(copied)
	}()
	var trk *tracker
	if l.mode == modeDirect {
		trk = trackDescendants(cmd.Process.Pid, 100*time.Millisecond)
	}
	rep := readReport(l.report)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Таймаут 0 — без предела: команду останавливает только ctx
	// (проверки проекта ограничены временем стадии).
	var expired <-chan time.Time
	if spec.timeout > 0 {
		timer := time.NewTimer(spec.timeout)
		defer timer.Stop()
		expired = timer.C
	}
	var waitErr error
	timedOut, cancelled := false, false
	select {
	case waitErr = <-done:
	case <-expired:
		timedOut = true
		waitErr = stopGroup(cmd.Process.Pid, done, spec.killGrace)
	case <-ctx.Done():
		cancelled = true
		waitErr = stopGroup(cmd.Process.Pid, done, spec.killGrace)
	}
	res := commandResult{timedOut: timedOut, isolated: l.mode == modeSandbox && !spec.network, sandboxed: l.mode == modeSandbox}
	// Выжившие потомки: группа и (в режиме direct) отслеженные процессы.
	if trk != nil {
		for _, k := range trk.finish() {
			if syscall.Kill(k.pid, syscall.SIGKILL) == nil {
				res.leftovers++
			}
		}
	}
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) == nil && l.mode == modeDirect && res.leftovers == 0 {
		res.leftovers = 1
	}
	r := <-rep
	res.leftovers += r.leftovers
	// Остаток вывода: pipe закрывается, когда умирают все его держатели;
	// процесс, которого убить не удалось, не задерживает результат.
	select {
	case <-copied:
	case <-time.After(time.Second):
		_ = pr.Close()
		<-copied
	}
	res.output = out.String()
	if r.err != "" {
		res.err = errors.New(r.err)
		return res
	}
	if cancelled {
		res.err = ctx.Err()
		return res
	}
	if timedOut {
		res.exitCode = -1
		return res
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.exitCode = exitErr.ExitCode()
		if res.exitCode < 0 {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.exitCode = 128 + int(ws.Signal())
			}
		}
	default:
		res.err = waitErr
	}
	return res
}

// stopGroup — SIGTERM группе процессов, через паузу SIGKILL; ждёт Wait.
func stopGroup(pid int, done <-chan error, grace time.Duration) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-done
	}
}

// helperReport — отчёт помощника: число завершённых фоновых процессов и
// ошибка подготовки песочницы.
type helperReport struct {
	leftovers int
	err       string
}

func readReport(f *os.File) <-chan helperReport {
	ch := make(chan helperReport, 1)
	if f == nil {
		ch <- helperReport{}
		return ch
	}
	go func() {
		var r helperReport
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), "=")
			switch k {
			case "leftovers":
				r.leftovers, _ = strconv.Atoi(v)
			case "error":
				r.err = v
			}
		}
		ch <- r
	}()
	return ch
}

// outputFunc — поток вывода команды в функцию вызывающего. Буфер io.Copy
// переиспользуется, поэтому функция получает копию.
type outputFunc func([]byte)

func (f outputFunc) Write(p []byte) (int, error) {
	f(append([]byte(nil), p...))
	return len(p), nil
}

// headTail — вывод команды с потолком: сохраняются начало и конец, середина
// заменяется пометкой с числом пропущенных байт.
type headTail struct {
	mu    sync.Mutex
	max   int
	head  []byte
	tail  []byte
	total int
}

func newHeadTail(max int) *headTail { return &headTail{max: max} }

func (h *headTail) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	written := len(p)
	h.total += written
	half := h.max / 2
	if room := half - len(h.head); room > 0 {
		n := min(room, len(p))
		h.head = append(h.head, p[:n]...)
		p = p[n:]
	}
	if len(p) > 0 {
		h.tail = append(h.tail, p...)
		if len(h.tail) > h.max-half {
			h.tail = h.tail[len(h.tail)-(h.max-half):]
		}
	}
	return written, nil
}

func (h *headTail) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := len(h.head) + len(h.tail)
	if h.total <= kept {
		return strings.ToValidUTF8(string(h.head)+string(h.tail), "")
	}
	return strings.ToValidUTF8(string(h.head), "") +
		fmt.Sprintf("\n… [вывод обрезан: пропущено %d байт из %d] …\n", h.total-kept, h.total) +
		strings.ToValidUTF8(string(h.tail), "")
}
