package scm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Подписка на webhook'и перечисляет все события, которые умеет обрабатывать
// конвейер (change fix-hosting-events): без этого хостинг их не шлёт.
func TestRegisterWebhookSubscribesAllEvents(t *testing.T) {
	var ghEvents []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Events []string `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		ghEvents = in.Events
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	if _, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pull_request", "pull_request_review", "workflow_run", "check_suite"} {
		var found bool
		for _, got := range ghEvents {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("подписка GitHub без события %q: %v", want, ghEvents)
		}
	}

	var glBody map[string]any
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&glBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if _, err := g.RegisterWebhook(context.Background(), "g/p", "https://rivet/hook", "s"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"merge_requests_events", "pipeline_events", "note_events"} {
		if glBody[want] != true {
			t.Fatalf("подписка GitLab без %q: %+v", want, glBody)
		}
	}
}

// Обновление уже существующего хука тоже переподписывает его на все
// события: у старого хука остался прежний список.
func TestUpdateWebhookSubscribesAllEvents(t *testing.T) {
	var patched []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Hook already exists on this repository"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":7,"config":{"url":"https://rivet/hook"}}]`))
		default: // PATCH
			var in struct {
				Events []string `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			patched = in.Events
			_, _ = w.Write([]byte(`{"id":7}`))
		}
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	ok, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s")
	if err != nil || !ok {
		t.Fatalf("обновление хука: %v %v", ok, err)
	}
	if len(patched) != len(hookEvents) {
		t.Fatalf("обновление должно переподписать на все события: %v", patched)
	}

	var glPut map[string]any
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":3,"url":"https://rivet/hook"}]`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&glPut)
		_, _ = w.Write([]byte(`{"id":3}`))
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if _, err := g.RegisterWebhook(context.Background(), "g/p", "https://rivet/hook", "s"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"merge_requests_events", "pipeline_events", "note_events"} {
		if glPut[want] != true {
			t.Fatalf("обновление GitLab без %q: %+v", want, glPut)
		}
	}
}
