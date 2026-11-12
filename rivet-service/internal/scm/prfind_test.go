package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Открытый PR ветки задачи находится только из ветки самого репозитория в
// базовую ветку задачи: PR из форка и PR в другую ветку не подходят, а
// несколько подходящих — ошибка, выбирать нельзя (fix-pr-without-test-step).
func TestOpenPR(t *testing.T) {
	ghBody := map[string]string{
		"agent/task-1": `[{"number":7,"html_url":"https://github.com/o/r/pull/7","head":{"ref":"agent/task-1","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}]`,
		// Хостинг вернул PR в другую базовую ветку и из форка: не подходят.
		"agent/task-2": `[{"number":8,"html_url":"u8","head":{"ref":"agent/task-2","repo":{"full_name":"o/r"}},"base":{"ref":"release"}},
			{"number":9,"html_url":"u9","head":{"ref":"agent/task-2","repo":{"full_name":"evil/r"}},"base":{"ref":"main"}}]`,
		"agent/task-3": `[{"number":10,"html_url":"u10","head":{"ref":"agent/task-3","repo":{"full_name":"o/r"}},"base":{"ref":"main"}},
			{"number":11,"html_url":"u11","head":{"ref":"agent/task-3","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}]`,
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/o/r/pulls" || q.Get("state") != "open" || q.Get("base") != "main" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for branch, body := range ghBody {
			if q.Get("head") == "o:"+branch {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	ctx := context.Background()
	if pr, ok, err := a.OpenPR(ctx, "o/r", "agent/task-1", "main"); err != nil || !ok || pr.Number != 7 {
		t.Fatalf("github: %+v %v %v", pr, ok, err)
	}
	if _, ok, err := a.OpenPR(ctx, "o/r", "agent/task-2", "main"); err != nil || ok {
		t.Fatalf("github: PR в другую базу и из форка не подходят: %v %v", ok, err)
	}
	if _, _, err := a.OpenPR(ctx, "o/r", "agent/task-3", "main"); !errors.Is(err, ErrAmbiguousPR) {
		t.Fatalf("github: несколько кандидатов — ошибка: %v", err)
	}
	if _, ok, err := a.OpenPR(ctx, "o/r", "agent/task-4", "main"); err != nil || ok {
		t.Fatalf("github без PR: %v %v", ok, err)
	}

	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "opened" || q.Get("target_branch") != "main" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch q.Get("source_branch") {
		case "agent/task-1":
			// MR из форка с той же веткой и свой MR.
			_, _ = w.Write([]byte(`[{"iid":4,"web_url":"fork","source_branch":"agent/task-1","target_branch":"main","source_project_id":99,"target_project_id":5},
				{"iid":3,"web_url":"own","source_branch":"agent/task-1","target_branch":"main","source_project_id":5,"target_project_id":5}]`))
		case "agent/task-2":
			_, _ = w.Write([]byte(`[{"iid":6,"web_url":"fork","source_branch":"agent/task-2","target_branch":"main","source_project_id":99,"target_project_id":5}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if pr, ok, err := g.OpenPR(ctx, "o/r", "agent/task-1", "main"); err != nil || !ok || pr.Number != 3 || pr.URL != "own" {
		t.Fatalf("gitlab: свой MR, а не форк: %+v %v %v", pr, ok, err)
	}
	if _, ok, err := g.OpenPR(ctx, "o/r", "agent/task-2", "main"); err != nil || ok {
		t.Fatalf("gitlab: MR из форка не подходит: %v %v", ok, err)
	}
}

// Временный сбой хостинга отличается от отказа, который повтор не исправит.
func TestTransient(t *testing.T) {
	for err, want := range map[error]bool{
		errors.New("github create PR: 502: bad gateway"):                                             true,
		errors.New("gitlab create MR: 503: unavailable"):                                             true,
		errors.New("github create PR: 429: rate"):                                                    true,
		errors.New("github create PR: 422: No commits between main and agent/x"):                     false,
		errors.New("github create PR: 403: forbidden"):                                               false,
		&RateLimitError{Msg: "github create PR: лимит запросов хостинга: 403: secondary rate limit"}: true,
		fmt.Errorf("create PR: %w", errors.New("github create PR: 502: x")):                          true,
		fmt.Errorf("post: %w", context.DeadlineExceeded):                                             true,
		nil: false,
	} {
		if got := Transient(err); got != want {
			t.Errorf("Transient(%v) = %v, ждали %v", err, got, want)
		}
	}
}

// Отказ «PR уже есть» адаптер узнаёт по коду ответа и разобранному телу:
// GitHub отдаёт JSON с отступами (422), GitLab — 409 со списком сообщений.
// Прочие 422 и 409 этим отказом не считаются.
func TestCreatePRExists(t *testing.T) {
	responses := map[string]struct {
		code int
		body string
	}{
		"agent/exists": {http.StatusUnprocessableEntity, `{
  "message": "Validation Failed",
  "errors": [
    {
      "resource": "PullRequest",
      "code": "custom",
      "message": "A pull request already exists for o:agent/exists."
    }
  ],
  "documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
  "status": "422"
}`},
		"agent/empty": {http.StatusUnprocessableEntity, `{
  "message": "Validation Failed",
  "errors": [
    {
      "resource": "PullRequest",
      "code": "custom",
      "message": "No commits between main and agent/empty"
    }
  ],
  "status": "422"
}`},
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Head string `json:"head"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		resp := responses[in.Head]
		w.WriteHeader(resp.code)
		_, _ = w.Write([]byte(resp.body))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	ctx := context.Background()
	if _, err := a.CreatePR(ctx, "o/r", "agent/exists", "main", "t", ""); !errors.Is(err, ErrPRExists) {
		t.Fatalf("github: многострочный 422 «already exists»: %v", err)
	}
	if _, err := a.CreatePR(ctx, "o/r", "agent/empty", "main", "t", ""); err == nil || errors.Is(err, ErrPRExists) || Transient(err) {
		t.Fatalf("github: 422 «No commits between» — другой отказ: %v", err)
	}

	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Source string `json:"source_branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.WriteHeader(http.StatusConflict)
		if in.Source == "agent/exists" {
			_, _ = w.Write([]byte("{\n  \"message\": [\n    \"Another open merge request already exists for this source branch: !3\"\n  ]\n}"))
			return
		}
		_, _ = w.Write([]byte("{\n  \"message\": \"Branch is locked\"\n}"))
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if _, err := g.CreatePR(ctx, "o/r", "agent/exists", "main", "t", ""); !errors.Is(err, ErrPRExists) {
		t.Fatalf("gitlab: многострочный 409 «already exists»: %v", err)
	}
	if _, err := g.CreatePR(ctx, "o/r", "agent/other", "main", "t", ""); err == nil || errors.Is(err, ErrPRExists) {
		t.Fatalf("gitlab: другой 409: %v", err)
	}
}

// Вторичный лимит GitHub на создание PR (403 с текстом или Retry-After) и
// 429 — временный отказ с паузой хостинга, а 403 по правам — нет.
func TestCreatePRRateLimited(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Head string `json:"head"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch in.Head {
		case "agent/secondary":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("{\n  \"message\": \"You have exceeded a secondary rate limit and have been temporarily blocked from content creation. Please retry your request again later.\",\n  \"documentation_url\": \"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api\"\n}"))
		case "agent/retry-after":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "blocked"}`))
		case "agent/429":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
		}
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	ctx := context.Background()
	_, err := a.CreatePR(ctx, "o/r", "agent/secondary", "main", "t", "")
	if !errors.Is(err, ErrRateLimited) || !Transient(err) || RetryAfter(err) != 0 {
		t.Fatalf("вторичный лимит без заголовка: %v", err)
	}
	if _, err := a.CreatePR(ctx, "o/r", "agent/retry-after", "main", "t", ""); !Transient(err) || RetryAfter(err) != 7*time.Second {
		t.Fatalf("лимит с Retry-After: %v, пауза %v", err, RetryAfter(err))
	}
	if _, err := a.CreatePR(ctx, "o/r", "agent/429", "main", "t", ""); !Transient(err) {
		t.Fatalf("429: %v", err)
	}
	if _, err := a.CreatePR(ctx, "o/r", "agent/forbidden", "main", "t", ""); err == nil || Transient(err) {
		t.Fatalf("403 по правам повтором не лечится: %v", err)
	}
}
