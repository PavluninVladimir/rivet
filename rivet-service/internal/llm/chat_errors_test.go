package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ошибка overloaded_error посреди SSE-потока (HTTP 200) повторяется: второй
// запрос доходит до нормального ответа.
func TestMidStreamOverloadedRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		mu.Unlock()
		if n == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		writeSSE(w, `{"input_tokens":10,"output_tokens":1}`, []sseBlock{textBlock("ok")}, "end_turn", 2)
	}))
	defer srv.Close()
	var waits []time.Duration
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: noSleep(&waits, &mu)}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || r.Text != "ok" {
		t.Fatalf("после повтора ждём ok: err=%v text=%q", err, r.Text)
	}
	if calls != 2 {
		t.Fatalf("ожидается повтор, запросов: %d", calls)
	}
}

// Пустой ответ Anthropic (end_turn без блоков) не сохраняется в Raw: его
// нельзя вернуть в истории (Anthropic отвергает пустое сообщение).
func TestEmptyAnthropicResponseNoRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"input_tokens":10,"output_tokens":1}`, nil, "end_turn", 0)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Raw) != 0 || r.Text != "" || len(r.ToolCalls) != 0 {
		t.Fatalf("пустой ответ: raw=%s text=%q calls=%d", r.Raw, r.Text, len(r.ToolCalls))
	}
}

// Нештатные причины остановки — ошибка (обрыв), а не ответ: вызовы не
// исполняются. Переполнение окна — ErrContextWindow.
func TestAbnormalStopReasons(t *testing.T) {
	// OpenAI-совместимые finish_reason.
	for _, fr := range []string{"insufficient_system_resource", "aborted", "error"} {
		body := fmt.Sprintf(`{"choices":[{"finish_reason":%q,"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.go\",\"content\":\"x\"}"}}]}}]}`, fr)
		r, err := parseOpenAIChat([]byte(body))
		if err == nil {
			t.Fatalf("%s: ожидается ошибка, получено %+v", fr, r)
		}
		if !retryable(err) {
			t.Fatalf("%s: прерванная генерация должна повторяться: %v", fr, err)
		}
	}
	// Переполнение окна у OpenAI-совместимых и Anthropic.
	r, err := parseOpenAIChat([]byte(`{"choices":[{"finish_reason":"model_context_window_exceeded","message":{"content":""}}]}`))
	if !errors.Is(err, ErrContextWindow) {
		t.Fatalf("окно: %v %+v", err, r)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"input_tokens":10,"output_tokens":1}`, []sseBlock{toolBlock("toolu_1", "run", `{"command":"make test"}`)}, "model_context_window_exceeded", 2)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	_, err = c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, ErrContextWindow) {
		t.Fatalf("anthropic окно: %v", err)
	}
}

// Поток Anthropic, оборванный чистым EOF без message_stop, даёт
// повторяемую ошибку, а не ответ с готовым текстом.
func TestStreamCleanEOFIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
		fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"VERDICT: APPROVED"}}`)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatalf("обрыв без message_stop должен быть ошибкой: %+v", r)
	}
	if !retryable(err) {
		t.Fatalf("обрыв потока должен повторяться: %v", err)
	}
}

// Anthropic 404 на неизвестную модель типизируется как «модель не
// поддерживается».
func TestAnthropicUnknownModelTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"},"request_id":"req_1"}`))
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: RetryPolicy{MaxRetries: -1}}
	_, err := c.Chat(context.Background(), ChatRequest{Model: "claude-nope", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, ErrModelUnsupported) {
		t.Fatalf("ожидается ErrModelUnsupported: %v", err)
	}
}

// Ответ 200 с объектом error (OpenRouter) и кодом 5xx повторяется;
// переполнение окна в теле 200 типизируется.
func TestOpenAI200ErrorObject(t *testing.T) {
	_, err := parseOpenAIChat([]byte(`{"error":{"code":502,"message":"Provider returned error"}}`))
	if !retryable(err) {
		t.Fatalf("200 с кодом 502 должен повторяться: %v", err)
	}
	_, err = parseOpenAIChat([]byte(`{"error":{"code":400,"message":"This model's maximum context length is 1000 tokens"}}`))
	if !errors.Is(err, ErrContextWindow) {
		t.Fatalf("переполнение в теле 200: %v", err)
	}
}

// reasoning_content возвращается обратно во всех сообщениях ассистента,
// включая ход до сообщения пользователя (DeepSeek thinking с инструментами).
func TestReasoningEchoedBack(t *testing.T) {
	msgs := openaiMessages(ChatRequest{Messages: []Message{
		{Role: RoleUser, Content: "задание"},
		{Role: RoleAssistant, Content: "думаю", Reasoning: "R1"},
		{Role: RoleUser, Content: "напоминание"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "run", Arguments: []byte(`{"command":"ls"}`)}}, Reasoning: "R2"},
		{Role: RoleTool, ToolCallID: "c", Content: "ok"},
	}})
	n := 0
	for _, m := range msgs {
		if m.ReasoningContent != "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("reasoning_content должен уйти в обоих ходах ассистента, нашлось %d", n)
	}
}

// Ошибка run с ненулевым кодом помечается текстом «ошибка:» и в
// anthropic-ветке (DeepSeek игнорирует is_error).
func TestAnthropicToolResultErrorPrefix(t *testing.T) {
	msgs, err := anthropicMessages([]Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "run", Arguments: []byte(`{"command":"false"}`)}}},
		{Role: RoleTool, ToolCallID: "c", Content: "код выхода 1", IsError: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil && strings.HasPrefix(c.OfText.Text, "ошибка:") {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("tool_result ошибки должен нести текст «ошибка:»")
	}
}

// Токены оборванной попытки (usage из message_start пришёл до ошибки
// потока) входят в расход вызова (Billed), но не в Usage успешной попытки:
// по Usage цикл оценивает окно контекста.
func TestUsageIncludesInterruptedAttempts(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		mu.Unlock()
		if n == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":700,"output_tokens":1}}}`)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		writeSSE(w, `{"input_tokens":300,"output_tokens":1}`, []sseBlock{textBlock("ok")}, "end_turn", 20)
	}))
	defer srv.Close()
	var waits []time.Duration
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: noSleep(&waits, &mu)}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage.InputTokens != 300 || r.Retried.InputTokens != 700 {
		t.Fatalf("вход успешной попытки %d (ждём 300), неудачных %d (ждём 700)", r.Usage.InputTokens, r.Retried.InputTokens)
	}
	if b := r.Billed(); b.InputTokens != 1000 || b.OutputTokens != 21 {
		t.Fatalf("к оплате по обеим попыткам: %+v, ждём вход 700+300", b)
	}
}

// Ни одна попытка не удалась: весь расход — в Retried вместе с ошибкой,
// Usage пуст (успешной попытки не было).
func TestUsageOfFailedCallIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":400,"output_tokens":1}}}`)
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var waits []time.Duration
	retry := noSleep(&waits, &mu)
	retry.MaxRetries = 2
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k", Retry: retry}
	r, err := c.Chat(context.Background(), ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("ждём ошибку после исчерпания повторов")
	}
	if r.Usage != (ChatUsage{}) || r.Retried.InputTokens != 1200 || r.Billed().InputTokens != 1200 {
		t.Fatalf("usage неудачного вызова: %+v / %+v, ждём 3×400 в Retried", r.Usage, r.Retried)
	}
}
