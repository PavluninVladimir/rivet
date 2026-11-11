package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func TestStartEmptyEpicAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	p, err := st.CreateProject(ctx, "p", "o/r", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.CreateEpic(ctx, p.ID, "E", "goal")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := srv.URL + "/api/v1/epics/" + e.ID + "/start"
	resp, body := call(t, "POST", endpoint, alice, "", nil)
	mustStatus(t, resp, http.StatusConflict, "пустой план")
	if !strings.Contains(string(body), "добавьте хотя бы одну задачу") {
		t.Fatalf("нет инструкции пользователю: %s", body)
	}
	got, err := st.GetEpic(ctx, e.ID)
	if err != nil || got.Status != domain.EpicPlanned {
		t.Fatalf("статус после отказа: %v, %v", got.Status, err)
	}
	if _, err := st.CreateTask(ctx, e.ID, store.NewTask{Title: "A"}); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", endpoint, alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "запуск после добавления задачи")
}
