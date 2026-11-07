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
	"github.com/PavluninVladimir/rivet/internal/policy"
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

// noPRHosting — fake-хостинг, который отказывает в создании PR (в ветке
// нет изменений).
type noPRHosting struct{ *scm.Fake }

func (noPRHosting) CreatePR(ctx context.Context, repo, branch, base, title, body string) (scm.PR, error) {
	return scm.PR{}, fmt.Errorf("github create PR: 422: No commits between %s and %s", base, branch)
}

// Merge задачи без PR (fix-pr-without-test-step): PR создаётся и мержится,
// а если хостинг отказал, ответ 502 pr_not_created и задача не done.
func TestTaskMergeWithoutPR(t *testing.T) {
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
	session := loginSession(t, srv, owner.Login, "pw-testpass")
	atMerge := func(title string) domain.Task {
		task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', step_id='merge', step_gen=1, pr_url='', branch=$2 WHERE id=$1`,
			task.ID, fmt.Sprintf("agent/task-%d", task.Num)); err != nil {
			t.Fatal(err)
		}
		return task
	}

	failing := atMerge("без изменений")
	fake := engine.Adapters.Fallback
	engine.Adapters.Fallback = noPRHosting{scm.NewFake()}
	resp, body := call(t, "POST", srv.URL+"/api/v1/tasks/"+failing.ID+"/merge", session, "", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("ожидался 502, got %d %s", resp.StatusCode, body)
	}
	var out struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Error.Code != "pr_not_created" ||
		!strings.Contains(out.Error.Message, "No commits") {
		t.Fatalf("тело ответа: %s", body)
	}
	if got, _ := st.GetTask(ctx, failing.ID); got.Status != domain.TaskReview || got.PRURL != "" {
		t.Fatalf("без PR задача остаётся в ожидании merge: %s %q", got.Status, got.PRURL)
	}

	engine.Adapters.Fallback = fake
	ok := atMerge("с изменениями")
	resp, body = call(t, "POST", srv.URL+"/api/v1/tasks/"+ok.ID+"/merge", session, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge: %d %s", resp.StatusCode, body)
	}
	got, _ := st.GetTask(ctx, ok.ID)
	if got.Status != domain.TaskDone || !strings.HasPrefix(got.PRURL, "https://fake.local/own/proj/pull/") {
		t.Fatalf("merge без PR должен создать PR: %s %q", got.Status, got.PRURL)
	}
}

// Повтор задачи, упавшей без PR перед review: PR создаётся до повтора,
// отказ хостинга — 502 pr_not_created без изменения задачи
// (fix-pr-without-test-step).
func TestTaskRetryCreatesPR(t *testing.T) {
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
	proc, _ := json.Marshal(policy.Defaults().EffectiveProcess())
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='failed', step_id='review', step_entry='ok', process=$2, pr_url='', branch='agent/task-1' WHERE id=$1`,
		task.ID, proc); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, owner.Login, "pw-testpass")

	fake := engine.Adapters.Fallback
	engine.Adapters.Fallback = noPRHosting{scm.NewFake()}
	resp, body := call(t, "POST", srv.URL+"/api/v1/tasks/"+task.ID+"/retry", session, "", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "pr_not_created") {
		t.Fatalf("повтор при отказе хостинга: %d %s", resp.StatusCode, body)
	}
	if got, _ := st.GetTask(ctx, task.ID); got.Status != domain.TaskFailed {
		t.Fatalf("задача не меняется: %s", got.Status)
	}
	engine.Adapters.Fallback = fake
	resp, body = call(t, "POST", srv.URL+"/api/v1/tasks/"+task.ID+"/retry", session, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("повтор: %d %s", resp.StatusCode, body)
	}
	if got, _ := st.GetTask(ctx, task.ID); got.Status != domain.TaskQueued || got.PRURL == "" || got.StepID != "review" {
		t.Fatalf("PR создан до повтора, шаг review: %s %q %s", got.Status, got.PRURL, got.StepID)
	}
}
