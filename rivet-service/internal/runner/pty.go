package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

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

func newMsgID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
