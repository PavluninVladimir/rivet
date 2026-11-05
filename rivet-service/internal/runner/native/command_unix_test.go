//go:build unix

package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// RunCommand без таймаута ждёт команду до конца, вывод идёт потоком в
// OnOutput и в результат, окружение — белый список с временным HOME.
func TestRunCommandStreamsOutput(t *testing.T) {
	t.Setenv("RIVET_CHECK_MARKER_ENV", "runner-only")
	dir := t.TempDir()
	var mu sync.Mutex
	var streamed strings.Builder
	res, err := RunCommand(context.Background(), CommandSpec{
		Dir: dir, Command: `echo first; sleep 0.3; echo "home=$HOME"; env | grep -c RIVET_CHECK_MARKER_ENV; exit 4`,
		OnOutput: func(b []byte) { mu.Lock(); streamed.Write(b); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 4 || res.TimedOut {
		t.Fatalf("код выхода: %+v", res)
	}
	mu.Lock()
	got := streamed.String()
	mu.Unlock()
	if got != res.Output || !strings.HasPrefix(got, "first\nhome=") || !strings.HasSuffix(got, "\n0\n") {
		t.Fatalf("поток и результат: %q / %q", got, res.Output)
	}
	if strings.Contains(got, "home="+os.Getenv("HOME")+"\n") {
		t.Fatalf("HOME команды — HOME runner'а: %q", got)
	}
}

// С RequireSandbox команда без песочницы не запускается вовсе: ошибка
// ErrNoSandbox, файла-маркера нет. С песочницей — идёт изолированно.
func TestRunCommandRequireSandbox(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	res, err := RunCommand(context.Background(), CommandSpec{Dir: dir, Command: "touch '" + marker + "'", RequireSandbox: true})
	if !NetworkIsolationAvailable() {
		if !errors.Is(err, ErrNoSandbox) {
			t.Fatalf("без песочницы ждём ErrNoSandbox: %v %+v", err, res)
		}
		if _, serr := os.Stat(marker); serr == nil {
			t.Fatal("команда запустилась без песочницы")
		}
		return
	}
	if err != nil || !res.Isolated {
		t.Fatalf("в песочнице: %v %+v", err, res)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Fatalf("команда в песочнице не записала рабочую копию: %v", serr)
	}
}

// Отмена ctx останавливает команду без таймаута и возвращает ошибку.
func TestRunCommandCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	_, err := RunCommand(ctx, CommandSpec{Dir: t.TempDir(), Command: "sleep 30", KillGrace: 200 * time.Millisecond})
	if !errors.Is(err, context.Canceled) || time.Since(started) > 10*time.Second {
		t.Fatalf("отмена: %v за %v", err, time.Since(started))
	}
}

// Временный HOME с каталогами только для чтения (кеш модулей Go)
// удаляется целиком.
func TestRemoveTreeReadOnlyDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	ro := filepath.Join(dir, "go", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "a.go"), []byte("package x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ro, filepath.Dir(ro)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	RemoveTree(dir)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("каталог не удалён: %v", err)
	}
}
