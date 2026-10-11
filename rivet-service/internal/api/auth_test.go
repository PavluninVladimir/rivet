package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// testServer поднимает API поверх изолированной БД (паттерн store/testStore).
func testServer(t *testing.T) (*store.Store, *httptest.Server) {
	st, srv, _ := testServerEngine(t)
	return st, srv
}

// testServerEngine — то же с движком: тесты, которым нужен тик планировщика.
func testServerEngine(t *testing.T) (*store.Store, *httptest.Server, *orchestrator.Engine) {
	t.Helper()
	base := os.Getenv("RIVET_DATABASE_URL")
	if base == "" {
		base = "postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("postgres недоступен: %v", err)
	}
	name := fmt.Sprintf("rivet_api_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(base)
	testURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := store.Migrate(ctx, testURL); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	// Оркестратор нужен обработчикам, которые ходят в конвейер (webhook,
	// merge): в тестах он на fake-хостинге и без отправки runner'ам.
	engine := orchestrator.New(st, scm.NewFake(), nil, nopSender{}, 90*time.Second)
	srv := httptest.NewServer((&Server{St: st, Engine: engine}).Handler())
	t.Cleanup(srv.Close)
	return st, srv, engine
}

// call — запрос с опциональными cookie сессии и bearer-токеном.
func call(t *testing.T, method, url, session, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp, out.Bytes()
}

func loginSession(t *testing.T, srv *httptest.Server, login, password string) string {
	t.Helper()
	resp, body := call(t, "POST", srv.URL+"/api/v1/auth/login", "", "",
		map[string]string{"login": login, "password": password})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("вход %s: HTTP %d %s", login, resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly {
				t.Fatal("cookie сессии должна быть HttpOnly")
			}
			return c.Value
		}
	}
	t.Fatal("нет cookie сессии в ответе login")
	return ""
}

func mustStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s: HTTP %d, ожидался %d", what, resp.StatusCode, want)
	}
}

// Сценарии спеки «Аутентификация пользователей»: вход/выход, 401 без данных.
func TestAuthLoginLogout(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}

	// Без аутентификации — 401 без данных установки.
	resp, body := call(t, "GET", srv.URL+"/api/v1/projects", "", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "без аутентификации")
	if strings.Contains(string(body), "demo") {
		t.Fatalf("401 не должен раскрывать данные: %s", body)
	}

	// Неверный пароль и неверный логин неразличимы.
	r1, b1 := call(t, "POST", srv.URL+"/api/v1/auth/login", "", "", map[string]string{"login": "root", "password": "wrong"})
	r2, b2 := call(t, "POST", srv.URL+"/api/v1/auth/login", "", "", map[string]string{"login": "ghost", "password": "wrong"})
	if r1.StatusCode != http.StatusUnauthorized || r2.StatusCode != http.StatusUnauthorized || string(b1) != string(b2) {
		t.Fatalf("ответы должны быть одинаковыми 401: %d %s против %d %s", r1.StatusCode, b1, r2.StatusCode, b2)
	}

	session := loginSession(t, srv, "root", "root-secret")
	resp, body = call(t, "GET", srv.URL+"/api/v1/auth/me", session, "", nil)
	mustStatus(t, resp, http.StatusOK, "me")
	if !strings.Contains(string(body), `"login":"root"`) {
		t.Fatalf("me должен вернуть пользователя: %s", body)
	}

	// Выход гасит сессию, повторное использование cookie — 401.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/auth/logout", session, "", nil)
	mustStatus(t, resp, http.StatusOK, "logout")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", session, "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "cookie после logout")
}

// Сценарий «Перебор пароля замедляется»: после серии ошибок вход блокируется
// даже с верным паролем, ответ остаётся одинаковым 401.
func TestLoginBackoff(t *testing.T) {
	st, srv := testServer(t)
	if err := st.Bootstrap(context.Background(), "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < throttleFreeAttempts+1; i++ {
		resp, _ := call(t, "POST", srv.URL+"/api/v1/auth/login", "", "", map[string]string{"login": "root", "password": "wrong"})
		mustStatus(t, resp, http.StatusUnauthorized, "неверный пароль")
	}
	resp, _ := call(t, "POST", srv.URL+"/api/v1/auth/login", "", "", map[string]string{"login": "root", "password": "root-secret"})
	mustStatus(t, resp, http.StatusUnauthorized, "верный пароль в окне задержки")
}

