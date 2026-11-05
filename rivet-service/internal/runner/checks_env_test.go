package runner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Проверки проекта в песочнице без сети и без инструментов из $HOME
// (финальное ревью add-native-agent, P2-1): сбой окружения — вердикт error
// с подсказкой, а не красная проверка; флаги -checks-network и
// -sandbox-ro. Тесты — проверки защиты на безобидных маркерах.

// TestCheckConnectProbe — проба внутри проверки: подключается к адресу и
// при ошибке печатает её текст и выходит с кодом 3. Вне проверки
// пропускается.
func TestCheckConnectProbe(t *testing.T) {
	if flag.Arg(0) != "connect-probe" {
		t.Skip("проба запускается только внутри проверки проекта")
	}
	c, err := net.DialTimeout("tcp", flag.Arg(1), 2*time.Second)
	if err != nil {
		fmt.Println(err)
		os.Exit(3)
	}
	_ = c.Close()
	fmt.Println("CONNECT-OK")
}

func connectProbeCmd(addr string) string {
	return shellQuote(os.Args[0]) + " -test.run=^TestCheckConnectProbe$ connect-probe " + shellQuote(addr)
}

// requireSandbox — тест только там, где песочница есть (Linux с user
// namespaces; в CI на macOS пропускается, гоняется в Docker).
func requireSandbox(t *testing.T) {
	t.Helper()
	if !native.NetworkIsolationAvailable() {
		t.Skip("песочница недоступна: " + native.SandboxUnavailableReason())
	}
}

// fakeTool — исполняемый скрипт-маркер name в каталоге bin.
func fakeTool(t *testing.T, bin, name, out string) {
	t.Helper()
	p := filepath.Join(bin, name)
	mustWriteFile(t, p, "#!/bin/sh\necho "+out+"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Классификация упавшей проверки: код 127 в песочнице и сетевая ошибка в
// песочнице без сети — сбой окружения; без песочницы, с сетью и при
// обычном падении — вердикт по коду.
func TestCheckEnvFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  native.CommandResult
		env  bool
	}{
		{"успех", native.CommandResult{ExitCode: 0, Sandboxed: true, Isolated: true, Output: "dial tcp ok"}, false},
		{"127 в песочнице", native.CommandResult{ExitCode: 127, Sandboxed: true, Isolated: true, Output: "sh: 1: npm: not found"}, true},
		{"127 в песочнице с сетью", native.CommandResult{ExitCode: 127, Sandboxed: true, Output: "npm: not found"}, true},
		{"127 без песочницы", native.CommandResult{ExitCode: 127, Output: "npm: not found"}, false},
		{"go без сети", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true,
			Output: "go: golang.org/x/text@v0.14.0: Get \"https://proxy.golang.org/...\": dial tcp: lookup proxy.golang.org on 10.0.0.1:53: dial udp 10.0.0.1:53: connect: network is unreachable"}, true},
		{"npm без сети", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true, Output: "npm ERR! code ENOTFOUND\nnpm ERR! errno ENOTFOUND"}, true},
		{"npm EAI_AGAIN", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true, Output: "request to https://registry.npmjs.org failed, reason: getaddrinfo EAI_AGAIN registry.npmjs.org"}, true},
		{"curl без сети", native.CommandResult{ExitCode: 6, Sandboxed: true, Isolated: true, Output: "curl: (6) Could not resolve host: example.com"}, true},
		{"pip без сети", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true, Output: "Temporary failure in name resolution"}, true},
		{"база на хосте", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true, Output: "dial tcp 127.0.0.1:5432: connect: connection refused"}, true},
		{"сетевая ошибка с -checks-network", native.CommandResult{ExitCode: 1, Sandboxed: true, Output: "dial tcp: lookup x: no such host"}, false},
		{"сетевая ошибка без песочницы", native.CommandResult{ExitCode: 1, Output: "network is unreachable"}, false},
		{"обычный красный тест", native.CommandResult{ExitCode: 1, Sandboxed: true, Isolated: true, Output: "--- FAIL: TestSum\n    sum_test.go:9: 2+2=5"}, false},
	} {
		if got := checkEnvFailure(tc.res) != ""; got != tc.env {
			t.Errorf("%s: сбой окружения %v, ждём %v", tc.name, got, tc.env)
		}
	}
}

