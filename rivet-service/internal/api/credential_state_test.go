package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Отозванный токен виден в состоянии подключения с причиной, а замена
// токена возвращает его в работу (change fix-credential-state, спека
// scm-integration «Отозванный доступ виден», «Замена токена»).
func TestRevokedCredentialsVisible(t *testing.T) {
	var revoked atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if revoked.Load() && auth == "Bearer ghp_first_token_value" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /api/v3/user":
			_, _ = w.Write([]byte(`{"login":"bot"}`))
		case "GET /api/v3/repos/own/proj":
			_, _ = w.Write([]byte(`{"full_name":"own/proj","default_branch":"main","permissions":{"push":true,"pull":true}}`))
		case "GET /api/v3/repos/own/proj/branches/main":
			_, _ = w.Write([]byte(`{"commit":{"sha":"abc"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(host.Close)
	f := seedRepoAPI(t, host, true)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")
	ctx := context.Background()

	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects", sess, "", map[string]any{
		"name": "demo", "provider": "github", "repo_url": host.URL + "/own/proj", "token": "ghp_first_token_value",
	})
	mustStatus(t, resp, http.StatusCreated, "создание проекта")
	var p struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	f.engine.WaitArchitecture()
	repo := func() repositoryView {
		t.Helper()
		resp, body := call(t, "GET", f.srv.URL+"/api/v1/projects/"+p.ID+"/repository", sess, "", nil)
		mustStatus(t, resp, http.StatusOK, "состояние подключения")
		var v repositoryView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	attention := func() []domain.Attention {
		t.Helper()
		resp, body := call(t, "GET", f.srv.URL+"/api/v1/attention", sess, "", nil)
		mustStatus(t, resp, http.StatusOK, "очередь эскалаций")
		var out []domain.Attention
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if v := repo(); v.State != "ok" || v.StateReason != "" {
		t.Fatalf("новое подключение рабочее: %+v", v)
	}

	// Токен отозвали на хостинге: первая же операция конвейера это видит.
	revoked.Store(true)
	// Снимок архитектуры читает вершину ветки: сбой хостинга он пишет в
	// свой статус, а состояние подключения отмечает адаптер.
	if err := f.engine.RefreshArchitectureNow(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	v := repo()
	if v.State != "invalid" || v.StateReason != "bad_token" || !strings.Contains(v.StateMessage, "токен не принят") {
		t.Fatalf("нерабочее состояние с причиной: %+v", v)
	}
	if atts := attention(); len(atts) != 1 || atts[0].Reason != "SCM_CREDENTIALS" || atts[0].TaskID != "" {
		t.Fatalf("эскалация уровня проекта: %+v", atts)
	}

	// Участник вводит новый токен: подключение снова рабочее, эскалация снята.
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+p.ID+"/credentials", sess, "",
		map[string]any{"token": "ghp_second_token_value"})
	mustStatus(t, resp, http.StatusOK, "замена токена")
	f.engine.WaitArchitecture()
	if v := repo(); v.State != "ok" || v.StateReason != "" || v.StateMessage != "" {
		t.Fatalf("после замены подключение рабочее: %+v", v)
	}
	if atts := attention(); len(atts) != 0 {
		t.Fatalf("эскалация снята: %+v", atts)
	}
}
