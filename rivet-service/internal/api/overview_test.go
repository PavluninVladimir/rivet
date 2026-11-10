package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Обзор проекта и отчёт (add-project-lead-view, задача 1.8): права, ошибки
// по контракту, заголовки отчёта, целевая дата Epic'а только владельцем,
// плановый бюджет месяца в политике проекта.
func TestProjectOverviewAPI(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	for _, login := range []string{"alice", "bob", "eve"} {
		if _, err := st.CreateUser(ctx, login, "", "pw-"+login+"-secret", false); err != nil {
			t.Fatal(err)
		}
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	bob := loginSession(t, srv, "bob", "pw-bob-secret")
	eve := loginSession(t, srv, "eve", "pw-eve-secret")

	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "Бюджет CRM", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	if err := st.AddMember(ctx, project.ID, "bob", domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)

	base := srv.URL + "/api/v1/projects/" + project.ID
	// Участник видит обзор, чужой получает 404.
	resp, body = call(t, "GET", base+"/overview?period=30d&tz=Europe/Moscow", bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "обзор участнику")
	var ov struct {
		ProjectID  string `json:"project_id"`
		Period     struct{ Name, TZ string }
		Thresholds struct {
			StalledMinutes int `json:"stalled_minutes"`
		}
		Epics []json.RawMessage
	}
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatal(err)
	}
	if ov.ProjectID != project.ID || ov.Period.Name != "30d" || ov.Period.TZ != "Europe/Moscow" || ov.Thresholds.StalledMinutes != 60 {
		t.Fatalf("обзор: %s", body)
	}
	resp, _ = call(t, "GET", base+"/overview", eve, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "обзор чужому")
	for q, field := range map[string]string{"period=year": "period", "tz=Mars/Base": "tz", "tz=Local": "tz"} {
		resp, body = call(t, "GET", base+"/overview?"+q, alice, "", nil)
		mustStatus(t, resp, http.StatusUnprocessableEntity, q)
		if !strings.Contains(string(body), `"field":"`+field+`"`) {
			t.Fatalf("%s: поле ошибки: %s", q, body)
		}
	}

	// Отчёт: Markdown, имя файла из названия проекта, лимит 92 суток.
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "tz": {"UTC"}}
	resp, body = call(t, "GET", base+"/report?"+q.Encode(), bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "отчёт")
	if ct := resp.Header.Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("тип отчёта: %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="rivet-byudzhet-crm-2026-10-26-2026-11-01.md"` {
		t.Fatalf("имя файла: %q", cd)
	}
	if !strings.HasPrefix(string(body), "# ") {
		t.Fatalf("отчёт: %s", body)
	}
	resp, body2 := call(t, "GET", base+"/report?"+q.Encode(), bob, "", nil)
	mustStatus(t, resp, http.StatusOK, "отчёт повторно")
	if string(body2) != string(body) {
		t.Fatal("отчёт за прошедший период побайтно стабилен")
	}
	long := url.Values{"from": {from.Add(-100 * 24 * time.Hour).Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	resp, _ = call(t, "GET", base+"/report?"+long.Encode(), alice, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "отчёт длиннее 92 суток")
	rev := url.Values{"from": {to.Format(time.RFC3339)}, "to": {from.Format(time.RFC3339)}}
	resp, _ = call(t, "GET", base+"/report?"+rev.Encode(), alice, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "from не раньше to")
	resp, _ = call(t, "GET", base+"/report", eve, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "отчёт чужому")

	// Целевая дата: владелец меняет, участник получает 403, архивный — 409.
	epicURL := srv.URL + "/api/v1/epics/" + epic.ID
	resp, _ = call(t, "PATCH", epicURL, bob, "", map[string]any{"due_at": "2026-12-01T09:00:00Z"})
	mustStatus(t, resp, http.StatusForbidden, "целевая дата участником")
	resp, body = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": "2026-12-01T09:00:00Z"})
	mustStatus(t, resp, http.StatusOK, "целевая дата владельцем")
	var got domain.Epic
	_ = json.Unmarshal(body, &got)
	if got.DueAt == nil || !got.DueAt.Equal(time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("целевая дата: %s", body)
	}
	resp, _ = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": "завтра"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "неверная дата")
	resp, _ = call(t, "PATCH", epicURL, alice, "", map[string]any{})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "пустое тело")
	resp, body = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": nil, "token_budget": 5000})
	mustStatus(t, resp, http.StatusOK, "снятие даты вместе с бюджетом")
	got = domain.Epic{}
	_ = json.Unmarshal(body, &got)
	if got.DueAt != nil || got.TokenBudget == nil || *got.TokenBudget != 5000 {
		t.Fatalf("после правки: %s", body)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE epics SET status='archived' WHERE id=$1`, epic.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": "2026-12-01T09:00:00Z"})
	mustStatus(t, resp, http.StatusConflict, "целевая дата архивного Epic'а")

	// Плановый бюджет месяца: 0 даёт 422 с полем, значение больше 0
	// сохраняется и видно в действующей политике.
	resp, body = call(t, "PUT", base+"/policy", alice, "", map[string]any{"monthly_budget_usd": 0})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "бюджет 0")
	if !strings.Contains(string(body), "monthly_budget_usd") {
		t.Fatalf("поле ошибки бюджета: %s", body)
	}
	resp, body = call(t, "PUT", base+"/policy", alice, "", map[string]any{"monthly_budget_usd": 120})
	mustStatus(t, resp, http.StatusOK, "бюджет 120")
	if !strings.Contains(string(body), `"monthly_budget_usd":120`) {
		t.Fatalf("политика: %s", body)
	}
}

// PATCH Epic'а атомарен (ревью круг 1, P2-3): неверный бюджет рядом с
// верной датой даёт 422, и ни дата, ни событие не сохраняются. Архивный
// Epic не получает ни дату, ни бюджет из того же запроса.
func TestPatchEpicAtomic(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "P", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)
	epicURL := srv.URL + "/api/v1/epics/" + epic.ID
	unchanged := func(what string) {
		t.Helper()
		got, err := st.GetEpic(ctx, epic.ID)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		_ = st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE epic_id=$1
			AND (type = 'epic.due_changed' OR (type = 'epic.status' AND text = 'бюджет Epic изменён'))`, epic.ID).Scan(&n)
		if got.DueAt != nil || got.TokenBudget != nil || n != 0 {
			t.Fatalf("%s: due_at=%v token_budget=%v событий правки %d", what, got.DueAt, got.TokenBudget, n)
		}
	}
	for _, bad := range []any{0, -5, "много"} {
		resp, body = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": "2026-12-01T09:00:00Z", "token_budget": bad})
		mustStatus(t, resp, http.StatusUnprocessableEntity, "неверный бюджет")
		if !strings.Contains(string(body), `"field":"token_budget"`) {
			t.Fatalf("поле ошибки: %s", body)
		}
		unchanged("после 422")
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE epics SET status='archived' WHERE id=$1`, epic.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PATCH", epicURL, alice, "", map[string]any{"due_at": "2026-12-01T09:00:00Z", "token_budget": 5000})
	mustStatus(t, resp, http.StatusConflict, "архивный Epic")
	unchanged("после 409")
}

// Отчёт по умолчанию строится до момента запроса без округления вниз до
// минуты (fix-console-rivetd-tails): задача, завершённая за пару секунд до
// запроса, считается сделанной, как и в обзоре.
func TestReportDefaultIncludesLastMinute(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", "", "pw-alice-secret", false); err != nil {
		t.Fatal(err)
	}
	alice := loginSession(t, srv, "alice", "pw-alice-secret")
	resp, body := call(t, "POST", srv.URL+"/api/v1/projects", alice, "", map[string]string{"name": "Минута", "repo": "o/r"})
	mustStatus(t, resp, http.StatusCreated, "проект")
	var project struct{ ID string }
	_ = json.Unmarshal(body, &project)
	resp, body = call(t, "POST", srv.URL+"/api/v1/projects/"+project.ID+"/epics", alice, "", map[string]string{"title": "E"})
	mustStatus(t, resp, http.StatusCreated, "epic")
	var epic domain.Epic
	_ = json.Unmarshal(body, &epic)
	resp, body = call(t, "POST", srv.URL+"/api/v1/epics/"+epic.ID+"/tasks", alice, "", map[string]any{"title": "T", "criteria": []string{"c"}})
	mustStatus(t, resp, http.StatusCreated, "задача")
	var task domain.Task
	_ = json.Unmarshal(body, &task)
	// Статус отдельно: триггер ставит done_at на переходе в done. Время
	// завершения — по часам процесса теста, как и момент запроса в API.
	done := time.Now().Add(-2 * time.Second)
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET started_at=$2, done_at=$3 WHERE id=$1`, task.ID, done.Add(-time.Hour), done); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "GET", srv.URL+"/api/v1/projects/"+project.ID+"/report?tz=UTC", alice, "", nil)
	mustStatus(t, resp, http.StatusOK, "отчёт по умолчанию")
	if !strings.Contains(string(body), "- Завершено задач: 1 ") {
		t.Fatalf("задача, завершённая в последнюю минуту, не попала в отчёт:\n%s", body)
	}
	if strings.Contains(string(body), "период ещё идёт") {
		t.Fatalf("отчёт по умолчанию не должен говорить, что период идёт:\n%s", body)
	}
}