// Каталоги -sandbox-ro: нельзя открыть песочнице $HOME runner'а, каталоги
// выше него, файл токена с его каталогом и рабочий каталог runner'а (и
// внутри него); каталог инструментов под $HOME и вне его — можно.
func TestCheckSandboxReadOnly(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "runner")
	nvm := filepath.Join(home, ".nvm")
	tokenDir := filepath.Join(root, "etc", "rivet")
	token := filepath.Join(tokenDir, "runner.token")
	workdir := filepath.Join(home, ".rivet-runner")
	tools := filepath.Join(root, "opt", "toolchains")
	for _, d := range []string{nvm, tokenDir, filepath.Join(workdir, "repos", "other"), tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile(t, token, "BENIGN-TOKEN-MARKER\n")
	mustWriteFile(t, filepath.Join(root, "file"), "x")
	link := filepath.Join(root, "link-to-home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	check := func(paths ...string) ([]string, error) {
		return CheckSandboxReadOnly(paths, home, workdir, []string{token})
	}
	for _, bad := range []string{home, filepath.Dir(home), root, "/", tokenDir, filepath.Dir(tokenDir), workdir,
		filepath.Join(workdir, "repos", "other"), link, filepath.Join(root, "missing"), filepath.Join(root, "file")} {
		if _, err := check(nvm, bad); err == nil {
			t.Errorf("-sandbox-ro %s должен быть отклонён", bad)
		} else if !strings.Contains(err.Error(), "-sandbox-ro") {
			t.Errorf("%s: текст ошибки без флага: %v", bad, err)
		}
	}
	got, err := check(nvm, tools, nvm+"/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != nvm || got[1] != tools {
		t.Fatalf("принятые каталоги: %v", got)
	}
}

