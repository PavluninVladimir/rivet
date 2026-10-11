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

// Один набор сценариев прогоняется против обоих адаптеров: конвейер не
// должен видеть разницы между PR и MR (спека scm-integration «Протокол
// SCM-адаптера»). Хостинг подменяется httptest-сервером.

type stub struct {
	// путь -> (код, тело); путь включает метод: "GET /api/v4/user"
	routes map[string]stubResp
	seen   map[string]string // путь -> тело запроса
}

type stubResp struct {
	code int
	body string
}

func newStub(routes map[string]stubResp) *stub {
	return &stub{routes: routes, seen: map[string]string{}}
}

func (s *stub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath: у GitLab путь репозитория закодирован (%2F), а
		// r.URL.Path его уже декодирует.
		key := r.Method + " " + r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		s.seen[key] = string(buf[:n])
		resp, ok := s.routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		w.WriteHeader(resp.code)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// adapterCase — фабрика адаптера и маршруты хостинга под один сценарий.
type adapterCase struct {
	name   string
	routes map[string]stubResp
	build  func(base, token string) Adapter
}

func bothAdapters(github, gitlab map[string]stubResp) []adapterCase {
	return []adapterCase{
		{"github", github, func(base, token string) Adapter {
			return &GitHub{Token: token, Client: httpClient(), Base: base, Web: base}
		}},
		{"gitlab", gitlab, func(base, token string) Adapter {
			return &GitLab{Token: token, Client: httpClient(), Base: base}
		}},
	}
}

func TestAdapterProbeSuccess(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{
			"GET /user":           {200, `{"login":"bot"}`},
			"GET /repos/own/proj": {200, `{"full_name":"own/proj","default_branch":"main","permissions":{"push":true,"pull":true}}`},
		},
		map[string]stubResp{
			"GET /api/v4/user":                {200, `{"username":"bot"}`},
			"GET /api/v4/projects/own%2Fproj": {200, `{"path_with_namespace":"own/proj","default_branch":"main","permissions":{"project_access":{"access_level":40}}}`},
		},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			res := c.build(srv.URL, "tok").Probe(context.Background(), "own/proj")
			if !res.OK || res.Reason != "" {
				t.Fatalf("ожидался успех: %+v", res)
			}
			if res.TokenOwner != "bot" || res.RepoPath != "own/proj" || res.DefaultBranch != "main" {
				t.Fatalf("атрибуты подключения: %+v", res)
			}
			if !res.CanPush || !res.CanMergeRequest {
				t.Fatalf("права: %+v", res)
			}
		})
	}
}

// Причины отказа различаются по смыслу: пользователю нужен конкретный текст.
func TestAdapterProbeReasons(t *testing.T) {
	scenarios := []struct {
		name           string
		github, gitlab map[string]stubResp
		want           string
	}{
		{
			"токен не принят",
			map[string]stubResp{"GET /user": {401, `{"message":"Bad credentials"}`}},
			map[string]stubResp{"GET /api/v4/user": {401, `{"message":"unauthorized"}`}},
			ReasonBadToken,
		},
		{
			"репозиторий не найден",
			map[string]stubResp{"GET /user": {200, `{"login":"bot"}`}},
			map[string]stubResp{"GET /api/v4/user": {200, `{"username":"bot"}`}},
			ReasonNotFound,
		},
		{
			"мало прав",
			map[string]stubResp{
				"GET /user":           {200, `{"login":"bot"}`},
				"GET /repos/own/proj": {200, `{"full_name":"own/proj","default_branch":"main","permissions":{"push":false,"pull":true}}`},
			},
			map[string]stubResp{
				"GET /api/v4/user":                {200, `{"username":"bot"}`},
				"GET /api/v4/projects/own%2Fproj": {200, `{"path_with_namespace":"own/proj","default_branch":"main","permissions":{"project_access":{"access_level":20}}}`},
			},
			ReasonNoScope,
		},
	}
	for _, sc := range scenarios {
		for _, c := range bothAdapters(sc.github, sc.gitlab) {
			t.Run(sc.name+"/"+c.name, func(t *testing.T) {
				srv := newStub(c.routes).server(t)
				res := c.build(srv.URL, "tok").Probe(context.Background(), "own/proj")
				if res.OK || res.Reason != sc.want {
					t.Fatalf("ожидалась причина %q, получено %+v", sc.want, res)
				}
				if res.Message == "" {
					t.Fatal("причина без текста для пользователя")
				}
			})
		}
	}
}

