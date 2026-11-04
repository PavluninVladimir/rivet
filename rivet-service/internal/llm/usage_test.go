package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CompleteWithUsage у OpenAI-совместимого провайдера: токены из usage,
// отсутствие usage — nil, обрыв по лимиту — ErrTruncated, а не текст.
func TestOpenAICompleteWithUsage(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Client{API: APIOpenAI, BaseURL: srv.URL}

	body = `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":30}}`
	text, u, err := c.CompleteWithUsage(ctx, "m", "hi", 0)
	if err != nil || text != "{}" || u.InputTokens == nil || *u.InputTokens != 120 || u.OutputTokens == nil || *u.OutputTokens != 30 {
		t.Fatalf("usage: %q %+v %v", text, u, err)
	}

	body = `{"choices":[{"message":{"content":"{}"}}]}`
	_, u, err = c.CompleteWithUsage(ctx, "m", "hi", 0)
	if err != nil || u.InputTokens != nil || u.OutputTokens != nil {
		t.Fatalf("без usage токены должны быть nil: %+v %v", u, err)
	}

	body = `{"choices":[{"message":{"content":"{\"findings\":[{\"te"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":16000}}`
	text, u, err = c.CompleteWithUsage(ctx, "m", "hi", 0)
	if !errors.Is(err, ErrTruncated) || text != "" || u.OutputTokens == nil || *u.OutputTokens != 16000 {
		t.Fatalf("обрыв: %q %+v %v", text, u, err)
	}
	if _, err := c.Complete(ctx, "m", "hi"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("Complete должен отдавать обрыв ошибкой: %v", err)
	}
}

// CompleteWithUsage у Anthropic: токены из usage, max_tokens — ErrTruncated.
func TestAnthropicCompleteWithUsage(t *testing.T) {
	stop := "end_turn"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			"content":[{"type":"text","text":"{\"findings\":[]}"}],"stop_reason":"` + stop + `",
			"usage":{"input_tokens":200,"output_tokens":40}}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k"}
	text, u, err := c.CompleteWithUsage(ctx, "claude-opus-5", "hi", 0)
	if err != nil || text != `{"findings":[]}` || u.InputTokens == nil || *u.InputTokens != 200 || *u.OutputTokens != 40 {
		t.Fatalf("usage: %q %+v %v", text, u, err)
	}
	for _, s := range []string{"max_tokens", "model_context_window_exceeded"} {
		stop = s
		text, _, err = c.CompleteWithUsage(ctx, "claude-opus-5", "hi", 0)
		if !errors.Is(err, ErrTruncated) || text != "" {
			t.Fatalf("обрыв %s: %q %v", s, text, err)
		}
	}
}

// Большой лимит ответа Anthropic запрашивается потоком: SDK не пускает
// обычный запрос, который может идти дольше 10 минут.
func TestAnthropicStreamingForLargeLimit(t *testing.T) {
	var gotStream bool
	var maxTokens float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotStream, _ = in["stream"].(bool)
		maxTokens, _ = in["max_tokens"].(float64)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":300,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"epics\":"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"[]}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":55}}`,
			`{"type":"message_stop"}`,
		}
		for _, e := range events {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(e), &head)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
		}
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k"}
	text, u, err := c.CompleteWithUsage(context.Background(), "claude-opus-5", "hi", 64000)
	if err != nil || text != `{"epics":[]}` || !gotStream || maxTokens != 64000 {
		t.Fatalf("поток: %q %v stream=%v max=%v", text, err, gotStream, maxTokens)
	}
	if u.InputTokens == nil || *u.InputTokens != 300 || u.OutputTokens == nil || *u.OutputTokens != 55 {
		t.Fatalf("usage потока: %+v", u)
	}
}

// OpenAI-совместимый провайдер с меньшим потолком ответа: повтор с
// потолком из текста ошибки (DeepSeek: 8192), понижение видно в Usage.
// Прерванный провайдером ответ — обрыв.
func TestOpenAIMaxTokensFallback(t *testing.T) {
	var limits []float64
	finish := "stop"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		limit, _ := in["max_tokens"].(float64)
		limits = append(limits, limit)
		if limit > 8192 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 8192]"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"` + finish + `"}]}`))
	}))
	defer srv.Close()
	c := Client{API: APIOpenAI, BaseURL: srv.URL}
	text, u, err := c.CompleteWithUsage(context.Background(), "deepseek-chat", "hi", 32000)
	if err != nil || text != "ok" || len(limits) != 2 || limits[1] != 8192 || u.MaxTokens != 8192 {
		t.Fatalf("повтор с потолком провайдера: %q %v %v max=%d", text, err, limits, u.MaxTokens)
	}
	finish = "insufficient_system_resource"
	if _, _, err := c.CompleteWithUsage(context.Background(), "m", "hi", 0); !errors.Is(err, ErrTruncated) {
		t.Fatalf("прерванный провайдером ответ — обрыв: %v", err)
	}
}

