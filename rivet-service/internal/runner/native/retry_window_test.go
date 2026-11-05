package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/llm"
)

// windowProvider — провайдер OpenAI с окном window и входом prompt на
// каждый запрос: запрос сверх окна отклоняется «maximum context length»,
// первый ход обрезан по длине на вызове finish.
type windowProvider struct {
	prompt, window int
	mu             sync.Mutex
	asked          []int
}

func (p *windowProvider) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	mt := int(body["max_tokens"].(float64))
	p.mu.Lock()
	p.asked = append(p.asked, mt)
	i := len(p.asked) - 1
	p.mu.Unlock()
	if p.prompt+mt > p.window {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "This model's maximum context length is exceeded", "type": "invalid_request_error"}})
		return
	}
	var choice map[string]any
	if i == 0 {
		choice = map[string]any{"index": 0, "finish_reason": "length", "message": map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "c0", "type": "function", "function": map[string]any{"name": ToolFinish, "arguments": `{"verdict":"ok","summ`}}}}}
	} else {
		args, _ := json.Marshal(map[string]any{"verdict": "ok", "summary": "ok"})
		choice = map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": ToolFinish, "arguments": string(args)}}}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []any{choice},
		"usage": map[string]any{"prompt_tokens": p.prompt, "completion_tokens": 10}})
}

func (p *windowProvider) run(t *testing.T, maxOut, modelMax int) ([]int, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	defer srv.Close()
	cl := llm.Client{API: llm.APIOpenAI, BaseURL: srv.URL, Key: "k",
		Retry: llm.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}}
	cfg := Config{Client: cl, Model: "m", Dir: t.TempDir(), OnStep: (&recorder{}).step,
		Exec: ExecConfig{KillGrace: 200 * time.Millisecond}, ContextWindow: p.window, MaxOutputTokens: maxOut, ModelMaxOutput: modelMax}
	_, err := Run(context.Background(), cfg, "# Задача\nx")
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.asked...), err
}

// Повтор обрезанного хода увеличивает предел ответа только в пределах
// остатка окна (финальное ревью add-native-agent, P3-1; порт
// TestReproRetryExceedsWindow): окно 100k, вход 65k, предел ответа 20k,
// предел модели 60k — повтор просит не 40k, а то, что помещается.
func TestRetryRespectsContextWindow(t *testing.T) {
	p := &windowProvider{prompt: 65000, window: 100000}
	asked, err := p.run(t, 20000, 60000)
	if err != nil {
		t.Fatalf("повтор вышел за окно: %v (max_tokens %v)", err, asked)
	}
	if len(asked) != 2 || asked[1] <= 20000 || p.prompt+asked[1] > p.window {
		t.Fatalf("предел повтора: %v", asked)
	}
}

// Места в окне для большего ответа нет: повтора нет, обрезанный вызов —
// ErrTruncatedCall, а не переполнение окна у провайдера.
func TestRetryWithoutRoomFailsTruncated(t *testing.T) {
	p := &windowProvider{prompt: 79000, window: 100000}
	asked, err := p.run(t, 20000, 60000)
	if !errors.Is(err, ErrTruncatedCall) || errors.Is(err, ErrContextOverflow) || len(asked) != 1 {
		t.Fatalf("без места в окне: %v, запросы %v", err, asked)
	}
}

// Предел ответа не меньше окна (ручной каталог) не переполняет окно каждым
// ходом: ответу остаётся половина окна.
func TestMaxOutputClampedToWindow(t *testing.T) {
	c := Config{ContextWindow: 6000, MaxOutputTokens: 8000}
	c.defaults()
	if c.MaxOutputTokens != 3000 {
		t.Fatalf("предел ответа: %d", c.MaxOutputTokens)
	}
	c = Config{ContextWindow: 4000}
	c.defaults()
	if c.MaxOutputTokens != 2000 {
		t.Fatalf("умолчание 8192 при окне 4000: %d", c.MaxOutputTokens)
	}
}
