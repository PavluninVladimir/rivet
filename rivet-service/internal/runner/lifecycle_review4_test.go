package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ревью add-agent-roles, круг 4: стадия, прерванная остановкой runner'а
// или обрывом канала, итога не шлёт; каждое подключение — новое поколение
// стадий; запрет push окружением поверх уже заданного конфига.

// codingStage — CODING с агентом-скриптом и управляемым контекстом;
// started закрывается, когда агент запустился.
func codingStage(t *testing.T, ctx context.Context, body string) (*agent, <-chan struct{}, func() []*pb.RunnerMsg) {
	t.Helper()
	tmp := testerRepo(t)
	marker := filepath.Join(tmp, "started")
	script := filepath.Join(tmp, "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap, AgentCmd: "sh " + script}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	var mu sync.Mutex
	var msgs []*pb.RunnerMsg
	emit := func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, m)
	}
	as := &pb.Assignment{TaskId: "t1", TaskNum: 1, Stage: pb.StageResult_CODING, Title: "x", Description: "d",
		Repo: "o/r", Branch: "agent/task-1", BaseBranch: "main", SessionId: "s-coding"}
	done := make(chan struct{})
	a.goStage(stageInfo{Task: "t1"}, func() {
		a.executeStage(ctx, as, emit)
		close(done)
	})
	started := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				close(started)
				return
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return a, started, func() []*pb.RunnerMsg {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return msgs
	}
}

func hasResult(msgs []*pb.RunnerMsg) bool {
	for _, m := range msgs {
		if m.GetStageResult() != nil || m.GetBlocked() != nil {
			return true
		}
	}
	return false
}

// Остановка runner'а (SIGTERM) посреди CODING: стадия ждёт отката, но
// итога не шлёт, и в журнал он не попадает (круг 4, P2-1).
func TestStageStoppedByRunnerSendsNoResult(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	a, started, msgs := codingStage(t, ctx, "sleep 30\n")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("агент не запустился")
	}
	stop()
	a.waitStages(10 * time.Second)
	if got := msgs(); hasResult(got) {
		t.Fatalf("итог прерванной стадии отправлен: %+v", got)
	}
}

// Отмена от control plane (CancelTask): итог стадии отправляется.
func TestStageCancelledByPlaneSendsResult(t *testing.T) {
	a, started, msgs := codingStage(t, context.Background(), "sleep 30\n")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("агент не запустился")
	}
	a.mu.Lock()
	c := a.cancel["t1"]
	a.mu.Unlock()
	c.byPlane.Store(true)
	c.cancel()
	if got := msgs(); !hasResult(got) {
		t.Fatalf("итог отменённой control plane'ом стадии: %+v", got)
	}
}

// bootClient — Register запоминает boot_id, канал не открывается.
type bootClient struct {
	pb.RunnerServiceClient
	mu    sync.Mutex
	boots []string
}

func (c *bootClient) Register(_ context.Context, in *pb.RegisterRequest, _ ...grpc.CallOption) (*pb.RegisterResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.boots = append(c.boots, in.BootId)
	return &pb.RegisterResponse{Accepted: true}, nil
}

func (c *bootClient) Channel(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[pb.RunnerMsg, pb.PlaneMsg], error) {
	return nil, errors.New("канал недоступен")
}

// Каждое подключение — новое поколение стадий: стадии прошлого канала
// отменены, и control plane по смене boot_id назначит их заново.
func TestBootIDPerConnection(t *testing.T) {
	c := &bootClient{}
	a := &agent{cfg: Config{ID: "r1"}, client: c}
	_ = a.session(context.Background())
	_ = a.session(context.Background())
	if len(c.boots) != 2 || c.boots[0] == "" || c.boots[0] == c.boots[1] {
		t.Fatalf("boot_id на каждом подключении новый: %q", c.boots)
	}
}

// Агент пушит по имени remote при уже заданном конфиге окружения: push
// не проходит, прежний ключ конфига сохраняется (круг 4, P3).
func TestAgentPushBlockedOverExistingEnvConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "rivet.probe")
	t.Setenv("GIT_CONFIG_VALUE_0", "kept")
	tmp := testerRepo(t)
	log := filepath.Join(tmp, "push.log")
	before := originSHA(t, tmp)
	res, _, _, _ := runTesterScript(t, tmp, `mkdir -p test; echo t > test/a.test.js; git add -A; git -c user.name=a -c user.email=a@a commit -qm t
echo "probe=$(git config rivet.probe)" > `+shellQuote(log)+`
git push -q origin HEAD:agent/task-1 >> `+shellQuote(log)+` 2>&1; echo "exit=$?" >> `+shellQuote(log)+`
echo "VERDICT: OK"`, 0)
	out, _ := os.ReadFile(log)
	if !strings.Contains(string(out), "probe=kept") {
		t.Fatalf("прежний конфиг окружения сохранён: %s", out)
	}
	if strings.Contains(string(out), "exit=0") {
		t.Fatalf("push агента по имени remote заблокирован: %s", out)
	}
	// Тесты зафиксировал runner своим push, а не агент.
	if !res.Ok || originSHA(t, tmp) == before || !originHas(t, tmp, "test/a.test.js") {
		t.Fatalf("тесты зафиксированы runner'ом: %+v", res)
	}
}