func TestAdapterProbeUnreachable(t *testing.T) {
	for _, c := range bothAdapters(map[string]stubResp{}, map[string]stubResp{}) {
		t.Run(c.name, func(t *testing.T) {
			// Адрес, который никто не слушает.
			res := c.build("http://127.0.0.1:1", "tok").Probe(context.Background(), "own/proj")
			if res.OK || res.Reason != ReasonUnreachable {
				t.Fatalf("ожидался unreachable: %+v", res)
			}
		})
	}
}

// Пустой repo — режим «создать новый»: проверяется только токен.
func TestAdapterProbeTokenOnly(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{"GET /user": {200, `{"login":"bot"}`}},
		map[string]stubResp{"GET /api/v4/user": {200, `{"username":"bot"}`}},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			res := c.build(srv.URL, "tok").Probe(context.Background(), "")
			if !res.OK || res.TokenOwner != "bot" {
				t.Fatalf("%+v", res)
			}
		})
	}
}

// Созданный репозиторий обязан быть инициализирован: иначе конвейеру
// не от чего ответвляться (design, решение 6).
func TestAdapterCreateRepoInitialises(t *testing.T) {
	ghStub := newStub(map[string]stubResp{
		"GET /user":        {200, `{"login":"bot"}`},
		"POST /user/repos": {201, `{"full_name":"bot/svc","html_url":"https://gh/bot/svc","default_branch":"main"}`},
	})
	glStub := newStub(map[string]stubResp{
		"GET /api/v4/namespaces?search=bot": {200, `[{"id":7,"full_path":"bot"}]`},
		"POST /api/v4/projects":             {201, `{"path_with_namespace":"bot/svc","web_url":"https://gl/bot/svc","default_branch":"main"}`},
	})
	cases := []struct {
		name     string
		st       *stub
		build    func(base string) Adapter
		initKey  string
		initFlag string
	}{
		{"github", ghStub, func(b string) Adapter {
			return &GitHub{Token: "t", Client: httpClient(), Base: b, Web: b}
		}, "POST /user/repos", `"auto_init":true`},
		{"gitlab", glStub, func(b string) Adapter {
			return &GitLab{Token: "t", Client: httpClient(), Base: b}
		}, "POST /api/v4/projects", `"initialize_with_readme":true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.st.server(t)
			info, err := c.build(srv.URL).CreateRepo(context.Background(),
				NewRepo{Owner: "bot", Name: "svc", Private: true})
			if err != nil {
				t.Fatal(err)
			}
			if info.Path != "bot/svc" || info.DefaultBranch != "main" || info.WebURL == "" {
				t.Fatalf("репозиторий: %+v", info)
			}
			if body := c.st.seen[c.initKey]; !strings.Contains(body, c.initFlag) {
				t.Fatalf("нет флага инициализации в запросе: %s", body)
			}
			if body := c.st.seen[c.initKey]; !strings.Contains(body, "private") {
				t.Fatalf("видимость не передана: %s", body)
			}
		})
	}
}

func TestAdapterCreateRepoExists(t *testing.T) {
	cases := []struct {
		name  string
		st    *stub
		build func(base string) Adapter
	}{
		{"github", newStub(map[string]stubResp{
			"GET /user":        {200, `{"login":"bot"}`},
			"POST /user/repos": {422, `{"errors":[{"message":"name already exists on this account"}]}`},
		}), func(b string) Adapter { return &GitHub{Token: "t", Client: httpClient(), Base: b, Web: b} }},
		{"gitlab", newStub(map[string]stubResp{
			"GET /api/v4/namespaces?search=bot": {200, `[{"id":7,"full_path":"bot"}]`},
			"POST /api/v4/projects":             {400, `{"message":{"path":["has already been taken"]}}`},
		}), func(b string) Adapter { return &GitLab{Token: "t", Client: httpClient(), Base: b} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.st.server(t)
			_, err := c.build(srv.URL).CreateRepo(context.Background(), NewRepo{Owner: "bot", Name: "svc"})
			if !errors.Is(err, ErrRepoExists) {
				t.Fatalf("ожидался ErrRepoExists, получено %v", err)
			}
		})
	}
}

// Нет прав администрировать репозиторий — подключение не блокируется.
func TestAdapterRegisterWebhook(t *testing.T) {
	cases := []struct {
		name           string
		okStub, noStub *stub
		build          func(base string) Adapter
	}{
		{"github",
			newStub(map[string]stubResp{"POST /repos/own/proj/hooks": {201, `{"id":1}`}}),
			newStub(map[string]stubResp{"POST /repos/own/proj/hooks": {403, `{"message":"forbidden"}`}}),
			func(b string) Adapter { return &GitHub{Token: "t", Client: httpClient(), Base: b, Web: b} }},
		{"gitlab",
			// GitLab сначала смотрит существующие хуки: дубликаты он
			// разрешает, а нам нужен ровно один с актуальным токеном.
			newStub(map[string]stubResp{
				"GET /api/v4/projects/own%2Fproj/hooks":  {200, `[]`},
				"POST /api/v4/projects/own%2Fproj/hooks": {201, `{"id":1}`},
			}),
			newStub(map[string]stubResp{
				"GET /api/v4/projects/own%2Fproj/hooks": {403, `{"message":"forbidden"}`},
			}),
			func(b string) Adapter { return &GitLab{Token: "t", Client: httpClient(), Base: b} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := c.build(c.okStub.server(t).URL).
				RegisterWebhook(context.Background(), "own/proj", "https://rivet/hook", "s3cr3t")
			if err != nil || !ok {
				t.Fatalf("регистрация: %v %v", ok, err)
			}
			ok, err = c.build(c.noStub.server(t).URL).
				RegisterWebhook(context.Background(), "own/proj", "https://rivet/hook", "s3cr3t")
			if err != nil || ok {
				t.Fatalf("без прав ожидались (false, nil): %v %v", ok, err)
			}
		})
	}
}

// Существующий хук с нашим URL обновляется, а не считается успешной
// регистрацией «как есть»: у старого остался бы прежний секрет, и все
// события начали бы отклоняться по подписи.
func TestAdapterWebhookUpsertsSecret(t *testing.T) {
	ghStub := newStub(map[string]stubResp{
		"POST /repos/own/proj/hooks":    {422, `{"errors":[{"message":"Hook already exists on this repository"}]}`},
		"GET /repos/own/proj/hooks":     {200, `[{"id":7,"config":{"url":"https://rivet/hook"}}]`},
		"PATCH /repos/own/proj/hooks/7": {200, `{"id":7}`},
	})
	glStub := newStub(map[string]stubResp{
		"GET /api/v4/projects/own%2Fproj/hooks":   {200, `[{"id":7,"url":"https://rivet/hook"}]`},
		"PUT /api/v4/projects/own%2Fproj/hooks/7": {200, `{"id":7}`},
	})
	cases := []struct {
		name      string
		st        *stub
		build     func(base string) Adapter
		updateKey string
	}{
		{"github", ghStub, func(b string) Adapter {
			return &GitHub{Token: "t", Client: httpClient(), Base: b, Web: b}
		}, "PATCH /repos/own/proj/hooks/7"},
		{"gitlab", glStub, func(b string) Adapter {
			return &GitLab{Token: "t", Client: httpClient(), Base: b}
		}, "PUT /api/v4/projects/own%2Fproj/hooks/7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.st.server(t)
			ok, err := c.build(srv.URL).
				RegisterWebhook(context.Background(), "own/proj", "https://rivet/hook", "новый-секрет")
			if err != nil || !ok {
				t.Fatalf("обновление хука: %v %v", ok, err)
			}
			if body := c.st.seen[c.updateKey]; !strings.Contains(body, "новый-секрет") {
				t.Fatalf("секрет не обновлён: %s", body)
			}
		})
	}
}

// Хук с другим URL не трогаем: регистрации не было, нужна ручная настройка.
func TestAdapterWebhookForeignHookNotRegistered(t *testing.T) {
	st := newStub(map[string]stubResp{
		"POST /repos/own/proj/hooks": {422, `{"errors":[{"message":"Hook already exists on this repository"}]}`},
		"GET /repos/own/proj/hooks":  {200, `[{"id":9,"config":{"url":"https://another/hook"}}]`},
	})
	g := &GitHub{Token: "t", Client: httpClient(), Base: st.server(t).URL}
	ok, err := g.RegisterWebhook(context.Background(), "own/proj", "https://rivet/hook", "s")
	if err != nil || ok {
		t.Fatalf("ожидались (false, nil): %v %v", ok, err)
	}
}

// Merge отдаёт sha merge-коммита независимо от хостинга.
func TestAdapterMergeReturnsSHA(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{"PUT /repos/own/proj/pulls/7/merge": {200, `{"sha":"abc123"}`}},
		map[string]stubResp{"PUT /api/v4/projects/own%2Fproj/merge_requests/7/merge": {200, `{"merge_commit_sha":"abc123"}`}},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			sha, err := c.build(srv.URL, "t").Merge(context.Background(), "own/proj", 7)
			if err != nil || sha != "abc123" {
				t.Fatalf("merge: %q %v", sha, err)
			}
		})
	}
}

// CreatePR/CreateMR возвращают номер и ссылку одинаково.
func TestAdapterCreatePR(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{"POST /repos/own/proj/pulls": {201, `{"number":12,"html_url":"https://gh/pr/12"}`}},
		map[string]stubResp{"POST /api/v4/projects/own%2Fproj/merge_requests": {201, `{"iid":12,"web_url":"https://gl/mr/12"}`}},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			pr, err := c.build(srv.URL, "t").
				CreatePR(context.Background(), "own/proj", "agent/task-1", "main", "t", "b")
			if err != nil || pr.Number != 12 || pr.URL == "" {
				t.Fatalf("PR: %+v %v", pr, err)
			}
		})
	}
}

func TestGitHubEnterpriseBaseURL(t *testing.T) {
	g := NewGitHubAt("https://ghe.example.com", "t")
	if g.Base != "https://ghe.example.com/api/v3" {
		t.Fatalf("API-хост Enterprise: %q", g.Base)
	}
	if cloud := NewGitHubAt("https://github.com", "t"); cloud.Base != "https://api.github.com" {
		t.Fatalf("API-хост облака: %q", cloud.Base)
	}
}

func TestFactoryCachesAndFallback(t *testing.T) {
	fake := NewFake()
	f := &Factory{Fallback: fake}
	// Пустой токен — запасной адаптер установки.
	a, err := f.For(ProviderGitHub, "https://github.com", "")
	if err != nil || a != Adapter(fake) {
		t.Fatalf("fallback: %v %v", a, err)
	}
	first, err := f.For(ProviderGitLab, "https://gitlab.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := f.For(ProviderGitLab, "https://gitlab.com", "tok")
	if first != again {
		t.Fatal("адаптер должен переиспользоваться из кеша")
	}
	other, _ := f.For(ProviderGitLab, "https://gitlab.com", "tok2")
	if other == first {
		t.Fatal("другой токен — другой адаптер")
	}
	if _, err := f.For("unknown", "https://x", "tok"); err == nil {
		t.Fatal("неизвестный провайдер должен давать ошибку")
	}
}

func TestGitLabDiffFormat(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"changes": []map[string]string{
		{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-x\n+y"},
	}})
	srv := newStub(map[string]stubResp{
		"GET /api/v4/projects/own%2Fproj/merge_requests/3/changes": {200, string(body)},
	}).server(t)
	g := &GitLab{Token: "t", Client: httpClient(), Base: srv.URL}
	diff, err := g.Diff(context.Background(), "own/proj", 3)
	if err != nil || !strings.Contains(diff, "diff --git a/a.go b/a.go") {
		t.Fatalf("diff: %q %v", diff, err)
	}
}

// GitLab с diff limits отдаёт часть изменений и overflow=true: diff
// возвращается вместе с ErrDiffTruncated (решения по путям — fail-closed).
func TestGitLabDiffOverflow(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"overflow": true, "changes": []map[string]string{
		{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-x\n+y"},
	}})
	srv := newStub(map[string]stubResp{
		"GET /api/v4/projects/own%2Fproj/merge_requests/3/changes": {200, string(body)},
	}).server(t)
	g := &GitLab{Token: "t", Client: httpClient(), Base: srv.URL}
	diff, err := g.Diff(context.Background(), "own/proj", 3)
	if !errors.Is(err, ErrDiffTruncated) || !strings.Contains(diff, "a.go") {
		t.Fatalf("overflow: %q %v", diff, err)
	}
}
