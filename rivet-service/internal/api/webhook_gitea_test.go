package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// События Gitea и Forgejo (change add-gitea-adapter): подпись HMAC тела в
// X-Gitea-Signature, вид события в X-Gitea-Event. Тела — в форме, которую
// шлёт настоящий Gitea 1.24.

func (f hookFixture) postGitea(t *testing.T, event, body string) *http.Response {
	t.Helper()
	return f.post(t, "/api/v1/webhooks/gitea", body, map[string]string{
		"X-Gitea-Event":     event,
		"X-Gitea-Signature": strings.TrimPrefix(sign(f.project.WebhookSecret, body), "sha256="),
		"Content-Type":      "application/json",
	})
}

func (f hookFixture) giteaPR(action string, merged bool) string {
	return fmt.Sprintf(`{"action":%q,"number":1,"repository":{"full_name":%q},"sender":{"login":"human"},
		"pull_request":{"html_url":%q,"merged":%v,"merge_commit_sha":"sha-merge","state":"closed",
		"head":{"ref":%q},"merged_by":{"login":"human"}}}`, action, f.project.RepoPath, f.prURL, merged, f.branch)
}

// Merge PR на Gitea с подписью проекта доводит задачу до done; без
// подписи или с чужой подписью событие отклоняется.
func TestWebhookGiteaMerge(t *testing.T) {
	f := seedHook(t, "gitea", "own/proj")
	body := f.giteaPR("closed", true)
	resp := f.post(t, "/api/v1/webhooks/gitea", body, map[string]string{"X-Gitea-Event": "pull_request"})
	mustStatus(t, resp, http.StatusUnauthorized, "без подписи")
	resp = f.post(t, "/api/v1/webhooks/gitea", body, map[string]string{
		"X-Gitea-Event": "pull_request", "X-Gitea-Signature": strings.TrimPrefix(sign("чужой секрет", body), "sha256=")})
	mustStatus(t, resp, http.StatusUnauthorized, "чужая подпись")
	// Подпись GitHub-формы (с префиксом) в заголовке Gitea не принимается.
	resp = f.post(t, "/api/v1/webhooks/gitea", body, map[string]string{
		"X-Gitea-Event": "pull_request", "X-Gitea-Signature": sign(f.project.WebhookSecret, body)})
	mustStatus(t, resp, http.StatusUnauthorized, "подпись с префиксом")
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("отклонённое событие не меняет задачу: %s", got)
	}

	resp = f.postGitea(t, "pull_request", body)
	mustStatus(t, resp, http.StatusOK, "merge на Gitea")
	if s := bodyStatus(t, resp); s != "done" {
		t.Fatalf("merge завершает задачу: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskDone {
		t.Fatalf("задача после merge: %s", got)
	}
}

// Forgejo подписывает тем же HMAC, но под своими заголовками.
func TestWebhookForgejoHeaders(t *testing.T) {
	f := seedHook(t, "gitea", "own/proj")
	body := f.giteaPR("closed", true)
	resp := f.post(t, "/api/v1/webhooks/gitea", body, map[string]string{
		"X-Forgejo-Event":     "pull_request",
		"X-Forgejo-Signature": strings.TrimPrefix(sign(f.project.WebhookSecret, body), "sha256="),
	})
	mustStatus(t, resp, http.StatusOK, "merge на Forgejo")
	if got := f.taskStatus(t); got != domain.TaskDone {
		t.Fatalf("задача после merge на Forgejo: %s", got)
	}
}

