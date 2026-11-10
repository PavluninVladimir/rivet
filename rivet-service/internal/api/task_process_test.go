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

// Граф процесса в деталке (api-contract fix-web-console-tails): GET
// /tasks/{id} отдаёт снимок процесса задачи и её переходы по шагам.
// Возврат с review виден в истории, правка процесса проекта снимок задачи
// в работе не меняет, у не начатой задачи снимка нет.
func TestTaskDetailProcessAndHistory(t *testing.T) {
	st, srv, engine := testServerEngine(t)
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
	mustStatus(t, resp, http.StatusCreated, "участник bob")
	// Процесс из людей: шаги проходят вердиктами без runner'ов.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{"role": "owner"}}}},
		{"id": "review", "kind": "review", "participants": []map[string]any{{"user": map[string]any{"role": "member"}}}},
		{"id": "merge", "kind": "merge"},
	}}})
	mustStatus(t, resp, http.StatusOK, "процесс с людьми")

	newEpic := func(title string) domain.Epic {
		resp, body := call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": title})
		mustStatus(t, resp, http.StatusCreated, "epic")
		var e domain.Epic
		_ = json.Unmarshal(body, &e)
		return e
	}
	epic := newEpic("E")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	// Задача плана, которую не запускали: снимка нет, переходов нет.
	idle, err := st.CreateTask(ctx, newEpic("Idle").ID, store.NewTask{Title: "B"})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/start", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "запуск epic")
	if err := engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	verdict := func(session, kind, v, detail string) {
		t.Helper()
		resp, body := call(t, "GET", srv.URL+"/api/v1/me/steps", session, "", nil)
		mustStatus(t, resp, http.StatusOK, "очередь")
		var items []struct {
			RunID int64 `json:"run_id"`
			Step  struct{ Kind string }
		}
		_ = json.Unmarshal(body, &items)
		if len(items) != 1 || items[0].Step.Kind != kind {
			t.Fatalf("очередь на шаг %s: %s", kind, body)
		}
		resp, body = call(t, "POST", srv.URL+"/api/v1/tasks/"+task.ID+"/runs/"+itoa(items[0].RunID)+"/verdict", session, "",
			map[string]string{"verdict": v, "detail": detail})
		mustStatus(t, resp, http.StatusOK, "вердикт "+kind)
		_ = body
	}
	verdict(alice, "code", "ok", "в ветке")
	verdict(bob, "review", "changes", "поправить заголовок")

	// Новый процесс проекта без review: задача в работе идёт по прежнему.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+project.ID+"/policy", alice, "", map[string]any{"process": map[string]any{"steps": []map[string]any{
		{"id": "code", "kind": "code", "participants": []map[string]any{{"user": map[string]any{"role": "owner"}}}},
		{"id": "merge", "kind": "merge"},
	}}})
	mustStatus(t, resp, http.StatusOK, "процесс без review")

	type processView struct {
		Step, Status string
		Process      *struct {
			Steps []struct {
				ID, Kind, Title string
				On              struct{ Ok, Changes, Fail string }
			}
		}
		History []struct {
			Step, Outcome, Next string
			TS                  time.Time
		} `json:"step_history"`
	}
	get := func(id string) (processView, []byte) {
		t.Helper()
		resp, body := call(t, "GET", srv.URL+"/api/v1/tasks/"+id+"/process", bob, "", nil)
		mustStatus(t, resp, http.StatusOK, "процесс задачи")
		var d processView
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatal(err)
		}
		return d, body
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/tasks/"+task.ID, bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "деталка")
	var detail struct{ Task struct{ Step, Status string } }
	_ = json.Unmarshal(body, &detail)
	if detail.Task.Step != "code" || detail.Task.Status != string(domain.TaskFixing) {
		t.Fatalf("после замечаний задача на code (fixing): %+v", detail.Task)
	}
	// Деталка процесс не несёт: он читается отдельным запросом.
	if jsonContains(body, `"step_history"`) {
		t.Fatalf("история переходов в GET /tasks/{id}: %s", body)
	}
	d, body := get(task.ID)
	if d.Step != "code" || d.Status != string(domain.TaskFixing) {
		t.Fatalf("шаг и статус задачи в ответе процесса: %s", body)
	}
	if d.Process == nil || len(d.Process.Steps) != 3 {
		t.Fatalf("снимок процесса задачи из трёх шагов: %s", body)
	}
	ids := []string{}
	for _, s := range d.Process.Steps {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "code,review,merge" || d.Process.Steps[1].On.Changes != "code" || d.Process.Steps[2].On.Ok != "done" {
		t.Fatalf("шаги и переходы снимка: %s", body)
	}
	got := []string{}
	for _, h := range d.History {
		if h.TS.IsZero() {
			t.Fatalf("переход без времени: %s", body)
		}
		got = append(got, h.Step+":"+h.Outcome+">"+h.Next)
	}
	if strings.Join(got, " ") != "code:ok>review review:changes>code" {
		t.Fatalf("история переходов: %v", got)
	}

	d, body = get(idle.ID)
	if d.Process != nil || len(d.History) != 0 || !jsonContains(body, `"process":null`) || !jsonContains(body, `"step_history":[]`) {
		t.Fatalf("у не начатой задачи нет снимка и переходов: %s", body)
	}

	// Не участник проекта процесс задачи не видит.
	if _, err := st.CreateUser(ctx, "carol", "", "pw-carol-secret", false); err != nil {
		t.Fatal(err)
	}
	carol := loginSession(t, srv, "carol", "pw-carol-secret")
	resp, _ = call(t, "GET", srv.URL+"/api/v1/tasks/"+task.ID+"/process", carol, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "процесс задачи чужого проекта")
}

