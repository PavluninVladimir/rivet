package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit — git в каталоге теста с явной identity.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runCheck выполняет команду проверки так же, как runner: /bin/sh -c в
// рабочей копии.
func runCheck(t *testing.T, dir, script string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Спека «Проверка архитектуры на шаге test»: runner без подкоманды
// пропускает ветку без изменений описания и проваливает ветку с ними с
// явной причиной; runner с подкомандой зовёт её.
func TestArchitectureGateCheckCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	origin := t.TempDir()
	runGit(t, origin, "init", "-q")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "add", "-A")
	runGit(t, origin, "commit", "-q", "-m", "seed")
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, filepath.Dir(work), "clone", "-q", origin, work)
	runGit(t, work, "checkout", "-q", "-b", "agent/task-1")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := architectureGateCheck("main", false, "").Cmd
	unset := "RIVET_RUNNER_BIN="
	if out, err := runCheck(t, work, cmd, unset); err != nil || !strings.Contains(out, "не менялось") {
		t.Fatalf("без изменений описания: %v %s", err, out)
	}

	if err := os.MkdirAll(filepath.Join(work, "docs", "architecture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "docs", "architecture", "architecture.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCheck(t, work, cmd, unset); err == nil || !strings.Contains(out, "обновите rivet-runner") {
		t.Fatalf("изменённое описание на старом runner'е: %v %s", err, out)
	}

	// Runner с подкомандой: вызов идёт в неё с базой origin/main.
	bin := filepath.Join(t.TempDir(), "rivet-runner")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"called $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runCheck(t, work, cmd, "RIVET_RUNNER_BIN="+bin); err != nil || !strings.Contains(out, "called architecture gate -base origin/main") {
		t.Fatalf("вызов подкоманды: %v %s", err, out)
	}
	describe := architectureGateCheck("main", true, "").Cmd
	if out, err := runCheck(t, work, describe, "RIVET_RUNNER_BIN="+bin); err != nil || !strings.Contains(out, "-force -only-docs") {
		t.Fatalf("проверка задачи описания: %v %s", err, out)
	}
	if out, err := runCheck(t, work, describe, unset); err == nil || !strings.Contains(out, "нужен rivet-runner") {
		t.Fatalf("задача описания без подкоманды: %v %s", err, out)
	}
}
