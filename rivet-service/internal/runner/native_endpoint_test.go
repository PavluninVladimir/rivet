package runner

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Control plane заполняет в Assignment.endpoint предел ответа, предельный
// выход модели и цену кешированного входа (design add-native-agent, 1.12.1).
// Стадия ревью на агенте Rivet: первый ход упирается в предел ответа с
// обрезанным вызовом — повтор идёт с удвоенным пределом, но не выше
// предела модели. Расход с кешированными токенами доходит до pb.Usage, а
// стоимость считается по цене кешированного входа.
func TestStageUsesEndpointLimitsAndCachedTokens(t *testing.T) {
	base, _ := gitRemote(t)
	var mu sync.Mutex
	var maxTokens []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		maxTokens = append(maxTokens, body["max_tokens"].(float64))
		i := len(maxTokens) - 1
		mu.Unlock()
		var choice map[string]any
		if i == 0 {
			// Вызов обрезан по длине: аргументы не JSON-объект.
			choice = map[string]any{"finish_reason": "length", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c0", "type": "function",
					"function": map[string]any{"name": "finish", "arguments": `{"verdict":"ok","summ`}}}}}
		} else {
			args, _ := json.Marshal(map[string]any{"verdict": "ok", "summary": "замечаний нет"})
			choice = map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "finish", "arguments": string(args)}}}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "deepseek-v4-flash", "choices": []any{choice},
			"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 100, "prompt_cache_hit_tokens": 600}})
	}))
	defer srv.Close()

	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative, Workdir: t.TempDir(), GitBase: "file://" + base + "/"}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	as := &pb.Assignment{TaskId: "t1", SessionId: "s1", TaskNum: 1, Stage: pb.StageResult_REVIEW, Title: "x",
		Repo: "repo", Branch: "feature/t1", BaseBranch: "main",
		Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: srv.URL, Model: "deepseek-v4-flash", Key: "sk-test",
			ContextWindow: 128000, MaxOutputTokens: 32768, ModelMaxOutput: 49152,
			HasPrices: true, InputPrice: 0.27, OutputPrice: 1.1, CachedInputPrice: 0.07}}
	var res *pb.StageResult
	var usage *pb.Usage
	a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		if r := m.GetStageResult(); r != nil {
			res = r
		}
		if u := m.GetUsage(); u != nil {
			usage = u
		}
	})
	if res == nil || !res.Ok {
		t.Fatalf("итог стадии ревью: %+v", res)
	}
	// Предел хода из endpoint, повтор обрезанного вызова — 2×32768, но не
	// выше предела модели 49152.
	if len(maxTokens) != 2 || maxTokens[0] != 32768 || maxTokens[1] != 49152 {
		t.Fatalf("max_tokens запросов: %v, ждём [32768 49152]", maxTokens)
	}
	if usage == nil || usage.GetTokensIn() != 2000 || usage.GetTokensOut() != 200 ||
		usage.TokensCached == nil || *usage.TokensCached != 1200 || usage.TokensCacheWrite == nil || *usage.TokensCacheWrite != 0 {
		t.Fatalf("usage стадии: %+v", usage)
	}
	want := (800*0.27 + 1200*0.07 + 200*1.1) / 1e6
	if usage.CostUsd == nil || math.Abs(*usage.CostUsd-want) > 1e-12 {
		t.Fatalf("стоимость по цене кешированного входа: %v, ждём %v", usage.CostUsd, want)
	}
}

// Запись в кеш провайдера (Anthropic) уходит в tokens_cache_write.
func TestNativeUsageCacheFields(t *testing.T) {
	r := nativeUsage(native.Usage{InputTokens: 5000, OutputTokens: 70, CachedInputTokens: 3000, CacheWriteTokens: 1500})
	if *r.TokensIn != 5000 || *r.TokensOut != 70 || *r.TokensCached != 3000 || *r.TokensCacheWrite != 1500 {
		t.Fatalf("usage: in=%d out=%d cached=%d write=%d", *r.TokensIn, *r.TokensOut, *r.TokensCached, *r.TokensCacheWrite)
	}
}
