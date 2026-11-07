package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Выпуск токена с привязкой к runner'ам (add-runner-identity, спека
// runners «Токены регистрации runner'ов», api-contract).
func TestRunnerTokenBindingsAPI(t *testing.T) {
	f := seedOps(t, false)
	ctx := context.Background()

	// Ошибка списка даёт 422 с полем runner_ids, токен не создаётся.
	long := strings.Repeat("r", domain.MaxRunnerIDLen+1)
	many := make([]string, maxTokenRunners+1)
	for i := range many {
		many[i] = "r" + strings.Repeat("x", i)
	}
	for name, ids := range map[string][]string{
		"пробел":      {"local worker"},
		"пустой":      {""},
		"повтор":      {"w1", "w1"},
		"длинный":     {long},
		"управляющий": {"w1\n"},
		"много":       many,
	} {
		resp, body := call(t, "POST", f.srv.URL+"/api/v1/runner-tokens", f.admin, "", map[string]any{"name": "bad", "runner_ids": ids})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "runner_ids: "+name)
		if !jsonContains(body, `"field":"runner_ids"`) {
			t.Fatalf("%s: ошибка без поля: %s", name, body)
		}
	}
	if list, _ := f.st.ListRunnerTokens(ctx); len(list) != 0 {
		t.Fatalf("токен создан при ошибке: %+v", list)
	}

	// Токен хоста: привязки в ответе выпуска и в списке.
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/runner-tokens", f.admin, "",
		map[string]any{"name": "local-host", "runner_ids": []string{"local-worker", "local-reviewer"}})
	mustStatus(t, resp, http.StatusCreated, "выпуск для runner'ов")
	var created struct {
		Token runnerTokenView `json:"token"`
	}
	_ = json.Unmarshal(body, &created)
	if len(created.Token.Runners) != 2 || created.Token.Runners[0].RunnerID != "local-worker" || created.Token.Runners[0].Source != "issue" {
		t.Fatalf("привязки при выпуске: %s", body)
	}
	// Без списка токен привяжется при первой регистрации, привязки пустым массивом, не null.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/runner-tokens", f.admin, "", map[string]any{"name": "free"})
	mustStatus(t, resp, http.StatusCreated, "выпуск без списка")
	if !jsonContains(body, `"runners":[]`) {
		t.Fatalf("токен без привязки: %s", body)
	}
	var freeTok struct {
		Token runnerTokenView `json:"token"`
	}
	_ = json.Unmarshal(body, &freeTok)
	if _, err := f.st.AuthorizeRunner(ctx, freeTok.Token.ID, "build-7", true); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/runner-tokens", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "список")
	var list []runnerTokenView
	_ = json.Unmarshal(body, &list)
	got := map[string]string{}
	for _, tok := range list {
		for _, b := range tok.Runners {
			got[tok.Name+"/"+b.RunnerID] = b.Source
		}
	}
	if len(got) != 3 || got["local-host/local-reviewer"] != "issue" || got["free/build-7"] != "first_use" {
		t.Fatalf("привязки в списке: %v (%s)", got, body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation&type=runner_token.bound", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "аудит привязки")
	if !jsonContains(body, "build-7") {
		t.Fatalf("событие привязки не в аудите: %s", body)
	}
}

// Состояние установки сообщает консоли, требует ли протокол клиентский
// сертификат: из этого собирается команда запуска runner'а.
func TestSystemStatusClientCert(t *testing.T) {
	f := seedOps(t, false)
	for _, required := range []bool{false, true} {
		f.api.GRPCTLS, f.api.GRPCClientCert = required, required
		resp, body := call(t, "GET", f.srv.URL+"/api/v1/system/status", f.admin, "", nil)
		mustStatus(t, resp, http.StatusOK, "статус")
		var out struct {
			Components []struct {
				Name string         `json:"name"`
				Data map[string]any `json:"data"`
			} `json:"components"`
		}
		_ = json.Unmarshal(body, &out)
		found := false
		for _, c := range out.Components {
			if c.Name == "runners" {
				found = true
				if c.Data["client_cert"] != required || c.Data["tls"] != required {
					t.Fatalf("client_cert=%v: %v", required, c.Data)
				}
			}
		}
		if !found {
			t.Fatalf("нет компонента runners: %s", body)
		}
	}
}
