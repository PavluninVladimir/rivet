package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// OpenAI-совместимый сервер: список моделей, дополнение, отказ ключа,
// заголовки подключения доходят до провайдера.
func TestOpenAIClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Title") != "rivet" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","name":"Model 1"},{"id":"m2"},{"id":""}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Client{API: APIOpenAI, BaseURL: srv.URL + "/v1/", Key: "good", Headers: map[string]string{"X-Title": "rivet"}}
	models, err := c.ListModels(ctx)
	if err != nil || len(models) != 2 || models[0].Label != "Model 1" || models[1].ID != "m2" {
		t.Fatalf("models: %v %v", models, err)
	}
	text, err := c.Complete(ctx, "m1", "hi")
	if err != nil || text != "[]" {
		t.Fatalf("complete: %q %v", text, err)
	}
	c.Key = "bad"
	if _, err := c.ListModels(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ожидался ErrUnauthorized, получено %v", err)
	}
	if _, err := (Client{API: APIOpenAI}).ListModels(ctx); err == nil {
		t.Fatal("пустой base URL должен давать ошибку")
	}
}

// Anthropic через SDK с base URL тестового сервера: список моделей и отказ ключа.
func TestAnthropicClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-api-key") != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5","display_name":"Claude Opus 5","type":"model","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Client{API: APIAnthropic, BaseURL: srv.URL, Key: "good"}
	models, err := c.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].Label != "Claude Opus 5" {
		t.Fatalf("models: %v %v", models, err)
	}
	c.Key = "bad"
	if _, err := c.ListModels(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ожидался ErrUnauthorized, получено %v", err)
	}
}
