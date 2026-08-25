package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Политики в API (change add-policy-presets, api-contract): пресеты
// установки — администратор, переопределения проекта — owner, участник
// читает действующие значения; PATCH /tasks/{id} — лимит попыток.
func TestPolicyAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, u, "", "pw-"+u+"-secret", false); err != nil {
			t.Fatal(err)
		}
	}
	root := loginSession(t, srv, "root", "root-secret")
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "", map[string]string{"login": "bob"})
	mustStatus(t, resp, http.StatusCreated, "участник")

	// Пресеты установки: только администратор; без версий — значения по умолчанию.
	resp, _ = call(t, "GET", srv.URL+"/api/v1/system/policy", alice, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "политика установки не админу")
	resp, body = call(t, "GET", srv.URL+"/api/v1/system/policy", root, "", nil)
	mustStatus(t, resp, http.StatusOK, "политика установки")
	var sys struct {
		Version *store.PolicyVersion `json:"version"`
		Presets struct {
			AttemptLimit int  `json:"attempt_limit"`
			AutoMerge    bool `json:"auto_merge"`
		} `json:"presets"`
	}
	_ = json.Unmarshal(body, &sys)
	if sys.Version != nil || sys.Presets.AttemptLimit != 3 || sys.Presets.AutoMerge {
		t.Fatalf("значения по умолчанию: %s", body)
	}
	// Невалидные пресеты — 422.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/system/policy", root, "", map[string]any{
		"auto_merge": false, "human_review_paths": []string{}, "attempt_limit": 0, "review_limit": 3,
		"daily_token_budget": nil, "auto_publish": true})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "лимит 0")
	resp, body = call(t, "PUT", srv.URL+"/api/v1/system/policy", root, "", map[string]any{
		"auto_merge": false, "human_review_paths": []string{"infra/**"}, "attempt_limit": 5, "review_limit": 3,
		"daily_token_budget": nil, "auto_publish": true})
	mustStatus(t, resp, http.StatusOK, "сохранение пресетов установки")
	_ = json.Unmarshal(body, &sys)
	if sys.Version == nil || sys.Version.Version != 1 || sys.Version.CreatedBy != "root" || sys.Presets.AttemptLimit != 5 {
		t.Fatalf("версия установки: %s", body)
	}
	// Событие активации — в ленте аудита установки.
	_, body = call(t, "GET", srv.URL+"/api/v1/events?scope=installation&type=policy.activated", root, "", nil)
	if !jsonContains(body, sys.Version.Hash) {
		t.Fatalf("нет события policy.activated установки: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/system/policy/versions", root, "", nil)
	mustStatus(t, resp, http.StatusOK, "история установки")
	if !jsonContains(body, `"version":1`) {
		t.Fatalf("история: %s", body)
	}

	// Проект наследует лимит 5; участник видит, но не меняет (403);
	// не участник — 404.
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/policy", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "политика проекта участнику")
	var pp struct {
		Effective struct {
			AttemptLimit int `json:"attempt_limit"`
		} `json:"effective"`
		EffectiveHash string `json:"effective_hash"`
		Overrides     struct {
			AttemptLimit *int `json:"attempt_limit"`
		} `json:"overrides"`
		Version             *store.PolicyVersion `json:"version"`
		InstallationVersion *store.PolicyVersion `json:"installation_version"`
	}
	_ = json.Unmarshal(body, &pp)
	if pp.Effective.AttemptLimit != 5 || pp.Overrides.AttemptLimit != nil || pp.Version != nil || pp.InstallationVersion == nil {
		t.Fatalf("наследование: %s", body)
	}
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", bob, "", map[string]any{"attempt_limit": 2})
	mustStatus(t, resp, http.StatusForbidden, "member меняет политику")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/policy", root, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "не участник")
	// PAT участника с ролью member тоже 403: у автоматики нет пути записи.
	bobUser, _ := st.Authenticate(ctx, "bob", "pw-bob-secret")
	_, pat, err := st.CreateAccessToken(ctx, bobUser.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", "", pat, map[string]any{"attempt_limit": 2})
	mustStatus(t, resp, http.StatusForbidden, "PAT member меняет политику")
	hist, _ := st.ListPolicyVersions(ctx, store.PolicyScopeProject, project.ID)
	if len(hist) != 0 {
		t.Fatal("версия проекта не должна создаваться при отказе")
	}

	// Владелец переопределяет: действующее значение 2, версия проекта с хэшем.
	resp, body = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"attempt_limit": 2})
	mustStatus(t, resp, http.StatusOK, "owner меняет политику")
	_ = json.Unmarshal(body, &pp)
	if pp.Effective.AttemptLimit != 2 || pp.Overrides.AttemptLimit == nil || pp.Version == nil || pp.Version.Version != 1 {
		t.Fatalf("переопределение: %s", body)
	}
	hashAfterOverride := pp.EffectiveHash
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"human_review_paths": []string{"[bad"}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "битый шаблон")
	// Возврат к наследованию — новая версия, действующее значение снова 5.
	resp, body = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"attempt_limit": nil})
	mustStatus(t, resp, http.StatusOK, "возврат к наследованию")
	_ = json.Unmarshal(body, &pp)
	if pp.Effective.AttemptLimit != 5 || pp.Version.Version != 2 || pp.EffectiveHash == hashAfterOverride {
		t.Fatalf("наследование после отката: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/policy/versions", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "история проекта участнику")
	var versions []store.PolicyVersion
	_ = json.Unmarshal(body, &versions)
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("история проекта: %s", body)
	}
	// Событие проекта видно участнику в обычной ленте.
	_, body = call(t, "GET", srv.URL+"/api/v1/events?project="+project.ID+"&type=policy.activated", bob, "", nil)
	if !jsonContains(body, versions[0].Hash) {
		t.Fatalf("нет события policy.activated проекта: %s", body)
	}

	// Бюджет в DTO проекта.
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID, bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "карточка проекта")
	if !jsonContains(body, `"budget":{"daily_tokens":null,"used_today":0,"paused_until":null}`) {
		t.Fatalf("бюджет в DTO: %s", body)
	}

	// Задача получает лимит из политики (5), PATCH меняет его.
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)
	resp, body = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/tasks", bob, "", map[string]any{"title": "T"})
	mustStatus(t, resp, http.StatusCreated, "задача")
	var task domain.Task
	_ = json.Unmarshal(body, &task)
	if task.AttemptLimit != 5 {
		t.Fatalf("лимит из политики: %+v", task)
	}
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+task.ID, bob, "", map[string]any{"attempt_limit": 0})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "лимит 0 на задаче")
	resp, body = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+task.ID, bob, "", map[string]any{"attempt_limit": 1})
	mustStatus(t, resp, http.StatusOK, "лимит 1 на задаче")
	if !jsonContains(body, `"AttemptLimit":1`) || !jsonContains(body, `"ReviewRejections":0`) {
		t.Fatalf("PATCH задачи: %s", body)
	}
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+task.ID, root, "", map[string]any{"attempt_limit": 2})
	mustStatus(t, resp, http.StatusNotFound, "не участник меняет задачу")
}

