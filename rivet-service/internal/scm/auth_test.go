package scm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Отказ хостинга по авторизации различается одинаково на всех адаптерах
// (change fix-credential-state): 401 — ErrUnauthorized, 403 —
// ErrForbidden, лимит запросов и прочие сбои — ни то, ни другое.

// authOps — операции конвейера, которые видят отказ хостинга.
func authOps(a Adapter) map[string]func() error {
	ctx := context.Background()
	ops := map[string]func() error{

		"CreatePR": func() error { _, err := a.CreatePR(ctx, "own/proj", "b", "main", "t", ""); return err },
		"Diff":     func() error { _, err := a.Diff(ctx, "own/proj", 7); return err },
		"Merge":    func() error { _, err := a.Merge(ctx, "own/proj", 7); return err },
		"HeadSHA":  func() error { _, err := a.HeadSHA(ctx, "own/proj", "main"); return err },
		"ReadFile": func() error { _, err := a.ReadFile(ctx, "own/proj", "main", "a.txt"); return err },
		"WriteFile": func() error {
			_, err := a.WriteFile(ctx, "own/proj", "main", "a.txt", "", "x", "m")
			return err
		},
		"BranchProtected": func() error { _, err := a.BranchProtected(ctx, "own/proj", "main"); return err },
		"TriggerPipeline": func() error {
			_, err := a.TriggerPipeline(ctx, "own/proj", "deploy.yml", "main", nil)
			return err
		},
		"PRFiles": func() error { _, err := a.(PRFileLister).PRFiles(ctx, "own/proj", 7); return err },
		"PathSHA": func() error { _, err := a.(TreeHasher).PathSHA(ctx, "own/proj", "main", "docs/x"); return err },
	}
	if finder, ok := a.(PRFinder); ok {
		ops["OpenPR"] = func() error { _, _, err := finder.OpenPR(ctx, "own/proj", "b", "main"); return err }
	}
	return ops
}

// statusServer отвечает одним кодом на любой запрос.
func statusServer(t *testing.T, code int, body string, hdr map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func authAdapters(base string) map[string]Adapter {
	return map[string]Adapter{
		"github": &GitHub{Token: "t", Client: httpClient(), Base: base, Web: base},
		"gitlab": &GitLab{Token: "t", Client: httpClient(), Base: base},
		"gitea":  &Gitea{Token: "t", Client: httpClient(), Base: base},
	}
}

func TestAdapterAuthRefusal(t *testing.T) {
	// Операции, у которых ответ разобран по смыслу: «нет защиты» и «нет
	// каталога» без ошибки, «нет файла» — своя ошибка.
	quiet := map[int]map[string]bool{
		403: {"BranchProtected": true},
		404: {"BranchProtected": true, "PathSHA": true},
	}
	for _, tc := range []struct {
		name                        string
		code                        int
		body                        string
		hdr                         map[string]string
		unauth, forbidden, notFound bool
		rateLimitedGitHub           bool
	}{
		{"401", 401, `{"message":"Bad credentials"}`, nil, true, false, false, false},
		{"403", 403, `{"message":"Resource not accessible by personal access token"}`, nil, false, true, false, false},
		{"403 лимит", 403, `{"message":"API rate limit exceeded"}`, map[string]string{"X-RateLimit-Remaining": "0"}, false, false, false, true},
		{"404", 404, `{"message":"Not Found"}`, nil, false, false, true, false},
		{"502", 502, `{"message":"bad gateway"}`, nil, false, false, false, false},
	} {
		srv := statusServer(t, tc.code, tc.body, tc.hdr)
		for name, a := range authAdapters(srv.URL) {
			for op, call := range authOps(a) {
				t.Run(tc.name+"/"+name+"/"+op, func(t *testing.T) {
					err := call()
					githubLimit := tc.rateLimitedGitHub && name == "github"
					code := tc.code
					if tc.rateLimitedGitHub && !githubLimit {
						code = 403 // у GitLab лимит приходит 429, а 403 — отказ в доступе
					}
					if quiet[code][op] && !githubLimit {
						if err != nil {
							t.Fatalf("ответ разобран по смыслу — без ошибки: %v", err)
						}
						return
					}
					if err == nil {
						t.Fatal("ожидалась ошибка")
					}
					if code == 404 && op == "ReadFile" && name != "gitea" {
						if !errors.Is(err, ErrFileNotFound) || AccessRefused(err) {
							t.Fatalf("нет файла — своя ошибка: %v", err)
						}
						return
					}
					unauth, forbidden, notFound := tc.unauth, tc.forbidden, tc.notFound
					switch {
					case githubLimit:
						if !errors.Is(err, ErrRateLimited) {
							t.Fatalf("лимит запросов GitHub: %v", err)
						}
					case tc.rateLimitedGitHub:
						forbidden = true
					}
					// 401 на merge GitLab — запрет операции рабочему токену без
					// права merge, а не отзыв.
					if code == 401 && name == "gitlab" && op == "Merge" {
						unauth, forbidden = false, true
					}
					if errors.Is(err, ErrUnauthorized) != unauth || errors.Is(err, ErrForbidden) != forbidden || errors.Is(err, ErrNotFound) != notFound {
						t.Fatalf("401=%v 403=%v 404=%v, ожидалось %v %v %v: %v", errors.Is(err, ErrUnauthorized),
							errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound), unauth, forbidden, notFound, err)
					}
					if AccessRefused(err) != (unauth || forbidden || notFound) {
						t.Fatalf("AccessRefused: %v", err)
					}
				})
			}
		}
	}
}