// Инструмент проверки под $HOME runner'а (nvm) в PATH: в песочнице его
// нет, проверка падает с кодом 127. Это сбой окружения с подсказкой, а не
// красная проверка (порт TestReproChecksToolInHome). С -sandbox-ro ~/.nvm
// инструмент виден только на чтение, остальной $HOME по-прежнему скрыт.
func TestChecksToolInHomeIsInfraError(t *testing.T) {
	requireSandbox(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWriteFile(t, filepath.Join(home, ".ssh", "id_rsa"), "BENIGN-HOME-MARKER\n")
	nvm := filepath.Join(home, ".nvm")
	bin := filepath.Join(nvm, "versions", "node", "v22", "bin")
	fakeTool(t, bin, "npm", "NPM-RAN")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws := filepath.Join(home, ".rivet-runner", "repos", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	check := []*pb.Check{{Name: "test", Cmd: "npm test"}}

	a := &agent{cfg: Config{Adapter: AdapterNative}}
	ok, report, err := a.runChecks(context.Background(), ws, check, func(string) {}, nil)
	if ok || !errors.Is(err, errChecksEnvironment) {
		t.Fatalf("инструмент под $HOME в песочнице — сбой окружения: ok=%v err=%v\n%s", ok, err, report)
	}
	if !strings.Contains(err.Error(), "код выхода 127") || !strings.Contains(err.Error(), "README") {
		t.Fatalf("причина и подсказка: %v", err)
	}

	a = &agent{cfg: Config{Adapter: AdapterNative, SandboxReadOnly: []string{nvm}}}
	cmd := "npm test; cat " + shellQuote(filepath.Join(home, ".ssh", "id_rsa")) + " 2>/dev/null; " +
		"(touch " + shellQuote(filepath.Join(bin, "new")) + " 2>/dev/null && echo RO-WRITABLE) || echo RO-READONLY"
	out, got := collect()
	ok, report, err = a.runChecks(context.Background(), ws, []*pb.Check{{Name: "test", Cmd: cmd}}, func(string) {}, out)
	if err != nil || !ok {
		t.Fatalf("с -sandbox-ro ~/.nvm: ok=%v err=%v\n%s", ok, err, report)
	}
	tr := got()
	for _, want := range []string{"NPM-RAN", "RO-READONLY"} {
		if !strings.Contains(tr, want) {
			t.Errorf("нет %q в выводе:\n%s", want, tr)
		}
	}
	if strings.Contains(tr, "BENIGN-HOME-MARKER") {
		t.Errorf("с -sandbox-ro остальной $HOME runner'а виден проверке:\n%s", tr)
	}
	if _, err := os.Stat(filepath.Join(bin, "new")); err == nil {
		t.Error("каталог -sandbox-ro изменён из песочницы")
	}
}

// Проверке нужна сеть (go mod download на пустом кеше модулей): в
// песочнице без сети это сбой окружения, а не красная проверка (порт
// TestReproChecksNeedNetwork). Нужен go вне $HOME runner'а.
func TestChecksNeedNetworkIsInfraError(t *testing.T) {
	requireSandbox(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go не найден")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if strings.HasPrefix(goBin, home+string(filepath.Separator)) {
		t.Skip("go под $HOME")
	}
	mod := filepath.Join(home, "go", "pkg", "mod")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", mod)
	ws := filepath.Join(home, ".rivet-runner", "repos", "demo")
	mustWriteFile(t, filepath.Join(ws, "go.mod"), "module demo\n\ngo 1.21\n\nrequire golang.org/x/text v0.14.0\n")
	mustWriteFile(t, filepath.Join(ws, "main.go"), "package main\n\nimport _ \"golang.org/x/text/language\"\n\nfunc main() {}\n")
	a := &agent{cfg: Config{Adapter: AdapterNative}}
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{{Name: "deps", Cmd: "go mod download && echo DL-OK"}}, func(string) {}, nil)
	if ok || !errors.Is(err, errChecksEnvironment) || !strings.Contains(err.Error(), "сетевая ошибка") {
		t.Fatalf("сеть недоступна — сбой окружения: ok=%v err=%v\n%s", ok, err, report)
	}
	t.Logf("итог: %v", err)
}

// Сетевая ошибка в песочнице без сети (проба на TEST-NET-1) — сбой
// окружения; та же проба на loopback хоста с -checks-network проходит, а
// $HOME runner'а и файл токена по-прежнему скрыты.
func TestChecksNetworkFlag(t *testing.T) {
	requireSandbox(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWriteFile(t, filepath.Join(home, ".ssh", "id_rsa"), "BENIGN-HOME-MARKER\n")
	tokenFile := filepath.Join(t.TempDir(), "runner.token")
	mustWriteFile(t, tokenFile, "BENIGN-TOKEN-MARKER\n")
	ws := filepath.Join(home, ".rivet-runner", "repos", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: Config{Adapter: AdapterNative, SecretFiles: []string{tokenFile}}}
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{{Name: "net", Cmd: connectProbeCmd("192.0.2.1:80")}}, func(string) {}, nil)
	if ok || !errors.Is(err, errChecksEnvironment) || !strings.Contains(report, "network is unreachable") {
		t.Fatalf("сетевая ошибка без сети — сбой окружения: ok=%v err=%v\n%s", ok, err, report)
	}

	srv := httptest.NewServer(nil) // слушатель на loopback хоста
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	a = &agent{cfg: Config{Adapter: AdapterNative, SecretFiles: []string{tokenFile}, ChecksNetwork: true}}
	var steps []string
	out, got := collect()
	cmd := "cat " + shellQuote(filepath.Join(home, ".ssh", "id_rsa")) + " 2>/dev/null; " +
		"cat " + shellQuote(tokenFile) + " 2>/dev/null; " + connectProbeCmd(addr)
	ok, report, err = a.runChecks(context.Background(), ws, []*pb.Check{{Name: "net", Cmd: cmd}},
		func(s string) { steps = append(steps, s) }, out)
	if err != nil || !ok {
		t.Fatalf("-checks-network: ok=%v err=%v\n%s", ok, err, report)
	}
	tr := got()
	if !strings.Contains(tr, "CONNECT-OK") {
		t.Errorf("с -checks-network проверка не достала loopback хоста:\n%s", tr)
	}
	for _, leak := range []string{"BENIGN-HOME-MARKER", "BENIGN-TOKEN-MARKER"} {
		if strings.Contains(tr, leak) {
			t.Errorf("с -checks-network проверка прочитала файл runner'а (%s):\n%s", leak, tr)
		}
	}
	if len(steps) == 0 || steps[0] != "проверки проекта в песочнице с сетью (-checks-network)" {
		t.Errorf("шаги: %v", steps)
	}
}

// -checks-network не даёт сеть командам агента, а каталог -sandbox-ro
// виден и им (только на чтение).
func TestChecksNetworkKeepsAgentOffline(t *testing.T) {
	requireSandbox(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	nvm := filepath.Join(home, ".nvm")
	bin := filepath.Join(nvm, "bin")
	fakeTool(t, bin, "nodetool", "TOOL-RAN")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	hostSrv := httptest.NewServer(nil)
	defer hostSrv.Close()
	addr := strings.TrimPrefix(hostSrv.URL, "http://")

	dir := gitRepo(t)
	cmd := "(" + dialProbeCmd(addr) + "; nodetool) > probe.txt 2>&1"
	p := &providerScript{calls: []map[string]any{
		{"name": "run", "args": map[string]any{"command": cmd}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "готово"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	as := &pb.Assignment{Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: srv.URL, Model: "m", Key: "sk-test"}}
	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative, ChecksNetwork: true, SandboxReadOnly: []string{nvm}}.forAssignment(as)
	sink, _, _ := nativeSink()
	if _, err := newAdapter(cfg).Run(context.Background(), dir, "# Задача\nx", sink); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "probe.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "DIAL-BLOCKED") || strings.Contains(string(b), "DIAL-OK") {
		t.Errorf("команда агента с -checks-network получила сеть:\n%s", b)
	}
	if !strings.Contains(string(b), "TOOL-RAN") {
		t.Errorf("каталог -sandbox-ro не виден команде агента:\n%s", b)
	}
}

// Стадия TESTING без роли: проверка, упавшая из-за окружения песочницы,
// даёт вердикт error с подсказкой (шаг повторится без расхода попытки).
func TestTestingStageEnvFailureIsInfra(t *testing.T) {
	requireSandbox(t)
	base, _ := gitRemote(t)
	home := os.Getenv("HOME") // gitRemote подменил HOME временным каталогом
	bin := filepath.Join(home, ".nvm", "bin")
	fakeTool(t, bin, "npm", "NPM-RAN")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := &agent{cfg: Config{Adapter: AdapterNative, Workdir: t.TempDir(), GitBase: "file://" + base + "/"},
		contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	a.adapter = newAdapter(a.cfg)
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{TaskId: "t1", SessionId: "s1", TaskNum: 1, Stage: pb.StageResult_TESTING,
		Repo: "repo", Branch: "feature/t1", BaseBranch: "main", Checks: []*pb.Check{{Name: "tests", Cmd: "npm test"}}},
		func(m *pb.RunnerMsg) {
			if r := m.GetStageResult(); r != nil {
				res = r
			}
		})
	if res == nil || res.Ok || res.Verdict != verdictInfraError || !strings.Contains(res.Detail, errChecksEnvironment.Error()) {
		t.Fatalf("итог стадии: %+v", res)
	}
}