// Движок политик в API (change add-policy-engine): режим виден в политике
// установки и в состоянии установки; в external-режиме локальная правка
// пресетов отклоняется (спека access-policy «Внешний контур политик»).
func TestPolicyEngineMode(t *testing.T) {
	ctx := context.Background()
	st, srv := testServer(t)
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")

	// Встроенный движок: правка политики работает, режим виден.
	_, body := call(t, "GET", srv.URL+"/api/v1/system/policy", root, "", nil)
	var view struct {
		Engine struct{ Mode, State, Detail string } `json:"engine"`
	}
	_ = json.Unmarshal(body, &view)
	if view.Engine.Mode != policy.ModeEmbedded || view.Engine.State != "ok" {
		t.Fatalf("режим встроенного движка: %s", body)
	}
	_, body = call(t, "GET", srv.URL+"/api/v1/system/status", root, "", nil)
	if !strings.Contains(string(body), `"policy"`) {
		t.Fatalf("компонент движка в состоянии установки: %s", body)
	}

	// Внешний движок: правка пресетов отклоняется, состояние деградирует,
	// когда он не отвечает.
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer opa.Close()
	ext, err := policy.NewEngine(policy.Config{Mode: policy.ModeExternal, URL: opa.URL})
	if err != nil {
		t.Fatal(err)
	}
	extSrv := httptest.NewServer((&Server{St: st, Policy: ext}).Handler())
	defer extSrv.Close()
	extRoot := loginSession(t, extSrv, "root", "root-secret")
	preset := map[string]any{"auto_merge": true, "human_review_paths": []string{}, "attempt_limit": 3,
		"review_limit": 3, "daily_token_budget": nil, "auto_publish": true}
	resp, _ := call(t, "PUT", extSrv.URL+"/api/v1/system/policy", extRoot, "", preset)
	mustStatus(t, resp, http.StatusConflict, "правка политики во внешнем режиме")
	_, body = call(t, "GET", extSrv.URL+"/api/v1/system/policy", extRoot, "", nil)
	_ = json.Unmarshal(body, &view)
	if view.Engine.Mode != policy.ModeExternal || view.Engine.State != "ok" {
		t.Fatalf("режим внешнего движка: %s", body)
	}
	opa.Close()
	_, body = call(t, "GET", extSrv.URL+"/api/v1/system/status", extRoot, "", nil)
	if !strings.Contains(string(body), "не отвечает") {
		t.Fatalf("недоступный движок — деградация с причиной: %s", body)
	}
}