// Текст ошибки прежний: код и ответ хостинга.
func TestStatusErrKeepsMessage(t *testing.T) {
	err := statusErr("github create PR", 401, []byte(`{"message":"Bad credentials"}`))
	if err.Error() != `github create PR: 401: {"message":"Bad credentials"}` || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ошибка: %v", err)
	}
	if err := statusErr("gitlab merge", 500, []byte("x")); AccessRefused(err) {
		t.Fatalf("500 не отказ по авторизации: %v", err)
	}
}

// Fake отзывает доступ к репозиторию из теста, а токен — из теста и по
// файлу стенда: операции получают 401, проверка — «токен не принят».
// Замена токена другим возвращает доступ, хотя файл на месте.
func TestFakeRevokedCredentials(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.SetRevoked("own/proj", true)
	for op, call := range authOps(f) {
		if err := call(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: ожидался 401: %v", op, err)
		}
	}
	if res := f.Probe(ctx, "own/proj"); res.OK || res.Reason != ReasonBadToken {
		t.Fatalf("проверка отозванного: %+v", res)
	}
	if _, err := f.HeadSHA(ctx, "own/other", "main"); err != nil {
		t.Fatalf("чужой репозиторий работает: %v", err)
	}
	f.SetRevoked("own/proj", false)
	if res := f.Probe(ctx, "own/proj"); !res.OK {
		t.Fatalf("доступ возвращён: %+v", res)
	}

	file := filepath.Join(t.TempDir(), "revoked")
	t.Setenv(FakeRevokedEnv, file)
	g := NewFake()
	old, fresh := g.ForToken("tok-old"), g.ForToken("tok-new")
	if _, err := old.HeadSHA(ctx, "own/proj", "main"); err != nil {
		t.Fatalf("без файла доступ есть: %v", err)
	}
	if err := os.WriteFile(file, []byte("tok-x\n tok-old \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for op, call := range authOps(old) {
		if err := call(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: отозванный токен получает 401: %v", op, err)
		}
	}
	if res := old.Probe(ctx, "own/proj"); res.OK || res.Reason != ReasonBadToken {
		t.Fatalf("проверка отозванного токена: %+v", res)
	}
	if _, err := old.PipelineRun(ctx, "own/proj", "p", "main", "1", time.Time{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("пайплайн отозванного: %v", err)
	}
	if _, err := fresh.CreatePR(ctx, "own/proj", "b", "main", "t", ""); err != nil {
		t.Fatalf("другой токен работает при том же файле: %v", err)
	}
	g.SetRevokedToken("tok-new", true)
	if res := fresh.Probe(ctx, "own/proj"); res.OK {
		t.Fatalf("токен, отозванный тестом: %+v", res)
	}
	// Фабрика стенда отдаёт fake, привязанный к токену проекта.
	fa, err := (&Factory{Fallback: g, Force: true}).For(ProviderGitHub, "https://github.com", "tok-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fa.HeadSHA(ctx, "own/proj", "main"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("фабрика привязывает токен: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := old.CreatePR(ctx, "own/proj", "b", "main", "t", ""); err != nil {
		t.Fatalf("файл удалён — доступ вернулся: %v", err)
	}
}
