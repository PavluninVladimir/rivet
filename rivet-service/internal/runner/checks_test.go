package runner

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Проверки проекта исполняют код модели и идут в песочнице агента
// (ревью интеграции, P2-5). Тесты — проверки защиты на безобидных
// файлах-маркерах и белом списке окружения.

// TestCheckProbe — проба внутри проверки: вне неё пропускается.
//
//	dial-probe <addr>  подключается к TCP-адресу на loopback хоста и
//	                   печатает DIAL-OK или DIAL-BLOCKED.
func TestCheckProbe(t *testing.T) {
	if flag.Arg(0) != "dial-probe" {
		t.Skip("проба запускается только внутри проверки проекта")
	}
	if c, err := net.DialTimeout("tcp", flag.Arg(1), 2*time.Second); err == nil {
		_ = c.Close()
		fmt.Println("DIAL-OK")
	} else {
		fmt.Println("DIAL-BLOCKED")
	}
}

func dialProbeCmd(addr string) string {
	return shellQuote(os.Args[0]) + " -test.run=^TestCheckProbe$ dial-probe " + shellQuote(addr)
}

// withSandbox подменяет доступность песочницы на время теста.
func withSandbox(t *testing.T, ok bool) {
	t.Helper()
	prev := sandboxAvailable
	sandboxAvailable = func() bool { return ok }
	t.Cleanup(func() { sandboxAvailable = prev })
}

func collect() (func([]byte), func() string) {
	var mu sync.Mutex
	var b strings.Builder
	return func(p []byte) { mu.Lock(); b.Write(p); mu.Unlock() },
		func() string { mu.Lock(); defer mu.Unlock(); return b.String() }
}

// Без песочницы и без -unsandboxed проверки не запускаются: стадия
// неуспешна с причиной, код из ветки не исполнялся.
func TestChecksRefusedWithoutSandbox(t *testing.T) {
	withSandbox(t, false)
	ws := t.TempDir()
	marker := filepath.Join(ws, "ran")
	a := &agent{cfg: Config{Adapter: AdapterNative}}
	_, _, err := a.runChecks(context.Background(), ws, []*pb.Check{{Name: "tests", Cmd: "touch " + shellQuote(marker)}},
		func(string) {}, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "песочница команд недоступна") || !strings.Contains(err.Error(), "-unsandboxed") {
		t.Fatalf("ждём отказ с причиной: %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("проверка запустилась без песочницы")
	}
}

// Стадия TESTING без роли на runner'е без песочницы и без -unsandboxed:
// неуспех с причиной и вердиктом error (сбой инфраструктуры, попытка
// разработчика не расходуется), а не прогон проверок на хосте.
func TestTestingStageRefusesChecksWithoutSandbox(t *testing.T) {
	withSandbox(t, false)
	base, _ := gitRemote(t)
	marker := filepath.Join(t.TempDir(), "ran")
	a := &agent{cfg: Config{Adapter: AdapterNative, Workdir: t.TempDir(), GitBase: "file://" + base + "/"},
		contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	a.adapter = newAdapter(a.cfg)
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{TaskId: "t1", SessionId: "s1", TaskNum: 1, Stage: pb.StageResult_TESTING,
		Repo: "repo", Branch: "feature/t1", BaseBranch: "main", Checks: []*pb.Check{{Name: "tests", Cmd: "touch " + shellQuote(marker)}}},
		func(m *pb.RunnerMsg) {
			if r := m.GetStageResult(); r != nil {
				res = r
			}
		})
	if res == nil || res.Ok || res.Verdict != verdictInfraError || !strings.Contains(res.Detail, "песочница команд недоступна") {
		t.Fatalf("итог стадии: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("проверка запустилась без песочницы")
	}
}

// Окружение проверки — белый список агента: переменных runner'а нет,
// HOME временный, вывод уходит в транскрипт потоком и в отчёт.
func TestChecksEnvWhitelisted(t *testing.T) {
	t.Setenv("RIVET_CHECK_MARKER_ENV", "runner-only")
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	a := &agent{cfg: Config{Adapter: AdapterNative, Unsandboxed: true}}
	out, got := collect()
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{
		{Name: "env", Cmd: `env | grep -c RIVET_CHECK_MARKER_ENV; echo "home=$HOME"; echo "ci=$CI"`},
		{Name: "red", Cmd: "echo boom; exit 3"},
	}, func(string) {}, out)
	if err != nil {
		t.Fatal(err)
	}
	if ok || !strings.Contains(report, "== red ==\nboom") || !strings.Contains(report, "FAIL: код выхода 3") {
		t.Fatalf("красная проверка в отчёте: %v\n%s", ok, report)
	}
	tr := got()
	if !strings.HasPrefix(tr, "0\nhome=") || !strings.Contains(tr, "ci=1\n") || !strings.Contains(tr, "boom\n") {
		t.Fatalf("транскрипт проверок: %q", tr)
	}
	if strings.Contains(tr, "home="+os.Getenv("HOME")+"\n") {
		t.Fatalf("HOME проверки — HOME runner'а: %q", tr)
	}
}

// Фоновые процессы проверки не переживают её: маркер не появляется.
func TestChecksKillLeftovers(t *testing.T) {
	ws := t.TempDir()
	late := filepath.Join(ws, "late")
	a := &agent{cfg: Config{Adapter: AdapterNative, Unsandboxed: true}}
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{
		{Name: "bg", Cmd: "(sleep 1; touch " + shellQuote(late) + ") & echo started"},
	}, func(string) {}, func([]byte) {})
	if err != nil || !ok {
		t.Fatalf("проверка: %v %v\n%s", ok, err, report)
	}
	time.Sleep(2 * time.Second)
	if _, serr := os.Stat(late); serr == nil {
		t.Fatalf("фоновый процесс проверки пережил её:\n%s", report)
	}
}

