package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/runner"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
)

const startupProbeEnv = "RIVET_RUNNER_STARTUP_PROBE"

// TestMain: с меткой startupProbeEnv тестовый бинарник проходит старт
// runner'а (prepareSecrets, с перезапуском) и печатает отчёт о себе:
// имена своих переменных окружения, свои аргументы, имена и длины
// секретов в памяти, признак dumpable. Значения секретов не печатаются.
func TestMain(m *testing.M) {
	if os.Getenv(startupProbeEnv) == "1" {
		// Окружение и аргументы, с которыми процесс запущен (до любых
		// unsetenv): у перезапущенного — то, что видно снаружи.
		if os.Getenv(secretsFileEnv) != "" {
			for _, kv := range os.Environ() {
				name, _, _ := strings.Cut(kv, "=")
				fmt.Println("startenv", name)
			}
			for _, a := range os.Args[1:] {
				fmt.Println("startarg", a)
			}
		}
		st, err := prepareSecrets(os.Args[1:])
		if err != nil {
			fmt.Println("error", err)
			os.Exit(2)
		}
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			fmt.Println("env", name)
		}
		for _, a := range st.args {
			fmt.Println("arg", a)
		}
		names := make([]string, 0, len(st.secrets))
		for k := range st.secrets {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("secret %s len=%d\n", k, len(st.secrets[k]))
		}
		fmt.Println("reexec", st.reexeced)
		fmt.Println("dumpable", processDumpable())
		// Песочница команд должна подниматься и у недампируемого runner'а.
		fmt.Println("sandbox", native.NetworkIsolationAvailable(), native.SandboxUnavailableReason())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSplitSecrets(t *testing.T) {
	args := []string{"-plane", "x:1", "-token", "t-1", "--token=t-2", "-id", "r1"}
	env := []string{"PATH=/bin", "RIVET_RUNNER_TOKEN=t-env", "RIVET_PROVIDER_KEY_DEEPSEEK=k", "RIVET_NATIVE_KEY=", "RIVET_PLANE_ADDR=x:1"}
	cleanArgs, cleanEnv, secrets, warnings := splitSecrets(args, env)
	if !slices.Equal(cleanArgs, []string{"-plane", "x:1", "-id", "r1"}) {
		t.Fatalf("аргументы без -token: %v", cleanArgs)
	}
	if !slices.Equal(cleanEnv, []string{"PATH=/bin", "RIVET_PLANE_ADDR=x:1"}) {
		t.Fatalf("окружение без секретов: %v", cleanEnv)
	}
	if secrets["RIVET_RUNNER_TOKEN"] != "t-2" || secrets["RIVET_PROVIDER_KEY_DEEPSEEK"] != "k" || len(warnings) != 2 {
		t.Fatalf("секреты %v, предупреждения %v", secrets, warnings)
	}
}

func TestReadTokenFilePermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte("rrt_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("файл, доступный группе: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := readTokenFile(p); err != nil || tok != "rrt_test" {
		t.Fatalf("токен из файла: %q %v", tok, err)
	}
}

// После старта runner работает с очищенным окружением и аргументами:
// секреты есть только в памяти, файл перезапуска удалён, на Linux процесс
// недампируемый.
func TestStartupCleansEnvironment(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-plane", "x:1", "-token", "test-token-value")
	cmd.Env = append(os.Environ(), startupProbeEnv+"=1", "TMPDIR="+tmp,
		"RIVET_PROVIDER_KEY_DEMO=test-key-value", "RIVET_NATIVE_KEY=test-native-value")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("старт: %v\n%s", err, out)
	}
	report := string(out)
	lines := strings.Split(report, "\n")
	started := false
	for _, l := range lines {
		if name, ok := strings.CutPrefix(l, "env "); ok && (runner.IsSecretEnv(name) || name == secretsFileEnv) {
			t.Errorf("в окружении runner'а после старта переменная %s", name)
		}
		// Окружение и аргументы перезапущенного процесса — то, что видно
		// снаружи (ps, /proc): секретов в них нет.
		if name, ok := strings.CutPrefix(l, "startenv "); ok {
			started = true
			if runner.IsSecretEnv(name) {
				t.Errorf("перезапущенный runner получил переменную %s", name)
			}
		}
		if a, ok := strings.CutPrefix(l, "startarg "); ok && strings.Contains(a, "token") {
			t.Errorf("в аргументах перезапущенного runner'а %q", a)
		}
		if a, ok := strings.CutPrefix(l, "arg "); ok && strings.Contains(a, "token") {
			t.Errorf("в аргументах runner'а после старта %q", a)
		}
	}
	if !started || !slices.Contains(lines, "reexec true") {
		t.Errorf("runner должен перезапуститься с очищенным окружением:\n%s", report)
	}
	for _, want := range []string{
		"secret RIVET_NATIVE_KEY len=17", "secret RIVET_PROVIDER_KEY_DEMO len=14", "secret RIVET_RUNNER_TOKEN len=16",
		"arg -plane", "arg x:1",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("нет %q в отчёте:\n%s", want, report)
		}
	}
	if strings.Contains(report, "test-token-value") || strings.Contains(report, "test-key-value") {
		t.Error("значение секрета в выводе")
	}
	if runtime.GOOS == "linux" && !slices.Contains(lines, "dumpable 0") {
		t.Errorf("процесс должен быть недампируемым:\n%s", report)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "rivet-runner-secrets-*")); len(left) > 0 {
		t.Errorf("файл секретов не удалён: %v", left)
	}
}

// Перезапуск с очищенным окружением не удался (временный каталог
// недоступен, файл секретов не создать): runner не стартует с секретами в
// окружении, а с RIVET_RUNNER_ALLOW_ENV_SECRETS=1 продолжает осознанно.
func TestStartupFailsClosedWhenReexecFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "нет-такого-каталога")
	start := func(extra ...string) (string, int) {
		cmd := exec.Command(os.Args[0], "-plane", "x:1")
		cmd.Env = append(append(os.Environ(), startupProbeEnv+"=1", "TMPDIR="+missing,
			"RIVET_RUNNER_TOKEN=test-token-value"), extra...)
		out, err := cmd.CombinedOutput()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("старт: %v", err)
		}
		return string(out), code
	}
	out, code := start()
	if code != 2 || !strings.Contains(out, "error не удалось перезапуститься с очищенным окружением") ||
		!strings.Contains(out, allowEnvSecretsEnv+"=1") {
		t.Fatalf("без разрешения runner не должен стартовать (код %d):\n%s", code, out)
	}
	out, code = start(allowEnvSecretsEnv + "=1")
	lines := strings.Split(out, "\n")
	if code != 0 || !slices.Contains(lines, "reexec false") || !slices.Contains(lines, "secret RIVET_RUNNER_TOKEN len=16") {
		t.Fatalf("с %s=1 runner продолжает без перезапуска (код %d):\n%s", allowEnvSecretsEnv, code, out)
	}
	if strings.Contains(out, "test-token-value") {
		t.Error("значение секрета в выводе")
	}
}
