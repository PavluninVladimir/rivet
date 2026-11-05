//go:build unix

package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func spec(dir, home, command string) commandSpec {
	return commandSpec{dir: dir, command: command, timeout: 10 * time.Second, home: home, maxOut: 32 << 10, killGrace: 300 * time.Millisecond}
}

// Окружение команды (спека «Исполнение команд»): ни ключей подключения, ни
// токенов runner'а и хостинга, HOME — временный каталог сессии.
func TestRunEnvHasNoSecrets(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-native-secret-1")
	t.Setenv("RIVET_RUNNER_TOKEN", "rivet-runner-secret-2")
	t.Setenv("RIVET_PROVIDER_KEY_DEEPSEEK", "sk-native-secret-3")
	t.Setenv("GH_TOKEN", "ghp_native_secret_4")
	dir, home := t.TempDir(), t.TempDir()
	res := runCommand(context.Background(), spec(dir, home, "env"))
	if res.err != nil || res.exitCode != 0 {
		t.Fatalf("env: %+v", res)
	}
	for _, s := range []string{"sk-native-secret", "rivet-runner-secret", "ghp_native_secret", "RIVET_"} {
		if strings.Contains(res.output, s) {
			t.Fatalf("в окружении команды %q:\n%s", s, res.output)
		}
	}
	for _, want := range []string{"HOME=" + home, "CI=1", "TERM=dumb", "PATH="} {
		if !strings.Contains(res.output, want) {
			t.Fatalf("нет %q в окружении:\n%s", want, res.output)
		}
	}
	res = runCommand(context.Background(), spec(dir, home, "pwd; exit 3"))
	if res.exitCode != 3 || !strings.Contains(res.output, filepath.Base(dir)) {
		t.Fatalf("код выхода и каталог: %+v", res)
	}
}

// Зависшая команда: по таймауту завершается вся группа процессов, включая
// фоновых потомков.
func TestRunTimeoutKillsGroup(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	sp := spec(dir, home, "sleep 30 & echo $! > child.pid; echo started; sleep 30")
	sp.timeout = 700 * time.Millisecond
	start := time.Now()
	res := runCommand(context.Background(), sp)
	if !res.timedOut || !strings.Contains(res.output, "started") {
		t.Fatalf("таймаут: %+v", res)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("команда держала %s", el)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("фоновый потомок %d жив после таймаута", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunOutputTruncated(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	res := runCommand(context.Background(), spec(dir, home, "i=0; while [ $i -lt 4000 ]; do echo \"строка номер $i\"; i=$((i+1)); done; echo КОНЕЦ"))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !strings.Contains(res.output, "вывод обрезан") || !strings.HasPrefix(res.output, "строка номер 0") || !strings.Contains(res.output, "КОНЕЦ") {
		t.Fatalf("начало и конец с пометкой: %q…", res.output[:80])
	}
	if len(res.output) > 32<<10+200 {
		t.Fatalf("вывод не обрезан: %d", len(res.output))
	}
}

func TestRunCancelKills(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	res := runCommand(ctx, spec(dir, home, "sleep 30"))
	if !errors.Is(res.err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("отмена: %+v за %s", res, time.Since(start))
	}
}
