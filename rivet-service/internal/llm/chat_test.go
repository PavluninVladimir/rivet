package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Записанные ответы провайдеров (форма полей сокращена до значимых): у
// DeepSeek аргументы строкой, reasoning_content и prompt_cache_hit_tokens; у
// Ollama аргументы объектом, без id и с finish_reason «stop»; у OpenRouter —
// prompt_tokens_details.cached_tokens.
const (
	deepseekToolCall = `{"id":"x","model":"deepseek-v4-flash","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","reasoning_content":"нужно прочитать файл","tool_calls":[{"id":"call_0_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"src/main.js\"}"}}]}}],"usage":{"prompt_tokens":1200,"completion_tokens":40,"prompt_cache_hit_tokens":1024,"prompt_cache_miss_tokens":176}}`
	ollamaToolCall   = `{"model":"qwen3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"run","arguments":{"command":"npm test"}}}]}}],"usage":{"prompt_tokens":300,"completion_tokens":12}}`
	openrouterText   = `{"model":"anthropic/claude","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Готово."}}],"usage":{"prompt_tokens":900,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":800}}}`
	lengthCut        = `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"начало ответа"}}],"usage":{"prompt_tokens":10,"completion_tokens":8192}}`
)

func noSleep(rec *[]time.Duration, mu *sync.Mutex) RetryPolicy {
	return RetryPolicy{Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		*rec = append(*rec, d)
		mu.Unlock()
		return nil
	}}
}

func TestOpenAIChatProviders(t *testing.T) {
	cases := []struct {
		name, body string
		check      func(t *testing.T, r ChatResponse)
	}{
		{"deepseek", deepseekToolCall, func(t *testing.T, r ChatResponse) {
			if r.Stop != StopToolCalls || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_0_a" || r.ToolCalls[0].Name != "read_file" {
				t.Fatalf("вызов: %+v", r)
			}
			if string(r.ToolCalls[0].Arguments) != `{"path":"src/main.js"}` {
				t.Fatalf("аргументы строкой должны стать объектом: %s", r.ToolCalls[0].Arguments)
			}
			if r.Reasoning != "нужно прочитать файл" || r.Usage.CachedInputTokens != 1024 || r.Usage.InputTokens != 1200 {
				t.Fatalf("reasoning/usage: %+v", r)
			}
		}},
		{"ollama", ollamaToolCall, func(t *testing.T, r ChatResponse) {
			if r.Stop != StopToolCalls || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_0" {
				t.Fatalf("вызов без id и со stop: %+v", r)
			}
			if !ValidArgs(r.ToolCalls[0].Arguments) || !strings.Contains(string(r.ToolCalls[0].Arguments), "npm test") {
				t.Fatalf("аргументы объектом: %s", r.ToolCalls[0].Arguments)
			}
		}},
		{"openrouter", openrouterText, func(t *testing.T, r ChatResponse) {
			if r.Stop != StopEnd || r.Text != "Готово." || r.Usage.CachedInputTokens != 800 {
				t.Fatalf("текст: %+v", r)
			}
		}},
		{"length", lengthCut, func(t *testing.T, r ChatResponse) {
			if r.Stop != StopLength || r.Text != "начало ответа" {
				t.Fatalf("обрыв: %+v", r)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := Client{API: APIOpenAI, BaseURL: srv.URL, Key: "k"}
			r, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "привет"}}})
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, r)
		})
	}
}

