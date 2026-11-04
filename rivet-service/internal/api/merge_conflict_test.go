package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Ручной merge конфликтующего PR отвечает 409 merge_conflict, а задача
// уже ушла агенту на разрешение конфликта (api-contract
// add-merge-conflict-recovery).
func TestTaskMergeConflict(t *testing.T) {
	ctx := context.Background()
	st, srv, engine := testServerEngine(t)
	owner, err := st.CreateUser(ctx, fmt.Sprintf("owner-%d", time.Now().UnixNano()), "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "demo", "own/proj", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	// PR на fake-хостинге с меткой конфликта: первый merge отклоняется.
	pr, err := engine.SCM.CreatePR(ctx, "own/proj", "agent/task-1", "main", "task-1: A "+scm.FakeConflictMarker, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', step_id='merge', step_gen=1, pr_url=$2, branch='agent/task-1' WHERE id=$1`,
		task.ID, pr.URL); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, owner.Login, "pw-testpass")

	resp, body := call(t, "POST", srv.URL+"/api/v1/tasks/"+task.ID+"/merge", session, "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ожидался 409, got %d %s", resp.StatusCode, body)
	}
	var out struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Error.Code != "merge_conflict" ||
		!strings.Contains(out.Error.Message, "возвращена агенту") {
		t.Fatalf("тело ответа: %s", body)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskFixing {
		t.Fatalf("задача должна уйти на разрешение конфликта: %s", got.Status)
	}
}
