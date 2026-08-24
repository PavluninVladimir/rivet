package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Правка плана через API (api-contract add-plan-editing).
func TestPlanEditingAPI(t *testing.T) {
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
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)

	mkTask := func(title string, deps []string) domain.Task {
		t.Helper()
		resp, body := call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/tasks", alice, "",
			map[string]any{"title": title, "deps": deps})
		mustStatus(t, resp, http.StatusCreated, "задача")
		var task domain.Task
		_ = json.Unmarshal(body, &task)
		return task
	}
	a := mkTask("A", nil)
	b := mkTask("B", []string{a.ID})

	// Правка полей + зависимостей.
	resp, body = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+b.ID, alice, "", map[string]any{
		"title": "B: уточнено", "criteria": []string{"к1"}, "deps": []string{}})
	mustStatus(t, resp, http.StatusOK, "правка")
	var got domain.Task
	_ = json.Unmarshal(body, &got)
	if got.Title != "B: уточнено" || len(got.Criteria) != 1 || len(got.Deps) != 0 {
		t.Fatalf("правка: %s", body)
	}
	// Цикл — 422.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+a.ID, alice, "", map[string]any{"deps": []string{a.ID}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "самоссылка")
	// attempt_limit продолжает работать вместе с полями плана.
	resp, body = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+a.ID, alice, "", map[string]any{
		"attempt_limit": 5, "description": "описание"})
	mustStatus(t, resp, http.StatusOK, "лимит+описание")
	_ = json.Unmarshal(body, &got)
	if got.AttemptLimit != 5 || got.Description != "описание" {
		t.Fatalf("совмещённая правка: %s", body)
	}
	// Пустой PATCH — 422.
	resp, _ = call(t, "PATCH", srv.URL+"/api/v1/tasks/"+a.ID, alice, "", map[string]any{})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "пустая правка")

	// Удаление в planned (чистая задача без истории); повтор — 404.
	c := mkTask("C", nil)
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/tasks/"+c.ID, alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "удаление")
	resp, _ = call(t, "DELETE", srv.URL+"/api/v1/tasks/"+c.ID, alice, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "повторное удаление")
	// A имеет историю (plan_edited) — 409 с подсказкой.
	resp, body = call(t, "DELETE", srv.URL+"/api/v1/tasks/"+a.ID, alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "история блокирует")
	if !jsonContains(body, "отмените") {
		t.Fatalf("нужна подсказка про отмену: %s", body)
	}
}
