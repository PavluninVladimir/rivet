package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Аутентичность webhook (спека scm-integration): секрет на проект,
// fail-closed без секретов, отклонение чужой подписи, приём валидной.
// Проект-кандидат ищется по репозиторию из тела — это выбор ключа, а не
// доверие; доверие даёт только проверка подписи.

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// hookFixture — проект с задачей в review, готовой к merge с хостинга.
type hookFixture struct {
	srv     *httptest.Server
	st      *store.Store
	project domain.Project
	branch  string
	prURL   string
}

func seedHook(t *testing.T, provider, repoPath string) hookFixture {
	t.Helper()
	ctx := context.Background()
	st, srv := testServer(t)
	owner, err := st.CreateUser(ctx, fmt.Sprintf("owner-%d", time.Now().UnixNano()), "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProjectWithRepo(ctx, "demo", nil, owner.ID, store.NewRepoConnection{
		Provider: provider, BaseURL: "https://" + provider + ".com", RepoPath: repoPath,
		DefaultBranch: "main",
	}, nil)
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
	// Секрет на проект выдаётся вместе с учётными данными; фикстура их не
	// заводит (это не про webhook), поэтому секрет проставляется напрямую.
	secret := fmt.Sprintf("hook-secret-%d", time.Now().UnixNano())
	if _, err := st.Pool.Exec(ctx,
		`UPDATE projects SET webhook_secret=$2 WHERE id=$1`, p.ID, secret); err != nil {
		t.Fatal(err)
	}
	p.WebhookSecret = secret
	// Ветка проставляется при назначении стадии; в тесте задача сразу
	// в review, поэтому ветку и PR задаём напрямую.
	prURL := "https://" + provider + ".com/" + repoPath + "/pull/1"
	branch := fmt.Sprintf("agent/task-%d", task.Num)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE tasks SET status='review', pr_url=$2, branch=$3 WHERE id=$1`,
		task.ID, prURL, branch); err != nil {
		t.Fatal(err)
	}
	return hookFixture{srv: srv, st: st, project: p, branch: branch, prURL: prURL}
}

func (f hookFixture) githubBody() string {
	return fmt.Sprintf(`{"action":"closed","repository":{"full_name":%q},
		"pull_request":{"merged":true,"html_url":%q,"merge_commit_sha":"sha-1",
		"head":{"ref":%q},"merged_by":{"login":"human"}}}`,
		f.project.RepoPath, f.prURL, f.branch)
}

func (f hookFixture) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f hookFixture) taskStatus(t *testing.T) domain.TaskStatus {
	t.Helper()
	var st domain.TaskStatus
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT status FROM tasks WHERE branch=$1`, f.branch).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Валидная подпись секретом проекта доводит задачу до done.
func TestWebhookAcceptsProjectSecret(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := f.githubBody()
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, body),
	})
	mustStatus(t, resp, http.StatusOK, "валидная подпись")
	if got := f.taskStatus(t); got != domain.TaskDone {
		t.Fatalf("задача должна стать done, got %s", got)
	}
}

// Секрет другого проекта не подходит: подделать событие чужого репозитория
// нельзя (спека «Секрет одного проекта не подходит другому»).
func TestWebhookRejectsForeignSecret(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := f.githubBody()

	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign("секрет-другого-проекта", body),
	})
	mustStatus(t, resp, http.StatusUnauthorized, "чужой секрет")

	// Без подписи и с подписью другого тела — тоже отказ.
	resp = f.post(t, "/api/v1/webhooks/github", body, map[string]string{"X-GitHub-Event": "pull_request"})
	mustStatus(t, resp, http.StatusUnauthorized, "без подписи")
	resp = f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, `{"another":"body"}`),
	})
	mustStatus(t, resp, http.StatusUnauthorized, "подпись другого тела")

	if got := f.taskStatus(t); got == domain.TaskDone {
		t.Fatal("отклонённое событие не должно менять конвейер")
	}
}

// Ни секрета проекта, ни секрета установки — приём выключен (fail-closed).
func TestWebhookFailClosedWithoutSecret(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE projects SET webhook_secret=NULL WHERE id=$1`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	body := f.githubBody()
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign("любой", body),
	})
	mustStatus(t, resp, http.StatusForbidden, "без секретов")
	if got := f.taskStatus(t); got == domain.TaskDone {
		t.Fatal("при выключенном приёме конвейер не меняется")
	}
}

// Событие чужого репозитория игнорируется: проект-кандидат не найден,
// существование проектов наружу не раскрывается.
func TestWebhookIgnoresUnknownRepo(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := strings.ReplaceAll(f.githubBody(), "own/proj", "stranger/repo")
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, body),
	})
	mustStatus(t, resp, http.StatusOK, "чужой репозиторий")
	if got := f.taskStatus(t); got == domain.TaskDone {
		t.Fatal("событие чужого репозитория не должно менять конвейер")
	}
}

// Merge MR в GitLab обрабатывается так же, как PR в GitHub, но с токеном
// в X-Gitlab-Token (дельта «Merge в GitLab»).
func TestWebhookGitLabMerge(t *testing.T) {
	f := seedHook(t, "gitlab", "group/sub/proj")
	body := fmt.Sprintf(`{"object_kind":"merge_request","user":{"username":"human"},
		"project":{"path_with_namespace":%q},
		"object_attributes":{"action":"merge","state":"merged","source_branch":%q,
		"url":%q,"merge_commit_sha":"sha-9"}}`, f.project.RepoPath, f.branch, f.prURL)

	// Чужой токен отклоняется.
	resp := f.post(t, "/api/v1/webhooks/gitlab", body, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": "чужой",
	})
	mustStatus(t, resp, http.StatusUnauthorized, "чужой токен GitLab")

	resp = f.post(t, "/api/v1/webhooks/gitlab", body, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": f.project.WebhookSecret,
	})
	mustStatus(t, resp, http.StatusOK, "валидный токен GitLab")
	if got := f.taskStatus(t); got != domain.TaskDone {
		t.Fatalf("MR-merge должен доводить задачу до done, got %s", got)
	}
}

// Событие без репозитория игнорируется: не по чему выбрать ни проект,
// ни ключ проверки подписи.
func TestWebhookIgnoresEventWithoutRepo(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"action":"closed","pull_request":{"merged":true,"head":{"ref":%q}}}`, f.branch)
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, body),
	})
	mustStatus(t, resp, http.StatusOK, "событие без репозитория")
	if got := f.taskStatus(t); got == domain.TaskDone {
		t.Fatal("событие без репозитория не должно менять конвейер")
	}
}

// Не-merge события игнорируются без изменения конвейера, но подпись у них
// всё равно проверяется (спека: проверяется каждое входящее событие).
func TestWebhookVerifiesNonMergeEvents(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := `{"action":"opened","repository":{"full_name":"own/proj"}}`
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign("чужой-секрет", body),
	})
	mustStatus(t, resp, http.StatusUnauthorized, "не-merge с чужой подписью")
}

// Не-merge события игнорируются без изменения конвейера.
func TestWebhookIgnoresNonMerge(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := `{"action":"opened","repository":{"full_name":"own/proj"}}`
	resp := f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, body),
	})
	mustStatus(t, resp, http.StatusOK, "ping/opened")
	if got := f.taskStatus(t); got == domain.TaskDone {
		t.Fatal("не-merge событие не должно менять конвейер")
	}
}
