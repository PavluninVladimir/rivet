package scm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Подписка на webhook'и перечисляет все события, которые умеет обрабатывать
// конвейер (change fix-hosting-events): без этого хостинг их не шлёт.
func TestRegisterWebhookSubscribesAllEvents(t *testing.T) {
	var ghEvents []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
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
	for _, want := range []string{"pull_request", "pull_request_review", "workflow_run", "check_suite", "push"} {
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
	for _, want := range []string{"merge_requests_events", "pipeline_events", "note_events", "push_events"} {
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
	// Сравниваем состав, а не длину: четыре чужих события прошли бы
	// проверку по количеству.
	want := map[string]bool{}
	for _, ev := range hookEvents {
		want[ev] = true
	}
	for _, ev := range patched {
		delete(want, ev)
	}
	if len(want) != 0 || len(patched) != len(hookEvents) {
		t.Fatalf("обновление должно переподписать ровно на %v, получили %v", hookEvents, patched)
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
	for _, want := range []string{"merge_requests_events", "pipeline_events", "note_events", "push_events"} {
		if glPut[want] != true {
			t.Fatalf("обновление GitLab без %q: %+v", want, glPut)
		}
	}
}

// Push GitLab подписывается только на основную ветку проекта
// (add-architecture-c4): пуши веток задач хостинг не шлёт.
func TestGitLabPushBranchFilter(t *testing.T) {
	var body map[string]any
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v4/projects/g%2Fp":
			_, _ = w.Write([]byte(`{"default_branch":"trunk"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		default:
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		}
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if _, err := g.RegisterWebhook(context.Background(), "g/p", "https://rivet/hook", "s"); err != nil {
		t.Fatal(err)
	}
	if body["push_events"] != true || body["push_events_branch_filter"] != "trunk" {
		t.Fatalf("push GitLab по основной ветке: %+v", body)
	}
}

// 403 из-за лимита запросов GitHub — ошибка ErrRateLimited, а не «нет
// прав» (ревью Opus, круг 2): переподписка повторит позже.
func TestRegisterWebhookRateLimited(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	ok, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s")
	if ok || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("лимит запросов: %v %v", ok, err)
	}
	// Вторичный лимит без заголовков, только текстом (ревью Opus, круг 3).
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`))
	}))
	defer secondary.Close()
	a.Base = secondary.URL
	if ok, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s"); ok || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("вторичный лимит без заголовков: %v %v", ok, err)
	}
	// У существующего хука один мутирующий запрос: GET и PATCH, без POST.
	var methods []string
	existing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":7,"config":{"url":"https://rivet/hook"}}]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer existing.Close()
	a.Base = existing.URL
	if ok, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s"); !ok || err != nil || strings.Join(methods, ",") != "GET,PATCH" {
		t.Fatalf("существующий хук: %v %v %v", ok, err, methods)
	}
	// 403 без признаков лимита — по-прежнему «нет прав».
	noRights := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer noRights.Close()
	a.Base = noRights.URL
	if ok, err := a.RegisterWebhook(context.Background(), "o/r", "https://rivet/hook", "s"); ok || err != nil {
		t.Fatalf("нет прав: %v %v", ok, err)
	}
}