// Активация политики установки приходит в поток проекта вживую
// (fix-web-console-tails, access-policy «Версионирование и аудит
// решений»): поток обычного участника проекта её получает, прежние
// активации не проигрываются, активация политики другого проекта в поток не
// попадает, лента активности проекта событий установки не содержит.
func TestStreamInstallationPolicyLive(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "bob", "", "pw-bob-secret", false); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, srv, "root", "root-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")
	newProject := func(name string) string {
		t.Helper()
		resp, body := call(t, "POST", srv.URL+"/api/v1/projects", root, "", map[string]string{"name": name, "repo": "o/" + name})
		mustStatus(t, resp, http.StatusCreated, "проект "+name)
		var p domain.Project
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	project, other := newProject("p"), newProject("other")
	resp, _ := call(t, "POST", srv.URL+"/api/v1/projects/"+project+"/members", root, "", map[string]string{"login": "bob"})
	mustStatus(t, resp, http.StatusCreated, "участник bob")

	policyURL := srv.URL + "/api/v1/system/policy"
	setLimit := func(n int) float64 {
		t.Helper()
		resp, body := call(t, "GET", policyURL, root, "", nil)
		mustStatus(t, resp, http.StatusOK, "политика установки")
		var cur struct{ Presets map[string]any }
		if err := json.Unmarshal(body, &cur); err != nil {
			t.Fatal(err)
		}
		cur.Presets["attempt_limit"] = n
		resp, body = call(t, "PUT", policyURL, root, "", cur.Presets)
		mustStatus(t, resp, http.StatusOK, "сохранение политики установки")
		var saved struct{ Version struct{ Version float64 } }
		if err := json.Unmarshal(body, &saved); err != nil || saved.Version.Version == 0 {
			t.Fatalf("версия в ответе сохранения: %v %s", err, body)
		}
		return saved.Version.Version
	}
	// История до подключения.
	setLimit(4)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", srv.URL+"/api/v1/stream?project="+project, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: bob})
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	mustStatus(t, stream, http.StatusOK, "поток проекта участника")
	lines := make(chan string, 100)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	// Политика другого проекта: её активация в поток не приходит.
	resp, _ = call(t, "PUT", srv.URL+"/api/v1/projects/"+other+"/policy", root, "", map[string]any{"attempt_limit": 2})
	mustStatus(t, resp, http.StatusOK, "политика другого проекта")
	live := setLimit(5)

	var activations []domain.Event
	typ := ""
	for line := range lines {
		if strings.HasPrefix(line, "event: ") {
			typ = strings.TrimPrefix(line, "event: ")
			continue
		}
		if typ != "policy.activated" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e domain.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		activations = append(activations, e)
		break
	}
	// Первая пришедшая активация должна быть живой активацией установки:
	// не историей и не активацией другого проекта.
	if len(activations) != 1 {
		t.Fatalf("живая активация политики установки не пришла: %+v", activations)
	}
	e := activations[0]
	if e.ProjectID != "" || e.Payload["scope"] != store.PolicyScopeInstallation || e.Payload["version"] != live || e.Payload["hash"] == "" {
		t.Fatalf("активация политики установки в потоке: %+v", e)
	}
	if _, has := e.Payload["content"]; has {
		t.Fatalf("содержимое политики в потоке: %+v", e.Payload)
	}

	// Лента активности проекта событий установки не содержит.
	resp, body := call(t, "GET", srv.URL+"/api/v1/events?project="+project+"&type=policy.activated", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "лента проекта")
	var feed []domain.Event
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed) != 0 {
		t.Fatalf("активация установки в ленте проекта: %s", body)
	}
}
