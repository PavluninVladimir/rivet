package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ревью add-agent-roles, круг 3: запрет push тестировщику живёт в
// окружении процесса агента, а не в конфиге репозитория; завершение
// runner'а ждёт стадии.

// runStageScript — стадия с агентом-скриптом body; без wait возвращает
// ожидание результата для проверок посреди стадии.
func runStageScript(t *testing.T, tmp, body string, stage pb.StageResult_Stage, role *pb.AgentRole) (string, func() *pb.StageResult) {
	t.Helper()
	script := filepath.Join(tmp, "agent-"+stage.String()+".sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap, AgentCmd: "sh " + script}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	var mu sync.Mutex
	var res *pb.StageResult
	emit := func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		if sr := m.GetStageResult(); sr != nil {
			res = sr
		}
	}
	as := &pb.Assignment{
		TaskId: "t1", TaskNum: 1, Stage: stage, Title: "x", Description: "d",
		Criteria: []string{"работает"}, Repo: "o/r", Branch: "agent/task-1", BaseBranch: "main", SessionId: "s-" + stage.String(),
		Role: role,
	}
	ws := filepath.Join(cfg.Workdir, "repos", workspaceKey(as))
	done := make(chan struct{})
	go func() {
		a.executeStage(context.Background(), as, emit)
		close(done)
	}()
	return ws, func() *pb.StageResult {
		<-done
		mu.Lock()
		defer mu.Unlock()
		if res == nil {
			t.Fatal("нет результата стадии")
		}
		return res
	}
}

// Во время сессии тестировщика конфиг репозитория не меняется: падение
// runner'а посреди сессии не оставит блок push. Блок, оставленный прежней
// версией runner'а, снимается при подготовке рабочей копии (круг 3, P2-1).
func TestPushBlockNotInRepoConfig(t *testing.T) {
	tmp := testerRepo(t)
	tester := &pb.AgentRole{Id: "tester", Title: "Тестировщик", Name: "Лев"}
	ws, get := runStageScript(t, tmp, `sleep 2; mkdir -p test; echo t > test/a.test.js; echo "VERDICT: OK"`, pb.StageResult_TESTING, tester)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(ws, "main.js")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("рабочая копия не подготовлена")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if out, _ := exec.Command("git", "-C", ws, "config", "--get-all", "remote.origin.pushurl").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("pushurl в конфиге репозитория во время сессии: %q", out)
	}
	if res := get(); !res.Ok {
		t.Fatalf("тестировщик: %+v", res)
	}
	// Блок прежней версии runner'а в конфиге каталога.
	if out, err := exec.Command("git", "-C", ws, "config", "remote.origin.pushurl", pushBlockedURL).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	_, get = runStageScript(t, tmp, `echo fix >> main.js`, pb.StageResult_FIXING, nil)
	if res := get(); !res.Ok {
		t.Fatalf("FIXING после брошенного блока: %+v", res)
	}
	if out, _ := exec.Command("git", "-C", ws, "config", "--get-all", "remote.origin.pushurl").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("брошенный блок снят: %q", out)
	}
}

// Ключ запрета добавляется к уже заданному конфигу окружения следующим
// номером.
func TestPushBlockEnv(t *testing.T) {
	got := strings.Join(pushBlockEnv([]string{"PATH=/bin"}), " ")
	if got != "GIT_CONFIG_KEY_0=remote.origin.pushurl GIT_CONFIG_VALUE_0="+pushBlockedURL+" GIT_CONFIG_COUNT=1" {
		t.Fatalf("без конфига окружения: %q", got)
	}
	got = strings.Join(pushBlockEnv([]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c", "GIT_CONFIG_COUNT=2"}), " ")
	if !strings.Contains(got, "GIT_CONFIG_KEY_2=remote.origin.pushurl") || !strings.HasSuffix(got, "GIT_CONFIG_COUNT=3") {
		t.Fatalf("поверх заданного конфига: %q", got)
	}
}

// Старый git даёт понятную ошибку вместо тихого push.
func TestGitVersionAtLeast(t *testing.T) {
	for out, ok := range map[string]bool{
		"git version 2.39.5 (Apple Git-154)\n": true,
		"git version 2.31.0":                   true,
		"git version 3.0.1":                    true,
		"git version 2.45.1.windows.1":         true,
		"git version 2.30.9":                   false,
		"git version 1.9.5":                    false,
		"нечто":                                false,
	} {
		err := gitVersionAtLeast(out, 2, 31)
		if (err == nil) != ok {
			t.Fatalf("%q: %v", out, err)
		}
		if err != nil && !strings.Contains(err.Error(), "2.31") {
			t.Fatalf("текст ошибки называет нужную версию: %v", err)
		}
	}
}

// Завершение runner'а ждёт стадии, но не дольше отведённого времени.
func TestWaitStagesGrace(t *testing.T) {
	a := &agent{}
	finished := make(chan struct{})
	a.goStage(stageInfo{Task: "t1"}, func() {
		time.Sleep(100 * time.Millisecond)
		close(finished)
	})
	a.waitStages(5 * time.Second)
	select {
	case <-finished:
	default:
		t.Fatal("стадия дождалась завершения")
	}
	release := make(chan struct{})
	a.goStage(stageInfo{Task: "t1"}, func() { <-release })
	start := time.Now()
	a.waitStages(100 * time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("ожидание ограничено: %v", d)
	}
	close(release)
}
