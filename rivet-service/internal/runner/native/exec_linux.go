//go:build linux

package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Помощник команд на Linux: runner запускает сам себя с аргументом
// helperArg, и помощник (а не runner) порождает sh. Так у дерева процессов
// команды есть свой «корень», который знает всех потомков:
//
//   - режим sandbox: помощник — PID 1 в новых user, mount, net и pid
//     namespace. Он поднимает loopback, монтирует кеши зависимостей только
//     на чтение, по возможности монтирует свой /proc (в нём видны только
//     процессы команды), сбрасывает привилегии и запускает sh. Когда
//     помощник выходит, ядро завершает все процессы namespace;
//   - режим reaper: помощник — subreaper без namespace: осиротевшие
//     потомки переходят к нему, и после выхода sh он завершает всё дерево.
//
// Песочницу runner поднимает не сам, а через промежуточный exec
// (sandbox-exec). Процесс runner'а недампируемый (PR_SET_DUMPABLE=0, см.
// cmd/rivet-runner), а Go при CLONE_NEWUSER пишет uid_map и gid_map
// потомка из родителя: у потомка до exec /proc/<pid> принадлежит root, и
// запись падает. Промежуточный процесс — свежий execve бинарника runner'а
// без namespace'ов и без секретов (окружение из белого списка, параметры
// через stdin), у него dumpable снова 1, и namespace'ы создаёт уже он.
// Промежуточный процесс остаётся в группе процессов команды и ждёт
// помощника, поэтому сигналы runner'а группе доходят до всего дерева.
//
// Диспетчеризация — в init пакета: любой бинарник, который включает этот
// пакет (rivet-runner, тесты), умеет быть помощником. Параметры помощник
// читает из stdin, отчёт пишет в fd 3.

const (
	helperArg = "__rivet_native_command__"
	helperEnv = "RIVET_NATIVE_HELPER=1"
)

type helperSpec struct {
	Mode     string   `json:"mode"` // sandbox-exec | sandbox | reaper
	Dir      string   `json:"dir"`
	Command  string   `json:"command"`
	Env      []string `json:"env"`
	ReadOnly []string `json:"read_only,omitempty"`
	Hide     []string `json:"hide,omitempty"`
	// Keep — каталоги, которые нужно сохранить доступными для записи
	// поверх скрытого $HOME: рабочая копия и временный HOME сессии (они
	// могут лежать под домашним каталогом runner'а).
	Keep []string `json:"keep,omitempty"`
	// GraceMS — сколько ждать команду после SIGTERM, прежде чем добить.
	GraceMS int64 `json:"grace_ms"`
	// Network — песочница без своего net namespace: у команды сеть хоста
	// (проверки проекта у runner'а с -checks-network).
	Network bool `json:"network,omitempty"`
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == helperArg && os.Getenv("RIVET_NATIVE_HELPER") == "1" {
		os.Exit(helperMain())
	}
}

var (
	sandboxOnce sync.Once
	sandboxOK   bool
	sandboxWhy  string
)

// NetworkIsolationAvailable — можно ли выполнять команды агента в
// песочнице без сети (user namespaces доступны без привилегий): пробный
// запуск помощника в режиме sandbox.
func NetworkIsolationAvailable() bool {
	sandboxOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rivet-sandbox-probe-*")
		if err != nil {
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		res := runSandboxProbe(dir)
		sandboxOK = res.err == nil && res.exitCode == 0 && res.isolated
		if res.err != nil {
			sandboxWhy = res.err.Error()
		}
	})
	return sandboxOK
}

// SandboxUnavailableReason — почему песочница недоступна (пусто, если
// доступна или не проверялась): для журнала runner'а.
func SandboxUnavailableReason() string {
	NetworkIsolationAvailable()
	return sandboxWhy
}

func runSandboxProbe(dir string) commandResult {
	l, err := helperCommand(commandSpec{dir: dir, command: "exit 0"}, commandEnv(dir, "", nil), modeSandbox)
	if err != nil {
		return commandResult{err: err}
	}
	defer l.close()
	out := newHeadTail(4096)
	l.cmd.Stdout, l.cmd.Stderr = out, out
	if err := l.cmd.Start(); err != nil {
		return commandResult{err: err}
	}
	for _, f := range l.cmd.ExtraFiles {
		_ = f.Close()
	}
	rep := readReport(l.report)
	done := make(chan error, 1)
	go func() { done <- l.cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
		werr = <-done
	}
	r := <-rep
	if r.err != "" {
		return commandResult{err: errors.New(r.err)}
	}
	if werr != nil {
		return commandResult{err: werr}
	}
	return commandResult{isolated: true}
}

