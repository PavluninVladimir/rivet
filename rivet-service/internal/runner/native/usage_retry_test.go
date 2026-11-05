package native

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/llm"
)

// Регрессия ревью интеграции, P1-2. Первая попытка первого хода: DeepSeek
// отвечает finish_reason insufficient_system_resource с usage (прерванная
// генерация, повторяется), вторая успешна с тем же входом. Вход хода 60k
// при окне 100k, то есть окно заполнено на 60%. Расход сессии считает обе
// попытки, а оценка окна — только успешную: без этого «вход последнего
// хода» становился 120k, и второй ход падал с ErrContextOverflow.
func TestRetryDoesNotInflateContext(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		usage := map[string]any{"prompt_tokens": 60000, "completion_tokens": 10, "total_tokens": 60010}
		var choice map[string]any
		switch i {
		case 0:
			choice = map[string]any{"index": 0, "finish_reason": "insufficient_system_resource", "message": map[string]any{"role": "assistant", "content": ""}}
		case 1:
			args, _ := json.Marshal(map[string]any{"command": "true"})
			choice = map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": ToolRun, "arguments": string(args)}}}}}
		default:
			args, _ := json.Marshal(map[string]any{"verdict": "ok", "summary": "ok"})
			choice = map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": fmt.Sprintf("c%d", i), "type": "function", "function": map[string]any{"name": ToolFinish, "arguments": string(args)}}}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "object": "chat.completion", "model": "deepseek-v4-flash", "choices": []any{choice}, "usage": usage})
	}))
	defer srv.Close()
	rec := &recorder{}
	cl := llm.Client{API: llm.APIOpenAI, BaseURL: srv.URL, Key: "k",
		Retry: llm.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}}
	cfg := Config{Client: cl, Model: "deepseek-v4-flash", Dir: t.TempDir(), OnStep: rec.step, OnTranscript: rec.transcript,
		Exec: ExecConfig{KillGrace: 200 * time.Millisecond}, ContextWindow: 100000}
	res, err := Run(context.Background(), cfg, "# Задача\nx")
	if err != nil {
		t.Fatalf("сессия на нормальном контексте упала после повтора провайдера: %v\nзаметки:\n%s", err, rec.notes())
	}
	if res.Verdict != "ok" || res.Turns != 2 {
		t.Fatalf("итог: %+v", res)
	}
	// Окно — по входу успешной попытки последнего хода: 60k из 100k.
	if res.Usage.CtxPct == nil || *res.Usage.CtxPct != 60 {
		t.Fatalf("ctx_pct: %v, ждём 60", res.Usage.CtxPct)
	}
	// К оплате — все три запроса, включая прерванную попытку.
	if res.Usage.InputTokens != 180000 || res.Usage.OutputTokens != 30 {
		t.Fatalf("расход сессии: %+v, ждём вход 3×60000 и выход 3×10", res.Usage)
	}
	if strings.Contains(rec.notes(), "сжат") {
		t.Fatalf("лишнее сжатие истории из-за повтора:\n%s", rec.notes())
	}
}
