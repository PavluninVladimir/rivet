package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Каталог агентов (add-agent-profiles): профиль с привязками из подключения,
// валидация шаблона, runner'ы получают модели профиля, агенты вне каталога,
// участники процесса и переопределения проекта проверяются по каталогу,
// окружение назначения собирается по шаблону с секретами по режиму.
func TestAgentsAPI(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	old := listModels
	listModels = func(context.Context, llm.Client) ([]llm.Model, error) {
		return []llm.Model{{ID: "gpt-fast"}, {ID: "gpt-slow"}}, nil
	}
	t.Cleanup(func() { listModels = old })

	// Подключение с обнаруженными моделями.
	resp, _ := call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", map[string]any{
		"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "https://router.local/v1", "key": "router-key-1234567890",
		"headers": []map[string]any{{"name": "X-Org", "value": "acme", "secret": false}, {"name": "X-Token", "value": "hdr-secret-123456", "secret": true}},
	})
	mustStatus(t, resp, http.StatusCreated, "подключение")
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/connections/router/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "обнаружение")

	// Предустановленные профили есть, участник читает каталог без шаблона.
	resp, body := call(t, "GET", f.srv.URL+"/api/v1/agents", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "каталог участнику")
	if !jsonContains(body, `"id":"claude-code"`) || jsonContains(body, `ANTHROPIC_API_KEY`) {
		t.Fatalf("каталог участнику: %s", body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/x", f.user, "", map[string]any{"name": "x", "adapter": "wrap"})
	mustStatus(t, resp, http.StatusForbidden, "профиль не-админом")

	// Валидация: неизвестная подстановка, модель вне подключения.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", map[string]any{
		"name": "Codex", "adapter": "wrap", "command": "codex exec", "env": []map[string]any{{"name": "OPENAI_API_KEY", "value": "{{token}}"}},
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неизвестная подстановка")
	if !jsonContains(body, `"field":"env[0].value"`) {
		t.Fatalf("поле подстановки: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", map[string]any{
		"name": "Codex", "adapter": "wrap", "command": "codex exec",
		"models": []map[string]any{{"connection_id": "router", "model": "gpt-nope"}},
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "модель вне подключения")
	if !jsonContains(body, `"field":"models[0]"`) {
		t.Fatalf("поле привязки: %s", body)
	}

	// Runner с агентом codex зарегистрирован до привязок: объявленные модели.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "cx-1", Agent: "codex", Models: []string{"declared-1"}, Capabilities: []string{"coding"}, Host: "h", Secure: true}); err != nil {
		t.Fatal(err)
	}
	// Профиль с двумя привязками и шаблоном: runner получает модели профиля.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", map[string]any{
		"name": "Codex", "adapter": "wrap", "command": "codex exec --skip-git-repo-check",
		"capabilities":  []string{"review"},
		"models":        []map[string]any{{"connection_id": "router", "model": "gpt-fast"}, {"connection_id": "router", "model": "gpt-slow"}},
		"default_model": map[string]any{"connection_id": "router", "model": "gpt-slow"},
		"env":           []map[string]any{{"name": "OPENAI_API_KEY", "value": "{{key}}"}, {"name": "OPENAI_BASE_URL", "value": "{{base_url}}"}, {"name": "X_ORG", "value": "{{header:X-Org}}"}, {"name": "X_TOKEN", "value": "{{header:X-Token}}"}},
		"args":          []string{"-m", "{{model}}"},
		"secrets":       "secure",
	})
	mustStatus(t, resp, http.StatusOK, "профиль codex")
	var a agentView
	_ = json.Unmarshal(body, &a)
	if a.DefaultModel == nil || a.DefaultModel.Model != "gpt-slow" || len(a.Models) != 2 || a.Runners != 1 || !a.Preset {
		t.Fatalf("профиль: %s", body)
	}
	rn, err := f.st.GetRunner(ctx, "cx-1")
	if err != nil || !rn.Catalog || len(rn.Models) != 2 || rn.Models[0] != "gpt-fast" || !containsString(rn.Capabilities, "review") || !containsString(rn.Capabilities, "coding") {
		t.Fatalf("runner после профиля: %+v %v", rn, err)
	}

	// Окружение назначения: секреты по режиму secure и каналу.
	prof, binding, ok, err := f.st.ResolveAgentModel(ctx, "codex", "", nil)
	if err != nil || !ok || binding.Model != "gpt-slow" {
		t.Fatalf("модель по умолчанию: %+v %v %v", binding, ok, err)
	}
	l, err := f.st.BuildAgentLaunch(ctx, prof, binding, f.api.Secrets, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Env["OPENAI_API_KEY"] != "router-key-1234567890" || l.Env["OPENAI_BASE_URL"] != "https://router.local/v1" || l.Env["X_ORG"] != "acme" || l.Env["X_TOKEN"] != "hdr-secret-123456" ||
		len(l.Args) != 2 || l.Args[1] != "gpt-slow" || len(l.SecretNames) != 2 || l.Command != "codex exec --skip-git-repo-check" {
		t.Fatalf("окружение с секретами: %+v", l)
	}
	l, err = f.st.BuildAgentLaunch(ctx, prof, binding, f.api.Secrets, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := l.Env["OPENAI_API_KEY"]; has || l.Env["OPENAI_BASE_URL"] == "" || l.Env["X_ORG"] != "acme" || len(l.SecretNames) != 0 {
		t.Fatalf("окружение без секретов: %+v", l)
	}
	// Переопределение проекта выбирает другую привязку, явная модель — свою.
	_, binding, _, _ = f.st.ResolveAgentModel(ctx, "codex", "", &policy.AgentModel{ConnectionID: "router", Model: "gpt-fast"})
	if binding.Model != "gpt-fast" {
		t.Fatalf("переопределение проекта: %+v", binding)
	}
	_, binding, _, _ = f.st.ResolveAgentModel(ctx, "codex", "gpt-fast", &policy.AgentModel{ConnectionID: "router", Model: "gpt-slow"})
	if binding.Model != "gpt-fast" || binding.ConnectionID != "router" {
		t.Fatalf("явная модель: %+v", binding)
	}

	// Список runner'ов показывает каталог и защищённый канал; внешний агент.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "legacy-1", Agent: "legacy-bot", Models: []string{"lm-1"}, Capabilities: []string{"coding"}, Host: "h"}); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/runners", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "runner'ы")
	if !jsonContains(body, `"Catalog":true`) || !jsonContains(body, `"Secure":true`) || !jsonContains(body, `"ProfileName":"Codex"`) {
		t.Fatalf("runner'ы с профилем: %s", body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/system/agents", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "каталог админу")
	if !jsonContains(body, `"external":[{"id":"legacy-bot","models":["lm-1"],"runners":1}]`) || !jsonContains(body, `OPENAI_API_KEY`) {
		t.Fatalf("каталог с внешними: %s", body)
	}

	// Процесс проекта: модель вне привязок — ошибка у поля; агент вне
	// каталога с объявленной моделью проходит; неизвестный агент — ошибка.
	proj, err := f.st.CreateProject(ctx, "p", "org/repo", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	process := func(kind, model string) map[string]any {
		return map[string]any{"process": map[string]any{"steps": []map[string]any{
			{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}},
			{"id": "review", "kind": "review", "participants": []map[string]any{{"agent": map[string]any{"kind": kind, "model": model}}}},
			{"id": "merge", "kind": "merge"},
		}}}
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("codex", "gpt-nope"))
	mustStatus(t, resp, http.StatusUnprocessableEntity, "модель вне привязок")
	if !jsonContains(body, `"step":"review"`) || !jsonContains(body, `"field":"participants[0].agent.model"`) {
		t.Fatalf("ошибка участника: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("ghost", ""))
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неизвестный агент")
	if !jsonContains(body, `"field":"participants[0].agent.kind"`) {
		t.Fatalf("ошибка агента: %s", body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("legacy-bot", "lm-1"))
	mustStatus(t, resp, http.StatusOK, "агент вне каталога")
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("codex", "gpt-fast"))
	mustStatus(t, resp, http.StatusOK, "агент из каталога")

	// Переопределение модели агента в проекте.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "",
		map[string]any{"agent_models": map[string]any{"codex": map[string]any{"connection_id": "router", "model": "gpt-nope"}}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "переопределение вне привязок")
	if !jsonContains(body, `"field":"agent_models.codex"`) {
		t.Fatalf("поле переопределения: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "",
		map[string]any{"agent_models": map[string]any{"codex": map[string]any{"connection_id": "router", "model": "gpt-fast"}}})
	mustStatus(t, resp, http.StatusOK, "переопределение")
	if !jsonContains(body, `"codex":{"name":"Codex"`) || !jsonContains(body, `"source":"project"`) || !jsonContains(body, `"effective":{"connection_id":"router","model":"gpt-fast"}`) {
		t.Fatalf("действующие модели агентов: %s", body)
	}

	// Удаление: предустановленный нельзя, профиль с runner'ом — 409, свободный — можно.
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "удаление предустановленного")
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/my-agent", f.admin, "", map[string]any{"name": "Mine", "adapter": "wrap", "command": "my-agent"})
	mustStatus(t, resp, http.StatusCreated, "свой профиль")
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "mine-1", Agent: "my-agent", Capabilities: []string{"coding"}, Host: "h"}); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/agents/my-agent", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "удаление с runner'ом")
	if !jsonContains(body, `"kind":"runner","id":"mine-1"`) {
		t.Fatalf("ссылки: %s", body)
	}
	// Отключение профиля возвращает runner'ам объявленные модели.
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", map[string]any{
		"name": "Codex", "adapter": "wrap", "command": "codex exec", "enabled": false,
		"models": []map[string]any{{"connection_id": "router", "model": "gpt-fast"}},
	})
	mustStatus(t, resp, http.StatusOK, "отключение профиля")
	rn, _ = f.st.GetRunner(ctx, "cx-1")
	if rn.Catalog || len(rn.Models) != 1 || rn.Models[0] != "declared-1" {
		t.Fatalf("runner после отключения профиля: %+v", rn)
	}
	// Подключение с привязками не удаляется.
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "подключение с привязками")
	if !jsonContains(body, `"kind":"agent","id":"codex"`) {
		t.Fatalf("ссылки подключения: %s", body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation&type=agent.updated", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "аудит")
	if !jsonContains(body, "agent.updated") || jsonContains(body, "router-key-1234567890") {
		t.Fatalf("аудит агентов: %s", body)
	}
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