// Review человека, итоги Actions и закрытие PR без merge — как у GitHub.
func TestWebhookGiteaEvents(t *testing.T) {
	f := seedHook(t, "gitea", "own/proj")
	review := func(kind, content string) string {
		return fmt.Sprintf(`{"action":"reviewed","number":1,"repository":{"full_name":%q},"sender":{"login":"alice"},
			"pull_request":{"html_url":%q,"head":{"ref":%q}},"review":{"type":%q,"content":%q}}`,
			f.project.RepoPath, f.prURL, f.branch, kind, content)
	}
	resp := f.postGitea(t, "pull_request_approved", review("pull_request_review_approved", "lgtm"))
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("одобрение конвейер не трогает: %s", s)
	}
	resp = f.postGitea(t, "pull_request_comment", review("pull_request_review_comment", "заметка"))
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("комментарий review конвейер не трогает: %s", s)
	}
	// Комментарий PR без review (issue_comment) не обрабатывается, как у GitHub.
	resp = f.postGitea(t, "issue_comment", fmt.Sprintf(`{"action":"created","is_pull":true,"repository":{"full_name":%q},
		"comment":{"body":"вопрос"},"pull_request":{"html_url":%q,"head":{"ref":%q}}}`, f.project.RepoPath, f.prURL, f.branch))
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("комментарий PR без review: %s", s)
	}
	resp = f.postGitea(t, "pull_request_rejected", review("pull_request_review_rejected", "поправь обработку ошибок"))
	if s := bodyStatus(t, resp); s != "done" {
		t.Fatalf("запрос изменений возвращает задачу в работу: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskFixing {
		t.Fatalf("после запроса изменений: %s", got)
	}
	evs := f.events(t, "task.review_external")
	if len(evs) != 3 {
		t.Fatalf("три review в timeline: %d", len(evs))
	}
	var changes bool
	for _, e := range evs {
		if e.Payload["state"] == "changes_requested" && e.Payload["author"] == "alice" && e.Payload["body"] == "поправь обработку ошибок" {
			changes = true
		}
	}
	if !changes {
		t.Fatalf("событие запроса изменений с автором и текстом: %+v", evs)
	}

	// Итог Actions: задача уже в fixing — только событие.
	run := fmt.Sprintf(`{"action":"completed","repository":{"full_name":%q},"workflow_run":{"name":"CI","status":"completed",
		"conclusion":"failure","head_branch":%q,"html_url":"https://gitea/own/proj/actions/runs/1"}}`, f.project.RepoPath, f.branch)
	resp = f.postGitea(t, "workflow_run", run)
	mustStatus(t, resp, http.StatusOK, "итог Actions")
	if evs := f.events(t, "task.checks_external"); len(evs) != 1 {
		t.Fatalf("итог проверок в timeline: %d", len(evs))
	}

	closed := f.giteaPR("closed", false)
	resp = f.postGitea(t, "pull_request", closed)
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("закрытие PR без merge: %s", s)
	}
	if evs := f.events(t, "task.pr_closed"); len(evs) != 1 {
		t.Fatalf("событие закрытия PR: %d", len(evs))
	}
	var reason string
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT reason FROM attention WHERE status <> 'resolved' ORDER BY created_at DESC LIMIT 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != string(domain.AttPRClosed) {
		t.Fatalf("эскалация закрытия PR: %s", reason)
	}

	// Открытие PR и прочие действия конвейеру не нужны.
	resp = f.postGitea(t, "pull_request", f.giteaPR("opened", false))
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("открытие PR: %s", s)
	}
}

// Разбор push: ветка и вершина; событие проекта на GitHub с тем же путём
// на эндпоинт Gitea не попадает (провайдер берётся из адреса).
func TestParseGiteaPush(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"sha-new","repository":{"full_name":"own/proj"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitea", strings.NewReader(body))
	r.Header.Set("X-Gitea-Event", "push")
	ev := parseGiteaEvent(r, []byte(body))
	if ev.Kind != hookPush || ev.Branch != "main" || ev.MergeSHA != "sha-new" || ev.RepoPath != "own/proj" {
		t.Fatalf("push: %+v", ev)
	}
	r.Header.Set("X-Gitea-Event", "workflow_run")
	running := `{"repository":{"full_name":"own/proj"},"workflow_run":{"status":"in_progress","head_branch":"b"}}`
	if ev := parseGiteaEvent(r, []byte(running)); ev.Kind != "" {
		t.Fatalf("идущий прогон вердикта не даёт: %+v", ev)
	}
	if ev := parseGiteaEvent(r, []byte(`не json`)); !ev.Malformed {
		t.Fatal("нечитаемое тело")
	}
}

func TestWebhookGiteaProjectOnOtherProvider(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	resp := f.postGitea(t, "pull_request", f.giteaPR("closed", true))
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("событие Gitea для проекта на GitHub: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("задача на GitHub не меняется: %s", got)
	}
}
