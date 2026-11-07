package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/scm"
)

// Подключение проекта к self-hosted Gitea (change add-gitea-adapter, спека
// scm-integration «Первая волна адаптеров»): провайдер указывается явно,
// URL инстанса произвольный, хвост адреса страницы отрезается.
func TestCreateProjectOnGitea(t *testing.T) {
	host := fakeHost(t, map[string]string{
		"GET /api/v1/version":                       `{"version":"28.3.0"}`,
		"POST /api/v1/repos/own/proj/pulls":         `{"message":"[Head]: Required"}`,
		"GET /api/v1/user":                          `{"login":"rivet-bot"}`,
		"GET /api/v1/repos/own/proj":                `{"full_name":"own/proj","html_url":"x","default_branch":"trunk","permissions":{"push":true,"pull":true}}`,
		"GET /api/v1/repos/own/proj/branches/trunk": `{"name":"trunk","commit":{"id":"sha-1"}}`,
	}, map[string]int{"POST /api/v1/repos/own/proj/pulls": 422})
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")

	// Без провайдера self-hosted URL не разобрать однозначно.
	resp, _ := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"repo_url": host.URL + "/own/proj", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "probe без провайдера")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "repo_url": host.URL + "/own/proj/src/branch/trunk", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusOK, "probe Gitea")
	var pr probeView
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.OK || pr.TokenOwner != "rivet-bot" || pr.RepoPath != "own/proj" || pr.DefaultBranch != "trunk" || pr.BaseURL != host.URL {
		t.Fatalf("probe Gitea: %+v", pr)
	}

	resp, body = call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "gitea-demo", "provider": "gitea", "repo_url": host.URL + "/own/proj", "token": "gitea-token-value",
	})
	mustStatus(t, resp, http.StatusCreated, "проект на Gitea")
	var p struct {
		ID            string `json:"ID"`
		Provider      string `json:"provider"`
		BaseURL       string `json:"base_url"`
		RepoPath      string `json:"repo_path"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Provider != "gitea" || p.BaseURL != host.URL || p.RepoPath != "own/proj" || p.DefaultBranch != "trunk" {
		t.Fatalf("проект: %+v", p)
	}
	f.engine.WaitArchitecture()
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/projects/"+p.ID+"/repository", sess, "", nil)
	mustStatus(t, resp, http.StatusOK, "состояние подключения")
	if strings.Contains(string(body), "gitea-token-value") {
		t.Fatalf("токен просочился: %s", body)
	}
	var st repositoryView
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Provider != "gitea" || st.State != "ok" || st.Credential == nil || st.Credential.Owner != "rivet-bot" {
		t.Fatalf("состояние: %+v", st)
	}

	// Gitea без URL инстанса: облачного инстанса по умолчанию нет.
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "repo_path": "own/proj", "token": "t",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "Gitea без инстанса")
	// Неизвестный провайдер называет поддержанные.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "bitbucket", "repo_url": host.URL + "/own/proj", "token": "t",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неизвестный провайдер")
	if !strings.Contains(string(body), "gitea") {
		t.Fatalf("причина называет gitea: %s", body)
	}
}

// Gitea за прокси под путём: подкаталог уходит в адрес инстанса, API и
// клон идут через него.
func TestProbeGiteaSubdirectory(t *testing.T) {
	host := fakeHost(t, map[string]string{
		"GET /gitea/api/v1/version":               `{"version":"28.3.0"}`,
		"GET /gitea/api/v1/user":                  `{"login":"rivet-bot"}`,
		"GET /gitea/api/v1/repos/team/svc":        `{"full_name":"team/svc","default_branch":"main","permissions":{"push":true,"pull":true}}`,
		"POST /gitea/api/v1/repos/team/svc/pulls": `{"message":"[Head]: Required"}`,
	}, map[string]int{"POST /gitea/api/v1/repos/team/svc/pulls": 422})
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "repo_url": host.URL + "/gitea/team/svc/src/branch/main", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusOK, "probe Gitea в подкаталоге")
	var pr probeView
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.OK || pr.RepoPath != "team/svc" || pr.BaseURL != host.URL+"/gitea" {
		t.Fatalf("инстанс с подкаталогом: %+v", pr)
	}
	// Режим «создать»: подкаталог в адресе инстанса сохраняется.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "base_url": host.URL + "/gitea/", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusOK, "probe инстанса в подкаталоге")
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.OK || pr.BaseURL != host.URL+"/gitea" {
		t.Fatalf("адрес инстанса с подкаталогом: %+v", pr)
	}
	// Инстанс опознан, репозитория нет: пояснения о поиске инстанса нет.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "repo_url": host.URL + "/gitea/team/none", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusOK, "probe несуществующего репозитория")
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.OK || strings.Contains(pr.Message, scm.GiteaNotLocated) {
		t.Fatalf("лишнее пояснение: %+v", pr)
	}
}

// Инстанс по адресу не опознан (API подкаталога закрыт авторизацией
// прокси): отказ проверки подключения поясняет, что дело может быть в
// поиске инстанса, а не в токене или репозитории.
func TestProbeGiteaNotLocated(t *testing.T) {
	host := fakeHost(t, map[string]string{
		"GET /gitea/api/v1/version": `{"message":"Unauthorized"}`,
		"GET /api/v1/user":          `{"login":"rivet-bot"}`,
	}, map[string]int{"GET /gitea/api/v1/version": 401})
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/scm/probe", sess, "", map[string]any{
		"provider": "gitea", "repo_url": host.URL + "/gitea/team/svc", "token": "gitea-tok",
	})
	mustStatus(t, resp, http.StatusOK, "probe неопознанного инстанса")
	var pr probeView
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.OK || pr.BaseURL != host.URL || !strings.Contains(pr.Message, scm.GiteaNotLocated) {
		t.Fatalf("отказ без пояснения о поиске инстанса: %+v", pr)
	}
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "gitea-hidden", "provider": "gitea", "repo_url": host.URL + "/gitea/team/svc", "token": "gitea-token-value",
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "проект на неопознанном инстансе")
	if !strings.Contains(string(body), scm.GiteaNotLocated) {
		t.Fatalf("причина без пояснения: %s", body)
	}
}

func TestTreeURLGitea(t *testing.T) {
	if got := treeURL("gitea", "https://gitea.example.com/own/proj", "main", "docs/architecture"); got != "https://gitea.example.com/own/proj/src/branch/main/docs/architecture" {
		t.Fatalf("ссылка на каталог: %s", got)
	}
}
