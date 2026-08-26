package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Процесс проекта в API политики (api-contract add-process-model): процесс
// в GET, PUT целиком, 422 с шагом и полем, тело без process не сбрасывает
// процесс проекта, участники-люди пока отклоняются.
func TestProjectProcessAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	url := srv.URL + "/api/v1/projects/" + project.ID + "/policy"

	// По умолчанию — процесс установки, в effective он раскрыт.
	resp, body = call(t, "GET", url, alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "политика")
	var view struct {
		ProcessSource string `json:"process_source"`
		Effective     struct {
			Process *struct {
				Steps []map[string]any `json:"steps"`
			} `json:"process"`
		} `json:"effective"`
	}
	_ = json.Unmarshal(body, &view)
	if view.ProcessSource != "installation" {
		t.Fatalf("источник процесса по умолчанию: %s", view.ProcessSource)
	}

	// Невалидный документ: переход на несуществующий шаг — 422 с шагом и полем.
	bad := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}, "on": map[string]any{"changes": "nope"}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, body = call(t, "PUT", url, alice, "", bad)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "невалидный процесс")
	if !jsonContains(body, `"step":"code"`) || !jsonContains(body, `"field":"on.changes"`) {
		t.Fatalf("422 должен указывать шаг и поле: %s", body)
	}

	// Участник-человек по логину участника проекта принимается
	// (add-process-humans); без логина и роли — отклоняется.
	human := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{"login": "alice"}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, _ = call(t, "PUT", url, alice, "", human)
	mustStatus(t, resp, http.StatusOK, "участник-человек")
	empty := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, body = call(t, "PUT", url, alice, "", empty)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "человек без логина и роли")
	if !jsonContains(body, "без логина и роли") {
		t.Fatalf("сообщение: %s", body)
	}

	// Валидный процесс с двумя ревьюерами сохраняется новой версией.
	good := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "review", "kind": "review", "require": "all", "participants": []map[string]any{
			{"agent": map[string]any{"kind": "codex", "model": "gpt-5"}}, {"agent": map[string]any{}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, body = call(t, "PUT", url, alice, "", good)
	mustStatus(t, resp, http.StatusOK, "процесс проекта")
	_ = json.Unmarshal(body, &view)
	if view.ProcessSource != "project" || view.Effective.Process == nil || len(view.Effective.Process.Steps) != 3 {
		t.Fatalf("после PUT: source=%s process=%+v", view.ProcessSource, view.Effective.Process)
	}
	if !jsonContains(body, `"model":"gpt-5"`) {
		t.Fatalf("участник с моделью в ответе: %s", body)
	}

	// Тело без ключа process (старый клиент правит пресет) не сбрасывает процесс.
	resp, body = call(t, "PUT", url, alice, "", map[string]any{"attempt_limit": 4})
	mustStatus(t, resp, http.StatusOK, "пресет без process")
	_ = json.Unmarshal(body, &view)
	if view.ProcessSource != "project" || !jsonContains(body, `"attempt_limit":4`) {
		t.Fatalf("процесс проекта должен сохраниться: source=%s %s", view.ProcessSource, body)
	}

	// Явный null возвращает процесс установки.
	resp, body = call(t, "PUT", url, alice, "", map[string]any{"process": nil})
	mustStatus(t, resp, http.StatusOK, "сброс процесса")
	_ = json.Unmarshal(body, &view)
	if view.ProcessSource != "installation" {
		t.Fatalf("после process=null: %s", view.ProcessSource)
	}
}
