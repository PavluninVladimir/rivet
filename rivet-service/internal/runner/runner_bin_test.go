package runner

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/archcli"
	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// RIVET_RUNNER_BIN (add-architecture-c4): проверки проекта и команды агента
// Rivet зовут подкоманду architecture бинаря runner'а. Переменная входит в
// их белый список окружения, а в песочнице бинарь виден только на чтение,
// даже если лежит под скрытым $HOME. Тесты — проверки защиты на безобидных
// файлах-маркерах.

// TestMain: тестовый бинарь пакета играет «rivet-runner architecture …»,
// как cmd/rivet-runner/main.go, и служит RIVET_RUNNER_BIN в тестах гейта.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "architecture" {
		os.Exit(archcli.Main(os.Args[2:], os.Stdout))
	}
	os.Exit(m.Run())
}

// runnerBinStub — исполняемый «бинарь runner'а»: печатает маркер и аргументы.
func runnerBinStub(t *testing.T, path string) {
	t.Helper()
	mustWriteFile(t, path, "#!/bin/sh\necho \"RUNNER-BIN-OK $*\"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Проверка проекта видит бинарь runner'а в RIVET_RUNNER_BIN. В песочнице
// бинарь под скрытым $HOME исполняется, но не пишется, а соседний файл его
// каталога остаётся скрытым.
func TestChecksGetRunnerBin(t *testing.T) {
	sandbox := native.NetworkIsolationAvailable()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin", "rivet-runner")
	runnerBinStub(t, bin)
	mustWriteFile(t, filepath.Join(home, "bin", "neighbour.txt"), "BENIGN-NEIGHBOUR-MARKER\n")
	ws := filepath.Join(home, "work", "repos", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: Config{Adapter: AdapterNative, Unsandboxed: !sandbox, RunnerBin: bin}}
	cmd := `"${RIVET_RUNNER_BIN:?нет RIVET_RUNNER_BIN}" architecture gate`
	if sandbox {
		cmd += `; cat "$(dirname "$RIVET_RUNNER_BIN")/neighbour.txt" 2>/dev/null; ` +
			`(echo tampered >> "$RIVET_RUNNER_BIN") 2>/dev/null && echo BIN-WRITABLE; true`
	}
	out, got := collect()
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{{Name: "bin", Cmd: cmd}}, func(string) {}, out)
	if err != nil || !ok {
		t.Fatalf("проверка с RIVET_RUNNER_BIN: %v %v\n%s", ok, err, report)
	}
	tr := got()
	if !strings.Contains(tr, "RUNNER-BIN-OK architecture gate") {
		t.Fatalf("бинарь runner'а не вызван:\n%s", tr)
	}
	if !sandbox {
		return
	}
	for _, leak := range []string{"BENIGN-NEIGHBOUR-MARKER", "BIN-WRITABLE"} {
		if strings.Contains(tr, leak) {
			t.Errorf("песочница открыла лишнее (%s):\n%s", leak, tr)
		}
	}
	if b, _ := os.ReadFile(bin); strings.Contains(string(b), "tampered") {
		t.Error("проверка изменила бинарь runner'а")
	}
}

// Команда run агента Rivet получает RIVET_RUNNER_BIN: задание Архитектора
// зовёт через неё architecture init, validate и views.
func TestNativeRunGetsRunnerBin(t *testing.T) {
	dir := gitRepo(t)
	bin := filepath.Join(t.TempDir(), "rivet-runner")
	runnerBinStub(t, bin)
	p := &providerScript{calls: []map[string]any{
		{"name": "run", "args": map[string]any{"command": `"$RIVET_RUNNER_BIN" architecture validate -dir docs/architecture`}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "описание проверено"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	t.Setenv("RIVET_NATIVE_BASE_URL", srv.URL)
	t.Setenv("RIVET_NATIVE_CONNECTION", "deepseek")
	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative, Model: "deepseek-v4-flash", Workdir: t.TempDir(),
		Unsandboxed: !native.NetworkIsolationAvailable(), RunnerBin: bin,
		Secrets: map[string]string{"RIVET_PROVIDER_KEY_DEEPSEEK": "sk-test"}}
	sink, _, _ := nativeSink()
	if _, err := newAdapter(cfg).Run(context.Background(), dir, "# Задача\nПроверь описание", sink); err != nil {
		t.Fatal(err)
	}
	var last struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(p.bodies[1], &last)
	if m := last.Messages[len(last.Messages)-1]; m.Role != "tool" || !strings.Contains(m.Content, "RUNNER-BIN-OK architecture validate -dir docs/architecture") {
		t.Fatalf("вывод команды с RIVET_RUNNER_BIN: %+v", m)
	}
}

// Гейт «Архитектура» задачи описания идёт проверкой проекта runner'а с
// агентом Rivet: подкоманда architecture бинаря runner'а, -origin из
// назначения. Прошлая стадия оставила в клоне подмену истории и адреса
// (refs/replace, refspec, url.*.insteadOf, адрес origin), подготовка
// рабочей копии её снимает, и гейт видит настоящую базу: ветка только с
// описанием проходит, ветка с правкой вне описания проваливается вердиктом
// по коду, а не сбоем окружения. В песочнице без сети гейт берёт базу из
// origin/main рабочей копии, потому что fetch с адреса проекта недоступен.
func TestArchitectureGateForRivetAgent(t *testing.T) {
	tmp := testerRepo(t)
	seed := filepath.Join(tmp, "seed")
	gitT(t, seed, "checkout", "-q", "-b", "agent/task-2", "main")
	if err := architecture.Init(filepath.Join(seed, architecture.DefaultDir), "demo"); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-q", "-m", "описание архитектуры")
	gitT(t, seed, "push", "-q", "origin", "agent/task-2")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap,
		RunnerBin: self, Unsandboxed: !native.NetworkIsolationAvailable()}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	as := &pb.Assignment{TaskId: "t2", TaskNum: 2, Stage: pb.StageResult_TESTING, Repo: "o/r",
		Branch: "agent/task-2", BaseBranch: "main", SessionId: "s2"}
	origin := cloneURL(cfg.GitBase, as)
	gate := []*pb.Check{{Name: "Архитектура", Cmd: `"${RIVET_RUNNER_BIN:?нужен rivet-runner с поддержкой проверки архитектуры}" ` +
		`architecture gate -base 'origin/main' -origin ` + shellQuote(origin) + ` -force -only-docs`}}
	fake := filepath.Join(tmp, "fake.git")
	gitT(t, tmp, "init", "-q", "--bare", fake)

	// spoil — состояние .git, которое оставила бы прошлая стадия: база
	// подменена вершиной ветки задачи, адрес origin переписан.
	spoil := func(dir string) {
		t.Helper()
		gitT(t, dir, "push", "-q", fake, "HEAD:refs/heads/main")
		gitT(t, dir, "replace", strings.TrimSpace(gitOutT(t, dir, "rev-parse", "origin/main")), "HEAD")
		gitT(t, dir, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/agent/task-2:refs/remotes/origin/main")
		gitT(t, dir, "config", "remote.origin.url", fake)
		gitT(t, dir, "config", "url."+fake+".insteadOf", origin)
	}
	runGate := func() (bool, string) {
		t.Helper()
		dir, err := a.workspace(context.Background(), as, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		spoil(dir)
		if _, err := a.workspace(context.Background(), as, func(string) {}); err != nil {
			t.Fatal(err)
		}
		ok, report, err := a.runChecks(context.Background(), dir, gate, func(string) {}, nil)
		if err != nil {
			t.Fatalf("гейт упал как сбой окружения: %v\n%s", err, report)
		}
		return ok, report
	}

	if ok, report := runGate(); !ok || !strings.Contains(report, "описание архитектуры корректно") {
		t.Fatalf("ветка только с описанием: %v\n%s", ok, report)
	}

	mustWriteFile(t, filepath.Join(seed, "main.js"), "правка вне описания\n")
	gitT(t, seed, "commit", "-q", "-a", "-m", "правка кода")
	gitT(t, seed, "push", "-q", "origin", "agent/task-2")
	if ok, report := runGate(); ok || !strings.Contains(report, "main.js") {
		t.Fatalf("ветка с правкой вне описания должна провалить гейт: %v\n%s", ok, report)
	}
}