// launch — помощник в режиме sandbox (если просили изоляцию и она есть)
// или reaper; если запустить себя нельзя — sh напрямую (direct). С
// requireSandbox отката нет: без песочницы команда не запускается.
func launch(spec commandSpec, env []string) (launched, error) {
	if (spec.isolate || spec.requireSandbox) && NetworkIsolationAvailable() {
		l, err := helperCommand(spec, env, modeSandbox)
		if err == nil {
			return l, nil
		}
		if spec.requireSandbox {
			return launched{}, fmt.Errorf("%w: %v", ErrNoSandbox, err)
		}
	}
	if spec.requireSandbox {
		return launched{}, fmt.Errorf("%w: %s", ErrNoSandbox, SandboxUnavailableReason())
	}
	if l, err := helperCommand(spec, env, modeReaper); err == nil {
		return l, nil
	}
	bin, err := exec.LookPath("sh")
	if err != nil {
		return launched{}, fmt.Errorf("оболочка недоступна: %w", err)
	}
	cmd := exec.Command(bin, "-c", spec.command)
	cmd.Dir = spec.dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return launched{cmd: cmd, mode: modeDirect, close: func() {}}, nil
}

func helperCommand(spec commandSpec, env []string, mode launchMode) (launched, error) {
	self, err := os.Executable()
	if err != nil {
		return launched{}, err
	}
	hs := helperSpec{Mode: "reaper", Dir: spec.dir, Command: spec.command, Env: env, GraceMS: spec.killGrace.Milliseconds()}
	if mode == modeSandbox {
		hs.Mode = "sandbox-exec"
		hs.Network = spec.network
		for _, c := range spec.readOnly {
			if cacheExists(c.Dir) {
				hs.ReadOnly = append(hs.ReadOnly, c.Dir)
			}
		}
		// Пути абсолютные от каталога runner'а: помощник работает в
		// рабочей копии и относительный путь искал бы там.
		for _, h := range spec.hidePaths {
			if abs, err := filepath.Abs(h); err == nil {
				h = abs
			}
			hs.Hide = append(hs.Hide, h)
		}
		for _, d := range []string{spec.dir, spec.home} {
			if d != "" {
				hs.Keep = append(hs.Keep, d)
			}
		}
	}
	raw, err := json.Marshal(hs)
	if err != nil {
		return launched{}, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return launched{}, err
	}
	cmd := exec.Command(self, helperArg)
	cmd.Dir = spec.dir
	// Окружение помощника — то же урезанное окружение команды плюс метка.
	cmd.Env = append(append([]string{}, env...), helperEnv)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.ExtraFiles = []*os.File{pw}
	// Namespace'ы здесь не создаются даже для песочницы: их создаёт
	// промежуточный процесс (sandbox-exec), см. комментарий в начале файла.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return launched{cmd: cmd, mode: mode, report: pr, close: func() {
		_ = pr.Close()
		_ = pw.Close()
	}}, nil
}

// sandboxAttr — атрибуты помощника песочницы: новые user, mount, net, pid
// и ipc namespace с отображением своего uid. network — без net namespace
// (сеть хоста, -checks-network). Группа процессов прежняя: сигналы
// runner'а группе доходят и до него.
func sandboxAttr(network bool) *syscall.SysProcAttr {
	uid, gid := os.Getuid(), os.Getgid()
	flags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC)
	// uid внутри тот же, что снаружи, поэтому после exec возможности
	// namespace пропали бы: помощнику их оставляют ambient-набором —
	// монтирование, loopback и сброс привилегий. Сам он их сбрасывает до
	// запуска команды.
	caps := []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_NET_ADMIN, unix.CAP_SETPCAP}
	if network {
		flags &^= syscall.CLONE_NEWNET
		caps = []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SETPCAP}
	}
	return &syscall.SysProcAttr{
		Pdeathsig:                  syscall.SIGKILL,
		Cloneflags:                 flags,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                caps,
	}
}

