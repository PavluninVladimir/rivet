package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Подключения к моделям и модель планировщика (add-model-connections):
// создание с проверкой, обнаружение и ручные модели, выбор модели
// декомпозиции, отключение, удаление с ссылками, аудит.
func TestConnectionsAPI(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()

	// Подставной провайдер: ключ «good» отдаёт модели, «bad» отклонён, иное — сеть.
	old := listModels
	listModels = func(_ context.Context, c llm.Client) ([]llm.Model, error) {
		switch c.Key {
		case "good-key-1234567890":
			if c.Headers["X-Title"] != "rivet" {
				return nil, errors.New("нет заголовка X-Title")
			}
			return []llm.Model{{ID: "m-fast", Label: "Fast"}, {ID: "m-slow"}}, nil
		case "bad-key-1234567890":
			return nil, fmt.Errorf("%w: HTTP 401", llm.ErrUnauthorized)
		}
		return nil, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { listModels = old })

	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/system/connections", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "подключения не-админом")

	// Без модели декомпозиция отказывает no_planner.
	proj, err := f.st.CreateProject(ctx, "p", "org/repo", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := f.st.CreateEpic(ctx, proj.ID, "e", "goal")
	if err != nil {
		t.Fatal(err)
	}
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/epics/"+epic.ID+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusServiceUnavailable, "декомпозиция без модели")
	if !jsonContains(body, "no_planner") {
		t.Fatalf("ожидался код no_planner: %s", body)
	}

	// Валидация с полем: плохой идентификатор, плохой base URL.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/Bad_ID", f.admin, "",
		map[string]any{"name": "x", "kind": "vendor", "api": "openai", "base_url": "https://x"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "плохой id")
	if !jsonContains(body, `"field":"id"`) {
		t.Fatalf("ожидалось поле id: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "",
		map[string]any{"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "router.local"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "плохой base URL")
	if !jsonContains(body, `"field":"base_url"`) {
		t.Fatalf("ожидалось поле base_url: %s", body)
	}

	// У вендора и агрегатора ключ обязателен: без него 422 у поля key.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "",
		map[string]any{"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "https://router.local/v1"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "агрегатор без ключа")
	if !jsonContains(body, `"field":"key"`) {
		t.Fatalf("ожидалось поле key: %s", body)
	}

	// Создание с ключом и секретным заголовком: проверено, ключ префиксом,
	// секреты наружу не уходят.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", map[string]any{
		"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "https://router.local/v1/",
		"key":     "good-key-1234567890",
		"headers": []map[string]any{{"name": "X-Title", "value": "rivet", "secret": false}, {"name": "X-Secret", "value": "hidden-value", "secret": true}},
	})
	mustStatus(t, resp, http.StatusCreated, "создание подключения")
	c := connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.State != "ok" || c.KeyPrefix != "good-key" || !c.HasKey || c.BaseURL != "https://router.local/v1" ||
		jsonContains(body, "good-key-1234567890") || jsonContains(body, "hidden-value") || len(c.Headers) != 2 {
		t.Fatalf("подключение после создания: %s", body)
	}

	// Обнаружение моделей, затем ручная модель с ценой и скрытие обнаруженной.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/connections/router/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "обнаружение")
	if !jsonContains(body, `"added":["m-fast","m-slow"]`) {
		t.Fatalf("обнаружение: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router/models", f.admin, "", map[string]any{
		"models": []map[string]any{
			{"id": "m-fast", "label": "Fast", "hidden": true},
			{"id": "m-slow", "label": "Slow", "input_price": 1500000},
			{"id": "m-manual", "label": "Manual", "input_price": 100, "output_price": 200},
		},
	})
	mustStatus(t, resp, http.StatusOK, "ручные модели")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if len(c.Models) != 3 || c.Models[0].Source != "discovered" || !c.Models[0].Hidden || c.Models[2].Source != "manual" || *c.Models[2].OutputPrice != 200 {
		t.Fatalf("список моделей: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router/models", f.admin, "", map[string]any{
		"models": []map[string]any{{"id": "m-x", "input_price": -1}},
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "отрицательная цена")
	if !jsonContains(body, `"field":"models[0].input_price"`) {
		t.Fatalf("поле цены: %s", body)
	}
	// Предел ответа на ход, предельный выход и цена кеша (add-native-agent,
	// задачи 1.9.1 и 1.12.1): у обнаруженной и ручной модели, с проверкой.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router/models", f.admin, "", map[string]any{
		"models": []map[string]any{
			{"id": "m-fast", "label": "Fast", "hidden": true},
			{"id": "m-slow", "label": "Slow", "input_price": 1500000, "cached_input_price": 150000, "max_output_tokens": 32768, "model_max_output": 65536},
			{"id": "m-manual", "label": "Manual", "input_price": 100, "output_price": 200, "max_output_tokens": 8192},
		},
	})
	mustStatus(t, resp, http.StatusOK, "пределы ответа и цена кеша")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if m := c.Models[1]; m.Source != "discovered" || m.CachedInputPrice == nil || *m.CachedInputPrice != 150000 ||
		m.MaxOutputTokens == nil || *m.MaxOutputTokens != 32768 || m.ModelMaxOutput == nil || *m.ModelMaxOutput != 65536 {
		t.Fatalf("обнаруженная модель с пределами: %s", body)
	}
	if m := c.Models[2]; m.Source != "manual" || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 8192 || m.ModelMaxOutput != nil {
		t.Fatalf("ручная модель с пределом: %s", body)
	}
	// Ответ идёт в то же окно, что и запрос: пределы ответа и выхода не
	// меньше окна отклоняются (финальное ревью add-native-agent, P3-1).
	for _, bad := range []struct {
		model map[string]any
		field string
	}{
		{map[string]any{"id": "m-x", "max_output_tokens": -1}, "max_output_tokens"},
		{map[string]any{"id": "m-x", "max_output_tokens": 65536, "model_max_output": 32768}, "max_output_tokens"},
		{map[string]any{"id": "m-x", "context_window": 32768, "max_output_tokens": 32768}, "max_output_tokens"},
		{map[string]any{"id": "m-x", "context_window": 65536, "max_output_tokens": 8192, "model_max_output": 65536}, "model_max_output"},
	} {
		resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router/models", f.admin, "", map[string]any{"models": []map[string]any{bad.model}})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "предел ответа")
		if !jsonContains(body, `"field":"models[0].`+bad.field+`"`) {
			t.Fatalf("поле %s: %s", bad.field, body)
		}
	}
	// Повторное обнаружение сохраняет ручную запись и цены, пропавшая помечается.
	listModels = func(context.Context, llm.Client) ([]llm.Model, error) { return []llm.Model{{ID: "m-slow"}}, nil }
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/connections/router/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "повторное обнаружение")
	if !jsonContains(body, `"missing":["m-fast"]`) || !jsonContains(body, `"m-manual"`) || !jsonContains(body, `"input_price":1500000`) {
		t.Fatalf("слияние списка: %s", body)
	}

	// Модель планировщика: скрытая не выбирается, обычная выбирается,
	// декомпозиция идёт из каталога.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/planner", f.admin, "", map[string]any{"connection_id": "router", "model": "m-fast"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "скрытая модель")
	if !jsonContains(body, `"field":"model"`) {
		t.Fatalf("поле модели: %s", body)
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/planner", f.admin, "", map[string]any{"connection_id": "router", "model": "m-manual"})
	mustStatus(t, resp, http.StatusOK, "выбор модели планировщика")
	if !jsonContains(body, `"source":"catalog"`) {
		t.Fatalf("источник: %s", body)
	}
	if pl, st := f.api.plannerStatus(); pl == nil || st.Source != planner.SourceCatalog || st.ConnectionID != "router" || st.Model != "m-manual" {
		t.Fatalf("планировщик не переключился: %+v", st)
	}

	// Модель планировщика нельзя скрыть или убрать из списка.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router/models", f.admin, "", map[string]any{
		"models": []map[string]any{{"id": "m-slow"}},
	})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "убрать модель планировщика")
	if !jsonContains(body, `"field":"models"`) {
		t.Fatalf("поле списка: %s", body)
	}

	// Неверный ключ: состояние invalid, декомпозиция отказывает с подключением.
	listModels = func(_ context.Context, c llm.Client) ([]llm.Model, error) {
		if c.Key == "bad-key-1234567890" {
			return nil, fmt.Errorf("%w: HTTP 401", llm.ErrUnauthorized)
		}
		return nil, errors.New("dial tcp: connection refused")
	}
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", map[string]any{
		"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "https://router.local/v1", "key": "bad-key-1234567890",
	})
	mustStatus(t, resp, http.StatusOK, "неверный ключ сохраняется")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.State != "invalid" || len(c.Headers) != 2 {
		t.Fatalf("после неверного ключа: %s", body)
	}
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/epics/"+epic.ID+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusServiceUnavailable, "декомпозиция с неверным ключом")
	if !jsonContains(body, "planner_invalid") || !jsonContains(body, `"connection_id":"router"`) {
		t.Fatalf("ожидался planner_invalid с подключением: %s", body)
	}
	// Сетевая ошибка при проверке — unchecked с пояснением.
	listModels = func(context.Context, llm.Client) ([]llm.Model, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/connections/router/check", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "повторная проверка")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.State != "unchecked" || c.CheckDetail == "" {
		t.Fatalf("ожидался unchecked с пояснением: %s", body)
	}
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/connections/router/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusBadGateway, "обнаружение при недоступном провайдере")

	// Отключение: планировщик недоступен, удаление с ссылкой — 409.
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", map[string]any{
		"name": "Router", "kind": "aggregator", "api": "openai", "base_url": "https://router.local/v1", "enabled": false,
	})
	mustStatus(t, resp, http.StatusOK, "отключение")
	if pl, st := f.api.plannerStatus(); pl != nil || st.State != "invalid" || !strings.Contains(st.Detail, "отключено") {
		t.Fatalf("после отключения: %+v", st)
	}
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "удаление используемого")
	if !jsonContains(body, `"kind":"planner"`) {
		t.Fatalf("ссылки: %s", body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/planner", f.admin, "", map[string]any{})
	mustStatus(t, resp, http.StatusOK, "сброс планировщика")
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/system/connections/router", f.admin, "", nil)
	mustStatus(t, resp, http.StatusNoContent, "удаление")
	if _, st := f.api.plannerStatus(); st.Source != planner.SourceNone {
		t.Fatalf("после удаления источник = %s", st.Source)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation&type=connection.deleted", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "аудит")
	if !jsonContains(body, "connection.deleted") {
		t.Fatalf("нет события удаления: %s", body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/events?scope=installation&type=connection.key_replaced", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "аудит ключа")
	if !jsonContains(body, `"key_prefix":"bad-key-"`) || jsonContains(body, "bad-key-1234567890") {
		t.Fatalf("событие замены ключа: %s", body)
	}
}

// Локальный сервер без ключа сохраняется и проверяется без авторизации.
func TestConnectionLocalWithoutKey(t *testing.T) {
	f := seedOps(t, false)
	old := listModels
	calls := 0
	listModels = func(_ context.Context, c llm.Client) ([]llm.Model, error) {
		calls++
		if c.Key != "" {
			return nil, errors.New("ключ не ожидался")
		}
		return []llm.Model{{ID: "local-7b"}}, nil
	}
	t.Cleanup(func() { listModels = old })
	resp, body := call(t, "PUT", f.srv.URL+"/api/v1/system/connections/lmstudio", f.admin, "",
		map[string]any{"name": "LM Studio", "kind": "local", "api": "openai", "base_url": "http://localhost:1234/v1"})
	mustStatus(t, resp, http.StatusCreated, "локальное подключение без ключа шифрования")
	if !jsonContains(body, `"state":"ok"`) || !jsonContains(body, `"has_key":false`) || calls != 1 {
		t.Fatalf("локальное подключение (проверок %d): %s", calls, body)
	}
	// Ключ без RIVET_SECRET_KEY сохранить нельзя.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/lmstudio", f.admin, "",
		map[string]any{"name": "LM Studio", "kind": "local", "api": "openai", "base_url": "http://localhost:1234/v1", "key": "sk-1234567890abc"})
	mustStatus(t, resp, http.StatusServiceUnavailable, "ключ без RIVET_SECRET_KEY")
	if !jsonContains(body, "no_secret_key") {
		t.Fatalf("ожидался no_secret_key: %s", body)
	}
}

// Запасной источник из окружения: без модели в каталоге планировщик из
// EnvPlanner, выбор из каталога его вытесняет, сброс возвращает окружение.
func TestPlannerEnvFallback(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	old := listModels
	listModels = func(context.Context, llm.Client) ([]llm.Model, error) {
		return []llm.Model{{ID: "deepseek-reasoner"}}, nil
	}
	t.Cleanup(func() { listModels = old })

	f.api.EnvPlanner = EnvPlanner{Provider: "anthropic", Key: "sk-env-1234567890"}
	if err := f.api.ReloadPlanner(ctx); err != nil {
		t.Fatal(err)
	}
	if pl, st := f.api.plannerStatus(); pl == nil || st.Source != planner.SourceEnv || st.ConnectionID != "anthropic" {
		t.Fatalf("ожидался планировщик из окружения: %+v", st)
	}
	resp, body := call(t, "GET", f.srv.URL+"/api/v1/system/planner", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "планировщик")
	if !jsonContains(body, `"source":"env"`) {
		t.Fatalf("источник при env: %s", body)
	}

	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/deepseek", f.admin, "",
		map[string]any{"name": "DeepSeek", "kind": "vendor", "api": "openai", "base_url": "https://api.deepseek.com", "key": "good-key-1234567890"})
	mustStatus(t, resp, http.StatusCreated, "подключение")
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/connections/deepseek/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "обнаружение")
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/planner", f.admin, "", map[string]any{"connection_id": "deepseek", "model": "deepseek-reasoner"})
	mustStatus(t, resp, http.StatusOK, "выбор из каталога")
	if _, st := f.api.plannerStatus(); st.Source != planner.SourceCatalog || st.Model != "deepseek-reasoner" {
		t.Fatalf("каталог должен вытеснить окружение: %+v", st)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/planner", f.admin, "", map[string]any{})
	mustStatus(t, resp, http.StatusOK, "сброс")
	if _, st := f.api.plannerStatus(); st.Source != planner.SourceEnv {
		t.Fatalf("после сброса ожидалось окружение: %+v", st)
	}
}

// Пресеты провайдеров (add-native-agent, задача 1.14.1, спека
// model-connections «Пресеты провайдеров»): список только администратору,
// неизвестный пресет — ошибка у поля в общем конверте без создания
// подключения; после сохранения из пресета сервер запрашивает список
// моделей, а модели пресета сохраняются с источником preset, только если
// провайдер список не отдал, и остаются моделями пресета после ручной
// правки и обнаружения.
func TestConnectionPresetsAPI(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	old := listModels
	listModels = func(_ context.Context, c llm.Client) ([]llm.Model, error) {
		switch {
		case strings.HasPrefix(c.Key, "ds-ok"):
			return []llm.Model{{ID: "deepseek-v4-flash"}, {ID: "deepseek-chat"}}, nil
		case strings.HasPrefix(c.Key, "ds-404"):
			return nil, errors.New("HTTP 404: not found")
		case c.Key == "" && strings.Contains(c.BaseURL, "11434"):
			return []llm.Model{{ID: "qwen3:8b"}}, nil
		}
		return nil, fmt.Errorf("%w: HTTP 401", llm.ErrUnauthorized)
	}
	t.Cleanup(func() { listModels = old })

	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/system/connections/presets", f.user, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "пресеты не-админу")
	resp, body := call(t, "GET", f.srv.URL+"/api/v1/system/connections/presets", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "пресеты")
	var presets struct {
		Presets []llm.Preset `json:"presets"`
	}
	_ = json.Unmarshal(body, &presets)
	if len(presets.Presets) != 6 || presets.Presets[0].ID != "deepseek" || presets.Presets[0].Models[0].MaxOutputTokens != 32768 ||
		jsonContains(body, `"models":null`) || jsonContains(body, "fake") {
		t.Fatalf("список пресетов: %s", body)
	}

	// Неизвестный пресет: общий конверт ошибки, подключение не создаётся.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/fake", f.admin, "", map[string]any{"preset": "fake", "key": "ds-ok-1234567890"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неизвестный пресет")
	if !jsonContains(body, `"error":{"code":"unknown_preset","message":"неизвестный пресет fake"}`) || !jsonContains(body, `"field":"preset"`) {
		t.Fatalf("отказ неизвестному пресету: %s", body)
	}
	if _, err := f.st.GetConnection(ctx, "fake"); err == nil {
		t.Fatal("подключение с неизвестным пресетом создано")
	}

	// DeepSeek отдаёт список: модели с провайдера, у известной пресету —
	// окно и предел ответа пресета; моделей пресета, которых нет у
	// провайдера, нет.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/deepseek", f.admin, "", map[string]any{"preset": "deepseek", "key": "ds-ok-1234567890"})
	mustStatus(t, resp, http.StatusCreated, "DeepSeek из пресета")
	c := connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.State != "ok" || c.API != "openai" || c.BaseURL != "https://api.deepseek.com" || len(c.Models) != 2 {
		t.Fatalf("DeepSeek из пресета: %s", body)
	}
	if m := c.Models[0]; m.ID != "deepseek-v4-flash" || m.Source != "discovered" || m.ContextWindow == nil || *m.ContextWindow != 128000 ||
		m.MaxOutputTokens == nil || *m.MaxOutputTokens != 32768 || m.ModelMaxOutput == nil || *m.ModelMaxOutput != 32768 {
		t.Fatalf("модель с провайдера и данными пресета: %+v", m)
	}
	if m := c.Models[1]; m.ID != "deepseek-chat" || m.Source != "discovered" || m.ContextWindow != nil || m.MaxOutputTokens != nil {
		t.Fatalf("модель вне пресета: %+v", m)
	}

	// Ollama без ключа: модели локального сервера.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/ollama", f.admin, "", map[string]any{"preset": "ollama"})
	mustStatus(t, resp, http.StatusCreated, "Ollama из пресета")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.Kind != "local" || c.BaseURL != "http://localhost:11434/v1" || len(c.Models) != 1 || c.Models[0].ID != "qwen3:8b" || c.Models[0].Source != "discovered" {
		t.Fatalf("Ollama из пресета: %s", body)
	}

	// Провайдер без списка (404): модели пресета с источником preset.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/ds-proxy", f.admin, "", map[string]any{"preset": "deepseek", "key": "ds-404-1234567890"})
	mustStatus(t, resp, http.StatusCreated, "пресет без списка у провайдера")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if len(c.Models) != 2 || c.Models[0].ID != "deepseek-v4-flash" || c.Models[1].ID != "deepseek-v4-pro" {
		t.Fatalf("модели пресета: %s", body)
	}
	for _, m := range c.Models {
		if m.Source != "preset" || m.ContextWindow == nil || *m.ContextWindow != 128000 || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 32768 {
			t.Fatalf("модель пресета: %+v", m)
		}
	}
	// Ручная правка и обнаружение не превращают модель пресета в ручную.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/connections/ds-proxy/models", f.admin, "", map[string]any{
		"models": []map[string]any{
			{"id": "deepseek-v4-flash", "label": "Flash", "context_window": 128000, "max_output_tokens": 16384},
			{"id": "deepseek-v4-pro", "label": "Pro", "context_window": 128000, "max_output_tokens": 32768, "model_max_output": 32768},
		},
	})
	mustStatus(t, resp, http.StatusOK, "правка моделей пресета")
	c = connectionView{}
	_ = json.Unmarshal(body, &c)
	if c.Models[0].Source != "preset" || c.Models[0].Label != "Flash" || *c.Models[0].MaxOutputTokens != 16384 || c.Models[1].Source != "preset" {
		t.Fatalf("модели пресета после правки: %s", body)
	}
	listModels = func(context.Context, llm.Client) ([]llm.Model, error) {
		return []llm.Model{{ID: "deepseek-v4-flash"}, {ID: "deepseek-chat"}}, nil
	}
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/connections/ds-proxy/discover", f.admin, "", nil)
	mustStatus(t, resp, http.StatusOK, "обнаружение у подключения из пресета")
	if !jsonContains(body, `"added":["deepseek-chat"]`) || !jsonContains(body, `"missing":[]`) || jsonContains(body, `"source":"manual"`) {
		t.Fatalf("обнаружение у подключения из пресета: %s", body)
	}

	// Предел ответа пресета доходит до endpoint'а агента Rivet.
	prof, binding, ok, err := f.st.ResolveAgentModel(ctx, "rivet", "", &policy.AgentModel{ConnectionID: "ds-proxy", Model: "deepseek-v4-pro"})
	if err != nil || !ok || binding.ConnectionID != "ds-proxy" {
		t.Fatalf("модель пресета у агента Rivet: %+v %v %v", binding, ok, err)
	}
	ep, err := f.st.BuildModelEndpoint(ctx, prof, binding, f.api.Secrets, false)
	if err != nil || ep.MaxOutputTokens != 32768 || ep.ModelMaxOutput != 32768 || ep.ContextWindow != 128000 {
		t.Fatalf("endpoint модели пресета: %+v %v", ep, err)
	}
}
