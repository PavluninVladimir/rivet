package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Агент не запустился из-за подключения модели (нет ключа): стадия любого
// вида возвращает вердикт connection, control plane эскалирует её кодом
// MODEL_CONNECTION, а не как провал проверок (fix-agent-without-connection).
func TestStageVerdictWithoutConnectionKey(t *testing.T) {
	base, _ := gitRemote(t)
	for _, stage := range []pb.StageResult_Stage{pb.StageResult_CODING, pb.StageResult_REVIEW, pb.StageResult_PROMPT} {
		cfg := Config{Adapter: AdapterNative, Model: "deepseek-v4-flash", Workdir: t.TempDir(), GitBase: "file://" + base + "/"}
		a := &agent{cfg: cfg, adapter: newAdapter(cfg), contexts: newContextHub(), cancel: map[string]*stageCancel{}}
		as := &pb.Assignment{TaskId: "t1", SessionId: "s1", Stage: stage, Repo: "repo", Branch: "feature/t1",
			BaseBranch: "main", TaskNum: 1, Title: "x", StepPrompt: "обнови changelog", Model: "deepseek-v4-flash",
			Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: "https://api.deepseek.com", Model: "deepseek-v4-flash", ConnectionId: "deepseek"}}
		var mu sync.Mutex
		var res *pb.StageResult
		a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
			mu.Lock()
			defer mu.Unlock()
			if r := m.GetStageResult(); r != nil {
				res = r
			}
		})
		if res == nil || res.Ok || res.Verdict != verdictModelConnection || !strings.Contains(res.Detail, "нет ключа подключения") {
			t.Fatalf("%s: итог стадии без ключа: %+v", stage, res)
		}
	}
}

// Тестировщик без ключа подключения: тот же вердикт connection, а не сбой
// инфраструктуры с повтором шага.
func TestTesterVerdictWithoutConnectionKey(t *testing.T) {
	tmp := testerRepo(t)
	cfg := Config{Adapter: AdapterNative, Model: "deepseek-v4-flash", Workdir: tmp + "/work", GitBase: "file://" + tmp + "/repos/", Unsandboxed: true}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	var mu sync.Mutex
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{
		TaskId: "t1", TaskNum: 1, Stage: pb.StageResult_TESTING, Title: "x", Description: "d",
		Criteria: []string{"работает"}, Repo: "o/r", Branch: "agent/task-1", BaseBranch: "main", SessionId: "s1",
		Role:     &pb.AgentRole{Id: "tester", Title: "Тестировщик", Name: "Лев"},
		Model:    "deepseek-v4-flash",
		Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: "https://api.deepseek.com", Model: "deepseek-v4-flash", ConnectionId: "deepseek"},
	}, func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		if r := m.GetStageResult(); r != nil {
			res = r
		}
	})
	if res == nil || res.Ok || res.Verdict != verdictModelConnection || !strings.Contains(res.Detail, "нет ключа подключения") {
		t.Fatalf("итог тестировщика без ключа: %+v", res)
	}
}

// Провайдер отклонил ключ во время работы (401): тот же вердикт connection,
// а не провал стадии и не замечания разработчику на review.
func TestStageVerdictProviderRejectsKey(t *testing.T) {
	base, _ := gitRemote(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	for _, stage := range []pb.StageResult_Stage{pb.StageResult_CODING, pb.StageResult_REVIEW} {
		cfg := Config{Adapter: AdapterNative, Model: "m", Workdir: t.TempDir(), GitBase: "file://" + base + "/", Unsandboxed: true}
		a := &agent{cfg: cfg, adapter: newAdapter(cfg), contexts: newContextHub(), cancel: map[string]*stageCancel{}}
		as := &pb.Assignment{TaskId: "t1", SessionId: "s1", Stage: stage, Repo: "repo", Branch: "feature/t1",
			BaseBranch: "main", TaskNum: 1, Title: "x", Model: "m",
			Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: srv.URL, Model: "m", ConnectionId: "local", Key: "fake-key-0000"}}
		var mu sync.Mutex
		var res *pb.StageResult
		a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
			mu.Lock()
			defer mu.Unlock()
			if r := m.GetStageResult(); r != nil {
				res = r
			}
		})
		if res == nil || res.Ok || res.Verdict != verdictModelConnection {
			t.Fatalf("%s: итог стадии при 401 провайдера: %+v", stage, res)
		}
	}
}
