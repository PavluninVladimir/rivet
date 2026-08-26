package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Очередь «мои шаги» и вердикт человека (api-contract add-process-humans):
// адресация по роли, 403 не адресату, 422 без замечаний, 409 повтор.
func TestHumanStepsAPI(t *testing.T) {
	st, srv, engine := testServerEngine(t)
	ctx := context.Background()
	for _, u := range []string{"alice", "bob", "carol"} {
		if _, err := st.CreateUser(ctx, u, "", "pw-"+u+"-secret", false); err != nil {
			t.Fatal(err)
		}
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")
	carol := loginSession(t, srv, "carol", "pw-carol-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "", map[string]string{"login": "bob"})
	mustStatus(t, resp, http.StatusCreated, "участник bob")

	// Логин не из проекта отклоняется с шагом и участником.
	resp, body = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{"login": "carol"}}}},
		{"id": "merge", "kind": "merge"},
	}}})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "логин не в проекте")
	if !jsonContains(body, `"step":"code"`) || !jsonContains(body, "не состоит в проекте") {
		t.Fatalf("ошибка членства: %s", body)
	}
	// Процесс: человек-исполнитель code по роли owner.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{"role": "owner"}}}},
		{"id": "review", "kind": "review", "participants": []map[string]any{{"user": map[string]any{"role": "member"}}}},
		{"id": "merge", "kind": "merge"},
	}}})
	mustStatus(t, resp, http.StatusOK, "процесс с людьми")

	// Epic с задачей, запуск: движок вводит задачу на шаг code с запуском человека.
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/start", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "запуск epic")
	if err := engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/me/steps", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "очередь владельца")
	var items []struct {
		RunID int64 `json:"run_id"`
		Step  struct{ Kind string }
		Ask   string
	}
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0].Step.Kind != "code" || items[0].Ask != "code" {
		t.Fatalf("очередь alice: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/me/steps", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "очередь участника")
	if !jsonContains(body, "[]") {
		t.Fatalf("у bob (member) шаг code не должен быть в очереди: %s", body)
	}
	runURL := srv.URL + "/api/v1/tasks/" + task.ID + "/runs/" + itoa(items[0].RunID) + "/verdict"
	// Не адресат — 403.
	resp, _ = call(t, "POST", runURL, bob, "", map[string]string{"verdict": "ok"})
	mustStatus(t, resp, http.StatusForbidden, "вердикт не адресата")
	// Невалидный вердикт — 422.
	resp, _ = call(t, "POST", runURL, alice, "", map[string]string{"verdict": "maybe"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "невалидный вердикт")
	// Готово от владельца — задача идёт дальше на review (человек-member).
	resp, body = call(t, "POST", runURL, alice, "", map[string]string{"verdict": "ok", "detail": "в ветке"})
	mustStatus(t, resp, http.StatusOK, "вердикт владельца")
	if !jsonContains(body, `"Status":"review"`) || !jsonContains(body, `"Step":"review"`) {
		t.Fatalf("после «готово» задача на review: %s", body)
	}
	// Повтор по закрытому запуску — 409.
	resp, _ = call(t, "POST", runURL, alice, "", map[string]string{"verdict": "ok"})
	mustStatus(t, resp, http.StatusConflict, "повторный вердикт")
	// Review ждёт bob: без замечаний вернуть нельзя.
	resp, body = call(t, "GET", srv.URL+"/api/v1/me/steps", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "очередь bob")
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0].Step.Kind != "review" {
		t.Fatalf("очередь bob: %s", body)
	}
	reviewURL := srv.URL + "/api/v1/tasks/" + task.ID + "/runs/" + itoa(items[0].RunID) + "/verdict"
	resp, _ = call(t, "POST", reviewURL, bob, "", map[string]string{"verdict": "changes"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "замечания без текста")
	// Не участник проекта задачу не видит вовсе (слой видимости): 404.
	resp, _ = call(t, "POST", reviewURL, carol, "", map[string]string{"verdict": "ok"})
	mustStatus(t, resp, http.StatusNotFound, "не участник проекта")
	// Деталка задачи показывает участников шага.
	resp, body = call(t, "GET", srv.URL+"/api/v1/tasks/"+task.ID, bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "деталка")
	if !jsonContains(body, `"step_runs"`) || !jsonContains(body, `"user":"role:member"`) {
		t.Fatalf("участники шага в деталке: %s", body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
