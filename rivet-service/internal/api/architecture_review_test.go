package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Тесты находок ревью Opus, круг 1 (add-architecture-c4).

// Хук, заведённый до подписки на push, переподписывается при старте:
// хостинг получает набор событий с push, версия подписки обновляется,
// повторный старт хостинг не трогает.
func TestResubscribeWebhooks(t *testing.T) {
	var posts []map[string]any
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/own/proj/hooks" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/own/proj/hooks" {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			posts = append(posts, in)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer host.Close()
	f := seedRepoAPI(t, host, true)
	ctx := context.Background()
	owner, err := f.st.CreateUser(ctx, "hook-owner", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.st.CreateProjectWithRepo(ctx, "demo", nil, owner.ID, store.NewRepoConnection{
		Provider: "github", BaseURL: host.URL, RepoPath: "own/proj", DefaultBranch: "main",
		Token: "ghp_x", TokenOwner: "bot",
	}, f.engine.Box)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetWebhookRegistered(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	s := &Server{St: f.st, Engine: f.engine, Secrets: f.engine.Box, PublicURL: "https://rivet.example"}
	s.ResubscribeWebhooks(ctx)
	if len(posts) != 1 {
		t.Fatalf("переподписка: запросов %d", len(posts))
	}
	events, _ := posts[0]["events"].([]any)
	hasPush := false
	for _, ev := range events {
		if ev == "push" {
			hasPush = true
		}
	}
	if !hasPush {
		t.Fatalf("переподписка без push: %+v", posts[0])
	}
	s.ResubscribeWebhooks(ctx)
	if len(posts) != 1 {
		t.Fatalf("повторная переподписка ходит на хостинг: %d", len(posts))
	}
}

// Шаг e2e-стенда «выключить автоописание»: пресеты из GET политики
// установки с auto_describe_architecture=false принимаются PUT, и проект
// без описания задачу при подключении не получает.
func TestStandDisablesAutoDescribe(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	engine.Adapters.Force = true
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	resp, body := call(t, "GET", srv.URL+"/api/v1/system/policy", root, "", nil)
	mustStatus(t, resp, http.StatusOK, "политика установки")
	var got struct {
		Presets map[string]any `json:"presets"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Presets["auto_describe_architecture"] != true {
		t.Fatalf("пресет по умолчанию в ответе: %s", body)
	}
	got.Presets["auto_describe_architecture"] = false
	resp, body = call(t, "PUT", srv.URL+"/api/v1/system/policy", root, "", got.Presets)
	mustStatus(t, resp, http.StatusOK, "пресеты стенда")
	if !strings.Contains(string(body), `"auto_describe_architecture":false`) {
		t.Fatalf("пресет не сохранился: %s", body)
	}

	resp, body = call(t, "POST", srv.URL+"/api/v1/projects", root, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var p struct{ ID string }
	_ = json.Unmarshal(body, &p)
	engine.WaitArchitecture()
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("задача описания при выключенном пресете")
	}
	skips := skipEvents(t, st, p.ID)
	if len(skips) != 1 || skips[0].Payload["reason"] != "preset_off" {
		t.Fatalf("пропуск автоописания: %+v", skips)
	}
}

func skipEvents(t *testing.T, st *store.Store, projectID string) []domain.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{ProjectID: projectID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, ev := range evs {
		if ev.Type == "architecture.describe_skipped" {
			out = append(out, ev)
		}
	}
	return out
}

// Новый репозиторий, созданный Rivet, уже содержит стартовый коммит
// хостинга (README): HeadSHA отвечает, описания нет, но повод
// «подключение» пропускается как пустой репозиторий.
func TestCreateRepoSkipsAutoDescribe(t *testing.T) {
	routes := map[string]string{
		"GET /api/v3/user":                        `{"login":"bot"}`,
		"POST /api/v3/user/repos":                 `{"full_name":"bot/svc","html_url":"https://host/bot/svc","default_branch":"main"}`,
		"GET /api/v3/repos/bot/svc/branches/main": `{"commit":{"sha":"readme-sha"}}`,
	}
	codes := map[string]int{"POST /api/v3/user/repos": http.StatusCreated}
	host := fakeHost(t, routes, codes)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "svc", "provider": "github", "base_url": host.URL, "token": "ghp_x",
		"create": map[string]any{"owner": "bot", "repo_name": "svc", "visibility": "private"},
	})
	mustStatus(t, resp, http.StatusCreated, "создание репозитория")
	var p struct{ ID string }
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	f.engine.WaitArchitecture()

	ctx := context.Background()
	row, ok, err := f.st.GetProjectArchitecture(ctx, p.ID)
	if err != nil || !ok || row.Status != "absent" || row.SHA != "readme-sha" {
		t.Fatalf("снимок нового репозитория: %+v %v %v", row, ok, err)
	}
	if _, ok, _ := f.st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("новый репозиторий получил задачу описания")
	}
	skips := skipEvents(t, f.st, p.ID)
	if len(skips) != 1 || skips[0].Payload["reason"] != "empty_repo" || skips[0].Payload["trigger"] != "connect" {
		t.Fatalf("пропуск автоописания нового репозитория: %+v", skips)
	}
}

// Проект устаревшей формы на глобальном токене установки: описание не
// читается (токен установки не открывает чужой репозиторий), повода
// «подключение» нет, причина видна в снимке.
func TestLegacyProjectWithoutCredentials(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "victim/private"})
	mustStatus(t, resp, http.StatusCreated, "проект устаревшей формы")
	var p struct{ ID string }
	_ = json.Unmarshal(body, &p)
	engine.WaitArchitecture()

	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+p.ID+"/architecture", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "снимок")
	var view struct {
		Status    string  `json:"status"`
		Error     *string `json:"error"`
		ErrorKind *string `json:"error_kind"`
		Task      any     `json:"task"`
		Current   any     `json:"current"`
	}
	_ = json.Unmarshal(body, &view)
	if view.Status != "error" || view.Error == nil || !strings.Contains(*view.Error, "учётных данных") ||
		view.ErrorKind == nil || *view.ErrorKind != "no_credentials" || view.Task != nil || view.Current != nil {
		t.Fatalf("снимок проекта без своих учётных данных: %s", body)
	}
	// Повторные поводы не пишут снимок заново.
	before, _ := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Limit: 500})
	engine.RefreshArchitecture(p.ID, true)
	engine.WaitArchitecture()
	after, _ := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Limit: 500})
	if len(after) != len(before) {
		t.Fatalf("повторное обновление без учётных данных пишет события: %d → %d", len(before), len(after))
	}
}

// Постановка задачи описания проходит через движок политик, как запуск
// Epic: запрет движка — 403, задача не создаётся.
func TestDescribeDeniedByPolicy(t *testing.T) {
	st, _, engine := testServerEngine(t)
	engine.Adapters.Force = true
	ctx := context.Background()
	denySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"allow":false,"reason":"prod_freeze"}}`))
	}))
	defer denySrv.Close()
	deny, err := policy.NewEngine(policy.Config{Mode: policy.ModeExternal, URL: denySrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{St: st, Engine: engine, Policy: deny}).Handler())
	defer srv.Close()
	owner, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects/"+p.ID+"/architecture/describe", alice, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "описание под запретом движка")
	if !strings.Contains(string(body), "prod_freeze") {
		t.Fatalf("причина отказа: %s", body)
	}
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("задача описания создана вопреки запрету")
	}
}