func TestReportRange(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 34, 27, 500_000_000, time.UTC)
	from, to, live, field, _ := reportRange(url.Values{}, now)
	if field != "" || !to.Equal(now) || !from.Equal(now.Add(-7*24*time.Hour)) || !live {
		t.Fatalf("по умолчанию: from=%v to=%v live=%v field=%q", from, to, live, field)
	}
	past := now.Add(-48 * time.Hour).Truncate(time.Hour)
	from, to, live, _, _ = reportRange(url.Values{"to": {past.Format(time.RFC3339)}}, now)
	if !to.Equal(past) || !from.Equal(past.Add(-7*24*time.Hour)) || live {
		t.Fatalf("прошедший to: from=%v to=%v live=%v", from, to, live)
	}
	future := now.Add(5 * 24 * time.Hour)
	if _, _, live, _, _ = reportRange(url.Values{"to": {future.Format(time.RFC3339)}}, now); !live {
		t.Fatal("to в будущем — отчёт текущий")
	}
	start := now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
	if from, to, _, _, _ = reportRange(url.Values{"from": {start.Format(time.RFC3339)}}, now); !from.Equal(start) || !to.Equal(now) {
		t.Fatalf("свой from: from=%v to=%v", from, to)
	}
	for q, want := range map[string]string{"to": "to", "from": "from"} {
		if _, _, _, field, _ := reportRange(url.Values{q: {"вчера"}}, now); field != want {
			t.Fatalf("%s=вчера: поле ошибки %q", q, field)
		}
	}
}
