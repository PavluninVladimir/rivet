package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Агентом может быть только агент Rivet: сторонний агент — ошибка запуска
// до подключения к control plane. Флагов адаптеров сторонних агентов нет.
func TestRunnerAcceptsOnlyRivetAgent(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex", "opencode", "fake"} {
		args := []string{"-agent", agent, "-workdir", t.TempDir()}
		err := run(args, map[string]string{tokenEnv: "test-token"})
		if err == nil || !strings.Contains(err.Error(), "агентом может быть только агент Rivet") {
			t.Errorf("агент %s: %v", agent, err)
		}
	}
	for _, flag := range []string{"-adapter", "-cmd", "-adapter-cmd", "-claude-bin"} {
		err := run([]string{flag, "x", "-workdir", t.TempDir()}, map[string]string{tokenEnv: "test-token"})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("флаг %s должен быть удалён: %v", flag, err)
		}
	}
}

// -sandbox-ro, который открыл бы песочнице $HOME runner'а, каталог выше
// него, файл токена с его каталогом или рабочий каталог runner'а, — ошибка
// запуска до подключения к control plane. Флаг и RIVET_RUNNER_SANDBOX_RO
// проверяются одинаково (финальное ревью add-native-agent, P2-1).
func TestRunnerRefusesUnsafeSandboxRO(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokenDir := filepath.Join(t.TempDir(), "rivet")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(tokenDir, "runner.token")
	if err := os.WriteFile(token, []byte("BENIGN-TOKEN-MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tokenFileEnv, token)
	workdir := filepath.Join(home, ".rivet-runner")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	nvm := filepath.Join(home, ".nvm")
	if err := os.MkdirAll(nvm, 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{tokenEnv: "test-token"}
	for _, bad := range []string{home, filepath.Dir(home), "/", tokenDir, workdir} {
		want := "-sandbox-ro " + bad + ": каталог открыл бы песочнице"
		err := run([]string{"-workdir", workdir, "-sandbox-ro", nvm, "-sandbox-ro", bad}, secrets)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("флаг -sandbox-ro %s: %v", bad, err)
		}
		t.Setenv("RIVET_RUNNER_SANDBOX_RO", nvm+string(os.PathListSeparator)+bad)
		err = run([]string{"-workdir", workdir}, secrets)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("RIVET_RUNNER_SANDBOX_RO с %s: %v", bad, err)
		}
		t.Setenv("RIVET_RUNNER_SANDBOX_RO", "")
	}
}

// Значение RIVET_RUNNER_SANDBOX_RO — умолчание: флаги в командной строке
// его заменяют, повторный флаг добавляет каталог.
func TestSandboxROFlagReplacesEnv(t *testing.T) {
	p := &pathList{paths: []string{"/from/env"}, fromEnv: true}
	for _, v := range []string{"/a", "/b"} {
		if err := p.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.String(); got != "/a"+string(os.PathListSeparator)+"/b" {
		t.Fatalf("список: %q", got)
	}
	if err := p.Set(" "); err == nil {
		t.Fatal("пустой путь должен быть ошибкой")
	}
}

func TestRunnerHelpNeverPrintsRegistrationToken(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"-unknown-option"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "usage")
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stderr
			os.Stderr = output
			t.Cleanup(func() { os.Stderr = previous; _ = output.Close() })
			token := "BENIGN-REGISTRATION-TOKEN-MARKER"
			err = run(args, map[string]string{tokenEnv: token})
			os.Stderr = previous
			if err == nil || (args[0] == "-h" && !errors.Is(err, flag.ErrHelp)) {
				t.Fatalf("unexpected help result: %v", err)
			}
			data, readErr := os.ReadFile(output.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(data), "-token string") {
				t.Fatal("usage did not describe token option")
			}
			if strings.Contains(string(data), token) {
				t.Fatal("usage exposed registration token")
			}
		})
	}
}
