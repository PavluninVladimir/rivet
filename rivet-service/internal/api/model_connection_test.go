package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Стадия агента Rivet без подключения к модели (fix-agent-without-connection):
// задача ждёт с причиной, и причина видна в API задачи, а очередь внимания
// пуста — это ожидание настройки, а не эскалация.
func TestTaskWaitsForModelConnectionInAPI(t *testing.T) {
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
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: owner.Login, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "nat", Agent: store.AgentRivet, Model: "deepseek-v4-flash",
		Capabilities: []string{"coding"}, Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, owner.Login, "pw-testpass")
	resp, body := call(t, "GET", srv.URL+"/api/v1/tasks/"+task.ID, session, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("задача: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Task domain.Task `json:"task"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	want := "нет подключения к модели deepseek-v4-flash: добавьте его в Управление → Подключения"
	if out.Task.Status != domain.TaskReady || out.Task.WaitReason != want {
		t.Fatalf("причина ожидания в API: %s %q", out.Task.Status, out.Task.WaitReason)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/attention", session, "", nil)
	var atts []domain.Attention
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &atts) != nil || len(atts) != 0 {
		t.Fatalf("ожидание подключения — не эскалация: %d %s", resp.StatusCode, body)
	}
}