// Без потолка в тексте ошибки лимит спускается на шаг: 16000, затем 8192.
// Чужая 400 про неподдерживаемый max_tokens не повторяется (репро ревью).
func TestOpenAIMaxTokensStepDownAndForeign400(t *testing.T) {
	var limits []float64
	body := `{"error":{"message":"max_tokens is too large for this model"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		limit, _ := in["max_tokens"].(float64)
		limits = append(limits, limit)
		if limit > 8192 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := Client{API: APIOpenAI, BaseURL: srv.URL}
	if _, _, err := c.CompleteWithUsage(context.Background(), "m", "hi", 32000); err != nil || len(limits) != 3 || limits[1] != 16000 || limits[2] != 8192 {
		t.Fatalf("шаг вниз: %v %v", err, limits)
	}
	limits = nil
	body = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`
	if _, _, err := c.CompleteWithUsage(context.Background(), "o3", "hi", 32000); err == nil || len(limits) != 1 {
		t.Fatalf("повтор на чужую 400 бесполезен: %d запросов, %v", len(limits), err)
	}
}

func sse(w http.ResponseWriter, typ, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
	w.(http.Flusher).Flush()
}

// Поток закрыт без message_stop и без ошибки (прокси): обрыв связи с
// токенами message_start, а не пустой ответ (репро ревью).
func TestAnthropicStreamCutWithoutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`)
		sse(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sse(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"findings\": [{\"kind\": \"pain\""}}`)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k"}
	text, usage, err := c.CompleteWithUsage(context.Background(), "claude-x", "p", 32000)
	if !errors.Is(err, ErrStreamCut) || text != "" || !IsTransient(err) {
		t.Fatalf("обрыв потока принят за ответ: %q %v", text, err)
	}
	if usage.InputTokens == nil || *usage.InputTokens != 1000 {
		t.Fatalf("токены message_start при обрыве: %+v", usage)
	}
}

// Ошибка посреди потока: токены message_start сохраняются, overloaded —
// временная ошибка (репро ревью).
func TestAnthropicStreamErrorKeepsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"usage":{"input_tokens":50000,"output_tokens":1}}}`)
		sse(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k"}
	_, usage, err := c.CompleteWithUsage(context.Background(), "claude-x", "p", 32000)
	if err == nil || usage.InputTokens == nil || *usage.InputTokens != 50000 {
		t.Fatalf("токены при ошибке потока: %+v %v", usage, err)
	}
	if !IsTransient(err) {
		t.Fatalf("overloaded посреди потока — временная ошибка: %v", err)
	}
}

// 400 Anthropic по max_tokens: повтор с потолком из текста ошибки (репро ревью).
func TestAnthropicMaxTokens400Retry(t *testing.T) {
	var limits []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		limit, _ := in["max_tokens"].(float64)
		limits = append(limits, limit)
		w.Header().Set("Content-Type", "application/json")
		if limit > 32000 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens for claude-opus-4-1"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
		sse(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sse(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		sse(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "k"}
	text, u, err := c.CompleteWithUsage(context.Background(), "claude-opus-4-1", "p", 64000)
	if err != nil || text != "ok" || len(limits) != 2 || limits[1] != 32000 || u.MaxTokens != 32000 {
		t.Fatalf("повтор Anthropic с потолком: %q %v %v max=%d", text, err, limits, u.MaxTokens)
	}
}
