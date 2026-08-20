package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Фикстура сессий: проект владельца, задача, закрытая сессия CODING с
// транскриптом (если MinIO доступен) и открытая TESTING без транскрипта.
type sessionsFixture struct {
	srv             *httptest.Server
	st              *store.Store
	task, empty     domain.Task
	withRef, noRef  string
	transcript      string
	owner, outsider string // логины (пароль pw)
	blobUp          bool
}

func seedSessions(t *testing.T) sessionsFixture {
	t.Helper()
	ctx := context.Background()
	st, _ := testServer(t)

	f := sessionsFixture{st: st, transcript: "строка вывода агента\n"}
	suffix := time.Now().UnixNano()
	f.owner, f.outsider = fmt.Sprintf("owner-%d", suffix), fmt.Sprintf("mallory-%d", suffix)
	ownerU, err := st.CreateUser(ctx, f.owner, "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, f.outsider, "", "pw-testpass", false); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, ownerU.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := st.CreateEpic(ctx, p.ID, "E", "")
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	f.empty, err = st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B"})
	if err != nil {
		t.Fatal(err)
	}

	// Blob по возможности: без MinIO транскрипт-ручка отвечает 404.
	var bl *blob.Store
	endpoint := os.Getenv("RIVET_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	if b, err := blob.New(endpoint, envDef("RIVET_S3_ACCESS_KEY", "rivet"),
		envDef("RIVET_S3_SECRET_KEY", "rivetsecret"), "rivet-test", false); err == nil {
		if err := b.EnsureBucket(ctx); err == nil {
			bl = b
			f.blobUp = true
		}
	}
	f.srv = httptest.NewServer((&Server{St: st, Blob: bl}).Handler())
	t.Cleanup(f.srv.Close)

	ref := ""
	if bl != nil {
		if ref, err = bl.Put(ctx, fmt.Sprintf("tests/api-%d.log", suffix), []byte(f.transcript)); err != nil {
			t.Fatal(err)
		}
	}
	f.withRef, err = st.CreateSession(ctx, domain.Session{
		TaskID: f.task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal, Scope: "CODING",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EndSession(ctx, f.withRef, ref); err != nil {
		t.Fatal(err)
	}
	f.noRef, err = st.CreateSession(ctx, domain.Session{
		TaskID: f.task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: "fake", Model: "m", Depth: domain.DepthMinimal, Scope: "TESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func envDef(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// GET /tasks/{id}/sessions: участник видит историю (стадии нормализованы,
// nullable tokens, пустая история — []); не-участник получает 404.
func TestTaskSessionsAPI(t *testing.T) {
	f := seedSessions(t)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")

	resp, body := call(t, "GET", f.srv.URL+"/api/v1/tasks/"+f.task.ID+"/sessions", sess, "", nil)
	mustStatus(t, resp, http.StatusOK, "сессии задачи")
	var got []struct {
		ID            string  `json:"id"`
		Stage         string  `json:"stage"`
		Tokens        *int64  `json:"tokens"`
		EndedAt       *string `json:"ended_at"`
		HasTranscript bool    `json:"has_transcript"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(got) != 2 || got[0].ID != f.withRef || got[1].ID != f.noRef {
		t.Fatalf("ожидались 2 сессии по порядку: %s", body)
	}
	if got[0].Stage != "coding" || got[1].Stage != "testing" {
		t.Fatalf("стадии не нормализованы: %s", body)
	}
	if got[0].Tokens != nil || got[1].Tokens != nil {
		t.Fatalf("tokens без usage должны быть null: %s", body)
	}
	if got[0].EndedAt == nil || got[1].EndedAt != nil {
		t.Fatalf("ended_at: первая закрыта, вторая открыта: %s", body)
	}
	if got[0].HasTranscript != f.blobUp || got[1].HasTranscript {
		t.Fatalf("has_transcript: %s", body)
	}

	// Пустая история — [], не null.
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/tasks/"+f.empty.ID+"/sessions", sess, "", nil)
	mustStatus(t, resp, http.StatusOK, "пустая история")
	if string(body) != "[]\n" {
		t.Fatalf("ожидался [], получено %q", body)
	}

	// Не-участник: 404, существование задачи не раскрывается.
	mal := loginSession(t, f.srv, f.outsider, "pw-testpass")
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/tasks/"+f.task.ID+"/sessions", mal, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "сессии для не-участника")

	// Без аутентификации — 401.
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/tasks/"+f.task.ID+"/sessions", "", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "сессии без входа")
}

// GET /sessions/{id}/transcript: участник читает сохранённый транскрипт;
// 404 не различает не-участника, отсутствие транскрипта и чужой id.
func TestSessionTranscriptAPI(t *testing.T) {
	f := seedSessions(t)
	sess := loginSession(t, f.srv, f.owner, "pw-testpass")

	if f.blobUp {
		resp, body := call(t, "GET", f.srv.URL+"/api/v1/sessions/"+f.withRef+"/transcript", sess, "", nil)
		mustStatus(t, resp, http.StatusOK, "транскрипт")
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Fatalf("Content-Type: %q", ct)
		}
		if string(body) != f.transcript {
			t.Fatalf("тело транскрипта: %q", body)
		}
	} else {
		t.Log("minio недоступен — проверяются только 404-ветки")
	}

	// Сессия без сохранённого транскрипта.
	resp, _ := call(t, "GET", f.srv.URL+"/api/v1/sessions/"+f.noRef+"/transcript", sess, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "сессия без транскрипта")

	// Не-участник.
	mal := loginSession(t, f.srv, f.outsider, "pw-testpass")
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/sessions/"+f.withRef+"/transcript", mal, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "транскрипт для не-участника")

	// Несуществующая сессия.
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/sessions/00000000-0000-0000-0000-000000000000/transcript", sess, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "несуществующая сессия")

	// Без аутентификации.
	resp, _ = call(t, "GET", f.srv.URL+"/api/v1/sessions/"+f.withRef+"/transcript", "", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized, "транскрипт без входа")
}