// В песочнице проверка не видит $HOME runner'а и файл токена, не ходит на
// loopback хоста, а рабочая копия под $HOME остаётся доступной на запись.
// Только там, где песочница есть (Linux с user namespaces).
func TestChecksRunInSandbox(t *testing.T) {
	if !native.NetworkIsolationAvailable() {
		t.Skip("песочница недоступна: " + native.SandboxUnavailableReason())
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWriteFile(t, filepath.Join(home, ".ssh", "id_rsa"), "BENIGN-HOME-MARKER\n")
	tokenFile := filepath.Join(t.TempDir(), "runner.token")
	mustWriteFile(t, tokenFile, "BENIGN-TOKEN-MARKER\n")
	ws := filepath.Join(home, ".rivet-runner", "repos", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(nil) // слушатель на loopback хоста
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	a := &agent{cfg: Config{Adapter: AdapterNative, SecretFiles: []string{tokenFile}}}
	var steps []string
	out, got := collect()
	cmd := "cat " + shellQuote(filepath.Join(home, ".ssh", "id_rsa")) + " 2>/dev/null; " +
		"cat " + shellQuote(tokenFile) + " 2>/dev/null; " +
		"echo written > written.txt && echo WS-WRITABLE; " + dialProbeCmd(addr)
	ok, report, err := a.runChecks(context.Background(), ws, []*pb.Check{{Name: "sandbox", Cmd: cmd}},
		func(s string) { steps = append(steps, s) }, out)
	if err != nil || !ok {
		t.Fatalf("проверка в песочнице: %v %v\n%s", ok, err, report)
	}
	tr := got()
	for _, leak := range []string{"BENIGN-HOME-MARKER", "BENIGN-TOKEN-MARKER", "DIAL-OK"} {
		if strings.Contains(tr, leak) {
			t.Errorf("проверка вышла за песочницу (%s):\n%s", leak, tr)
		}
	}
	for _, want := range []string{"WS-WRITABLE", "DIAL-BLOCKED"} {
		if !strings.Contains(tr, want) {
			t.Errorf("нет %q в выводе проверки:\n%s", want, tr)
		}
	}
	if b, err := os.ReadFile(filepath.Join(ws, "written.txt")); err != nil || string(b) != "written\n" {
		t.Errorf("запись в рабочую копию из песочницы: %q %v", b, err)
	}
	if len(steps) == 0 || steps[0] != "проверки проекта в песочнице без сети" {
		t.Errorf("шаги: %v", steps)
	}
}

// Без песочницы (macOS, -unsandboxed) та же проверка видит loopback хоста:
// честный режим без изоляции, о нём говорит шаг стадии.
func TestChecksUnsandboxedStep(t *testing.T) {
	if native.NetworkIsolationAvailable() {
		t.Skip("песочница есть: режим без неё здесь не проверить")
	}
	a := &agent{cfg: Config{Adapter: AdapterNative, Unsandboxed: true}}
	var steps []string
	ok, _, err := a.runChecks(context.Background(), t.TempDir(), []*pb.Check{{Name: "x", Cmd: "true"}},
		func(s string) { steps = append(steps, s) }, nil)
	if err != nil || !ok || len(steps) == 0 || !strings.Contains(steps[0], "без песочницы (-unsandboxed)") {
		t.Fatalf("шаги без песочницы: %v %v %v", ok, err, steps)
	}
}

// Проверки после сессии тестировщика идут со снимком .git (P2-4): код
// проверки, дописавший .git/config, валит стадию сбоем инфраструктуры, а
// .git/config возвращается к снимку.
func TestTesterChecksGitTamper(t *testing.T) {
	tmp := testerRepo(t)
	res := runTester(t, tmp, "Категории", []*pb.Check{{Name: "tests", Cmd: `printf '[rivet]\n\tprobe = tampered\n' >> .git/config`}})
	if res.Ok || res.Verdict != verdictInfraError || !strings.Contains(res.Detail, "код проверки изменил служебные файлы git") {
		t.Fatalf("подмена .git проверкой: %+v", res)
	}
	ws := filepath.Join(tmp, "work", "repos", workspaceKey(&pb.Assignment{Repo: "o/r"}))
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); strings.Contains(string(b), "tampered") {
		t.Fatalf(".git/config не возвращён:\n%s", b)
	}
}

// Runner выкатки (без агента) стадий задач не исполняет: ни агента, ни
// проверок проекта на машине выкатки. У тестировщика это сбой
// инфраструктуры, попытка разработчика не расходуется.
func TestDeployRunnerRefusesTaskStages(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	a := &agent{cfg: Config{Workdir: t.TempDir(), Unsandboxed: true}, contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	a.adapter = newAdapter(a.cfg)
	for _, as := range []*pb.Assignment{
		{TaskId: "t1", SessionId: "s1", Stage: pb.StageResult_CODING, Repo: "o/r", Branch: "agent/task-1"},
		{TaskId: "t2", SessionId: "s2", Stage: pb.StageResult_TESTING, Repo: "o/r", Branch: "agent/task-2",
			Checks: []*pb.Check{{Name: "tests", Cmd: "touch " + shellQuote(marker)}}},
		{TaskId: "t3", SessionId: "s3", Stage: pb.StageResult_TESTING, Repo: "o/r", Branch: "agent/task-3",
			Role: &pb.AgentRole{Id: "tester", Title: "Тестировщик"}},
	} {
		var res *pb.StageResult
		a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
			if r := m.GetStageResult(); r != nil {
				res = r
			}
		})
		if res == nil || res.Ok || !strings.Contains(res.Detail, "runner выкатки без агента") {
			t.Fatalf("%s: %+v", as.TaskId, res)
		}
		if wantInfra := as.Role != nil; wantInfra != (res.Verdict == verdictInfraError) {
			t.Fatalf("%s: вердикт %q", as.TaskId, res.Verdict)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("проверка проекта запустилась на runner'е выкатки")
	}
}

// Команда агента Rivet без песочницы и без -unsandboxed не исполняется:
// модель получает ошибку инструмента, маркер не создан.
func TestNativeRunRequiresSandbox(t *testing.T) {
	dir := gitRepo(t)
	marker := filepath.Join(dir, "ran")
	p := &providerScript{calls: []map[string]any{
		{"name": "run", "args": map[string]any{"command": "touch " + shellQuote(marker)}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "готово"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	as := &pb.Assignment{Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: srv.URL, Model: "m", Key: "sk-test"}}
	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative}.forAssignment(as)
	sink, events, _ := nativeSink()
	if _, err := newAdapter(cfg).Run(context.Background(), dir, "# Задача\nx", sink); err != nil {
		t.Fatal(err)
	}
	var runOK *bool
	for _, ev := range *events {
		if ev.Kind == "tool" && ev.Tool == "run" {
			ok := ev.Ok
			runOK = &ok
		}
	}
	_, statErr := os.Stat(marker)
	if !native.NetworkIsolationAvailable() {
		if runOK == nil || *runOK || statErr == nil {
			t.Fatalf("без песочницы команда агента не должна исполниться: шаг ok=%v, маркер %v", runOK, statErr)
		}
		return
	}
	if runOK == nil || !*runOK || statErr != nil {
		t.Fatalf("в песочнице команда агента исполняется: шаг ok=%v, маркер %v", runOK, statErr)
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
