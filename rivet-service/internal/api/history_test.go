package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Импорт истории (api-contract add-history-import): только владелец,
// невалидный манифест — 422, итог с числами созданных.
func TestHistoryImportAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	for _, u := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, u, "", "pw-"+u+"-secret", false); err != nil {
			t.Fatal(err)
		}
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/members", alice, "", map[string]string{"login": "bob"})
	mustStatus(t, resp, http.StatusCreated, "участник")

	manifest := map[string]any{"source": "openspec", "epics": []map[string]any{{
		"key": "2026-08-12-harden-core", "title": "Ядро", "created_at": "2026-08-12T00:00:00Z", "done_at": "2026-08-12T15:30:00Z",
		"tasks": []map[string]any{{"title": "1.1 Попытки", "done": true, "repo": "rivet", "pr_url": "https://gh/r/pull/8"}},
	}}}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/history", bob, "", manifest)
	mustStatus(t, resp, http.StatusForbidden, "участник импортирует")
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/history", alice, "", manifest)
	mustStatus(t, resp, http.StatusOK, "владелец импортирует")
	if !jsonContains(body, `"epics_created":1`) || !jsonContains(body, `"tasks_created":1`) {
		t.Fatalf("итог импорта: %s", body)
	}
	// Повтор — обновление без дубликатов.
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/history", alice, "", manifest)
	mustStatus(t, resp, http.StatusOK, "повторный импорт")
	if !jsonContains(body, `"epics_created":0`) || !jsonContains(body, `"epics_updated":1`) {
		t.Fatalf("повторный импорт: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/epics", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "список Epic'ов")
	if !jsonContains(body, `"SourceKey":"2026-08-12-harden-core"`) || !jsonContains(body, `"Status":"done"`) {
		t.Fatalf("Epic истории в списке: %s", body)
	}
	// Лента: latest=1 отдаёт последние события, без него — первые по id.
	resp, body = call(t, "GET", srv.URL+"/api/v1/events?project="+project.ID+"&limit=1&latest=1", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "последние события")
	if !jsonContains(body, `"Type":"history.imported"`) {
		t.Fatalf("latest=1 должен отдать последнее событие: %s", body)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/events?project="+project.ID+"&limit=1", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "первые события")
	if jsonContains(body, `"Type":"history.imported"`) {
		t.Fatalf("без latest порядок с начала ленты: %s", body)
	}
	// Невалидный манифест: без ключа.
	bad := map[string]any{"source": "openspec", "epics": []map[string]any{{"title": "без ключа", "created_at": "2026-08-12T00:00:00Z", "done_at": "2026-08-12T00:00:00Z"}}}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/history", alice, "", bad)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "манифест без ключа")
}
