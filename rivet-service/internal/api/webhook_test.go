package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// События хостинга в конвейере (change add-scm-events): внешние проверки,
// review человека и закрытие PR без merge.

// bodyStatus — статус ответа webhook'а (done / noted / ignored).
func bodyStatus(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out struct{ Status string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Status
}

func (f hookFixture) postSigned(t *testing.T, event, body string) *http.Response {
	t.Helper()
	return f.post(t, "/api/v1/webhooks/github", body, map[string]string{
		"X-GitHub-Event":      event,
		"X-Hub-Signature-256": sign(f.project.WebhookSecret, body),
	})
}

func (f hookFixture) events(t *testing.T, typ string) []domain.Event {
	t.Helper()
	evs, err := f.st.Events(context.Background(), store.EventFilter{Type: typ, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// Провал внешних проверок возвращает задачу в работу, успех — только
// событие (спека scm-integration «Внешние проверки провалились»).
func TestWebhookExternalChecks(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	ok := fmt.Sprintf(`{"repository":{"full_name":%q},"workflow_run":{"name":"CI","status":"completed",
		"conclusion":"success","head_branch":%q,"html_url":"https://ci/1"}}`, f.project.RepoPath, f.branch)
	resp := f.postSigned(t, "workflow_run", ok)
	mustStatus(t, resp, http.StatusOK, "успешные проверки")
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("успех не меняет конвейер: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("задача должна остаться в review, got %s", got)
	}

	bad := fmt.Sprintf(`{"repository":{"full_name":%q},"workflow_run":{"name":"CI","status":"completed",
		"conclusion":"failure","head_branch":%q,"html_url":"https://ci/2"}}`, f.project.RepoPath, f.branch)
	resp = f.postSigned(t, "workflow_run", bad)
	mustStatus(t, resp, http.StatusOK, "проваленные проверки")
	if s := bodyStatus(t, resp); s != "done" {
		t.Fatalf("провал должен вернуть задачу в работу: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskFixing {
		t.Fatalf("задача должна вернуться в fixing, got %s", got)
	}
	if evs := f.events(t, "task.checks_external"); len(evs) != 2 {
		t.Fatalf("оба итога проверок в timeline: %d", len(evs))
	}

	// Повторная доставка того же события: задача уже не в review — только
	// событие, второй попытки не тратится.
	resp = f.postSigned(t, "workflow_run", bad)
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("повтор не должен реагировать дважды: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskFixing {
		t.Fatalf("статус после повтора: %s", got)
	}
}

// Review человека: запрос изменений возвращает задачу в работу с
// замечаниями, одобрение — информационное событие.
func TestWebhookExternalReview(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	review := func(state, body string) string {
		return fmt.Sprintf(`{"action":"submitted","repository":{"full_name":%q},
			"pull_request":{"head":{"ref":%q}},
			"review":{"state":%q,"body":%q,"html_url":"https://pr/1#r1","user":{"login":"reviewer"}}}`,
			f.project.RepoPath, f.branch, state, body)
	}
	resp := f.postSigned(t, "pull_request_review", review("approved", "ок"))
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("одобрение конвейер не трогает: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("после одобрения статус: %s", got)
	}

	resp = f.postSigned(t, "pull_request_review", review("changes_requested", "поправь обработку ошибок"))
	if s := bodyStatus(t, resp); s != "done" {
		t.Fatalf("запрос изменений возвращает задачу в работу: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskFixing {
		t.Fatalf("после запроса изменений статус: %s", got)
	}
	evs := f.events(t, "task.review_external")
	if len(evs) != 2 {
		t.Fatalf("оба review в timeline: %d", len(evs))
	}
	var seen bool
	for _, e := range evs {
		if e.Payload["state"] == "changes_requested" && e.Payload["author"] == "reviewer" {
			seen = true
			if body, _ := e.Payload["body"].(string); body == "" {
				t.Fatalf("замечания должны сохраниться: %+v", e.Payload)
			}
		}
	}
	if !seen {
		t.Fatalf("событие запроса изменений: %+v", evs)
	}
}

// Закрытый без merge PR не завершает задачу, но поднимает эскалацию.
func TestWebhookPRClosedWithoutMerge(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"action":"closed","repository":{"full_name":%q},
		"sender":{"login":"human"},
		"pull_request":{"merged":false,"html_url":%q,"head":{"ref":%q}}}`,
		f.project.RepoPath, f.prURL, f.branch)
	resp := f.postSigned(t, "pull_request", body)
	mustStatus(t, resp, http.StatusOK, "PR закрыт без merge")
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("закрытие PR не меняет конвейер: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("задача не должна завершаться, got %s", got)
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
		t.Fatalf("эскалация: %s", reason)
	}
}

// Отменённый прогон проверок вердикта не даёт: задача не возвращается в
// работу и «проверки прошли» не пишется.
func TestWebhookIgnoresCancelledChecks(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"repository":{"full_name":%q},"workflow_run":{"name":"CI","status":"completed",
		"conclusion":"cancelled","head_branch":%q}}`, f.project.RepoPath, f.branch)
	resp := f.postSigned(t, "workflow_run", body)
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("отменённый прогон: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("состояние не должно меняться: %s", got)
	}
	if evs := f.events(t, "task.checks_external"); len(evs) != 0 {
		t.Fatalf("события быть не должно: %+v", evs)
	}
}

// Повторное закрытие PR не плодит эскалации.
func TestWebhookPRClosedTwice(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"action":"closed","repository":{"full_name":%q},"sender":{"login":"human"},
		"pull_request":{"merged":false,"html_url":%q,"head":{"ref":%q}}}`,
		f.project.RepoPath, f.prURL, f.branch)
	for i := 0; i < 2; i++ {
		if s := bodyStatus(t, f.postSigned(t, "pull_request", body)); s != "noted" {
			t.Fatalf("закрытие PR #%d: %s", i+1, s)
		}
	}
	var n int
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attention WHERE reason=$1 AND status <> 'resolved'`,
		string(domain.AttPRClosed)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("эскалация должна быть одна, стало %d", n)
	}
}

// Событие о чужой ветке игнорируется без изменения состояния.
func TestWebhookIgnoresForeignBranch(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"repository":{"full_name":%q},"workflow_run":{"name":"CI","status":"completed",
		"conclusion":"failure","head_branch":"чужая/ветка"}}`, f.project.RepoPath)
	resp := f.postSigned(t, "workflow_run", body)
	mustStatus(t, resp, http.StatusOK, "чужая ветка")
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("чужая ветка должна игнорироваться: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("состояние не должно меняться: %s", got)
	}
}

// GitLab: пайплайн, снятие одобрения и закрытие MR доходят до конвейера
// теми же путями, что и события GitHub.
func TestWebhookGitLabEvents(t *testing.T) {
	f := seedHook(t, "gitlab", "group/sub/proj")
	post := func(event, body string) *http.Response {
		return f.post(t, "/api/v1/webhooks/gitlab", body, map[string]string{
			"X-Gitlab-Event": event,
			"X-Gitlab-Token": f.project.WebhookSecret,
			"Content-Type":   "application/json",
		})
	}
	pipeline := fmt.Sprintf(`{"object_kind":"pipeline","project":{"path_with_namespace":%q},
		"user":{"username":"ci"},"object_attributes":{"ref":%q,"status":"failed","url":"https://gl/pipe/1"}}`,
		f.project.RepoPath, f.branch)
	resp := post("Pipeline Hook", pipeline)
	mustStatus(t, resp, http.StatusOK, "пайплайн упал")
	if s := bodyStatus(t, resp); s != "done" {
		t.Fatalf("упавший пайплайн возвращает задачу в работу: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskFixing {
		t.Fatalf("после падения пайплайна: %s", got)
	}

	// Комментарий к MR — информационное событие.
	note := fmt.Sprintf(`{"object_kind":"note","project":{"path_with_namespace":%q},
		"user":{"username":"human"},"merge_request":{"source_branch":%q},
		"object_attributes":{"noteable_type":"MergeRequest","note":"вопрос по коду","url":"https://gl/mr/1#note_1"}}`,
		f.project.RepoPath, f.branch)
	resp = post("Note Hook", note)
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("комментарий конвейер не трогает: %s", s)
	}
	if evs := f.events(t, "task.review_external"); len(evs) != 1 {
		t.Fatalf("комментарий в timeline: %d", len(evs))
	}

	// Закрытие MR без merge — эскалация.
	closed := fmt.Sprintf(`{"object_kind":"merge_request","project":{"path_with_namespace":%q},
		"user":{"username":"human"},"object_attributes":{"action":"close","source_branch":%q,"url":%q}}`,
		f.project.RepoPath, f.branch, f.prURL)
	resp = post("Merge Request Hook", closed)
	if s := bodyStatus(t, resp); s != "noted" {
		t.Fatalf("закрытие MR: %s", s)
	}
	if evs := f.events(t, "task.pr_closed"); len(evs) != 1 {
		t.Fatalf("событие закрытия MR: %d", len(evs))
	}
}

// Review со старого PR не попадает в задачу на той же ветке: событие
// сверяется с задачей по адресу PR (change fix-hosting-events).
func TestWebhookReviewFromForeignPR(t *testing.T) {
	f := seedHook(t, "github", "own/proj")
	body := fmt.Sprintf(`{"action":"submitted","repository":{"full_name":%q},
		"pull_request":{"html_url":"https://github.com/own/proj/pull/999","head":{"ref":%q}},
		"review":{"state":"changes_requested","body":"старое замечание","html_url":"https://pr/999#r1",
		"user":{"login":"reviewer"}}}`, f.project.RepoPath, f.branch)
	resp := f.postSigned(t, "pull_request_review", body)
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("review чужого PR должно игнорироваться: %s", s)
	}
	if got := f.taskStatus(t); got != domain.TaskReview {
		t.Fatalf("состояние не должно меняться: %s", got)
	}
}

// GitLab squash-merge несёт sha в другом поле: задача завершается, версия
// для автопубликации не теряется.
func TestWebhookGitLabSquashMerge(t *testing.T) {
	f := seedHook(t, "gitlab", "group/proj")
	body := fmt.Sprintf(`{"object_kind":"merge_request","project":{"path_with_namespace":%q},
		"user":{"username":"human"},"object_attributes":{"action":"merge","source_branch":%q,
		"url":%q,"squash_commit_sha":"sha-squash"}}`, f.project.RepoPath, f.branch, f.prURL)
	resp := f.post(t, "/api/v1/webhooks/gitlab", body, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": f.project.WebhookSecret,
	})
	mustStatus(t, resp, http.StatusOK, "squash-merge")
	if got := f.taskStatus(t); got != domain.TaskDone {
		t.Fatalf("задача должна завершиться: %s", got)
	}
	// Версия для автопубликации не потерялась: событие merge несёт sha
	// из squash_commit_sha (auto-окружений в фикстуре нет, поэтому
	// проверяем сам разбор).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	if ev := parseGitLabEvent(req, []byte(body)); ev.MergeSHA != "sha-squash" {
		t.Fatalf("sha squash-merge: %q", ev.MergeSHA)
	}
}

// Комментарий со старого MR не привязывается к текущей задаче на той же
// ветке: сверка идёт по адресу MR.
func TestWebhookGitLabNoteFromForeignMR(t *testing.T) {
	f := seedHook(t, "gitlab", "group/proj2")
	body := fmt.Sprintf(`{"object_kind":"note","project":{"path_with_namespace":%q},
		"user":{"username":"human"},"merge_request":{"source_branch":%q,"url":"https://gl/mr/999"},
		"object_attributes":{"noteable_type":"MergeRequest","note":"старый комментарий","url":"https://gl/mr/999#note_1"}}`,
		f.project.RepoPath, f.branch)
	resp := f.post(t, "/api/v1/webhooks/gitlab", body, map[string]string{
		"X-Gitlab-Event": "Note Hook",
		"X-Gitlab-Token": f.project.WebhookSecret,
	})
	if s := bodyStatus(t, resp); s != "ignored" {
		t.Fatalf("комментарий чужого MR должен игнорироваться: %s", s)
	}
}