// Сценарий «Personal access token»: создание, использование, отзыв, срок.
func TestAccessTokens(t *testing.T) {
	st, srv := testServer(t)
	if err := st.Bootstrap(context.Background(), "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, "root", "root-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/tokens", session, "", map[string]string{"name": "cli"})
	mustStatus(t, resp, http.StatusCreated, "создание PAT")
	var created struct {
		Secret string
		Token  domain.AccessToken
	}
	if err := json.Unmarshal(body, &created); err != nil || !strings.HasPrefix(created.Secret, "rvt_") {
		t.Fatalf("нет секрета rvt_: %s", body)
	}

	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusOK, "запрос с PAT")

	// Список не содержит секрета.
	_, body = call(t, "GET", srv.URL+"/api/v1/tokens", session, "", nil)
	if strings.Contains(string(body), created.Secret) {
		t.Fatal("секрет не должен возвращаться в списке")
	}

	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/tokens/"+created.Token.ID, session, "", nil)
	mustStatus(t, resp, http.StatusOK, "отзыв PAT")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "отозванный PAT")

	// Истёкший токен не аутентифицирует.
	past := time.Now().Add(-time.Hour)
	resp, body = call(t, "POST", srv.URL+"/api/v1/tokens", session, "",
		map[string]any{"name": "old", "expires_at": past.Format(time.RFC3339)})
	mustStatus(t, resp, http.StatusCreated, "создание истёкшего PAT")
	_ = json.Unmarshal(body, &created)
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "истёкший PAT")
}

// Слой 1: второй пользователь не видит чужой проект ни в одном списке,
// точечные ручки отвечают 404, у админа обхода нет.
func TestProjectVisibility(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	root := loginSession(t, srv, "root", "root-secret")

	// alice создаёт проект и epic.
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "",
		map[string]string{"name": "secret-project", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "создание проекта")
	var project domain.Project
	_ = json.Unmarshal(body, &project)
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "",
		map[string]string{"title": "Epic", "goal": "g"})
	mustStatus(t, resp, http.StatusCreated, "создание epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)

	// Админ root — не участник: списки пусты, точечные ручки 404 (без обхода).
	_, body = call(t, "GET", srv.URL+"/api/v1/projects", root, "", nil)
	if strings.Contains(string(body), project.ID) {
		t.Fatalf("админ не должен видеть чужой проект: %s", body)
	}
	for what, url := range map[string]string{
		"epics проекта": srv.URL + "/api/v1/projects/" + project.ID + "/epics",
		"карточка epic": srv.URL + "/api/v1/epics/" + epic.ID,
		"участники":     srv.URL + "/api/v1/projects/" + project.ID + "/members",
		"SSE":           srv.URL + "/api/v1/stream?project=" + project.ID,
	} {
		resp, _ = call(t, "GET", url, root, "", nil)
		mustStatus(t, resp, http.StatusNotFound, "чужой "+what)
	}
	_, body = call(t, "GET", srv.URL+"/api/v1/events", root, "", nil)
	if strings.Contains(string(body), project.ID) {
		t.Fatalf("события чужого проекта видны: %s", body)
	}

	// Участие открывает доступ; удаление последнего участника запрещено.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "",
		map[string]string{"login": "root"})
	mustStatus(t, resp, http.StatusCreated, "добавление участника")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/epics/"+epic.ID, root, "", nil)
	mustStatus(t, resp, http.StatusOK, "epic после добавления")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/projects/"+project.ID+"/members/root", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "удаление участника")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/projects/"+project.ID+"/members/alice", alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "удаление последнего участника")
}

