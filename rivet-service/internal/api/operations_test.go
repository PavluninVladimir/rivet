package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Сценарии спек runners, observability, epic-decomposition
// (change add-operations-management).

type opsFixture struct {
	st    *store.Store
	srv   *httptest.Server
	api   *Server
	admin string // сессия администратора
	user  string // сессия участника без прав
	uid   domain.User
}

func seedOps(t *testing.T, withKey bool) opsFixture {
	t.Helper()
	ctx := context.Background()
	st, _ := testServer(t)
	suffix := time.Now().UnixNano()
	adminLogin, userLogin := fmt.Sprintf("root-%d", suffix), fmt.Sprintf("user-%d", suffix)
	if _, err := st.CreateUser(ctx, adminLogin, "", "pw-testpass", true); err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, userLogin, "", "pw-testpass", false)
	if err != nil {
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
	api := &Server{St: st, Secrets: box, Version: "test", ProtocolVersion: "5", StartedAt: time.Now()}
	if err := api.ReloadPlanner(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return opsFixture{
		st: st, srv: srv, api: api,
		admin: loginSession(t, srv, adminLogin, "pw-testpass"),
		user:  loginSession(t, srv, userLogin, "pw-testpass"),
		uid:   u,
	}
}

func TestRunnerTokensAPI(t *testing.T) {
	f := seedOps(t, false)
	ctx := context.Background()

	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/runner-tokens", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "токены не-админом")

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/runner-tokens", f.admin, "", map[string]any{"name": "fleet"})
	mustStatus(t, resp, http.StatusCreated, "создание токена")
	var created struct {
		Token  runnerTokenView `json:"token"`
		Secret string          `json:"secret"`
	}
	_ = json.Unmarshal(body, &created)
	if created.Secret == "" || created.Token.Prefix == "" || created.Token.RevokedAt != nil {
		t.Fatalf("неожиданный ответ создания: %s", body)
	}

	// Секрет принимается и отмечает использование; в списке секрета нет.
	tok, err := f.st.RunnerTokenBySecret(ctx, created.Secret)
	if err != nil || tok.LastUsed == nil {
		t.Fatalf("секрет не принят: %v", err)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/runner-tokens", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "список токенов")
	var list []runnerTokenView
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("список = %s", body)
	}
	if string(body) != "" && jsonContains(body, created.Secret) {
		t.Fatal("секрет утёк в список")
	}

	// Отзыв: повторный отзыв — 409, секрет больше не принимается.
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/runner-tokens/"+created.Token.ID, f.admin, "", nil)
	mustStatus(t, resp, http.StatusNoContent, "отзыв")
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/runner-tokens/"+created.Token.ID, f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "повторный отзыв")
	if _, err := f.st.RunnerTokenBySecret(ctx, created.Secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("отозванный секрет принят: %v", err)
	}
	// Неизвестный и чужой по формату секреты — тот же отказ.
	if _, err := f.st.RunnerTokenBySecret(ctx, "rrt_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("неизвестный секрет принят")
	}

	// События установки: created и revoked видны администратору в аудите,
	// участнику — 403, а в обычной ленте их нет.
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "аудит")
	if !jsonContains(body, "runner_token.created") || !jsonContains(body, "runner_token.revoked") {
		t.Fatalf("в аудите нет событий токена: %s", body)
	}
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "аудит участником")
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "обычная лента")
	if jsonContains(body, "runner_token.created") {
		t.Fatalf("событие установки утекло в ленту проектов: %s", body)
	}
}

func TestSystemStatus(t *testing.T) {
	f := seedOps(t, false)
	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/system/status", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "статус не-админом")

	resp, body := call(t, "GET", f.srv.URL+"/api/v1/system/status", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "статус")
	var out struct {
		Status     string          `json:"status"`
		Version    string          `json:"version"`
		Components []componentView `json:"components"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Version != "test" || out.Status != "degraded" {
		t.Fatalf("статус = %s", body)
	}
	got := map[string]string{}
	for _, c := range out.Components {
		got[c.Name] = c.Status
	}
	// База отвечает; blob не подключён, ключа и модели нет, runner'ов нет —
	// деградация, не отказ (спека «Хранилище транскриптов не подключено»).
	want := map[string]string{"database": "ok", "blob": "degraded", "secrets": "degraded", "planner": "degraded", "runners": "degraded"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("компонент %s = %q, ожидался %q (%s)", k, got[k], v, body)
		}
	}
}

func TestUsageInstallationScope(t *testing.T) {
	f := seedOps(t, false)
	ctx := context.Background()
	// Проект участника и чужой проект: участник видит свой, администратор в
	// установочном срезе — оба с названиями.
	mine, err := f.st.CreateProject(ctx, "mine", "org/mine", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.st.CreateUser(ctx, fmt.Sprintf("other-%d", time.Now().UnixNano()), "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.st.CreateProject(ctx, "theirs", "org/theirs", nil, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	ten := int64(10)
	for i, p := range []domain.Project{mine, theirs} {
		if err := f.st.RecordUsage(ctx, store.UsageInput{
			SourceMsgID: fmt.Sprintf("m-%d-%d", time.Now().UnixNano(), i), ProjectID: p.ID,
			RunnerID: "r1", Model: "m", TokensIn: &ten, TokensOut: &ten, DurationS: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/usage?scope=installation&group_by=project", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "установочный срез участником")

	resp, body := call(t, "GET", f.srv.URL+"/api/v1/usage?group_by=project", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "usage участника")
	var rows []store.UsageRow
	_ = json.Unmarshal(body, &rows)
	if len(rows) != 1 || rows[0].Key != mine.ID || rows[0].Label != "mine" {
		t.Fatalf("usage участника = %s", body)
	}

	resp, body = call(t, "GET", f.srv.URL+"/api/v1/usage?scope=installation&group_by=project", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "установочный срез")
	_ = json.Unmarshal(body, &rows)
	if len(rows) != 2 || !jsonContains(body, `"label":"theirs"`) {
		t.Fatalf("установочный срез = %s", body)
	}
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/usage?group_by=bogus", f.admin, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неизвестная группировка")
}

func jsonContains(body []byte, s string) bool { return strings.Contains(string(body), s) }