// Внешний merge PR задачи описания с файлом вне docs/architecture: задача
// не становится done, уходит в failed с причиной (ревью Opus, круг 2).
func TestHostingMergeArchitectureOutsideDocs(t *testing.T) {
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
	const secret = "hook-secret-outside"
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET webhook_secret=$2 WHERE id=$1`, p.ID, secret); err != nil {
		t.Fatal(err)
	}
	engine.Adapters.Fallback.(*scm.Fake).SetPRFiles("own/proj", 3, []string{"docs/architecture/architecture.yaml", "src/main.go"})
	_, taskID, err := engine.DescribeArchitecture(ctx, p.ID, "manual", "owner")
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/own/proj/pull/3"
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', pr_url=$2, branch='agent/task-arch' WHERE id=$1`, taskID, prURL); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"action":"closed","repository":{"full_name":"own/proj"},
		"pull_request":{"merged":true,"html_url":%q,"merge_commit_sha":"sha-3","head":{"ref":"agent/task-arch"},"merged_by":{"login":"human"}}}`, prURL)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Status string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out.Status != "rejected" {
		t.Fatalf("ответ webhook: %d %q", resp.StatusCode, out.Status)
	}
	task, _ := st.GetTask(ctx, taskID)
	if task.Status != domain.TaskFailed {
		t.Fatalf("задача описания после внешнего merge с кодом: %s", task.Status)
	}

	// Причина провала в DTO задачи и в задаче снимка, без поиска в ленте
	// (ревью консоли, круг 2).
	sess := loginSession(t, srv, "owner", "pw-testpass")
	tresp, raw := call(t, "GET", srv.URL+"/api/v1/tasks/"+taskID, sess, "", nil)
	mustStatus(t, tresp, http.StatusOK, "задача")
	var got struct {
		Task struct {
			Failure *struct {
				Reason     string   `json:"reason"`
				Files      []string `json:"files"`
				FilesTotal int      `json:"files_total"`
				Merged     bool     `json:"merged"`
			} `json:"failure"`
		} `json:"task"`
	}
	_ = json.Unmarshal(raw, &got)
	f := got.Task.Failure
	if f == nil || f.Reason != "architecture_outside_docs" || strings.Join(f.Files, ",") != "src/main.go" || f.FilesTotal != 1 || !f.Merged {
		t.Fatalf("failure задачи: %s", raw)
	}
	tresp, raw = call(t, "GET", srv.URL+"/api/v1/projects/"+p.ID+"/architecture", sess, "", nil)
	mustStatus(t, tresp, http.StatusOK, "снимок")
	if !strings.Contains(string(raw), `"reason":"architecture_outside_docs"`) {
		t.Fatalf("failure задачи описания в снимке: %s", raw)
	}
}

// Merge задачи описания на хостинге не запускает автопубликацию, обычная
// задача запускает (контроль фикстуры).
func TestHostingMergeArchitectureTaskSkipsDeploy(t *testing.T) {
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
	const secret = "hook-secret-deploy"
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET webhook_secret=$2 WHERE id=$1`, p.ID, secret); err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "prod", ExecType: "ssh", Trigger: "auto",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(branch string, n int) {
		t.Helper()
		prURL := fmt.Sprintf("https://github.com/own/proj/pull/%d", n)
		body := fmt.Sprintf(`{"action":"closed","repository":{"full_name":"own/proj"},
			"pull_request":{"merged":true,"html_url":%q,"merge_commit_sha":"sha-%d","head":{"ref":%q},"merged_by":{"login":"human"}}}`,
			prURL, n, branch)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		mustStatus(t, resp, http.StatusOK, "webhook merge")
	}
	merge := func(taskID, branch string, n int) {
		t.Helper()
		prURL := fmt.Sprintf("https://github.com/own/proj/pull/%d", n)
		if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', pr_url=$2, branch=$3 WHERE id=$1`, taskID, prURL, branch); err != nil {
			t.Fatal(err)
		}
		deliver(branch, n)
	}

	_, archTask, err := engine.DescribeArchitecture(ctx, p.ID, "manual", "owner")
	if err != nil {
		t.Fatal(err)
	}
	merge(archTask, "agent/task-arch", 1)
	task, _ := st.GetTask(ctx, archTask)
	if task.Status != domain.TaskDone {
		t.Fatalf("задача описания после merge на хостинге: %s", task.Status)
	}
	if deps, _ := st.ListDeployments(ctx, env.ID, 10); len(deps) != 0 {
		t.Fatalf("merge задачи описания запустил публикацию: %d", len(deps))
	}
	// Точки merge нет: задача описания не входит в состав релиза
	// (add-sdlc-cycle), и повтор доставки её не ставит.
	deliver("agent/task-arch", 1)
	if task, _ = st.GetTask(ctx, archTask); task.MergedAt != nil {
		t.Fatalf("у задачи описания точка merge: %v", task.MergedAt)
	}

	epic, err := st.CreateEpic(ctx, p.ID, "E", "")
	if err != nil {
		t.Fatal(err)
	}
	work, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	merge(work.ID, "agent/task-work", 2)
	if deps, _ := st.ListDeployments(ctx, env.ID, 10); len(deps) != 1 {
		t.Fatalf("merge обычной задачи: публикаций %d, ожидалась одна", len(deps))
	}
	if w, _ := st.GetTask(ctx, work.ID); w.MergedAt == nil {
		t.Fatal("у обычной задачи нет точки merge")
	}
}

