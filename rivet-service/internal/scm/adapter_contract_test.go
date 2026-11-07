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

// Один набор сценариев прогоняется против всех адаптеров: конвейер не
// должен видеть разницы между PR и MR (спека scm-integration «Протокол
// SCM-адаптера»). Хостинг подменяется httptest-сервером. Сценарии, где
// ответы Gitea устроены иначе, — в gitea_test.go.

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

// allAdapters — те же сценарии и для Gitea (change add-gitea-adapter):
// пустые маршруты Gitea — сценарий для неё не прогоняется.
func allAdapters(github, gitlab, gitea map[string]stubResp) []adapterCase {
	cases := bothAdapters(github, gitlab)
	if gitea != nil {
		cases = append(cases, adapterCase{"gitea", gitea, func(base, token string) Adapter {
			return NewGitea(base, token)
		}})
	}
	return cases
}

func TestAdapterProbeSuccess(t *testing.T) {
	cases := allAdapters(
		map[string]stubResp{
			"GET /user":           {200, `{"login":"bot"}`},
			"GET /repos/own/proj": {200, `{"full_name":"own/proj","default_branch":"main","permissions":{"push":true,"pull":true}}`},
		},
		map[string]stubResp{
			"GET /api/v4/user":                {200, `{"username":"bot"}`},
			"GET /api/v4/projects/own%2Fproj": {200, `{"path_with_namespace":"own/proj","default_branch":"main","permissions":{"project_access":{"access_level":40}}}`},
		},
		map[string]stubResp{
			"GET /api/v1/user":                  {200, `{"login":"bot"}`},
			"GET /api/v1/repos/own/proj":        {200, `{"full_name":"own/proj","default_branch":"main","permissions":{"push":true,"pull":true}}`},
			"POST /api/v1/repos/own/proj/pulls": {422, `{"message":"[Head]: Required"}`},
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
		name                  string
		github, gitlab, gitea map[string]stubResp
		want                  string
	}{
		{
			"токен не принят",
			map[string]stubResp{"GET /user": {401, `{"message":"Bad credentials"}`}},
			map[string]stubResp{"GET /api/v4/user": {401, `{"message":"unauthorized"}`}},
			map[string]stubResp{"GET /api/v1/user": {401, `{"message":"user does not exist [uid: 0, name: ]"}`}},
			ReasonBadToken,
		},
		{
			"репозиторий не найден",
			map[string]stubResp{"GET /user": {200, `{"login":"bot"}`}},
			map[string]stubResp{"GET /api/v4/user": {200, `{"username":"bot"}`}},
			map[string]stubResp{"GET /api/v1/user": {200, `{"login":"bot"}`}},
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
			map[string]stubResp{
				"GET /api/v1/user":           {200, `{"login":"bot"}`},
				"GET /api/v1/repos/own/proj": {200, `{"full_name":"own/proj","default_branch":"main","permissions":{"push":false,"pull":true}}`},
			},
			ReasonNoScope,
		},
	}
	for _, sc := range scenarios {
		for _, c := range allAdapters(sc.github, sc.gitlab, sc.gitea) {
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
	for _, c := range allAdapters(map[string]stubResp{}, map[string]stubResp{}, map[string]stubResp{}) {
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
	cases := allAdapters(
		map[string]stubResp{"GET /user": {200, `{"login":"bot"}`}},
		map[string]stubResp{"GET /api/v4/user": {200, `{"username":"bot"}`}},
		map[string]stubResp{"GET /api/v1/user": {200, `{"login":"bot"}`}},
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
		{"gitea", newStub(map[string]stubResp{
			"GET /api/v1/user":        {200, `{"login":"bot"}`},
			"POST /api/v1/user/repos": {201, `{"full_name":"bot/svc","html_url":"https://gt/bot/svc","default_branch":"main"}`},
		}), func(b string) Adapter { return NewGitea(b, "t") }, "POST /api/v1/user/repos", `"auto_init":true`},
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
		{"gitea", newStub(map[string]stubResp{
			"GET /api/v1/user":        {200, `{"login":"bot"}`},
			"POST /api/v1/user/repos": {409, `{"message":"The repository with the same name already exists."}`},
		}), func(b string) Adapter { return NewGitea(b, "t") }},
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
			// GitHub тоже сначала смотрит существующие хуки: у найденного
			// один мутирующий запрос (PATCH) вместо POST с отказом 422.
			newStub(map[string]stubResp{
				"GET /repos/own/proj/hooks":  {200, `[]`},
				"POST /repos/own/proj/hooks": {201, `{"id":1}`},
			}),
			newStub(map[string]stubResp{"GET /repos/own/proj/hooks": {403, `{"message":"forbidden"}`}}),
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
		{"gitea",
			newStub(map[string]stubResp{
				"GET /api/v1/repos/own/proj/hooks?limit=50&page=1": {200, `[]`},
				"POST /api/v1/repos/own/proj/hooks":                {201, `{"id":1}`},
			}),
			newStub(map[string]stubResp{
				"GET /api/v1/repos/own/proj/hooks?limit=50&page=1": {403, `{"message":"forbidden"}`},
			}),
			func(b string) Adapter { return NewGitea(b, "t") }},
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
		{"gitea", newStub(map[string]stubResp{
			"GET /api/v1/repos/own/proj/hooks?limit=50&page=1": {200, `[{"id":7,"config":{"url":"https://rivet/hook"}}]`},
			"PATCH /api/v1/repos/own/proj/hooks/7":             {200, `{"id":7}`},
		}), func(b string) Adapter { return NewGitea(b, "t") }, "PATCH /api/v1/repos/own/proj/hooks/7"},
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
	cases := allAdapters(
		map[string]stubResp{"PUT /repos/own/proj/pulls/7/merge": {200, `{"sha":"abc123"}`}},
		map[string]stubResp{"PUT /api/v4/projects/own%2Fproj/merge_requests/7/merge": {200, `{"merge_commit_sha":"abc123"}`}},
		map[string]stubResp{
			"POST /api/v1/repos/own/proj/pulls/7/merge": {200, ``},
			"GET /api/v1/repos/own/proj/pulls/7":        {200, `{"number":7,"merged":true,"merge_commit_sha":"abc123"}`},
		},
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

// Конфликт с базовой веткой оба адаптера сообщают одной ошибкой (спека
// scm-integration «Распознавание конфликта при merge»).
func TestAdapterMergeConflict(t *testing.T) {
	const ghMerge = "PUT /repos/own/proj/pulls/7/merge"
	const ghPR = "GET /repos/own/proj/pulls/7"
	const glMerge = "PUT /api/v4/projects/own%2Fproj/merge_requests/7/merge"
	const glMR = "GET /api/v4/projects/own%2Fproj/merge_requests/7"
	cases := []struct {
		name     string
		github   map[string]stubResp
		gitlab   map[string]stubResp
		conflict bool
	}{
		{"конфликт по тексту отказа", map[string]stubResp{
			ghMerge: {405, `{"message":"Pull Request has merge conflicts","status":"405"}`},
		}, map[string]stubResp{
			glMerge: {406, `{"message":"Branch cannot be merged"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":true}`},
		}, true},
		{"конфликт по состоянию PR", map[string]stubResp{
			ghMerge: {405, `{"message":"Pull Request is not mergeable"}`},
			ghPR:    {200, `{"number":7,"mergeable":false,"mergeable_state":"dirty"}`},
		}, map[string]stubResp{
			glMerge: {422, `{"message":"422 Unprocessable"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":false,"detailed_merge_status":"conflict"}`},
		}, true},
		{"обязательные проверки не конфликт", map[string]stubResp{
			ghMerge: {405, `{"message":"Pull Request is not mergeable"}`},
			ghPR:    {200, `{"number":7,"mergeable":false,"mergeable_state":"blocked"}`},
		}, map[string]stubResp{
			glMerge: {405, `{"message":"405 Method Not Allowed"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":false,"detailed_merge_status":"ci_must_pass"}`},
		}, false},
		{"mergeable ещё не посчитан", map[string]stubResp{
			ghMerge: {405, `{"message":"Pull Request is not mergeable"}`},
			ghPR:    {200, `{"number":7,"mergeable":null,"mergeable_state":"unknown"}`},
		}, map[string]stubResp{
			glMerge: {406, `{"message":"Branch cannot be merged"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":false,"detailed_merge_status":"checking"}`},
		}, false},
		{"нет прав на merge не конфликт, даже если PR конфликтует", map[string]stubResp{
			ghMerge: {403, `{"message":"protected branch"}`},
			ghPR:    {200, `{"number":7,"mergeable":false,"mergeable_state":"dirty"}`},
		}, map[string]stubResp{
			glMerge: {403, `{"message":"403 Forbidden"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":true}`},
		}, false},
		{"ошибка хостинга не конфликт", map[string]stubResp{
			ghMerge: {502, `{"message":"bad gateway"}`},
			ghPR:    {200, `{"number":7,"mergeable":false,"mergeable_state":"dirty"}`},
		}, map[string]stubResp{
			glMerge: {500, `{"message":"500 Internal Server Error"}`},
			glMR:    {200, `{"iid":7,"has_conflicts":true}`},
		}, false},
	}
	for _, tc := range cases {
		for _, c := range bothAdapters(tc.github, tc.gitlab) {
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				srv := newStub(c.routes).server(t)
				_, err := c.build(srv.URL, "t").Merge(context.Background(), "own/proj", 7)
				if err == nil {
					t.Fatal("ожидался отказ merge")
				}
				if got := errors.Is(err, ErrMergeConflict); got != tc.conflict {
					t.Fatalf("конфликт=%v, ожидалось %v: %v", got, tc.conflict, err)
				}
			})
		}
	}
}

// Fake имитирует конфликт по метке в заголовке: первый merge отклонён,
// следующий проходит, PR без метки мержится сразу.
func TestFakeMergeConflictOnce(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	marked, _ := f.CreatePR(ctx, "e2e/demo", "agent/task-1", "main", "task-1: правка "+FakeConflictMarker, "")
	plain, _ := f.CreatePR(ctx, "e2e/demo", "agent/task-2", "main", "task-2: правка", "")
	if _, err := f.Merge(ctx, "e2e/demo", marked.Number); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("первый merge помеченного PR: %v", err)
	}
	if _, err := f.Merge(ctx, "e2e/demo", marked.Number); err != nil {
		t.Fatalf("второй merge помеченного PR: %v", err)
	}
	if _, err := f.Merge(ctx, "e2e/demo", plain.Number); err != nil {
		t.Fatalf("PR без метки: %v", err)
	}
}

// CreatePR/CreateMR возвращают номер и ссылку одинаково.
func TestAdapterCreatePR(t *testing.T) {
	cases := allAdapters(
		map[string]stubResp{"POST /repos/own/proj/pulls": {201, `{"number":12,"html_url":"https://gh/pr/12"}`}},
		map[string]stubResp{"POST /api/v4/projects/own%2Fproj/merge_requests": {201, `{"iid":12,"web_url":"https://gl/mr/12"}`}},
		map[string]stubResp{"POST /api/v1/repos/own/proj/pulls": {201, `{"number":12,"html_url":"https://gt/pulls/12"}`}},
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
