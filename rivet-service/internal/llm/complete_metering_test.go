package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func usageValue(n int64) *int64 { return &n }

func TestCompleteUsagePresenceAndCache(t *testing.T) {
	cases := []struct {
		name       string
		api        API
		body       string
		in, out    *int64
		cached     *int64
		write      *int64
		incomplete bool
	}{
		{"openai missing output", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10}}`, usageValue(10), nil, nil, nil, false},
		{"openai explicit zero", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`, usageValue(10), usageValue(0), nil, nil, false},
		{"openai negative counter", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":-1,"completion_tokens":5}}`, nil, usageValue(5), nil, nil, false},
		{"openai reported cache zero", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}`, usageValue(10), usageValue(5), usageValue(0), nil, false},
		{"openai reported cache", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":4}}`, usageValue(10), usageValue(5), usageValue(4), nil, false},
		{"openai contradictory cache", APIOpenAI, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`, usageValue(10), usageValue(5), usageValue(4), nil, true},
		{"anthropic missing usage", APIAnthropic, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`, nil, nil, nil, nil, true},
		{"anthropic missing output", APIAnthropic, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, usageValue(10), nil, usageValue(0), usageValue(0), false},
		{"anthropic explicit zero", APIAnthropic, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, usageValue(10), usageValue(0), usageValue(0), usageValue(0), false},
		{"anthropic total includes cache", APIAnthropic, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}`, usageValue(16), usageValue(5), usageValue(4), usageValue(2), false},
		{"anthropic cache split absent", APIAnthropic, `{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, usageValue(10), usageValue(5), nil, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, use, err := (Client{API: tc.api, BaseURL: srv.URL}).CompleteWithUsage(context.Background(), "m", "p", 100)
			if err != nil {
				t.Fatal(err)
			}
			for name, pair := range map[string][2]*int64{"input": {use.InputTokens, tc.in}, "output": {use.OutputTokens, tc.out}, "cached": {use.CachedInputTokens, tc.cached}, "write": {use.CacheWriteTokens, tc.write}} {
				if (pair[0] == nil) != (pair[1] == nil) || (pair[0] != nil && *pair[0] != *pair[1]) {
					t.Fatalf("%s presence/value: got=%v want=%v usage=%+v", name, pair[0], pair[1], use)
				}
			}
			if use.CostIncomplete != tc.incomplete {
				t.Fatalf("cost incomplete=%v want %v", use.CostIncomplete, tc.incomplete)
			}
		})
	}
}

func TestAnthropicCompleteStreamRequiresReportedFinalOutput(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(fmt.Sprintf("final-delta-%v", final), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
				if final {
					sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`)
				}
				sse(w, "message_stop", `{"type":"message_stop"}`)
			}))
			defer srv.Close()
			_, use, err := (Client{API: APIAnthropic, BaseURL: srv.URL}).CompleteWithUsage(context.Background(), "m", "p", 32000)
			if err != nil || use.InputTokens == nil || *use.InputTokens != 10 {
				t.Fatalf("stream: %+v %v", use, err)
			}
			if final {
				if use.OutputTokens == nil || *use.OutputTokens != 0 {
					t.Fatalf("explicit final0 lost: %+v", use)
				}
			} else if use.OutputTokens != nil {
				t.Fatalf("initial partial output became final cost evidence: %+v", use)
			}
		})
	}
}

func TestCompleteLimitRetriesDoNotPriceOnlyLastAttempt(t *testing.T) {
	for _, api := range []API{APIOpenAI, APIAnthropic} {
		t.Run(string(api), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens valid range is [1, 8192]"}}`))
					return
				}
				if api == APIOpenAI {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`))
				}
			}))
			defer srv.Close()
			_, use, err := (Client{API: api, BaseURL: srv.URL}).CompleteWithUsage(context.Background(), "m", "p", 16000)
			if err != nil || calls.Load() != 2 || !use.CostIncomplete || use.InputTokens == nil || *use.InputTokens != 10 {
				t.Fatalf("retried usage: calls=%d usage=%+v err=%v", calls.Load(), use, err)
			}
		})
	}
}
