package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ревью add-agent-roles, круг 6: фоновый процесс агента, который держит
// PTY, не держит стадию; зависшая стадия завершает runner через
// stuckLimit; подготовка каталога с --prune и откатом хвостов.

// killPIDFile убивает процесс, чей pid записан в файл (фоновый процесс
// агента из теста).
func killPIDFile(t *testing.T, path string) {
	t.Cleanup(func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// Отменённая стадия завершается, хотя агент оставил фоновый процесс,
// который держит PTY и не умирает по SIGHUP (круг 6, P2).
func TestCancelledStageWithOrphanFinishes(t *testing.T) {
	tmp := testerRepo(t)
	pidFile := filepath.Join(tmp, "orphan.pid")
	killPIDFile(t, pidFile)
	script := filepath.Join(tmp, "agent.sh")
	body := "#!/bin/sh\n(trap '' HUP; echo $$ > " + shellQuote(pidFile) + "; exec sleep 30) &\necho started\nsleep 20\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap, AgentCmd: "sh " + script}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg)}
	ctx, cancel := context.WithCancel(context.Background())
	as := &pb.Assignment{TaskId: "t1", TaskNum: 1, Stage: pb.StageResult_FIXING, Title: "x", Description: "d",
		Repo: "o/r", Branch: "agent/task-1", BaseBranch: "main", SessionId: "s1"}
	a.goStage(stageInfo{Task: as.TaskId}, func() { a.executeStage(ctx, as, func(*pb.RunnerMsg) {}) })
	time.Sleep(2 * time.Second)
	cancel()
	start := time.Now()
	if !a.waitStages(10*time.Second) || time.Since(start) > 8*time.Second {
		t.Fatalf("отменённая стадия завершилась: %v", time.Since(start))
	}
}

// Агент вышел, оставив фоновый процесс вне группы (setsid) на PTY: вывод
// дочитывается коротко, master закрывается без EOF.
func TestPTYReturnsWithDetachedOrphan(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid недоступен")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")
	killPIDFile(t, pidFile)
	start := time.Now()
	out, err := runPTY(context.Background(), dir, "setsid sh -c 'echo $$ > "+pidFile+"; exec sleep 30' & sleep 0.3; echo готово", nil, nil)
	if err != nil || !strings.Contains(out, "готово") {
		t.Fatalf("вывод команды: %q %v", out, err)
	}
	if d := time.Since(start); d > 2*ptyDrainTimeout+3*time.Second {
		t.Fatalf("runPTY вернулся без EOF: %v", d)
	}
}

// То же для фонового процесса в группе агента при обычном выходе.
func TestPTYReturnsWithBackgroundOrphan(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")
	killPIDFile(t, pidFile)
	start := time.Now()
	out, err := runPTY(context.Background(), dir, "(trap '' HUP; echo $$ > "+pidFile+"; exec sleep 30) & sleep 0.3; echo готово", nil, nil)
	if err != nil || !strings.Contains(out, "готово") {
		t.Fatalf("вывод команды: %q %v", out, err)
	}
	if d := time.Since(start); d > 2*ptyDrainTimeout+3*time.Second {
		t.Fatalf("runPTY вернулся без EOF: %v", d)
	}
}

// Стадия прошлого подключения не завершилась за stuckLimit: runner
// завершается с понятной причиной, в ней задача, сессия и стадия.
func TestNextGenerationStuckLimit(t *testing.T) {
	a := &agent{grace: 20 * time.Millisecond, stuck: 100 * time.Millisecond}
	block := make(chan struct{})
	defer close(block)
	a.goStage(stageInfo{Task: "t9", Session: "s9", Stage: "FIXING"}, func() { <-block })
	start := time.Now()
	err := a.nextGeneration(context.Background())
	if !errors.Is(err, errStagesStuck) || !strings.Contains(err.Error(), "t9") || !strings.Contains(err.Error(), "s9") ||
		!strings.Contains(err.Error(), "FIXING") {
		t.Fatalf("зависшая стадия: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("предел ожидания: %v", d)
	}
}

// runStageIn — стадия stage ветки branch в каталоге workdir.
func runStageIn(t *testing.T, tmp, workdir string, stage pb.StageResult_Stage, branch, body string) *pb.StageResult {
	t.Helper()
	script := filepath.Join(tmp, "stage-"+stage.String()+"-"+filepath.Base(workdir)+".sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: workdir, GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap, AgentCmd: "sh " + script}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg)}
	var mu sync.Mutex
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{TaskId: "t8", TaskNum: 8, Stage: stage,
		Title: "x", Description: "d", Repo: "o/r", Branch: branch, BaseBranch: "main", SessionId: "s8"},
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

// Ветка, удалённая на хостинге, не продолжается по старой ссылке
// (--prune), а хвост прерванной стадии не попадает в FIXING (круг 6, P3).
func TestWorkspacePrepPrunesAndCleans(t *testing.T) {
	tmp := testerRepo(t)
	work := filepath.Join(tmp, "work")
	const br = "agent/task-8"
	if res := runStageIn(t, tmp, work, pb.StageResult_CODING, br, "echo a > a.js\n"); !res.Ok {
		t.Fatalf("первая попытка: %+v", res)
	}
	origin := filepath.Join(tmp, "repos", "o", "r.git")
	if out, err := exec.Command("git", "--git-dir", origin, "branch", "-D", br).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if res := runStageIn(t, tmp, work, pb.StageResult_CODING, br, "echo b > b.js\n"); !res.Ok {
		t.Fatalf("CODING после удаления ветки: %+v", res)
	}
	if originFile(tmp, br, "a.js") || !originFile(tmp, br, "b.js") {
		t.Fatal("удалённая на хостинге ветка начата заново от базовой")
	}
	ws := filepath.Join(work, "repos", workspaceKey(&pb.Assignment{Repo: "o/r", Branch: br}))
	if err := os.WriteFile(filepath.Join(ws, "junk.txt"), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.js"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := runStageIn(t, tmp, work, pb.StageResult_FIXING, br, "echo fix > fix.js\n"); !res.Ok {
		t.Fatalf("FIXING: %+v", res)
	}
	got, _ := exec.Command("git", "--git-dir", origin, "show", br+":b.js").Output()
	if originFile(tmp, br, "junk.txt") || strings.TrimSpace(string(got)) != "b" || !originFile(tmp, br, "fix.js") {
		t.Fatalf("хвост прерванной стадии не попал в ветку: b.js=%q", got)
	}
}