// Форма запроса: system первым, вызовы ассистента с аргументами строкой,
// результаты инструментов ролью tool, reasoning возвращается, инструменты
// в формате function.
func TestOpenAIChatRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("запрос: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(openrouterText))
	}))
	defer srv.Close()
	c := Client{API: APIOpenAI, BaseURL: srv.URL, Key: "k"}
	_, err := c.Chat(context.Background(), ChatRequest{
		Model: "m", System: "системное", MaxTokens: 100,
		Tools: []ToolSpec{{Name: "read_file", Description: "читает", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
		Messages: []Message{
			{Role: RoleUser, Content: "задание"},
			{Role: RoleAssistant, Reasoning: "думаю", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)}}},
			{Role: RoleTool, ToolCallID: "c1", ToolName: "read_file", Content: "нет файла", IsError: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("сообщения: %v", msgs)
	}
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "думаю" {
		t.Fatalf("reasoning не вернулся: %v", asst)
	}
	call := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["arguments"] != `{"path":"a"}` {
		t.Fatalf("аргументы должны уйти строкой: %#v", call["arguments"])
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || !strings.HasPrefix(tool["content"].(string), "ошибка") {
		t.Fatalf("результат инструмента: %v", tool)
	}
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read_file" || got["max_tokens"].(float64) != 100 {
		t.Fatalf("инструменты: %v", got["tools"])
	}
}

func TestChatRetries(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		_, _ = w.Write([]byte(openrouterText))
	}))
	defer srv.Close()
	var waits []time.Duration
	var retries []int
	pol := noSleep(&waits, &mu)
	pol.OnRetry = func(a int, _ time.Duration, _ error) { retries = append(retries, a) }
	c := Client{API: APIOpenAI, BaseURL: srv.URL, Retry: pol}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || r.Text != "Готово." {
		t.Fatalf("после двух 429 ответ должен пройти: %v %+v", err, r)
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 2*time.Second || len(retries) != 2 {
		t.Fatalf("Retry-After в приоритете: %v %v", waits, retries)
	}
}

func TestChatRetryExhaustedAndNonRetryable(t *testing.T) {
	var mu sync.Mutex
	status, body := 0, ""
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	run := func(st int, b string) (int, []time.Duration, error) {
		status, body, calls = st, b, 0
		var waits []time.Duration
		c := Client{API: APIOpenAI, BaseURL: srv.URL, Retry: noSleep(&waits, &mu)}
		_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
		return calls, waits, err
	}

	n, waits, err := run(http.StatusServiceUnavailable, "busy")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 || n != 6 || !strings.Contains(err.Error(), "5 повторов") {
		t.Fatalf("5xx: пять повторов и ошибка инфраструктуры: %v calls=%d", err, n)
	}
	// Экспоненциальная пауза с джиттером ±20%: 1, 2, 4, 8, 16 с.
	for i, w := range waits {
		want := time.Second << i
		if w < want*8/10 || w > want*12/10 {
			t.Fatalf("пауза %d: %v, ожидалось около %v", i, w, want)
		}
	}
	if n, _, err := run(http.StatusUnauthorized, "bad key"); !errors.Is(err, ErrUnauthorized) || n != 1 {
		t.Fatalf("401 не повторяется: %v %d", err, n)
	}
	if n, _, err := run(http.StatusBadRequest, `{"error":{"message":"Model Not Exist"}}`); !errors.Is(err, ErrModelUnsupported) || n != 1 {
		t.Fatalf("неизвестная модель: %v %d", err, n)
	}
	if n, _, err := run(http.StatusBadRequest, `{"error":{"message":"bad tool schema"}}`); err == nil || errors.Is(err, ErrModelUnsupported) || n != 1 {
		t.Fatalf("400 без модели не повторяется и не «модель»: %v %d", err, n)
	}
}

func TestChatCancelStopsRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := Client{API: APIOpenAI, BaseURL: srv.URL, Retry: RetryPolicy{Sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}}}
	_, err := c.Chat(ctx, ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена прерывает повторы: %v", err)
	}
}

func TestNormalizeArgs(t *testing.T) {
	for in, want := range map[string]string{
		`"{\"a\":1}"`: `{"a":1}`,
		`{"a":1}`:     `{"a":1}`,
		``:            `{}`,
		`null`:        `{}`,
		`""`:          `{}`,
		`{"a":`:       `{"a":`,
	} {
		if got := string(normalizeArgs(json.RawMessage(in))); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if ValidArgs(json.RawMessage(`{"a":`)) || !ValidArgs(json.RawMessage(`{}`)) {
		t.Fatal("ValidArgs")
	}
}