// sandboxExecMain — промежуточный процесс: запускает помощника песочницы
// в новых namespace'ах, ждёт его и отдаёт его код выхода. Остановкой
// команды занимается помощник (он получает те же сигналы группы), поэтому
// здесь сигналы только принимаются, чтобы не выйти раньше помощника.
func sandboxExecMain(hs helperSpec, report *os.File, fail func(string, ...any) int) int {
	runtime.LockOSThread() // Pdeathsig помощника привязан к этому потоку
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	self, err := os.Executable()
	if err != nil {
		return fail("песочница: %v", err)
	}
	hs.Mode = "sandbox"
	raw, err := json.Marshal(hs)
	if err != nil {
		return fail("песочница: %v", err)
	}
	cmd := exec.Command(self, helperArg)
	cmd.Dir = hs.Dir
	cmd.Env = os.Environ() // белый список команды и метка помощника
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{report}
	cmd.SysProcAttr = sandboxAttr(hs.Network)
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	case errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EINVAL):
		return fail("ядро не даёт создать user namespace (seccomp, AppArmor или лимит user.max_user_namespaces): %v", err)
	default:
		return fail("песочница: %v", err)
	}
}

// ─── помощник ────────────────────────────────────────────────────────────

// helperSpecLimit — предел параметров помощника на stdin.
const helperSpecLimit = 16 << 20

func helperMain() int {
	// Канал отчёта не должен достаться команде: иначе она могла бы
	// подделать отчёт или держать его открытым.
	syscall.CloseOnExec(3)
	report := os.NewFile(3, "report")
	fail := func(format string, a ...any) int {
		if report != nil {
			fmt.Fprintf(report, "error=%s\n", strings.ReplaceAll(fmt.Sprintf(format, a...), "\n", " "))
		}
		return 125
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, helperSpecLimit))
	if err != nil {
		return fail("помощник: %v", err)
	}
	var hs helperSpec
	if err := json.Unmarshal(raw, &hs); err != nil {
		return fail("помощник: %v", err)
	}
	if hs.Mode == "sandbox-exec" {
		return sandboxExecMain(hs, report, fail)
	}
	// Привилегии и fork — на одном потоке: сброс возможностей действует
	// на поток, а sh наследует права потока, который его порождает.
	runtime.LockOSThread()
	signal.Ignore(syscall.SIGHUP)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	sandbox := hs.Mode == "sandbox"
	if sandbox {
		if err := setupSandbox(hs.Hide, hs.Keep, hs.ReadOnly, hs.Network); err != nil {
			return fail("песочница: %v", err)
		}
	} else {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			return fail("subreaper: %v", err)
		}
	}
	if err := dropPrivileges(); err != nil {
		return fail("сброс привилегий: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", hs.Command)
	cmd.Dir = hs.Dir
	cmd.Env = hs.Env
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fail("запуск команды: %v", err)
	}
	shPid := cmd.Process.Pid
	exited := make(chan struct{})
	code := 0
	go func() {
		_ = cmd.Wait()
		if ps := cmd.ProcessState; ps != nil {
			code = ps.ExitCode()
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		}
		close(exited)
	}()
	select {
	case <-exited:
	case <-stop:
		// runner останавливает команду: SIGTERM всему дереву (включая
		// ушедших из группы), через половину паузы — SIGKILL. Детей здесь
		// не забираем, чтобы не перехватить статус sh у cmd.Wait.
		signalTree(sandbox, unix.SIGTERM)
		select {
		case <-exited:
		case <-time.After(time.Duration(hs.GraceMS) * time.Millisecond / 2):
			signalTree(sandbox, unix.SIGKILL)
			<-exited
		}
	}
	n := killTree(sandbox, shPid)
	if report != nil {
		fmt.Fprintf(report, "leftovers=%d\n", n)
	}
	return code
}

// signalTree посылает сигнал всем потомкам помощника.
func signalTree(sandbox bool, sig unix.Signal) {
	if sandbox {
		_ = unix.Kill(-1, sig)
		return
	}
	for _, pid := range descendantsOf(os.Getpid()) {
		_ = unix.Kill(pid, sig)
	}
}

