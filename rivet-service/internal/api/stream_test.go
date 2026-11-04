package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Поток проекта отдаёт события установки только вживую (add-agent-roles):
// история занятости агентов при подключении не проигрывается, а событие
// после подключения приходит.
func TestStreamGlobalEventsLiveOnly(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", root, "", map[string]string{"name": "p", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project domain.Project
	if err := json.Unmarshal(body, &project); err != nil {
		t.Fatal(err)
	}
	// История до подключения: занятость агента в другом проекте.
	if _, err := st.AppendEvent(ctx, store.AgentWorkEvent("maks", "Макс", true)); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", srv.URL+"/api/v1/stream?project="+project.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: root})
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	mustStatus(t, stream, http.StatusOK, "поток проекта")
	lines := make(chan string, 100)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	// Заголовки пришли: начало живых событий уже зафиксировано.
	if _, err := st.AppendEvent(ctx, store.AgentWorkEvent("maks", "Макс", false)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range lines {
		if strings.HasPrefix(line, "event: ") {
			got = append(got, strings.TrimPrefix(line, "event: "))
		}
		if line == "event: "+store.EventAgentFree {
			break
		}
	}
	for _, typ := range got {
		if typ == store.EventAgentBusy {
			t.Fatalf("история событий установки проиграна: %v", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != store.EventAgentFree {
		t.Fatalf("живое событие занятости не пришло: %v", got)
	}
}
