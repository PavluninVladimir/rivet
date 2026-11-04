package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ревью add-agent-roles, круг 5: счётчик стадий на подключение без
// повторного использования WaitGroup, регистрация после отката стадий
// прошлого подключения, повтор CODING после доставленного push.

// Ожидание по таймауту не оставляет горутину, и стадия следующего
// подключения не роняет runner (паника «WaitGroup is reused»).
func TestStageGroupReuseAfterTimeout(t *testing.T) {
	for i := 0; i < 20000; i++ {
		a := &agent{}
		release := make(chan struct{})
		a.goStage(stageInfo{Task: "t1"}, func() { <-release })
		if a.waitStages(time.Microsecond) {
			t.Fatal("стадия ещё идёт")
		}
		close(release)
		a.goStage(stageInfo{Task: "t1"}, func() {})
		if !a.waitStages(time.Second) {
			t.Fatal("стадии закончились")
		}
	}
}

// Стадия прошлого подключения сворачивается дольше stopGrace: новое
// подключение ждёт её и только потом запускает свои, работы в одном
// каталоге не пересекаются.
func TestNextGenerationWaitsRollback(t *testing.T) {
	a := &agent{grace: 30 * time.Millisecond}
	var oldDone atomic.Bool
	a.goStage(stageInfo{Task: "t1"}, func() {
		time.Sleep(300 * time.Millisecond)
		oldDone.Store(true)
	})
	start := time.Now()
	if err := a.nextGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !oldDone.Load() || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("новое подключение дождалось отката прошлого: %v", time.Since(start))
	}
	var overlap atomic.Bool
	a.goStage(stageInfo{Task: "t1"}, func() { overlap.Store(!oldDone.Load()) })
	if !a.waitStages(time.Second) || overlap.Load() {
		t.Fatal("стадия нового подключения после отката прошлого")
	}
	// Остановка runner'а во время ожидания не держит процесс.
	block := make(chan struct{})
	a.goStage(stageInfo{Task: "t1"}, func() { <-block })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.nextGeneration(ctx); err == nil {
		t.Fatal("отмена прерывает ожидание")
	}
	close(block)
}

// runCoding — стадия CODING ветки branch в каталоге workdir.
func runCoding(t *testing.T, tmp, workdir, branch, body string) *pb.StageResult {
	t.Helper()
	script := filepath.Join(tmp, "coding-"+filepath.Base(workdir)+".sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: workdir, GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap, AgentCmd: "sh " + script}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	var mu sync.Mutex
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{TaskId: "t7", TaskNum: 7, Stage: pb.StageResult_CODING,
		Title: "x", Description: "d", Repo: "o/r", Branch: branch, BaseBranch: "main", SessionId: "s7"},
		func(m *pb.RunnerMsg) {
			mu.Lock()
			defer mu.Unlock()
			if sr := m.GetStageResult(); sr != nil {
				res = sr
			}
		})
	if res == nil {
		t.Fatal("нет результата стадии")
	}
	return res
}

func originFile(tmp, branch, path string) bool {
	return exec.Command("git", "--git-dir", filepath.Join(tmp, "repos", "o", "r.git"), "cat-file", "-e", branch+":"+path).Run() == nil
}

// Повтор CODING после push, который дошёл до origin (итог прошлой попытки
// потерян): стадия продолжает ветку задачи, работа прошлой попытки
// остаётся, push проходит; незакоммиченные правки прерванной попытки в
// ветку не попадают (круг 5, P3-3).
func TestCodingRetryAfterDeliveredPush(t *testing.T) {
	tmp := testerRepo(t)
	work := filepath.Join(tmp, "work")
	const br = "agent/task-7"
	if res := runCoding(t, tmp, work, br, "echo a > a.js\n"); !res.Ok {
		t.Fatalf("первая попытка: %+v", res)
	}
	// Хвост прерванной попытки в том же каталоге.
	ws := filepath.Join(work, "repos", workspaceKey(&pb.Assignment{Repo: "o/r", Branch: br}))
	if err := os.WriteFile(filepath.Join(ws, "junk.txt"), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := runCoding(t, tmp, work, br, "echo b > b.js\n"); !res.Ok {
		t.Fatalf("повтор в том же каталоге не эскалирует: %+v", res)
	}
	if !originFile(tmp, br, "a.js") || !originFile(tmp, br, "b.js") || originFile(tmp, br, "junk.txt") {
		t.Fatal("работа обеих попыток в ветке, хвост прерванной нет")
	}
	// Повтор на другом runner'е (свежий каталог).
	if res := runCoding(t, tmp, filepath.Join(tmp, "work2"), br, "echo c > c.js\n"); !res.Ok {
		t.Fatalf("повтор на другом runner'е: %+v", res)
	}
	if !originFile(tmp, br, "a.js") || !originFile(tmp, br, "c.js") {
		t.Fatal("работа прошлых попыток сохранена")
	}
}