// Мутация, запрещённая движком, отклоняется, даже если право у человека
// есть; ошибка движка отвечает 503 (спека access-policy «Точки принуждения»).
func TestPolicyMutationGate(t *testing.T) {
	ctx := context.Background()
	st, _ := testServer(t)
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	denySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"allow":false,"reason":"prod_freeze"}}`))
	}))
	defer denySrv.Close()
	deny, err := policy.NewEngine(policy.Config{Mode: policy.ModeExternal, URL: denySrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{St: st, Policy: deny}).Handler())
	defer srv.Close()
	root := loginSession(t, srv, "root", "root-secret")
	_, body := call(t, "POST", srv.URL+"/api/v1/projects", root, "", map[string]string{"name": "p", "repo": "o/r"})
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	_, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", root, "",
		map[string]string{"title": "E", "goal": "g"})
	var epic struct{ ID string }
	_ = json.Unmarshal(body, &epic)
	resp, body := call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/start", root, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "мутация, запрещённая движком")
	if !strings.Contains(string(body), "prod_freeze") {
		t.Fatalf("причина отказа: %s", body)
	}

	// Движок не отвечает — мутация отклоняется как недоступность установки.
	denySrv.Close()
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/start", root, "", nil)
	mustStatus(t, resp, http.StatusServiceUnavailable, "движок не дал решения")
}

// Источник политики проекта (change add-policy-git-provider): включение
// git-провайдера требует защищённой ветки, а правка через API при нём
// отклоняется.
func TestProjectPolicySource(t *testing.T) {
	ctx := context.Background()
	st, srv := testServer(t)
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	_, body := call(t, "POST", srv.URL+"/api/v1/projects", root, "", map[string]string{"name": "p", "repo": "o/r"})
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)

	// По умолчанию — хранилище Rivet.
	_, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/policy", root, "", nil)
	var view struct {
		Source struct{ Kind, File, Ref string } `json:"source"`
	}
	_ = json.Unmarshal(body, &view)
	if view.Source.Kind != "store" {
		t.Fatalf("источник по умолчанию: %s", body)
	}

	// Включение git-провайдера: fake-хостинг считает ветку защищённой.
	resp, body := call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy/source",
		root, "", map[string]string{"kind": "git"})
	mustStatus(t, resp, http.StatusOK, "включение git-провайдера")
	_ = json.Unmarshal(body, &view)
	if view.Source.Kind != "git" || view.Source.File == "" {
		t.Fatalf("источник после включения: %s", body)
	}

	// Правка политики через API при git-источнике отклоняется.
	resp, body = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", root, "",
		map[string]any{"auto_merge": true})
	mustStatus(t, resp, http.StatusConflict, "правка политики при git-источнике")
	if !strings.Contains(string(body), "policy_from_git") {
		t.Fatalf("код ошибки: %s", body)
	}

	// Возврат к хранилищу Rivet снова разрешает правку.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy/source",
		root, "", map[string]string{"kind": "store"})
	mustStatus(t, resp, http.StatusOK, "возврат источника")
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", root, "",
		map[string]any{"auto_merge": true})
	mustStatus(t, resp, http.StatusOK, "правка после возврата")
}