// Слой 2: администрирование — только админ; деактивация гасит сессии,
// реактивация не воскрешает credentials.
func TestAdminAndDeactivation(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	var bob domain.User
	var err error
	if bob, err = st.CreateUser(ctx, "bob", "", "pw-bob-secret", false); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	bobSession := loginSession(t, srv, "bob", "pw-bob-secret")

	// Не-админ: 403 на users и drain.
	resp, _ := call(t, "GET", srv.URL+"/api/v1/users", bobSession, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "users не-админом")
	resp, _ = call(t, "POST", srv.URL+"/api/v1/runners/r1/drain", bobSession, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "drain не-админом")

	// PAT боба переживает до деактивации, но не после.
	respTok, bodyTok := call(t, "POST", srv.URL+"/api/v1/tokens", bobSession, "", map[string]string{"name": "cli"})
	mustStatus(t, respTok, http.StatusCreated, "PAT боба")
	var created struct{ Secret string }
	_ = json.Unmarshal(bodyTok, &created)

	disabled := true
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+bob.ID, root, "", map[string]any{"disabled": disabled})
	mustStatus(t, resp, http.StatusOK, "деактивация")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", bobSession, "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "сессия деактивированного")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "PAT деактивированного")
	resp, _ = call(t, "POST", srv.URL+"/api/v1/auth/login", "", "", map[string]string{"login": "bob", "password": "pw-bob-secret"})
	mustStatus(t, resp, http.StatusUnauthorized, "вход деактивированного")

	// Реактивация возвращает вход, но не старые credentials.
	disabled = false
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+bob.ID, root, "", map[string]any{"disabled": disabled})
	mustStatus(t, resp, http.StatusOK, "реактивация")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", bobSession, "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "старая сессия после реактивации")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "старый PAT после реактивации")
	_ = loginSession(t, srv, "bob", "pw-bob-secret")

	// Последний активный админ не деактивируется.
	rootID := ""
	_, body := call(t, "GET", srv.URL+"/api/v1/users", root, "", nil)
	var users []domain.User
	_ = json.Unmarshal(body, &users)
	for _, u := range users {
		if u.Login == "root" {
			rootID = u.ID
		}
	}
	disabled = true
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+rootID, root, "", map[string]any{"disabled": disabled})
	mustStatus(t, resp, http.StatusConflict, "деактивация последнего админа")
}

// CSRF: мутация с cookie и чужим Origin/Sec-Fetch-Site отклоняется,
// родной Origin и запросы без браузерных заголовков проходят.
func TestCSRFOriginCheck(t *testing.T) {
	st, srv := testServer(t)
	if err := st.Bootstrap(context.Background(), "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, "root", "root-secret")

	mutate := func(hdr map[string]string) int {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/api/v1/projects", strings.NewReader(`{"name":"p","repo":"o/r"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := mutate(map[string]string{"Origin": "http://evil.example"}); got != http.StatusForbidden {
		t.Fatalf("чужой Origin: HTTP %d, ожидался 403", got)
	}
	if got := mutate(map[string]string{"Sec-Fetch-Site": "cross-site"}); got != http.StatusForbidden {
		t.Fatalf("cross-site: HTTP %d, ожидался 403", got)
	}
	if got := mutate(map[string]string{"Origin": srv.URL}); got != http.StatusCreated {
		t.Fatalf("родной Origin: HTTP %d, ожидался 201", got)
	}
	if got := mutate(nil); got != http.StatusCreated {
		t.Fatalf("без Origin (curl): HTTP %d, ожидался 201", got)
	}
}

// Формат login: URL-safe, иначе 422.
func TestCreateUserLoginValidation(t *testing.T) {
	st, srv := testServer(t)
	if err := st.Bootstrap(context.Background(), "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	for _, bad := range []string{"", "с пробелом", "a/b", strings.Repeat("x", 65)} {
		resp, _ := call(t, "POST", srv.URL+"/api/v1/users", root, "", map[string]string{"login": bad, "password": "pw-testpass"})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "login "+bad)
	}
}

