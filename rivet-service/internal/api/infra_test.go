package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds/ruvdstest"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Подключения инфраструктуры и действия с хостом окружения
// (add-environment-provisioning): токен и подтверждения только из сессии
// консоли, подтверждает владелец проекта, участник видит и обновляет.

const fakeRUVDSToken = "fake-ruvds-token-for-tests"

type infraFixture struct {
	envFixture
	fake   *ruvdstest.Fake
	engine *orchestrator.Engine
	owner  string // владелец без прав администратора
}

func seedInfraAPI(t *testing.T) infraFixture {
	t.Helper()
	ctx := context.Background()
	st, _ := testServer(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	fake := ruvdstest.New()
	rs := fake.Start()
	t.Cleanup(rs.Close)
	f := infraFixture{fake: fake}
	f.st = st
	suffix := time.Now().UnixNano()
	f.admin, f.member, f.owner = fmt.Sprintf("boss-%d", suffix), fmt.Sprintf("dev-%d", suffix), fmt.Sprintf("own-%d", suffix)
	adminU, err := st.CreateUser(ctx, f.admin, "", "pw-testpass", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.member, "", "pw-testpass", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.owner, "", "pw-testpass", false); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, adminU.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.projectID = p.ID
	if err := st.AddMember(ctx, p.ID, f.member, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, p.ID, f.owner, domain.RoleOwner); err != nil {
		t.Fatal(err)
	}
	engine := orchestrator.New(st, scm.NewFake(), nil, nopSender{}, 90*time.Second)
	engine.Box = box
	engine.Infra = &infra.Factory{RUVDSBaseURL: rs.URL}
	f.engine = engine
	f.srv = httptest.NewServer((&Server{St: st, Engine: engine, Secrets: box}).Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func pat(t *testing.T, srv *httptest.Server, session string) string {
	t.Helper()
	resp, body := call(t, "POST", srv.URL+"/api/v1/tokens", session, "", map[string]string{"name": "cli"})
	mustStatus(t, resp, http.StatusCreated, "PAT")
	var out struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Secret
}

func TestInfraConnectionAPI(t *testing.T) {
	f := seedInfraAPI(t)
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	base := f.srv.URL + "/api/v1/system/infra-connections"

	resp, _ := call(t, "PUT", base+"/ruvds", member, "", map[string]any{"provider": "ruvds", "name": "RUVDS", "token": fakeRUVDSToken})
	mustStatus(t, resp, http.StatusForbidden, "не администратор")
	// Токен по PAT не принимается: его вводит человек в консоли.
	token := pat(t, f.srv, admin)
	resp, body := call(t, "PUT", base+"/ruvds", "", token, map[string]any{"provider": "ruvds", "name": "RUVDS", "token": fakeRUVDSToken})
	mustStatus(t, resp, http.StatusForbidden, "токен по PAT")
	if !strings.Contains(string(body), "session_required") {
		t.Fatalf("отказ PAT: %s", body)
	}
	resp, body = call(t, "PUT", base+"/ruvds", admin, "", map[string]any{"provider": "ruvds", "name": "RUVDS", "token": fakeRUVDSToken})
	mustStatus(t, resp, http.StatusCreated, "создание")
	var out struct {
		Connection infraConnView `json:"connection"`
	}
	_ = json.Unmarshal(body, &out)
	c := out.Connection
	if c.State != "ok" || c.Balance == nil || c.Balance.Amount != 10000 || c.TokenPrefix == "" ||
		strings.Contains(string(body), fakeRUVDSToken) {
		t.Fatalf("подключение: %s", body)
	}
	// Неверный токен: invalid, значение нигде не показано.
	resp, body = call(t, "PUT", base+"/bad", admin, "", map[string]any{"provider": "ruvds", "name": "Bad", "token": "fake-invalid-token-x"})
	mustStatus(t, resp, http.StatusCreated, "неверный токен")
	if !strings.Contains(string(body), `"state":"invalid"`) || strings.Contains(string(body), "fake-invalid-token-x") {
		t.Fatalf("неверный токен: %s", body)
	}
	// Правка без токена по PAT допустима (токен не меняется).
	resp, _ = call(t, "PUT", base+"/ruvds", "", token, map[string]any{"provider": "ruvds", "name": "RUVDS main"})
	mustStatus(t, resp, http.StatusOK, "правка названия")
	resp, body = call(t, "GET", base+"/ruvds/catalog", admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "справочники")
	if !strings.Contains(string(body), "Ubuntu 24.04") || strings.Contains(string(body), "Windows") {
		t.Fatalf("справочники: %s", body)
	}
	// Аудит без значения токена.
	var leaked int
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE payload::text LIKE '%'||$1||'%' OR text LIKE '%'||$1||'%'`,
		fakeRUVDSToken).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("токен попал в event log")
	}
	// Подключение окружения нельзя удалить.
	envBody := ruvdsEnvBody("staging")
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/projects/"+f.projectID+"/environments", admin, "", envBody)
	mustStatus(t, resp, http.StatusCreated, "окружение RUVDS: "+string(body))
	resp, body = call(t, "DELETE", base+"/ruvds", admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "удаление используемого")
	if !strings.Contains(string(body), "demo/staging") {
		t.Fatalf("ссылки: %s", body)
	}
}

func ruvdsEnvBody(name string) map[string]any {
	return map[string]any{
		"name": name, "exec_type": "ssh", "trigger": "manual",
		"config": map[string]any{"deploy_cmd": "docker compose up -d", "verify_cmd": "docker compose ps",
			"host": "attacker.example", "host_key": "",
			"provision": map[string]any{"provider": "ruvds", "connection": "ruvds", "datacenter": 1, "tariff_id": 14,
				"os_id": 52, "payment_period": 2, "cpu": 2, "ram": 2, "drive": 20, "drive_tariff_id": 1,
				"ssh_public_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForRivetTestsOnly deploy@runner"}},
	}
}

func TestHostActionsAPI(t *testing.T) {
	f := seedInfraAPI(t)
	ctx := context.Background()
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	owner := loginSession(t, f.srv, f.owner, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	resp, _ := call(t, "PUT", f.srv.URL+"/api/v1/system/infra-connections/ruvds", admin, "",
		map[string]any{"provider": "ruvds", "name": "RUVDS", "token": fakeRUVDSToken})
	mustStatus(t, resp, http.StatusCreated, "подключение")

	// Ошибка привязки: у поля.
	bad := ruvdsEnvBody("prod")
	bad["config"].(map[string]any)["provision"].(map[string]any)["ssh_public_key"] = "not a key"
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/projects/"+f.projectID+"/environments", admin, "", bad)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "плохой ключ")
	if !strings.Contains(string(body), "config.provision.ssh_public_key") {
		t.Fatalf("поле ошибки: %s", body)
	}
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/projects/"+f.projectID+"/environments", admin, "", ruvdsEnvBody("staging"))
	mustStatus(t, resp, http.StatusCreated, "окружение")
	var env environmentView
	_ = json.Unmarshal(body, &env)
	if env.Config.Host != "" || env.Host == nil || env.Host.State != domain.HostQuoting {
		t.Fatalf("окружение с привязкой: host=%q хост=%+v", env.Config.Host, env.Host)
	}
	// Цена посчитана тиком: решение ждёт владельца.
	hostURL := f.srv.URL + "/api/v1/environments/" + env.ID + "/host/"
	waitState := func(want string) envHostView {
		t.Helper()
		for i := 0; i < 50; i++ {
			resp, body := call(t, "GET", f.srv.URL+"/api/v1/projects/"+f.projectID+"/environments", member, "", nil)
			mustStatus(t, resp, http.StatusOK, "список")
			var list []environmentView
			_ = json.Unmarshal(body, &list)
			for _, e := range list {
				if e.ID == env.ID && e.Host != nil && e.Host.State == want {
					return *e.Host
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("хост не дошёл до %s", want)
		return envHostView{}
	}
	tickHosts(t, f)
	h := waitState(domain.HostAwaitingApproval)
	if h.Quote == nil || h.Quote.Amount != 658 || h.Quote.PeriodLabel != "1 месяц" || h.SpecLabels["os"] != "Ubuntu 24.04" {
		t.Fatalf("решение: %+v", h)
	}
	amount := map[string]any{"amount": h.Quote.Amount}
	// Участник не подтверждает, PAT владельца не подтверждает.
	resp, _ = call(t, "POST", hostURL+"approve", member, "", amount)
	mustStatus(t, resp, http.StatusForbidden, "участник подтверждает")
	ownerPAT := pat(t, f.srv, owner)
	resp, body = call(t, "POST", hostURL+"approve", "", ownerPAT, amount)
	mustStatus(t, resp, http.StatusForbidden, "подтверждение по PAT")
	if !strings.Contains(string(body), "session_required") {
		t.Fatalf("отказ PAT: %s", body)
	}
	resp, body = call(t, "POST", hostURL+"approve", owner, "", map[string]any{"amount": 1})
	mustStatus(t, resp, http.StatusConflict, "чужая сумма")
	if !strings.Contains(string(body), "price_changed") {
		t.Fatalf("чужая сумма: %s", body)
	}
	if f.fake.Creates() != 0 {
		t.Fatal("создание до подтверждения")
	}
	resp, body = call(t, "POST", hostURL+"approve", owner, "", amount)
	mustStatus(t, resp, http.StatusOK, "подтверждение: "+string(body))
	if !strings.Contains(string(body), `"state":"approved"`) {
		t.Fatalf("после подтверждения: %s", body)
	}
	// Решение в event log с логином владельца.
	var n int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='environment.host' AND actor_id=$1
		AND payload->>'action'='approve'`, f.owner).Scan(&n); err != nil || n != 1 {
		t.Fatalf("событие подтверждения: %d %v", n, err)
	}
	// Повтор действия не в своём состоянии.
	resp, body = call(t, "POST", hostURL+"approve", owner, "", amount)
	mustStatus(t, resp, http.StatusConflict, "повторное подтверждение")
	if !strings.Contains(string(body), "bad_state") {
		t.Fatalf("повтор: %s", body)
	}
	// Смена провайдера при живом хосте отклоняется, правка команд: нет.
	upd := ruvdsEnvBody("staging")["config"].(map[string]any)
	upd["deploy_cmd"] = "docker compose up -d --build"
	resp, body = call(t, "PATCH", f.srv.URL+"/api/v1/environments/"+env.ID, admin, "", map[string]any{"config": upd})
	mustStatus(t, resp, http.StatusOK, "правка команд: "+string(body))
	upd["provision"].(map[string]any)["cpu"] = 4
	resp, _ = call(t, "PATCH", f.srv.URL+"/api/v1/environments/"+env.ID, admin, "", map[string]any{"config": upd})
	mustStatus(t, resp, http.StatusConflict, "смена ресурсов при живом хосте")
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/environments/"+env.ID, admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "удаление окружения с хостом")
	// Обновление стоимости: участнику можно, но только у готового хоста.
	resp, _ = call(t, "POST", hostURL+"refresh", member, "", nil)
	mustStatus(t, resp, http.StatusConflict, "обновление не готового хоста")
	resp, _ = call(t, "POST", hostURL+"delete", member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "участник удаляет")
	// Эскалация видна с предметом-хостом.
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/attention", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "эскалации")
	if strings.Contains(string(body), "HOST_CREATE") {
		t.Fatalf("решение не закрылось подтверждением: %s", body)
	}
}

// tickHosts: проход планировщика: хосты, которым пора, обрабатываются.
func tickHosts(t *testing.T, f infraFixture) {
	t.Helper()
	if _, err := f.st.Pool.Exec(context.Background(), `UPDATE env_hosts SET next_attempt_at=now(), lease_until=NULL`); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