// Решения по контракту (ревью консоли, круг 1): describe при упавшей задаче
// описания отменяет её и ставит новую в тот же системный Epic, 201.
func TestDescribeReplacesFailedTask(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	engine.Adapters.Force = true
	ctx := context.Background()
	owner, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epicID, oldID, err := engine.DescribeArchitecture(ctx, p.ID, "manual", "alice")
	if err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	base := srv.URL + "/api/v1/projects/" + p.ID + "/architecture/describe"
	// Идущая задача: 409.
	resp, _ := call(t, "POST", base, alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "describe при идущей задаче")
	// Задача упала: describe отменяет её и ставит новую.
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='failed' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	resp, body := call(t, "POST", base, alice, "", nil)
	mustStatus(t, resp, http.StatusCreated, "describe после неудачи")
	var created struct {
		EpicID string `json:"epic_id"`
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(body, &created)
	if created.EpicID != epicID || created.TaskID == "" || created.TaskID == oldID {
		t.Fatalf("новая задача в том же Epic: %s, прежний Epic %s", body, epicID)
	}
	if old, _ := st.GetTask(ctx, oldID); old.Status != domain.TaskCancelled {
		t.Fatalf("упавшая задача: %s", old.Status)
	}
	evs, _ := st.Events(ctx, store.EventFilter{TaskID: oldID, Limit: 100})
	found := false
	for _, ev := range evs {
		if ev.Type == "task.status" && ev.Payload["reason"] == "architecture_replaced" {
			found = true
		}
	}
	if !found {
		t.Fatal("нет события отмены упавшей задачи")
	}
	if info, ok, _ := st.ArchitectureTask(ctx, p.ID); !ok || info.TaskID != created.TaskID {
		t.Fatalf("задача для показа: %+v", info)
	}
}

// Проект без собственных учётных данных: describe и refresh — 422
// no_credentials (ревью Opus, круг 2).
func TestNoCredentialsDescribeRefresh(t *testing.T) {
	st, srv, _ := testServerEngine(t)
	ctx := context.Background()
	owner, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "p", "victim/private", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	for _, path := range []string{"/describe", "/refresh"} {
		resp, body := call(t, "POST", srv.URL+"/api/v1/projects/"+p.ID+"/architecture"+path, alice, "", nil)
		mustStatus(t, resp, http.StatusUnprocessableEntity, path+" без учётных данных")
		if !strings.Contains(string(body), "no_credentials") {
			t.Fatalf("%s: %s", path, body)
		}
	}
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("задача описания на глобальном токене")
	}
}

