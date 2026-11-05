//go:build unix

package native

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Пробы — тестовый бинарник, запущенный внутри команды агента. Вне команды
// они пропускаются: позиционный аргумент задаёт только сама команда.
//
//	escape-probe <dir>  уходит в свою сессию (setsid), через 1,5 с пишет
//	                    файл-маркер <dir>/late — безобидный фоновый процесс,
//	                    который пытается пережить команду;
//	net-probe           проверяет loopback и отсутствие внешней сети.
func TestProbe(t *testing.T) {
	switch flag.Arg(0) {
	case "escape-probe":
		_, _ = syscall.Setsid()
		time.Sleep(1500 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(flag.Arg(1), "late"), []byte("жив"), 0o644)
	case "net-probe":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("LOOPBACK-FAIL", err)
			return
		}
		go func() {
			if c, err := ln.Accept(); err == nil {
				_, _ = c.Write([]byte("pong"))
				_ = c.Close()
			}
		}()
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			fmt.Println("LOOPBACK-FAIL", err)
			return
		}
		buf := make([]byte, 4)
		_, _ = c.Read(buf)
		fmt.Println("LOOPBACK-" + map[bool]string{true: "OK", false: "FAIL"}[string(buf) == "pong"])
		if _, err := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second); err != nil {
			fmt.Println("NET-BLOCKED")
		} else {
			fmt.Println("NET-OPEN")
		}
	default:
		t.Skip("проба запускается только внутри команды агента")
	}
}

func probeCmd(name string, a ...string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	parts := []string{q(os.Args[0]), "-test.run=^TestProbe$", name}
	for _, s := range a {
		parts = append(parts, q(s))
	}
	return strings.Join(parts, " ")
}

// modes — режимы, доступные на этой машине: без песочницы всегда, с
// песочницей — если её можно поднять.
func modes() []bool {
	if NetworkIsolationAvailable() {
		return []bool{false, true}
	}
	return []bool{false}
}

func noMarker(t *testing.T, dir string) {
	t.Helper()
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); err == nil {
		t.Fatal("фоновый процесс команды пережил run и создал файл-маркер")
	}
}

// После завершения команды не остаётся её фоновых процессов, в том числе
// ушедших в свою сессию.
func TestRunKillsLeftoverProcesses(t *testing.T) {
	for _, isolate := range modes() {
		t.Run(fmt.Sprintf("isolate=%v", isolate), func(t *testing.T) {
			t.Parallel()
			dir, home := t.TempDir(), t.TempDir()
			sp := spec(dir, home, probeCmd("escape-probe", dir)+" & sleep 0.5; echo started")
			sp.isolate = isolate
			res := runCommand(context.Background(), sp)
			if res.err != nil || res.exitCode != 0 || !strings.Contains(res.output, "started") {
				t.Fatalf("команда: %+v", res)
			}
			if res.leftovers == 0 {
				t.Fatalf("фоновый процесс не учтён: %+v", res)
			}
			noMarker(t, dir)
		})
	}
}

// По таймауту завершаются и процессы, ушедшие из группы команды.
func TestRunTimeoutKillsEscapedProcesses(t *testing.T) {
	for _, isolate := range modes() {
		t.Run(fmt.Sprintf("isolate=%v", isolate), func(t *testing.T) {
			t.Parallel()
			dir, home := t.TempDir(), t.TempDir()
			sp := spec(dir, home, probeCmd("escape-probe", dir)+" & sleep 30")
			sp.timeout = 700 * time.Millisecond
			sp.isolate = isolate
			res := runCommand(context.Background(), sp)
			if !res.timedOut {
				t.Fatalf("таймаут: %+v", res)
			}
			noMarker(t, dir)
		})
	}
}

// Окружение команды собрано из белого списка: имена переменных — только
// разрешённые (плюс служебные, которые выставляет сама оболочка).
func TestRunEnvWhitelist(t *testing.T) {
	t.Setenv("RIVET_NATIVE_KEY", "test-value")
	t.Setenv("RIVET_PROVIDER_KEY_DEMO", "test-value")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-value")
	cache := t.TempDir()
	for _, isolate := range modes() {
		dir, home := t.TempDir(), t.TempDir()
		sp := spec(dir, home, "env")
		sp.isolate = isolate
		sp.readOnly = []CacheDir{{Env: "GOMODCACHE", Dir: cache}, {Env: "npm_config_cache", Dir: filepath.Join(cache, "missing")}}
		res := runCommand(context.Background(), sp)
		if res.err != nil || res.exitCode != 0 {
			t.Fatalf("env: %+v", res)
		}
		allowed := []string{"PATH", "HOME", "TMPDIR", "LANG", "TERM", "CI", "NO_COLOR", "GIT_TERMINAL_PROMPT", "GOCACHE", "GOMODCACHE",
			"PWD", "OLDPWD", "SHLVL", "_"}
		for _, l := range strings.Split(strings.TrimSpace(res.output), "\n") {
			name, _, _ := strings.Cut(l, "=")
			if !slices.Contains(allowed, name) {
				t.Errorf("isolate=%v: переменная %q не из белого списка", isolate, name)
			}
		}
		if !strings.Contains(res.output, "GOMODCACHE="+cache+"\n") {
			t.Errorf("кеш модулей не передан: %s", res.output)
		}
	}
}

// Песочница Linux: своя сеть только с loopback, свой PID namespace, кеши
// зависимостей только на чтение.
func TestSandboxIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("песочница есть только на Linux")
	}
	if !NetworkIsolationAvailable() {
		t.Skip("песочница недоступна, команды выполняются без изоляции сети: " + SandboxUnavailableReason())
	}
	dir, home, cache := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(cache, "mod.txt"), "модуль\n")
	sp := spec(dir, home, probeCmd("net-probe")+`; echo "pidns=$(readlink /proc/self/ns/pid)"; echo "netns=$(readlink /proc/self/ns/net)"; cat "$GOMODCACHE/mod.txt"; `+
		`(touch "$GOMODCACHE/new" 2>/dev/null && echo CACHE-WRITABLE) || echo CACHE-READONLY; touch ws-ok && echo WS-WRITABLE`)
	sp.isolate = true
	sp.readOnly = []CacheDir{{Env: "GOMODCACHE", Dir: cache}}
	res := runCommand(context.Background(), sp)
	if res.err != nil || !res.isolated {
		t.Fatalf("песочница: %+v", res)
	}
	// Свои PID и сетевой namespace: идентификаторы отличаются от
	// namespace самого runner'а.
	for _, ns := range []string{"pid", "net"} {
		own, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.output, ns+"ns=") || strings.Contains(res.output, ns+"ns="+own+"\n") {
			t.Errorf("команда в namespace %s runner'а:\n%s", ns, res.output)
		}
	}
	for _, want := range []string{"LOOPBACK-OK", "NET-BLOCKED", "модуль", "CACHE-READONLY", "WS-WRITABLE"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("нет %q в выводе:\n%s", want, res.output)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "new")); err == nil {
		t.Error("кеш зависимостей изменён из песочницы")
	}
}
