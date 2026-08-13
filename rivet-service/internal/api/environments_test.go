package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Фикстура окружений: админ (владелец проекта), участник, не-участник.
type envFixture struct {
	srv                    *httptest.Server
	st                     *store.Store
	projectID              string
	admin, member, mallory string // логины (пароль pw)
}

func seedEnvAPI(t *testing.T) envFixture {
	t.Helper()
	ctx := context.Background()
	st, _ := testServer(t)

	f := envFixture{st: st}
	suffix := time.Now().UnixNano()
	f.admin = fmt.Sprintf("boss-%d", suffix)
	f.member = fmt.Sprintf("dev-%d", suffix)
	f.mallory = fmt.Sprintf("mallory-%d", suffix)
	adminU, err := st.CreateUser(ctx, f.admin, "", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.member, "", "pw", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.mallory, "", "pw", false); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, adminU.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.projectID = p.ID
	if err := st.AddMember(ctx, p.ID, f.member); err != nil {
		t.Fatal(err)
	}

	// Engine нужен ручке deploy (HeadSHA у SCM).
	engine := orchestrator.New(st, scm.NewFake(), nil, nopSender{}, 90*time.Second)
	f.srv = httptest.NewServer((&Server{St: st, Engine: engine}).Handler())
	t.Cleanup(f.srv.Close)
	return f
}

type nopSender struct{}

func (nopSender) Send(string, *pb.PlaneMsg) bool { return false }

func validEnvBody(name string) map[string]any {
	return map[string]any{
		"name": name, "exec_type": "ssh", "trigger": "manual",
		"config": map[string]any{"deploy_cmd": "true", "verify_cmd": "true"},
	}
}

// CRUD окружений: только админ; участник видит список, не-участник — 404.
func TestEnvironmentAPIRolesAndValidation(t *testing.T) {
	f := seedEnvAPI(t)
	admin := loginSession(t, f.srv, f.admin, "pw")
	member := loginSession(t, f.srv, f.member, "pw")
	mallory := loginSession(t, f.srv, f.mallory, "pw")
	base := f.srv.URL + "/api/v1/projects/" + f.projectID + "/environments"

	// Участник не создаёт (403), админ создаёт (201).
	resp, _ := call(t, "POST", base, member, "", validEnvBody("staging"))
	mustStatus(t, resp, http.StatusForbidden, "создание участником")
	resp, body := call(t, "POST", base, admin, "", validEnvBody("staging"))
	mustStatus(t, resp, http.StatusCreated, "создание админом")
	var env struct {
		ID     string `json:"id"`
		Paused bool   `json:"paused"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}

	// 409 на дубль, 422 без Verify.
	resp, _ = call(t, "POST", base, admin, "", validEnvBody("staging"))
	mustStatus(t, resp, http.StatusConflict, "дубль имени")
	noVerify := validEnvBody("prod")
	noVerify["config"] = map[string]any{"deploy_cmd": "true"}
	resp, _ = call(t, "POST", base, admin, "", noVerify)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "без Verify")

	// Участник видит список, не-участник получает 404 проекта.
	resp, body = call(t, "GET", base, member, "", nil)
	mustStatus(t, resp, http.StatusOK, "список участнику")
	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil || len(list) != 1 {
		t.Fatalf("ожидалось одно окружение: %s", body)
	}
	resp, _ = call(t, "GET", base, mallory, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "список не-участнику")

	// PATCH участником — 403; админом — 200 (replace config с валидацией).
	envURL := f.srv.URL + "/api/v1/environments/" + env.ID
	resp, _ = call(t, "PATCH", envURL, member, "", map[string]any{"trigger": "auto"})
	mustStatus(t, resp, http.StatusForbidden, "правка участником")
	resp, _ = call(t, "PATCH", envURL, admin, "", map[string]any{"trigger": "auto"})
	mustStatus(t, resp, http.StatusOK, "правка админом")
	// config заменяется целиком (replace): пустой config теряет Verify → 422.
	resp, _ = call(t, "PATCH", envURL, admin, "", map[string]any{"config": map[string]any{"deploy_cmd": "true"}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "replace config без Verify")

	// Пустая история — [].
	resp, body = call(t, "GET", envURL+"/deployments", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "история")
	if string(body) != "[]\n" {
		t.Fatalf("ожидался [], получено %q", body)
	}

	// DELETE участником — 403, админом — 200.
	resp, _ = call(t, "DELETE", envURL, member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "удаление участником")
	resp, _ = call(t, "DELETE", envURL, admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "удаление админом")
}

// Запуск и resume: участник запускает (202 queued, повтор коалесцируется),
// на паузе — 409 до resume; лог без blob — 404.
func TestEnvironmentDeployAndResume(t *testing.T) {
	f := seedEnvAPI(t)
	ctx := context.Background()
	admin := loginSession(t, f.srv, f.admin, "pw")
	member := loginSession(t, f.srv, f.member, "pw")
	base := f.srv.URL + "/api/v1/projects/" + f.projectID + "/environments"

	resp, body := call(t, "POST", base, admin, "", validEnvBody("staging"))
	mustStatus(t, resp, http.StatusCreated, "создание")
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	envURL := f.srv.URL + "/api/v1/environments/" + env.ID

	// Участник запускает: 202 queued с версией HEAD от SCM.
	resp, body = call(t, "POST", envURL+"/deploy", member, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "ручной запуск")
	var dep struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Status != "queued" || dep.Version == "" {
		t.Fatalf("ожидалась queued с версией: %+v", dep)
	}
	// Повторный запуск коалесцируется в ту же queued.
	resp, body = call(t, "POST", envURL+"/deploy", member, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "повторный запуск")
	var dep2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &dep2)
	if dep2.ID != dep.ID {
		t.Fatalf("повтор должен коалесцироваться: %s != %s", dep2.ID, dep.ID)
	}

	// Пауза (провал) → deploy 409, resume участником → deploy снова работает.
	if err := f.st.SetEnvPaused(ctx, env.ID, true); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", envURL+"/deploy", member, "", nil)
	mustStatus(t, resp, http.StatusConflict, "запуск на паузе")
	resp, _ = call(t, "POST", envURL+"/resume", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "resume")
	resp, _ = call(t, "POST", envURL+"/deploy", member, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "запуск после resume")

	// История не пуста, лог не сохранён — 404.
	resp, body = call(t, "GET", envURL+"/deployments", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "история")
	var deps []struct {
		ID     string `json:"id"`
		HasLog bool   `json:"has_log"`
	}
	if err := json.Unmarshal(body, &deps); err != nil || len(deps) != 1 {
		t.Fatalf("ожидалась одна публикация: %s", body)
	}
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/deployments/"+deps[0].ID+"/log", member, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "лог без blob")

	// Не-участник не видит ни окружение, ни deploy.
	mallory := loginSession(t, f.srv, f.mallory, "pw")
	resp, _ = call(t, "POST", envURL+"/deploy", mallory, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "deploy не-участником")
}
