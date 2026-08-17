package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Подключение репозитория при создании проекта (спека scm-integration).
// Хостинг подменяется httptest-сервером, ключ шифрования — временный.

type repoFixture struct {
	srv            *httptest.Server
	st             *store.Store
	host           *httptest.Server // подставной хостинг
	owner, mallory string
}

func randomKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// fakeHost — GitHub-совместимый хостинг: своё поведение задаётся маршрутами.
func fakeHost(t *testing.T, routes map[string]string, codes map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		body, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		if code, ok := codes[key]; ok {
			w.WriteHeader(code)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seedRepoAPI(t *testing.T, host *httptest.Server, withKey bool) repoFixture {
	t.Helper()
	ctx := context.Background()
	st, _ := testServer(t)
	f := repoFixture{st: st, host: host}
	suffix := time.Now().UnixNano()
	f.owner, f.mallory = fmt.Sprintf("owner-%d", suffix), fmt.Sprintf("mallory-%d", suffix)
	if _, err := st.CreateUser(ctx, f.owner, "", "pw", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.mallory, "", "pw", false); err != nil {
		t.Fatal(err)
	}
	key := ""
	if withKey {
		key = randomKey(t)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	// Адаптеры ходят на подставной хостинг: провайдер github, инстанс —
	// URL httptest (NewGitHubAt добавит /api/v3, поэтому маршруты в стабе
	// заданы с этим префиксом).
	engine := orchestrator.New(st, scm.NewFake(), nil, nopSender{}, 90*time.Second)
	engine.Box = box
	f.srv = httptest.NewServer((&Server{St: st, Engine: engine, Secrets: box}).Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// hostRoutes — маршруты GitHub Enterprise (NewGitHubAt добавляет /api/v3).
func hostRoutes(extra map[string]string) map[string]string {
	base := map[string]string{
		"GET /api/v3/user":           `{"login":"bot"}`,
		"GET /api/v3/repos/own/proj": `{"full_name":"own/proj","default_branch":"main","permissions":{"push":true,"pull":true}}`,
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// Проверка подключения возвращает владельца токена, репозиторий и права.
func TestScmProbe(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_x",
	})
	mustStatus(t, resp, http.StatusOK, "probe")
	var out probeView
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.TokenOwner != "bot" || out.RepoPath != "own/proj" || !out.CanPush {
		t.Fatalf("probe: %+v", out)
	}

	// Непройденная проверка — 200 с причиной, а не ошибка транспорта.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "github", "repo_url": host.URL + "/own/missing", "token": "ghp_x",
	})
	mustStatus(t, resp, http.StatusOK, "probe чужого репозитория")
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || out.Reason != scm.ReasonNotFound || out.Message == "" {
		t.Fatalf("ожидался not_found с текстом: %+v", out)
	}

	// Мусорный URL — 422.
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"repo_url": "ssh://git@host/x.git", "token": "t",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "невалидный URL")
}

// Проект создаётся только после успешной проверки; токен наружу не отдаётся.
func TestCreateProjectConnectsRepo(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_secret_value",
	})
	mustStatus(t, resp, http.StatusCreated, "создание проекта")
	if strings.Contains(string(body), "ghp_secret_value") {
		t.Fatalf("токен просочился в ответ: %s", body)
	}
	var p struct {
		ID       string `json:"ID"`
		Provider string `json:"provider"`
		RepoPath string `json:"repo_path"`
		WebURL   string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Provider != "github" || p.RepoPath != "own/proj" || p.WebURL == "" {
		t.Fatalf("проект: %+v", p)
	}

	// Состояние подключения: владелец и префикс есть, токена нет.
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/projects/"+p.ID+"/repository", sess, "", nil)
	mustStatus(t, resp, http.StatusOK, "состояние подключения")
	if strings.Contains(string(body), "ghp_secret_value") {
		t.Fatalf("токен просочился в состояние: %s", body)
	}
	var st repositoryView
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Credential == nil || st.Credential.Owner != "bot" || st.Credential.TokenPrefix != "ghp_secr" {
		t.Fatalf("учётные данные: %+v", st.Credential)
	}
	if st.State != "ok" || st.RepoPath != "own/proj" {
		t.Fatalf("состояние: %+v", st)
	}

	// Не-участник не видит подключение.
	mal := loginSession(t, f.srv, f.mallory, "pw")
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/projects/"+p.ID+"/repository", mal, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "состояние для не-участника")
}

// Проверка не прошла — проект не создан.
func TestCreateProjectRejectsBadAccess(t *testing.T) {
	host := fakeHost(t, map[string]string{"GET /api/v3/user": `{"login":"bot"}`}, nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_x",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "непройденная проверка")
	if !strings.Contains(string(body), scm.ReasonNotFound) {
		t.Fatalf("в ответе нет причины: %s", body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/projects", sess, "", nil)
	mustStatus(t, resp, http.StatusOK, "список проектов")
	if strings.Contains(string(body), "demo") {
		t.Fatalf("проект не должен был создаться: %s", body)
	}
}

// Режим «создать новый»: репозиторий создаётся и подключается; занятое имя — 409.
func TestCreateProjectCreatesRepo(t *testing.T) {
	routes := map[string]string{
		"GET /api/v3/user":        `{"login":"bot"}`,
		"POST /api/v3/user/repos": `{"full_name":"bot/svc","html_url":"https://host/bot/svc","default_branch":"main"}`,
	}
	codes := map[string]int{"POST /api/v3/user/repos": http.StatusCreated}
	host := fakeHost(t, routes, codes)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "svc", "provider": "github", "base_url": host.URL, "token": "ghp_x",
		"create": map[string]any{"owner": "bot", "repo_name": "svc", "visibility": "private"},
	})
	mustStatus(t, resp, http.StatusCreated, "создание репозитория")
	var p struct {
		RepoPath      string `json:"repo_path"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.RepoPath != "bot/svc" || p.DefaultBranch != "main" {
		t.Fatalf("подключённый репозиторий: %+v", p)
	}

	// Занятое имя — 409.
	codes["POST /api/v3/user/repos"] = http.StatusUnprocessableEntity
	routes["POST /api/v3/user/repos"] = `{"errors":[{"message":"name already exists on this account"}]}`
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "svc2", "provider": "github", "base_url": host.URL, "token": "ghp_x",
		"create": map[string]any{"owner": "bot", "repo_name": "svc", "visibility": "private"},
	})
	mustStatus(t, resp, http.StatusConflict, "занятое имя репозитория")
}

// Без ключа шифрования подключение с токеном отключено (fail-closed).
func TestCreateProjectWithoutSecretKey(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, false)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, _ := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_x",
	})
	mustStatus(t, resp, http.StatusServiceUnavailable, "нет ключа шифрования")

	// Устаревшая форма при этом работает: она без токена.
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "legacy", "repo": "own/legacy",
	})
	mustStatus(t, resp, http.StatusCreated, "устаревшая форма")
}

// Замена учётных данных без пересоздания проекта.
func TestReplaceCredentials(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	_, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_first_token",
	})
	var p struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}

	resp, body := call(t, "PUT", f.srv.URL+"/api/v1/projects/"+p.ID+"/credentials", sess, "",
		map[string]any{"token": "ghp_second_token"})
	mustStatus(t, resp, http.StatusOK, "замена токена")
	var st repositoryView
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Credential == nil || st.Credential.TokenPrefix != "ghp_seco" {
		t.Fatalf("новый токен не сохранён: %+v", st.Credential)
	}

	// Невалидный токен не заменяет рабочий.
	host2 := fakeHost(t, map[string]string{"GET /api/v3/user": `{"login":"bot"}`}, nil)
	_ = host2
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+p.ID+"/credentials", sess, "",
		map[string]any{"token": ""})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "пустой токен")
}

// Правка названия и проверок; checks заменяются целиком.
func TestPatchProjectSettings(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")
	_, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_x",
		"checks": []map[string]string{{"name": "tests", "cmd": "go test ./..."}},
	})
	var p struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	url := f.srv.URL + "/api/v1/projects/" + p.ID

	resp, body := call(t, "PATCH", url, sess, "", map[string]any{
		"name":   "demo-2",
		"checks": []map[string]string{{"name": "lint", "cmd": "golangci-lint run"}},
	})
	mustStatus(t, resp, http.StatusOK, "правка настроек")
	if !strings.Contains(string(body), "demo-2") || !strings.Contains(string(body), "golangci-lint") {
		t.Fatalf("настройки не применились: %s", body)
	}
	if strings.Contains(string(body), "go test") {
		t.Fatalf("checks должны заменяться целиком: %s", body)
	}

	// Пустое имя и битая проверка — 422.
	resp, _ = call(t, "PATCH", url, sess, "", map[string]any{"name": "  "})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "пустое имя")
	resp, _ = call(t, "PATCH", url, sess, "", map[string]any{
		"checks": []map[string]string{{"name": "no-cmd"}}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "проверка без команды")

	// Не-участник получает 404.
	mal := loginSession(t, f.srv, f.mallory, "pw")
	resp, _ = call(t, "PATCH", url, mal, "", map[string]any{"name": "hijack"})
	mustStatus(t, resp, http.StatusNotFound, "правка не-участником")
}

// Секрет webhook и ссылка на учётные данные не должны утекать в API:
// проект отдаётся явным DTO, а не доменной структурой.
func TestProjectResponsesHideSecrets(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	_, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_secret_value",
	})
	var p struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	var secret string
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(webhook_secret,'') FROM projects WHERE id=$1`, p.ID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret == "" {
		t.Fatal("проект с учётными данными должен получить собственный секрет webhook")
	}
	for _, path := range []string{"/api/v1/projects", "/api/v1/projects/" + p.ID + "/repository"} {
		_, body := call(t, "GET", f.srv.URL+path, sess, "", nil)
		for _, leak := range []string{secret, "ghp_secret_value", "WebhookSecret", "CredentialID"} {
			if strings.Contains(string(body), leak) {
				t.Fatalf("%s раскрывает %q: %s", path, leak, body)
			}
		}
	}
}

// Провайдер fake — внутренний: снаружи его принимать нельзя, иначе проверка
// доступа всегда успешна и подключить можно что угодно.
func TestFakeProviderRejectedFromOutside(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	resp, _ := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "fake", "repo_url": "https://fake.local/any/repo", "token": "x",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "probe с provider=fake")

	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "hijack", "provider": "fake", "repo_url": "https://fake.local/any/repo", "token": "x",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "создание с provider=fake")
}

// Устаревшая форма не получает собственный секрет: у таких проектов
// хостинг настроен на общий секрет установки.
func TestLegacyProjectKeepsInstallationSecret(t *testing.T) {
	host := fakeHost(t, hostRoutes(nil), nil)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw")

	_, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "legacy", "repo": "own/legacy",
	})
	var p struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	var secret string
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(webhook_secret,'') FROM projects WHERE id=$1`, p.ID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		t.Fatalf("проект без учётных данных не должен получать свой секрет: %q", secret)
	}
}