// Тело webhook'а сверх лимита — 413 payload_too_large: 1 МиБ для обычных
// событий, 8 МиБ для push.
func TestWebhookPayloadTooLarge(t *testing.T) {
	_, srv := testServer(t)
	for _, tc := range []struct {
		event string
		size  int
	}{
		{"pull_request", webhookBodyLimit + 1},
		{"push", pushBodyLimit + 1},
	} {
		body := `{"pad":"` + strings.Repeat("x", tc.size) + `"}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", tc.event)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "payload_too_large") {
			t.Fatalf("%s больше лимита: %d %s", tc.event, resp.StatusCode, raw)
		}
	}
	// Push в пределах своего лимита, но больше обычного, читается.
	body := `{"ref":"refs/heads/main","repository":{"full_name":"x/y"},"pad":"` + strings.Repeat("x", webhookBodyLimit+1) + `"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatal("push больше 1 МиБ отклонён")
	}
}

// Внешний merge обычной задачи при процессе без шага deploy: публикация не
// ставится, пишется deploy.deferred step_disabled, как при merge в Rivet.
func TestHostingMergeDeployDisabled(t *testing.T) {
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
	const secret = "hook-secret-nodeploy"
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET webhook_secret=$2 WHERE id=$1`, p.ID, secret); err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "prod", ExecType: "ssh", Trigger: "auto",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	epic, err := st.CreateEpic(ctx, p.ID, "E", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	// Снимок процесса задачи без шага deploy.
	proc := policy.Resolved{Steps: []policy.ResolvedStep{
		{ID: "code", Kind: policy.StepCode, On: policy.Transitions{Ok: "merge"}},
		{ID: "merge", Kind: policy.StepMerge, On: policy.Transitions{Ok: policy.TargetDone}},
	}}
	raw, _ := json.Marshal(proc)
	prURL := "https://github.com/own/proj/pull/9"
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', pr_url=$2, branch='agent/task-9', process=$3, process_hash=$4 WHERE id=$1`,
		task.ID, prURL, raw, policy.Hash(proc)); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"action":"closed","repository":{"full_name":"own/proj"},
		"pull_request":{"merged":true,"html_url":%q,"merge_commit_sha":"sha-9","head":{"ref":"agent/task-9"},"merged_by":{"login":"human"}}}`, prURL)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mustStatus(t, resp, http.StatusOK, "webhook merge")
	if got, _ := st.GetTask(ctx, task.ID); got.Status != domain.TaskDone {
		t.Fatalf("задача после merge: %s", got.Status)
	}
	if deps, _ := st.ListDeployments(ctx, env.ID, 10); len(deps) != 0 {
		t.Fatalf("публикация при процессе без deploy: %d", len(deps))
	}
	evs, _ := st.Events(ctx, store.EventFilter{TaskID: task.ID, Limit: 100})
	deferred := false
	for _, ev := range evs {
		if ev.Type == "deploy.deferred" && ev.Payload["reason"] == "step_disabled" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("нет deploy.deferred step_disabled")
	}
}