// killTree завершает всех потомков помощника и забирает их; возвращает
// число завершённых процессов (кроме sh).
func killTree(sandbox bool, shPid int) int {
	killed := 0
	for i := 0; i < 100; i++ {
		var targets []int
		if sandbox {
			// PID 1 namespace: kill(-1) достаёт все процессы namespace.
			if unix.Kill(-1, 0) == nil {
				_ = unix.Kill(-1, unix.SIGKILL)
				targets = []int{-1}
			}
		} else {
			targets = descendantsOf(os.Getpid())
			for _, pid := range targets {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
		reaped := reapAll(shPid)
		killed += reaped
		if len(targets) == 0 && reaped == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return killed
}

// reapAll забирает завершившихся детей (кроме sh, его ждёт cmd.Wait).
func reapAll(shPid int) int {
	n := 0
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return n
		}
		if pid != shPid {
			n++
		}
	}
}

// descendantsOf — все потомки pid по /proc (родитель и время старта).
func descendantsOf(pid int) []int {
	parents := map[int]bool{pid: true}
	procs := snapshotProcs()
	var out []int
	for changed := true; changed; {
		changed = false
		for _, p := range procs {
			if parents[p.ppid] && !parents[p.pid] {
				parents[p.pid] = true
				out = append(out, p.pid)
				changed = true
			}
		}
	}
	return out
}

// setupSandbox — подготовка namespace: приватные монтирования, скрытие
// файлов runner'а (home и секреты), кеши только на чтение, свой /proc
// (если ядро позволяет), loopback. network — net namespace не создавался
// (сеть хоста), loopback поднимать не нужно.
//
// Команда агента идёт под тем же uid, что и runner, поэтому в mount
// namespace её каталог $HOME runner'а закрывается пустым tmpfs, а файлы
// секретов (файл токена) — поверх. Кеши зависимостей, которые лежат под
// $HOME, сохраняются: их поддеревья снимаются до скрытия (OpenTree) и
// возвращаются на место только на чтение. Так же, одним файлом, на место
// возвращается бинарь runner'а (RIVET_RUNNER_BIN): соседние файлы его
// каталога под скрытым $HOME остаются скрытыми.
func setupSandbox(hide, keep, readOnly []string, network bool) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("приватные монтирования: %w", err)
	}
	// Один и тот же каталог не монтируем дважды (рабочая копия и HOME
	// сессии могут совпадать), иначе его клон-дескриптор закроется после
	// первого MoveMount. Кеш, попавший и в keep, остаётся доступным для
	// записи (keep важнее).
	keep = uniqueStrings(keep)
	readOnly = uniqueStrings(readOnly, keep...)
	// Снимаем кеши и сохраняемые каталоги заранее: после tmpfs над $HOME
	// их исходный путь уже не доступен, а клон поддерева помнит содержимое.
	pins := map[string]pinned{}
	pin := func(dir string) {
		if _, ok := pins[dir]; ok {
			return
		}
		st, err := os.Stat(dir)
		if err != nil || (!st.IsDir() && !st.Mode().IsRegular()) {
			return
		}
		if fd, err := unix.OpenTree(unix.AT_FDCWD, dir, uint(unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_RECURSIVE)); err == nil {
			pins[dir] = pinned{fd: fd, file: !st.IsDir()}
		}
	}
	for _, dir := range keep {
		pin(dir)
	}
	for _, dir := range readOnly {
		pin(dir)
	}
	for _, dir := range hide {
		if err := hideDir(dir); err != nil {
			for _, p := range pins {
				_ = unix.Close(p.fd)
			}
			return fmt.Errorf("скрытие %s: %w", dir, err)
		}
	}
	// Рабочая копия и HOME сессии возвращаются доступными для записи,
	// кеши — только на чтение.
	for _, dir := range keep {
		if err := placeMount(dir, pins[dir], false); err != nil {
			return fmt.Errorf("каталог %s: %w", dir, err)
		}
	}
	for _, dir := range readOnly {
		if err := placeMount(dir, pins[dir], true); err != nil {
			return fmt.Errorf("кеш %s только на чтение: %w", dir, err)
		}
	}
	// Файлы секретов, оставшиеся вне скрытого $HOME, закрываем пустышкой.
	for _, p := range hide {
		_ = hideFileIfExists(p)
	}
	// Свой /proc показывает только процессы команды. В контейнерах с
	// закрытыми частями /proc ядро не даёт смонтировать новый — тогда
	// остаётся прежний, это не ошибка.
	_ = unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	if network {
		return nil
	}
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	return nil
}

