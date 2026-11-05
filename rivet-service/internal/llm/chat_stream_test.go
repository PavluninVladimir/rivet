package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseBlock — блок потока Anthropic: событие начала и дельты.
type sseBlock struct {
	start  string
	deltas []string
}

// writeSSE отдаёт ответ Anthropic потоком событий, как Messages API.
func writeSSE(w http.ResponseWriter, usage string, blocks []sseBlock, stop string, outTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name, data string) { _, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":`+usage+`}}`)
	for i, b := range blocks {
		ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, b.start))
		for _, d := range b.deltas {
			ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, d))
		}
		ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, outTokens))
	ev("message_stop", `{"type":"message_stop"}`)
}

func textBlock(t string) sseBlock {
	d, _ := json.Marshal(map[string]string{"type": "text_delta", "text": t})
	return sseBlock{start: `{"type":"text","text":""}`, deltas: []string{string(d)}}
}

func toolBlock(id, name, partial string) sseBlock {
	d, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": partial})
	return sseBlock{start: fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name), deltas: []string{string(d)}}
}

// Anthropic: поток, инструменты input_schema, tool_use в ответе, результаты
// инструментов склеены в одно сообщение пользователя, cache_control на
// системе и хвосте, usage с кешем, повтор 529, большой max_tokens.
func TestAnthropicChat(t *testing.T) {
	var got map[string]any
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		writeSSE(w, `{"input_tokens":100,"cache_read_input_tokens":900,"cache_creation_input_tokens":0,"output_tokens":1}`,
			[]sseBlock{textBlock("Запускаю тесты."), toolBlock("toolu_1", "run", `{"command":"npm test"}`)}, "tool_use", 30)
	}))
	defer srv.Close()
	var waits []time.Duration
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: noSleep(&waits, &mu)}
	r, err := c.Chat(context.Background(), ChatRequest{
		Model: "claude-x", System: "системное", MaxTokens: 64000,
		Tools: []ToolSpec{{Name: "run", Description: "команда", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
		Messages: []Message{
			{Role: RoleUser, Content: "задание"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_0", Name: "run", Arguments: json.RawMessage(`{"command":"ls"}`)}, {ID: "toolu_00", Name: "run", Arguments: json.RawMessage(`{"command":"pwd"}`)}}},
			{Role: RoleTool, ToolCallID: "toolu_0", Content: "a\nb"},
			{Role: RoleTool, ToolCallID: "toolu_00", Content: "/w", IsError: true},
			{Role: RoleUser, Content: "контекст от Rivet"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("529 с Retry-After повторяется: %v", waits)
	}
	if r.Stop != StopToolCalls || r.Text != "Запускаю тесты." || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "toolu_1" || !r.ToolCalls[0].Usable() {
		t.Fatalf("ответ: %+v", r)
	}
	if string(r.ToolCalls[0].Arguments) != `{"command":"npm test"}` || len(r.Raw) == 0 {
		t.Fatalf("аргументы из потока и Raw: %s %s", r.ToolCalls[0].Arguments, r.Raw)
	}
	if r.Usage.InputTokens != 1000 || r.Usage.CachedInputTokens != 900 || r.Usage.OutputTokens != 30 {
		t.Fatalf("usage: %+v", r.Usage)
	}
	if got["stream"] != true || got["max_tokens"].(float64) != 64000 {
		t.Fatalf("потоковый запрос с большим max_tokens: %v", got["stream"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("результаты и контекст должны быть одним сообщением пользователя: %d", len(msgs))
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 3 || last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["is_error"] != true || last[2].(map[string]any)["type"] != "text" {
		t.Fatalf("блоки последнего сообщения: %v", last)
	}
	if last[2].(map[string]any)["cache_control"] == nil {
		t.Fatalf("хвост истории без cache_control: %v", last[2])
	}
	sys := got["system"].([]any)[0].(map[string]any)
	if sys["text"] != "системное" || sys["cache_control"] == nil {
		t.Fatalf("system: %v", sys)
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "run" || tool["input_schema"].(map[string]any)["required"].([]any)[0] != "command" {
		t.Fatalf("инструмент: %v", tool)
	}
}

// Thinking-блок с подписью возвращается в следующем запросе без изменений,
// его текст доступен для транскрипта.
func TestAnthropicThinkingReplay(t *testing.T) {
	var bodies []map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		thinking := sseBlock{start: `{"type":"thinking","thinking":"","signature":""}`,
			deltas: []string{`{"type":"thinking_delta","thinking":"сначала тесты"}`, `{"type":"signature_delta","signature":"SIG-123"}`}}
		writeSSE(w, `{"input_tokens":10,"output_tokens":1}`, []sseBlock{thinking, toolBlock("toolu_1", "run", `{"command":"npm test"}`)}, "tool_use", 5)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Thinking != "сначала тесты" {
		t.Fatalf("текст thinking: %q", r.Thinking)
	}
	hist := []Message{{Role: RoleUser, Content: "x"}, {Role: RoleAssistant, ToolCalls: r.ToolCalls, Raw: r.Raw}, {Role: RoleTool, ToolCallID: "toolu_1", Content: "ok"}}
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: hist}); err != nil {
		t.Fatal(err)
	}
	asst := bodies[1]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	th := asst[0].(map[string]any)
	if th["type"] != "thinking" || th["signature"] != "SIG-123" || th["thinking"] != "сначала тесты" {
		t.Fatalf("thinking-блок должен вернуться как пришёл: %v", asst)
	}
}

// Обрезанный по max_tokens tool_use и отказ модели не исполняются;
// неизвестная модель — ErrModelUnsupported.
func TestAnthropicChatTruncatedRefusedErrors(t *testing.T) {
	mode := "max"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "max":
			// Частичный ввод — валидный JSON, но неполный объект.
			writeSSE(w, `{"input_tokens":5,"output_tokens":1}`, []sseBlock{toolBlock("t1", "write_file", `{"path":"a.txt"}`)}, "max_tokens", 10)
		case "refusal":
			writeSSE(w, `{"input_tokens":5,"output_tokens":1}`, []sseBlock{textBlock("Не могу."), toolBlock("t1", "run", `{"command":"rm -rf /"}`)}, "refusal", 3)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope not found"}}`))
		}
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "c", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || r.Stop != StopLength || len(r.ToolCalls) != 1 || !r.ToolCalls[0].Truncated || r.ToolCalls[0].Usable() {
		t.Fatalf("обрезанный вызов: %v %+v", err, r)
	}
	mode = "refusal"
	r, err = c.Chat(context.Background(), ChatRequest{Model: "c", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || r.Stop != StopRefused || len(r.ToolCalls) != 0 {
		t.Fatalf("отказ: %v %+v", err, r)
	}
	mode = "404"
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "claude-nope", Messages: []Message{{Role: RoleUser, Content: "x"}}}); !errors.Is(err, ErrModelUnsupported) {
		t.Fatalf("неизвестная модель Anthropic: %v", err)
	}
}

// SDK не подхватывает учётные данные и адрес из окружения runner'а.
func TestAnthropicIgnoresEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leaked-token")
	t.Setenv("ANTHROPIC_API_KEY", "leaked-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	var auth, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, key = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		writeSSE(w, `{"input_tokens":1,"output_tokens":1}`, []sseBlock{textBlock("ok")}, "end_turn", 1)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "own-key", Retry: RetryPolicy{MaxRetries: -1}}
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "c", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if key != "own-key" || strings.Contains(auth, "leaked") {
		t.Fatalf("учётные данные из окружения: key=%q auth=%q", key, auth)
	}
}

// OpenAI-совместимые: отказ фильтра, обрыв с вызовом, отвергнутый
// max_tokens, модерация, Ollama без инструментов, таймаут HTTP-клиента.
func TestOpenAIRefusalTruncationAndFallbacks(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var fields []string
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		calls++
		n := calls
		for _, f := range []string{"max_tokens", "max_completion_tokens"} {
			if _, ok := b[f]; ok {
				fields = append(fields, f)
			}
		}
		mu.Unlock()
		switch mode {
		case "filter":
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"content_filter","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{\"command\":\"x\"}"}}]}}]}`))
		case "length":
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a\"}"}}]}}]}`))
		case "param":
			if _, ok := b["max_tokens"]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`))
				return
			}
			_, _ = w.Write([]byte(openrouterText))
		case "moderation":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"Your input was flagged by moderation","code":403}}`))
		case "notools":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"registry.ollama.ai/library/gemma:2b does not support tools"}}`))
		case "slow":
			if n == 1 {
				time.Sleep(300 * time.Millisecond)
			}
			_, _ = w.Write([]byte(openrouterText))
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	c := Client{API: APIOpenAI, BaseURL: srv.URL, Retry: noSleep(&waits, &mu)}
	req := ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}

	mode = "filter"
	if r, err := c.Chat(context.Background(), req); err != nil || r.Stop != StopRefused || len(r.ToolCalls) != 0 {
		t.Fatalf("content_filter: %v %+v", err, r)
	}
	mode = "length"
	if r, err := c.Chat(context.Background(), req); err != nil || len(r.ToolCalls) != 1 || r.ToolCalls[0].Usable() {
		t.Fatalf("обрыв с вызовом: %v %+v", err, r)
	}
	mu.Lock()
	mode, fields = "param", nil
	mu.Unlock()
	if r, err := c.Chat(context.Background(), req); err != nil || r.Text != "Готово." || strings.Join(fields, ",") != "max_tokens,max_completion_tokens" {
		t.Fatalf("запасное поле предела: %v %v", err, fields)
	}
	mode = "moderation"
	if _, err := c.Chat(context.Background(), req); !errors.Is(err, ErrRefused) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("модерация не ошибка ключа: %v", err)
	}
	mode = "notools"
	if _, err := c.Chat(context.Background(), req); !errors.Is(err, ErrModelUnsupported) {
		t.Fatalf("модель без инструментов: %v", err)
	}
	mu.Lock()
	mode, calls, waits = "slow", 0, nil
	mu.Unlock()
	c.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
	if r, err := c.Chat(context.Background(), req); err != nil || r.Text != "Готово." || len(waits) != 1 {
		t.Fatalf("таймаут HTTP-клиента повторяется: %v %d", err, len(waits))
	}
	if f := (Client{BaseURL: "https://api.openai.com/v1"}).maxTokensField(); f != "max_completion_tokens" {
		t.Fatalf("OpenAI: %s", f)
	}
}
