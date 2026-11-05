package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/store"
)

// Архитектура проекта в API (add-architecture-c4, api-contract): снимок
// виден участнику, чужой проект неотличим от несуществующего, вторая
// задача описания отклоняется 409, в системный Epic задачи не добавляются.
func TestArchitectureAPI(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	// Fake-установка: проект устаревшей формы (без своих учётных данных)
	// читается только на стенде без внешних хостингов.
	engine.Adapters.Force = true
	ctx := context.Background()
	for _, u := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, u, "", "pw-"+u+"-secret", false); err != nil {
			t.Fatal(err)
		}
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	base := srv.URL + "/api/v1/projects/" + project.ID + "/architecture"
	// Подключение репозитория строит снимок в фоне.
	engine.WaitArchitecture()

	resp, body = call(t, "GET", base, alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "снимок")
	var view struct {
		Status  string          `json:"status"`
		Current json.RawMessage `json:"current"`
		Source  struct {
			Branch string `json:"branch"`
			Path   string `json:"path"`
			URL    string `json:"url"`
		} `json:"source"`
		Task *struct {
			TaskID  string          `json:"task_id"`
			Trigger string          `json:"trigger"`
			Role    json.RawMessage `json:"role"`
			Runner  json.RawMessage `json:"runner"`
		} `json:"task"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	// Описания в репозитории нет: снимок absent, автоописание поставило задачу.
	if view.Status != "absent" || string(view.Current) != "null" || view.Source.Path != "docs/architecture" ||
		view.Source.URL != "https://github.com/o/r/tree/main/docs/architecture" {
		t.Fatalf("снимок без описания: %s", body)
	}
	// Шаг ещё никто не ведёт: role и runner — null (api-contract).
	if view.Task == nil || view.Task.Trigger != "connect" || string(view.Task.Role) != "null" || string(view.Task.Runner) != "null" {
		t.Fatalf("задача автоописания: %s", body)
	}

	resp, _ = call(t, "GET", base, bob, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "снимок чужого проекта")
	resp, _ = call(t, "POST", base+"/describe", bob, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "описание чужого проекта")

	resp, body = call(t, "POST", base+"/describe", alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "вторая задача описания")
	var conflict struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				EpicID string `json:"epic_id"`
				TaskID string `json:"task_id"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("ответ 409 не разбирается, details должен быть объектом: %v %s", err, body)
	}
	if conflict.Error.Code != "architecture_task_active" || conflict.Error.Details.TaskID != view.Task.TaskID ||
		conflict.Error.Details.EpicID == "" {
		t.Fatalf("ответ 409: %s", body)
	}

	// Снимок того же SHA только что прочитан: 200 fresh без чтения и события.
	resp, body = call(t, "POST", base+"/refresh", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "обновление свежего снимка")
	var fresh struct {
		Status     string `json:"status"`
		RetryAfter int    `json:"retry_after_s"`
	}
	if err := json.Unmarshal(body, &fresh); err != nil || fresh.Status != "fresh" || fresh.RetryAfter <= 0 || fresh.RetryAfter > 30 {
		t.Fatalf("ответ fresh: %s", body)
	}
	// Пауза прошла: 202 pending, чтение запущено.
	if _, err := st.Pool.Exec(ctx, `UPDATE project_architecture SET read_at = now() - interval '1 minute' WHERE project_id = $1`, project.ID); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "POST", base+"/refresh", alice, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "обновление снимка")
	engine.WaitArchitecture()
	if !strings.Contains(string(body), `"pending"`) {
		t.Fatalf("ответ pending: %s", body)
	}

	// В системный Epic задачи не добавляются.
	epicID := ""
	if err := st.Pool.QueryRow(ctx, `SELECT epic_id FROM tasks WHERE id = $1`, view.Task.TaskID).Scan(&epicID); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epicID+"/tasks", alice, "", map[string]any{"title": "лишняя"})
	mustStatus(t, resp, http.StatusConflict, "задача в Epic архитектуры")
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epicID+"/decompose", alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "декомпозиция Epic архитектуры")
	// Архивация системного Epic с незавершённой задачей не обходит правило
	// одной задачи описания.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epicID+"/pause", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "пауза Epic архитектуры")
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epicID+"/archive", alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "архивация Epic архитектуры с задачей")
}

// Push в основную ветку перечитывает снимок, push в другую ветку
// игнорируется (спека project-architecture «Обновление снимка»).
func TestWebhookPushRefreshesArchitecture(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	engine.Adapters.Force = true
	ctx := context.Background()
	owner, err := st.CreateUser(ctx, "owner", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProjectWithRepo(ctx, "demo", nil, owner.ID, store.NewRepoConnection{
		Provider: "github", BaseURL: "https://github.com", RepoPath: "own/proj", DefaultBranch: "main",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "hook-secret-arch"
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET webhook_secret=$2 WHERE id=$1`, p.ID, secret); err != nil {
		t.Fatal(err)
	}
	push := func(ref string) string {
		body := fmt.Sprintf(`{"ref":%q,"after":"sha-2","repository":{"full_name":"own/proj"}}`, ref)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Status string }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Status
	}
	if got := push("refs/heads/feature/x"); got != "ignored" {
		t.Fatalf("push в другую ветку: %q", got)
	}
	engine.WaitArchitecture()
	if _, ok, _ := st.GetProjectArchitecture(ctx, p.ID); ok {
		t.Fatal("push в другую ветку перечитал снимок")
	}
	if got := push("refs/heads/main"); got != "noted" {
		t.Fatalf("push в основную ветку: %q", got)
	}
	engine.WaitArchitecture()
	if r, ok, _ := st.GetProjectArchitecture(ctx, p.ID); !ok || r.Status != "absent" {
		t.Fatalf("снимок после push: %+v %v", r, ok)
	}
}