// uniqueStrings — список без повторов и без значений из exclude, порядок
// сохраняется.
func uniqueStrings(in []string, exclude ...string) []string {
	seen := map[string]bool{}
	for _, e := range exclude {
		seen[e] = true
	}
	var out []string
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// hideDir закрывает каталог пустым tmpfs (если каталог есть); файл по
// этому пути закрывается отдельно (hideFileIfExists).
func hideDir(path string) error {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !st.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	return unix.Mount("tmpfs", path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700")
}

// hideFileIfExists закрывает файл пустышкой (bind /dev/null): его
// содержимое команде недоступно. Нет файла — ничего не делает (возможно,
// он уже скрыт tmpfs над родительским каталогом).
func hideFileIfExists(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil
	}
	return unix.Mount("/dev/null", path, "", unix.MS_BIND, "")
}

// pinned — снятое до скрытия $HOME поддерево: дескриптор клона и признак
// файла (бинарь runner'а), а не каталога.
type pinned struct {
	fd   int
	file bool
}

// cacheExists — путь кеша или бинаря есть: каталог или обычный файл.
func cacheExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}

// placeMount возвращает снятое поддерево на место (или заново делает bind);
// readOnly — перемонтировать только на чтение (кеши, бинарь runner'а) или
// оставить доступным для записи (рабочая копия, HOME сессии).
func placeMount(dir string, p pinned, readOnly bool) error {
	if p.fd > 0 {
		defer func() { _ = unix.Close(p.fd) }()
		if err := mountPoint(dir, p.file); err != nil {
			return err
		}
		if err := unix.MoveMount(p.fd, "", unix.AT_FDCWD, dir, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			return err
		}
		if !readOnly {
			return nil
		}
		return remountReadOnly(dir)
	}
	if readOnly {
		return bindReadOnly(dir)
	}
	// Без клона каталог остаётся как есть (он не под скрытым $HOME).
	return nil
}

// mountPoint — точка монтирования под скрытым $HOME: каталог или пустой
// файл с промежуточными каталогами.
func mountPoint(path string, file bool) error {
	if !file {
		return os.MkdirAll(path, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func bindReadOnly(dir string) error {
	if err := unix.Mount(dir, dir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	return remountReadOnly(dir)
}

// remountReadOnly — bind-монтирование только на чтение. Перемонтирование
// должно сохранить заблокированные флаги исходного монтирования (nosuid,
// nodev, noexec), иначе ядро откажет.
func remountReadOnly(dir string) error {
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	err := unix.Mount("", dir, "", flags, "")
	if err == nil {
		return nil
	}
	var st unix.Statfs_t
	if serr := unix.Statfs(dir, &st); serr == nil {
		if st.Flags&unix.ST_NOSUID != 0 {
			flags |= unix.MS_NOSUID
		}
		if st.Flags&unix.ST_NODEV != 0 {
			flags |= unix.MS_NODEV
		}
		if st.Flags&unix.ST_NOEXEC != 0 {
			flags |= unix.MS_NOEXEC
		}
		return unix.Mount("", dir, "", flags, "")
	}
	return err
}

// loopbackUp поднимает интерфейс lo в новом net namespace: без него
// connect на 127.0.0.1 падает, а тесты с локальным сервером не работают.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// dropPrivileges — без новых привилегий через exec, без возможностей и
// без особых прав uid 0 (runner, запущенный от root, отображается в root
// namespace — securebits не дают exec вернуть возможности).
func dropPrivileges() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	// SECBIT_NOROOT, SECBIT_NO_SETUID_FIXUP, SECBIT_NO_CAP_AMBIENT_RAISE с
	// блокировками и заблокированный (выключенный) SECBIT_KEEP_CAPS.
	const secbits = 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<5 | 1<<6 | 1<<7
	if err := unix.Prctl(unix.PR_SET_SECUREBITS, secbits, 0, 0, 0); err != nil && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("securebits: %w", err)
	}
	for c := 0; c <= 63; c++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break
			}
			if !errors.Is(err, unix.EPERM) {
				return fmt.Errorf("bounding set: %w", err)
			}
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}
