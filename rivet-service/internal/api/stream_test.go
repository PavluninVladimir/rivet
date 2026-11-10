package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

// sseMsg — сообщение потока: курсор, тип и текст события журнала.
type sseMsg struct {
	id, event, text string
}

// openProjectStream открывает поток проекта (lastID — Last-Event-ID, пусто —
// без заголовка) и отдаёт сообщения по мере прихода. Поток закрывается
// отменой контекста теста.
func openProjectStream(t *testing.T, ctx context.Context, url, session, lastID string) <-chan sseMsg {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	mustStatus(t, resp, http.StatusOK, "поток проекта")
	out := make(chan sseMsg, 100)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var m sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				out <- m
				m = sseMsg{}
			case strings.HasPrefix(line, "id: "):
				m.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var e struct{ Text string }
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
				m.text = e.Text
			}
		}
	}()
	return out
}

// nextMsg ждёт следующее сообщение потока.
func nextMsg(t *testing.T, msgs <-chan sseMsg) sseMsg {
	t.Helper()
	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("поток закрылся")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("нет сообщения потока за 5 секунд")
	}
	return sseMsg{}
}

// Поток проекта без курсора начинается с момента подключения (спека
// observability «Поток событий проекта», fix-console-rivetd-tails):
// история не проигрывается, первое сообщение — курсор начала, событие после
// подключения приходит. С курсором доигрываются события после него, курсор
// из первого сообщения доигрывает разрыв без событий, курсор больше
// последнего события и нечисловой считаются моментом подключения.
func TestStreamCursor(t *testing.T) {
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
	url := srv.URL + "/api/v1/stream?project=" + project.ID
	emit := func(text string) int64 {
		t.Helper()
		id, err := st.AppendEvent(ctx, store.EventInput{ActorKind: domain.ActorSystem, Type: "project.settings", ProjectID: project.ID, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var history []int64
	for i := range 5 {
		history = append(history, emit(fmt.Sprintf("история %d", i)))
	}

	// Без курсора: первое сообщение — курсор последнего события, история
	// не приходит, живое событие приходит.
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	msgs := openProjectStream(t, sctx, url, root, "")
	first := nextMsg(t, msgs)
	if first.event != "" || first.id == "" {
		t.Fatalf("первое сообщение без курсора: %+v", first)
	}
	start, _ := strconv.ParseInt(first.id, 10, 64)
	if start < history[len(history)-1] {
		t.Fatalf("курсор начала %d раньше последнего события истории %d", start, history[len(history)-1])
	}
	live := emit("живое")
	if m := nextMsg(t, msgs); m.text != "живое" || m.id != strconv.FormatInt(live, 10) {
		t.Fatalf("вместо живого события пришло %+v", m)
	}
	cancel()

	// Курсор второго события истории: доигрываются третье, четвёртое,
	// пятое и живое, по порядку.
	sctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	msgs = openProjectStream(t, sctx, url, root, strconv.FormatInt(history[1], 10))
	if m := nextMsg(t, msgs); m.id != strconv.FormatInt(history[1], 10) || m.event != "" {
		t.Fatalf("первое сообщение при переподключении: %+v", m)
	}
	for _, want := range []string{"история 2", "история 3", "история 4", "живое"} {
		if m := nextMsg(t, msgs); m.text != want {
			t.Fatalf("доигрывание: ждали %q, пришло %+v", want, m)
		}
	}
	cancel()

	// Разрыв без событий: курсор из первого сообщения доигрывает событие,
	// записанное за разрыв.
	gap := emit("за разрыв")
	sctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	msgs = openProjectStream(t, sctx, url, root, strconv.FormatInt(live, 10))
	_ = nextMsg(t, msgs)
	if m := nextMsg(t, msgs); m.text != "за разрыв" || m.id != strconv.FormatInt(gap, 10) {
		t.Fatalf("событие разрыва не доиграно: %+v", m)
	}
	cancel()

	// Курсор больше последнего события и нечисловой курсор: поток с
	// момента подключения, следующее событие не пропущено.
	for _, bogus := range []string{strconv.FormatInt(gap+1000, 10), "abc", "-5"} {
		sctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		msgs = openProjectStream(t, sctx, url, root, bogus)
		if m := nextMsg(t, msgs); m.id != strconv.FormatInt(gap, 10) {
			t.Fatalf("курсор %q: начало потока %+v, ждали %d", bogus, m, gap)
		}
		next := emit("после " + bogus)
		if m := nextMsg(t, msgs); m.text != "после "+bogus || m.id != strconv.FormatInt(next, 10) {
			t.Fatalf("курсор %q: вместо следующего события %+v", bogus, m)
		}
		cancel()
		gap = next
	}
}

func TestStreamCursorParse(t *testing.T) {
	for _, c := range []struct {
		header string
		want   int64
	}{
		{"", 100}, {"  ", 100}, {"42", 42}, {" 42 ", 42}, {"0", 0}, {"100", 100},
		{"101", 100}, {"-1", 100}, {"12abc", 100}, {"9223372036854775808", 100},
	} {
		if got := streamCursor(c.header, 100); got != c.want {
			t.Errorf("streamCursor(%q, 100) = %d, ждали %d", c.header, got, c.want)
		}
	}
}