// Logout — операция cookie-сессии: Bearer-запросу гасить нечего.
func TestLogoutRequiresCookie(t *testing.T) {
	st, srv := testServer(t)
	if err := st.Bootstrap(context.Background(), "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, "root", "root-secret")
	_, body := call(t, "POST", srv.URL+"/api/v1/tokens", session, "", map[string]string{"name": "cli"})
	var created struct{ Secret string }
	_ = json.Unmarshal(body, &created)
	resp, _ := call(t, "POST", srv.URL+"/api/v1/auth/logout", "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "logout по Bearer")
}

// SSE — только живая cookie-сессия: битая cookie с валидным PAT не открывает
// поток мимо сессии.
func TestSSECookieOnly(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, "root", "root-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", session, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project domain.Project
	_ = json.Unmarshal(body, &project)
	_, bodyTok := call(t, "POST", srv.URL+"/api/v1/tokens", session, "", map[string]string{"name": "cli"})
	var created struct{ Secret string }
	_ = json.Unmarshal(bodyTok, &created)

	resp, _ = call(t, "GET", srv.URL+"/api/v1/stream?project="+project.ID, "bogus", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "SSE с битой cookie и валидным PAT")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/stream?project="+project.ID, "", created.Secret, nil)
	mustStatus(t, resp, http.StatusUnauthorized, "SSE только по Bearer")
}

// Redaction: TaskID runner'а виден только участнику проекта задачи.
func TestRunnerTaskRedaction(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	root := loginSession(t, srv, "root", "root-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project domain.Project
	_ = json.Unmarshal(body, &project)
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E", "goal": ""})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)
	resp, body = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/tasks", alice, "",
		map[string]any{"title": "T", "description": "", "criteria": []string{}, "deps": []string{}})
	mustStatus(t, resp, http.StatusCreated, "задача")
	var task domain.Task
	_ = json.Unmarshal(body, &task)

	if err := st.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "fake", Model: "m", Host: "h", Capabilities: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE runners SET task_id=$1, status='running' WHERE id='r1'`, task.ID); err != nil {
		t.Fatal(err)
	}

	_, body = call(t, "GET", srv.URL+"/api/v1/runners", alice, "", nil)
	if !strings.Contains(string(body), task.ID) {
		t.Fatalf("участник должен видеть TaskID: %s", body)
	}
	_, body = call(t, "GET", srv.URL+"/api/v1/runners", root, "", nil)
	if strings.Contains(string(body), task.ID) {
		t.Fatalf("TaskID чужой задачи должен скрываться: %s", body)
	}
}

// Сценарии «Выдача и снятие прав администратора» и «Последний администратор
// защищён» (спека access-policy).
func TestAdminRightsGrantAndRevoke(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	alice, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	aliceSess := loginSession(t, srv, "alice", "pw-alice-secret")

	// Пока alice не админ, администрирование ей закрыто.
	resp, _ := call(t, "GET", srv.URL+"/api/v1/users", aliceSess, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "список пользователей не админу")

	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+alice.ID, root, "", map[string]any{"admin": true})
	mustStatus(t, resp, http.StatusOK, "выдача прав администратора")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/users", aliceSess, "", nil)
	mustStatus(t, resp, http.StatusOK, "список пользователей новому админу")

	// Событие о выдаче прав попало в event log.
	events, err := st.Events(ctx, store.EventFilter{Type: "user.admin_changed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ActorID != "root" {
		t.Fatalf("ожидалось событие о выдаче прав от root, получено %+v", events)
	}

	// Снятие прав возвращает отказ на администрирование.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+alice.ID, root, "", map[string]any{"admin": false})
	mustStatus(t, resp, http.StatusOK, "снятие прав администратора")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/users", aliceSess, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "список пользователей после снятия прав")

	// Последний активный админ не может снять права сам с себя.
	rootUser, err := st.Authenticate(ctx, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+rootUser.ID, root, "", map[string]any{"admin": false})
	mustStatus(t, resp, http.StatusConflict, "снятие прав у последнего админа")
}

// Сценарии «Смена своего пароля», «Сброс пароля администратором» и вход
// одноразовым паролем (спека access-policy).
func TestPasswordChangeAndReset(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	alice, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	first := loginSession(t, srv, "alice", "pw-alice-secret")
	second := loginSession(t, srv, "alice", "pw-alice-secret")

	// Короткий пароль и неверный текущий отклоняются.
	resp, _ := call(t, "POST", srv.URL+"/api/v1/auth/password", first, "",
		map[string]string{"current": "pw-alice-secret", "new": "short"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "короткий новый пароль")
	resp, _ = call(t, "POST", srv.URL+"/api/v1/auth/password", first, "",
		map[string]string{"current": "wrong-password", "new": "brand-new-password"})
	mustStatus(t, resp, http.StatusUnauthorized, "неверный текущий пароль")

	// Успешная смена: текущая сессия жива, вторая завершена.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/auth/password", first, "",
		map[string]string{"current": "pw-alice-secret", "new": "brand-new-password"})
	mustStatus(t, resp, http.StatusNoContent, "смена пароля")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", first, "", nil)
	mustStatus(t, resp, http.StatusOK, "текущая сессия после смены пароля")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", second, "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "остальные сессии после смены пароля")
	_ = loginSession(t, srv, "alice", "brand-new-password")

	// Сброс администратором: одноразовый пароль, сессии погашены.
	resp, body := call(t, "POST", srv.URL+"/api/v1/users/"+alice.ID+"/password/reset", root, "", nil)
	mustStatus(t, resp, http.StatusOK, "сброс пароля")
	var reset struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &reset); err != nil || reset.Password == "" {
		t.Fatalf("ожидался одноразовый пароль: %s", body)
	}
	resp, _ = call(t, "GET", srv.URL+"/api/v1/auth/me", first, "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "сессия после сброса пароля")

	// Вход одноразовым паролем работает, остальное API закрыто до смены.
	temp := loginSession(t, srv, "alice", reset.Password)
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects", temp, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "API до смены пароля")
	if !strings.Contains(string(body), "password_change_required") {
		t.Fatalf("ожидался код password_change_required: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/auth/me", temp, "", nil)
	mustStatus(t, resp, http.StatusOK, "профиль до смены пароля")
	if !strings.Contains(string(body), `"must_change_password":true`) {
		t.Fatalf("профиль должен требовать смену пароля: %s", body)
	}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/auth/password", temp, "",
		map[string]string{"current": reset.Password, "new": "after-reset-password"})
	mustStatus(t, resp, http.StatusNoContent, "смена одноразового пароля")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects", temp, "", nil)
	mustStatus(t, resp, http.StatusOK, "API после смены пароля")
}

// Сценарии «Участник без роли owner не меняет настройки» и «Проект не
// остаётся без owner» (спека domain-model).
func TestProjectMemberRoles(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "bob", "", "pw-bob-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "",
		map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project domain.Project
	_ = json.Unmarshal(body, &project)

	// Создатель проекта — владелец.
	_, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "", nil)
	if !strings.Contains(string(body), `"role":"owner"`) {
		t.Fatalf("создатель должен быть owner: %s", body)
	}

	// bob добавлен участником: видит проект, но не меняет его настройки.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "",
		map[string]string{"login": "bob"})
	mustStatus(t, resp, http.StatusCreated, "добавление участника")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/members", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "участник видит состав")
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/projects/"+project.ID, bob, "", map[string]any{"name": "hijacked"})
	mustStatus(t, resp, http.StatusForbidden, "member меняет настройки проекта")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/projects/"+project.ID+"/members/alice", bob, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "member исключает участника")

	// Epic'и участнику доступны: роль не влияет на работу с задачами.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", bob, "",
		map[string]string{"title": "E", "goal": ""})
	mustStatus(t, resp, http.StatusCreated, "member создаёт epic")

	// Последний владелец не понижается и не исключается.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/projects/"+project.ID+"/members/alice", alice, "",
		map[string]string{"role": "member"})
	mustStatus(t, resp, http.StatusConflict, "понижение последнего owner")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/projects/"+project.ID+"/members/alice", alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "исключение последнего owner")

	// После повышения bob'а владельцем alice может уйти.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/projects/"+project.ID+"/members/bob", alice, "",
		map[string]string{"role": "owner"})
	mustStatus(t, resp, http.StatusOK, "повышение до owner")
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/projects/"+project.ID, bob, "", map[string]any{"name": "renamed"})
	mustStatus(t, resp, http.StatusOK, "новый owner меняет настройки")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/projects/"+project.ID+"/members/alice", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "исключение бывшего owner")
}

// Находки ревью: деактивация не оставляет проект без активного владельца,
// свой пароль сбросом не меняется.
func TestDeactivationKeepsProjectOwner(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	alice, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "bob", "", "pw-bob-secret", false); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	aliceSess := loginSession(t, srv, "alice", "pw-alice-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", aliceSess, "",
		map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project domain.Project
	_ = json.Unmarshal(body, &project)

	// alice — единственный активный владелец: администратор её не отключит.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+alice.ID, root, "", map[string]any{"disabled": true})
	mustStatus(t, resp, http.StatusConflict, "деактивация последнего владельца проекта")

	// Со вторым владельцем деактивация проходит.
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", aliceSess, "",
		map[string]string{"login": "bob", "role": "owner"})
	mustStatus(t, resp, http.StatusCreated, "второй владелец")
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+alice.ID, root, "", map[string]any{"disabled": true})
	mustStatus(t, resp, http.StatusOK, "деактивация при втором владельце")

	// Отключённый владелец не считается: bob теперь единственный активный.
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bobID := ""
	for _, u := range users {
		if u.Login == "bob" {
			bobID = u.ID
		}
	}
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/users/"+bobID, root, "", map[string]any{"disabled": true})
	mustStatus(t, resp, http.StatusConflict, "деактивация оставшегося владельца")
}

func TestSelfPasswordResetRejected(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	rootUser, err := st.Authenticate(ctx, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := call(t, "POST", srv.URL+"/api/v1/users/"+rootUser.ID+"/password/reset", root, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "сброс собственного пароля")
	// Сессия администратора цела.
	resp, _ = call(t, "GET", srv.URL+"/api/v1/users", root, "", nil)
	mustStatus(t, resp, http.StatusOK, "сессия после отказа")
}
