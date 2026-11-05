package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Каталог агентов (add-native-agent, спека agents «Каталог агентов»,
// «Привязка моделей к агенту», «Участник процесса из каталога» и
// «Переопределение модели агента в проекте»): один встроенный профиль rivet,
// его модели — каталог подключений; правятся название, модель по
// умолчанию, режим секретов, лимиты и признак «включено»; других агентов
// нет, rivet не удаляется; endpoint назначения собирается из подключения с
// секретами по режиму.
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

	// Участник видит один профиль агента Rivet с моделями каталога, без
	// шаблона окружения.
	resp, body := call(t, "GET", f.srv.URL+"/api/v1/agents", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "каталог участнику")
	var catalog struct {
		Agents   []agentView `json:"agents"`
		External []any       `json:"external"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Agents) != 1 || len(catalog.External) != 0 || jsonContains(body, `"env"`) {
		t.Fatalf("каталог участнику: %s", body)
	}
	if a := catalog.Agents[0]; a.ID != "rivet" || a.Adapter != "native" || !a.Preset || !a.Enabled || a.Command != "" ||
		len(a.Models) != 2 || a.Models[0].ConnectionID != "router" || a.Models[0].Model != "gpt-fast" || a.Models[1].Model != "gpt-slow" ||
		a.DefaultModel == nil || a.DefaultModel.Model != "gpt-fast" {
		t.Fatalf("профиль агента Rivet: %s", body)
	}

	// Профиль меняет только администратор; других агентов создать нельзя.
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.user, "", map[string]any{"name": "x"})
	mustStatus(t, resp, http.StatusForbidden, "профиль не-админом")
	for _, id := range []string{"my-agent", "codex", "claude-code"} {
		resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/"+id, f.admin, "", map[string]any{
			"name": "Mine", "adapter": "wrap", "command": "my-agent",
		})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "профиль "+id)
		if !jsonContains(body, `"error":{"code":"agent_unsupported","message":"агентом может быть только агент Rivet"}`) || !jsonContains(body, `"field":"id"`) {
			t.Fatalf("отказ профилю %s: %s", id, body)
		}
	}

	// Модель по умолчанию — только модель каталога; лимиты неотрицательны.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", map[string]any{
		"default_model": map[string]any{"connection_id": "router", "model": "gpt-nope"},
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "модель по умолчанию вне каталога")
	if !jsonContains(body, `"field":"default_model"`) {
		t.Fatalf("поле модели по умолчанию: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", map[string]any{"limits": map[string]any{"max_turns": -1}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "отрицательный лимит")
	if !jsonContains(body, `"field":"limits"`) {
		t.Fatalf("поле лимитов: %s", body)
	}

	// Runner агента Rivet: объявленные модели заменяются моделями каталога.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "rv-1", Agent: store.AgentRivet, Models: []string{"declared-1"}, Capabilities: []string{"coding"},
		Host: "h", Secure: true, Features: []string{domain.FeatureRoles, "native"}}); err != nil {
		t.Fatal(err)
	}
	// Поля CLI-агентов в теле игнорируются: команды и адаптеров кроме
	// native нет.
	full := map[string]any{
		"name": "Агент Rivet", "secrets": "secure",
		"default_model": map[string]any{"connection_id": "router", "model": "gpt-slow"},
		"limits":        map[string]any{"max_turns": 120, "max_tokens": 2000000, "max_minutes": 45},
		"adapter":       "wrap", "command": "codex exec", "env": []map[string]any{{"name": "OPENAI_API_KEY", "value": "{{key}}"}},
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", full)
	mustStatus(t, resp, http.StatusOK, "профиль rivet")
	var a agentView
	_ = json.Unmarshal(body, &a)
	if a.DefaultModel == nil || a.DefaultModel.Model != "gpt-slow" || len(a.Models) != 2 || a.Runners != 1 || !a.Preset ||
		a.Adapter != "native" || a.Command != "" || len(a.Env) != 0 || a.Secrets != "secure" ||
		a.Limits != (domain.AgentLimits{MaxTurns: 120, MaxTokens: 2000000, MaxMinutes: 45}) {
		t.Fatalf("профиль: %s", body)
	}
	rn, err := f.st.GetRunner(ctx, "rv-1")
	if err != nil || !rn.Catalog || len(rn.Models) != 2 || rn.Models[0] != "gpt-fast" || rn.Models[1] != "gpt-slow" || !containsString(rn.Capabilities, "coding") {
		t.Fatalf("runner агента Rivet: %+v %v", rn, err)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/system/agents", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "каталог админу")
	if !jsonContains(body, `"limits":{"max_turns":120,"max_tokens":2000000,"max_minutes":45}`) || !jsonContains(body, `"external":[]`) ||
		jsonContains(body, "router-key-1234567890") || jsonContains(body, "hdr-secret-123456") {
		t.Fatalf("каталог админу: %s", body)
	}

	// Endpoint назначения: ключ и секретный заголовок — только с секретами.
	prof, binding, ok, err := f.st.ResolveAgentModel(ctx, store.AgentRivet, "", nil)
	if err != nil || !ok || binding.ConnectionID != "router" || binding.Model != "gpt-slow" {
		t.Fatalf("модель по умолчанию: %+v %v %v", binding, ok, err)
	}
	ep, err := f.st.BuildModelEndpoint(ctx, prof, binding, f.api.Secrets, true)
	if err != nil {
		t.Fatal(err)
	}
	if ep.API != "openai" || ep.BaseURL != "https://router.local/v1" || ep.Model != "gpt-slow" || ep.ConnectionID != "router" ||
		ep.Key != "router-key-1234567890" || ep.Headers["X-Org"] != "acme" || ep.Headers["X-Token"] != "hdr-secret-123456" ||
		!ep.SecretsIncluded || strings.Join(ep.SecretNames, ",") != "header:X-Token,key" || ep.Limits.MaxTurns != 120 {
		t.Fatalf("endpoint с секретами: %+v", ep)
	}
	ep, err = f.st.BuildModelEndpoint(ctx, prof, binding, f.api.Secrets, false)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Key != "" || ep.BaseURL == "" || ep.Headers["X-Org"] != "acme" || ep.Headers["X-Token"] != "" || ep.SecretsIncluded || len(ep.SecretNames) != 0 {
		t.Fatalf("endpoint без секретов: %+v", ep)
	}
	// Переопределение проекта выбирает свою пару, явная модель — свою.
	_, binding, _, _ = f.st.ResolveAgentModel(ctx, store.AgentRivet, "", &policy.AgentModel{ConnectionID: "router", Model: "gpt-fast"})
	if binding.Model != "gpt-fast" {
		t.Fatalf("переопределение проекта: %+v", binding)
	}
	_, binding, _, _ = f.st.ResolveAgentModel(ctx, store.AgentRivet, "gpt-fast", &policy.AgentModel{ConnectionID: "router", Model: "gpt-slow"})
	if binding.Model != "gpt-fast" || binding.ConnectionID != "router" {
		t.Fatalf("явная модель: %+v", binding)
	}

	// Процесс проекта: тип агента участника — только rivet, модель — из
	// каталога; ошибка у шага и поля.
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
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("rivet", "gpt-nope"))
	mustStatus(t, resp, http.StatusUnprocessableEntity, "модель вне каталога")
	if !jsonContains(body, `"step":"review"`) || !jsonContains(body, `"field":"participants[0].agent.model"`) {
		t.Fatalf("ошибка модели участника: %s", body)
	}
	for _, kind := range []string{"codex", "ghost"} {
		resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process(kind, ""))
		mustStatus(t, resp, http.StatusUnprocessableEntity, "агент "+kind)
		if !jsonContains(body, `"step":"review"`) || !jsonContains(body, `"field":"participants[0].agent.kind"`) {
			t.Fatalf("ошибка типа агента %s: %s", kind, body)
		}
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "", process("rivet", "gpt-fast"))
	mustStatus(t, resp, http.StatusOK, "агент Rivet с моделью каталога")

	// Переопределение модели агента Rivet в проекте.
	for id, model := range map[string]string{"rivet": "gpt-nope", "codex": "gpt-fast"} {
		resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "",
			map[string]any{"agent_models": map[string]any{id: map[string]any{"connection_id": "router", "model": model}}})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "переопределение "+id+"/"+model)
		if !jsonContains(body, `"field":"agent_models.`+id+`"`) {
			t.Fatalf("поле переопределения %s: %s", id, body)
		}
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/projects/"+proj.ID+"/policy", f.user, "",
		map[string]any{"agent_models": map[string]any{"rivet": map[string]any{"connection_id": "router", "model": "gpt-fast"}}})
	mustStatus(t, resp, http.StatusOK, "переопределение")
	if !jsonContains(body, `"rivet":{"name":"Агент Rivet"`) || !jsonContains(body, `"source":"project"`) || !jsonContains(body, `"effective":{"connection_id":"router","model":"gpt-fast"}`) {
		t.Fatalf("действующие модели агента: %s", body)
	}

	// Агент Rivet встроенный: не удаляется, только выключается.
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "удаление агента Rivet")
	if !jsonContains(body, `"error":{"code":"agent_preset","message":"агент Rivet предустановлен, его можно только выключить"}`) || !jsonContains(body, `"field":"id"`) {
		t.Fatalf("отказ удалению: %s", body)
	}
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/system/agents/codex", f.admin, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "удаление несуществующего профиля")
	// Выключение профиля возвращает runner'у объявленные модели, включение —
	// модели каталога.
	full["enabled"] = false
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", full)
	mustStatus(t, resp, http.StatusOK, "выключение профиля")
	rn, _ = f.st.GetRunner(ctx, "rv-1")
	if rn.Catalog || len(rn.Models) != 1 || rn.Models[0] != "declared-1" {
		t.Fatalf("runner после выключения профиля: %+v", rn)
	}
	full["enabled"] = true
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", full)
	mustStatus(t, resp, http.StatusOK, "включение профиля")
	if rn, _ = f.st.GetRunner(ctx, "rv-1"); !rn.Catalog || len(rn.Models) != 2 {
		t.Fatalf("runner после включения профиля: %+v", rn)
	}
	// Режим секретов без значения в запросе не сбрасывается в secure:
	// поле безопасности сохраняется (ревью P3-5).
	full["secrets"] = "never"
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", full)
	mustStatus(t, resp, http.StatusOK, "режим never")
	delete(full, "secrets")
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", full)
	mustStatus(t, resp, http.StatusOK, "правка без режима секретов")
	if a = (agentView{}); json.Unmarshal(body, &a) != nil || a.Secrets != "never" {
		t.Fatalf("режим секретов после правки без него: %s", body)
	}
	// Так же ведут себя название, лимиты и модель по умолчанию: поле без
	// значения сохраняется, переданное заменяет (финальное ревью
	// add-native-agent, P3-3). Так правит профиль e2e-стенд.
	putAgent := func(what string, in map[string]any) agentView {
		t.Helper()
		resp, body := call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", in)
		mustStatus(t, resp, http.StatusOK, what)
		var a agentView
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	storedDefault := func() string {
		t.Helper()
		var v *string
		if err := f.st.Pool.QueryRow(ctx, `SELECT default_model->>'model' FROM agents WHERE id='rivet'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "<auto>"
		}
		return *v
	}
	want := domain.AgentLimits{MaxTurns: 120, MaxTokens: 2000000, MaxMinutes: 45}
	putAgent("название", map[string]any{"name": "Мой агент"})
	if a = putAgent("только секреты", map[string]any{"secrets": "secure"}); a.Name != "Мой агент" || a.Secrets != "secure" || a.Limits != want ||
		a.DefaultModel == nil || a.DefaultModel.Model != "gpt-slow" || storedDefault() != "gpt-slow" || !a.Enabled {
		t.Fatalf("непереданные поля сброшены: %+v", a)
	}
	if a = putAgent("только лимиты", map[string]any{"limits": map[string]any{"max_turns": 50}}); a.Limits != (domain.AgentLimits{MaxTurns: 50}) ||
		storedDefault() != "gpt-slow" || a.Secrets != "secure" || a.Name != "Мой агент" {
		t.Fatalf("лимиты заменяются целиком, остальное сохраняется: %+v", a)
	}
	if a = putAgent("лимиты по умолчанию", map[string]any{"limits": map[string]any{}}); a.Limits != (domain.AgentLimits{}) {
		t.Fatalf("limits {} — умолчания: %+v", a)
	}
	putAgent("лимиты", map[string]any{"limits": want})
	if a = putAgent("лимиты null", map[string]any{"limits": nil}); a.Limits != (domain.AgentLimits{}) {
		t.Fatalf("limits null — умолчания: %+v", a)
	}
	if a = putAgent("автоматическая модель", map[string]any{"default_model": nil}); storedDefault() != "<auto>" ||
		a.DefaultModel == nil || a.DefaultModel.Model != "gpt-fast" || a.Name != "Мой агент" {
		t.Fatalf("default_model null — автоматический выбор: %+v, в базе %s", a, storedDefault())
	}
	if a = putAgent("название по умолчанию", map[string]any{"name": nil}); a.Name != "Агент Rivet" {
		t.Fatalf("name null — «Агент Rivet»: %+v", a)
	}
	for field, bad := range map[string]any{"limits": "x", "default_model": 5, "name": 5} {
		resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/agents/rivet", f.admin, "", map[string]any{field: bad})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "тип поля "+field)
		if !jsonContains(body, `"field":"`+field+`"`) {
			t.Fatalf("поле %s: %s", field, body)
		}
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
