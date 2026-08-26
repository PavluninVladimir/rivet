package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Ограничения установки на процесс (спека process «Ограничения установки»,
// api-contract add-process-editor): 422 с полем locks, violations в ответе.
func TestProcessLocksAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "root", "", "pw-root-secret", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "pw-root-secret")
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	url := srv.URL + "/api/v1/projects/" + project.ID + "/policy"

	// Проект без review сохраняется, пока ограничений нет.
	noReview := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, _ = call(t, "PUT", url, alice, "", noReview)
	mustStatus(t, resp, http.StatusOK, "процесс без review")

	// Администратор требует review и человека на нём: ответ перечисляет проект.
	resp, body = call(t, "PUT", srv.URL+"/api/v1/system/policy", root, "", map[string]any{
		"attempt_limit": 3, "review_limit": 3, "auto_publish": true,
		"process_locks": map[string]any{"required_kinds": []string{"review"}, "human_review": true},
	})
	mustStatus(t, resp, http.StatusOK, "ограничения установки")
	if !jsonContains(body, `"violations"`) || !jsonContains(body, `"project":"p"`) || !jsonContains(body, "включённый шаг типа review") {
		t.Fatalf("нарушения в ответе: %s", body)
	}
	// Теперь процесс без review не сохраняется.
	resp, body = call(t, "PUT", url, alice, "", noReview)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "ограничение установки")
	if !jsonContains(body, `"field":"locks"`) {
		t.Fatalf("422 с полем locks: %s", body)
	}
	// Review только с агентами тоже не проходит, с человеком — проходит.
	agentsOnly := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "review", "kind": "review", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, body = call(t, "PUT", url, alice, "", agentsOnly)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "человек на review обязателен")
	if !jsonContains(body, `"step":"review"`) {
		t.Fatalf("шаг в ошибке: %s", body)
	}
	withHuman := map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "review", "kind": "review", "participants": []map[string]any{{"agent": map[string]any{}}, {"user": map[string]any{"role": "owner"}}}},
		{"id": "check", "kind": "prompt", "prompt": "проверь миграции", "participants": []map[string]any{{"agent": map[string]any{}}}},
		{"id": "merge", "kind": "merge"},
	}}}
	resp, _ = call(t, "PUT", url, alice, "", withHuman)
	mustStatus(t, resp, http.StatusOK, "процесс с человеком и prompt")
	// prompt без текста — 422 с полем prompt.
	withHuman["process"].(map[string]any)["steps"].([]map[string]any)[2]["prompt"] = ""
	resp, body = call(t, "PUT", url, alice, "", withHuman)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "prompt без текста")
	if !jsonContains(body, `"field":"prompt"`) {
		t.Fatalf("поле prompt: %s", body)
	}
}
