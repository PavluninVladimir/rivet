package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func TestTaskDeveloperOwnerAPIAndPausedEpic(t *testing.T) {
	f := seedDiscovery(t, "")
	ctx := context.Background()
	e, err := f.st.CreateEpic(ctx, f.project, "Existing running Epic", "Keep goal")
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.st.CreateTask(ctx, e.ID, store.NewTask{Title: "Company counter", Description: "UI only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(ctx, `UPDATE epics SET status='paused',started_at=now() WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	base := f.srv.URL + "/api/v1/tasks/" + task.ID
	for _, c := range []struct {
		token  string
		status int
	}{{f.member, http.StatusForbidden}, {f.admin, http.StatusNotFound}} {
		resp, _ := call(t, "PUT", base+"/developer", c.token, "", map[string]any{"agent_id": "eva"})
		mustStatus(t, resp, c.status, "owner-only project-scoped selection")
	}
	for _, body := range []map[string]any{{}, {"agent_id": ""}, {"agent_id": 3}, {"agent_id": "irina"}, {"agent_id": "eva", "model": "fake"}} {
		resp, _ := call(t, "PUT", base+"/developer", f.user, "", body)
		mustStatus(t, resp, http.StatusUnprocessableEntity, "invalid pin")
	}
	resp, _ := call(t, "PUT", base+"/developer", f.user, "", map[string]any{"agent_id": "missing-agent"})
	mustStatus(t, resp, http.StatusNotFound, "missing developer")
	resp, raw := call(t, "PUT", base+"/developer", f.user, "", map[string]any{"agent_id": "eva"})
	mustStatus(t, resp, http.StatusOK, "pin in paused already-started Epic")
	var got domain.Task
	_ = json.Unmarshal(raw, &got)
	if got.DeveloperAgentID != "eva" || got.DeveloperAgentName != "Ева" || got.Status != domain.TaskQueued || got.Description != task.Description {
		t.Fatalf("wrong pin or task edited: %s", raw)
	}
	for _, c := range []struct {
		token string
		can   bool
	}{{f.user, true}, {f.member, false}} {
		resp, raw = call(t, "GET", base, c.token, "", nil)
		mustStatus(t, resp, http.StatusOK, "pin capability")
		var view struct {
			Can  bool        `json:"can_set_developer"`
			Task domain.Task `json:"task"`
		}
		_ = json.Unmarshal(raw, &view)
		if view.Can != c.can || view.Task.DeveloperAgentID != "eva" {
			t.Fatalf("wrong owner capability: %s", raw)
		}
	}
	resp, raw = call(t, "DELETE", f.srv.URL+"/api/v1/system/team/eva", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "pinned agent delete")
	if !strings.Contains(string(raw), "закреплён") {
		t.Fatalf("unreadable pin conflict: %s", raw)
	}
	resp, _ = call(t, "PUT", base+"/developer", f.user, "", map[string]any{"agent_id": nil})
	mustStatus(t, resp, http.StatusOK, "clear pin")
	if _, err = f.st.Pool.Exec(ctx, `UPDATE tasks SET started_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PUT", base+"/developer", f.user, "", map[string]any{"agent_id": "eva"})
	mustStatus(t, resp, http.StatusConflict, "started task cannot pin")
	resp, raw = call(t, "GET", base, f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "started task read")
	var view struct {
		Can bool `json:"can_set_developer"`
	}
	_ = json.Unmarshal(raw, &view)
	if view.Can {
		t.Fatal("started task shown pin action")
	}
}
