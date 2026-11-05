package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Скриптовый адаптер — тестовая подмена агента (add-native-agent): в
// продукте есть только агент Rivet (native), а тесты конвейера runner'а
// (стадии, тестировщик, перезапуски, usage) гоняют вместо модели
// shell-команду с маркерами VERDICT:/BLOCKED:/USAGE: — так же, как раньше
// это делала обёртка CLI-агентов. Включается адаптером "wrap" в Config.

// Имена адаптеров, которыми тесты включают скриптовый адаптер.
const (
	AdapterWrap       = "wrap"
	AdapterClaudeCode = "claude-code"
)

func init() {
	testAdapter = func(cfg Config) adapter {
		if cfg.Adapter == AdapterNative {
			return nil
		}
		return &wrapAdapter{cfg: cfg}
	}
}

// wrapAdapter — универсальная CLI-обёртка: команда агента в PTY, разбор
// маркера USAGE: из вывода (минимальная глубина, поведение до этого change).
type wrapAdapter struct {
	cfg Config
}

func (w *wrapAdapter) Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error) {
	pf, err := promptFile(w.cfg.Workdir, prompt)
	if err != nil {
		return agentRun{}, err
	}
	defer pf.cleanup()
	// Модель назначения уходит обёрнутому агенту переменной окружения:
	// что с ней делать, решает команда агента.
	env := []string{"RIVET_PROMPT_FILE=" + pf.path}
	if w.cfg.Model != "" {
		env = append(env, "RIVET_MODEL="+w.cfg.Model)
	}
	// Окружение профиля агента поверх окружения runner'а; аргументы профиля
	// добавляются к команде обёртки.
	env = append(env, w.cfg.ExtraEnv...)
	cmd := w.cfg.AgentCmd
	for _, arg := range w.cfg.ExtraArgs {
		cmd += " " + shellQuote(arg)
	}
	out, err := runPTY(ctx, dir, cmd, env, sink.transcript)
	return agentRun{FinalText: out, Usage: parseUsage(out)}, err
}

type tmpFile struct {
	path    string
	cleanup func()
}

// promptFile кладёт промпт во временный файл: PTY эхоирует stdin в вывод и
// ломает разбор транскрипта, поэтому промпт передаётся путём в окружении.
func promptFile(workdir, prompt string) (tmpFile, error) {
	f, err := createTemp(workdir, "prompt-*.md", prompt)
	if err != nil {
		return tmpFile{}, err
	}
	return f, nil
}

// createTemp — временный файл с содержимым и функцией очистки.
func createTemp(dir, pattern, content string) (tmpFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return tmpFile{}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return tmpFile{}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return tmpFile{}, err
	}
	return tmpFile{path: f.Name(), cleanup: cleanup}, nil
}

// parseUsage разбирает последнюю строку «USAGE: {json}» вывода агента.
// Нет маркера или битый JSON — пустой отчёт (запись уйдёт без токенов).
func parseUsage(out string) usageReport {
	rest, found := lastSentinelLine(out, "USAGE:")
	if !found {
		return usageReport{}
	}
	var r usageReport
	if err := json.Unmarshal([]byte(rest), &r); err != nil {
		slog.Warn("битый USAGE:-отчёт агента — игнорируется", "err", err)
		return usageReport{}
	}
	if r.CtxPct != nil && (*r.CtxPct < 0 || *r.CtxPct > 100) {
		slog.Warn("ctx_pct вне диапазона 0–100 — игнорируется", "ctx_pct", *r.CtxPct)
		r.CtxPct = nil
	}
	return r
}

// ptyDrainTimeout — сколько после выхода команды дочитывается вывод PTY.
// Фоновый процесс агента вне группы (setsid) держит slave открытым, и EOF
// не придёт никогда: master закрывается по таймауту.
const ptyDrainTimeout = 2 * time.Second

// runPTY запускает команду в псевдотерминале: агенты ведут себя как в живом
// терминале, а вывод стримится чанками (live-наблюдение, глубина minimal).
// Промпт передаётся не через stdin (PTY эхоирует ввод в вывод и ломает
// парсинг транскрипта), а окружением — команда получает RIVET_PROMPT_FILE.
func runPTY(ctx context.Context, dir, command string, env []string, transcript func([]byte)) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	// pty.Start делает команду лидером своей сессии и группы процессов.
	// Отмена убивает всю группу: фоновые процессы агента (dev-сервер,
	// watcher, `nohup … &`) иначе пережили бы стадию и держали бы PTY.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = adapterWaitDelay
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		return "", err
	}

	var mu sync.Mutex
	var out strings.Builder
	closed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 16*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				mu.Lock()
				if !closed {
					out.Write(buf[:n])
					if transcript != nil {
						chunk := make([]byte, n)
						copy(chunk, buf[:n])
						transcript(chunk)
					}
				}
				mu.Unlock()
			}
			if err != nil {
				return // EOF, EIO после выхода процесса или закрытие master
			}
		}
	}()
	werr := cmd.Wait()
	// Команда вышла: вывод дочитывается коротко, master закрывается, не
	// дожидаясь EOF (его не будет, пока slave держит процесс вне группы).
	_ = f.SetReadDeadline(time.Now().Add(ptyDrainTimeout))
	t := time.NewTimer(ptyDrainTimeout + 500*time.Millisecond)
	select {
	case <-done:
	case <-t.C:
	}
	t.Stop()
	mu.Lock()
	closed = true
	res := out.String()
	mu.Unlock()
	_ = f.Close()
	return res, werr
}

// adapterWaitDelay — сколько ждём закрытия вывода после выхода процесса
// скриптового агента.
const adapterWaitDelay = 5 * time.Second
