package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

var fakeLineRe = regexp.MustCompile(`^(\d+)\| \*\*Р:\*\* (.*)$`)

// fakeAnalystLLM — OpenAI-совместимый сервер с ответами Аналитика по
// содержимому задания: находка на реплику респондента, потребность на
// находку, Epic'и e1 и e2 (e2 зависит от e1). gate держит первый запрос.
func fakeAnalystLLM(t *testing.T, gate chan struct{}) *httptest.Server {
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gate != nil {
			once.Do(func() { <-gate })
		}
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		prompt := in.Messages[0].Content
		var reply string
		switch first, _, _ := strings.Cut(prompt, "\n"); first {
		case discovery.MarkerSummary:
			var findings []map[string]any
			for _, line := range strings.Split(prompt, "\n") {
				if m := fakeLineRe.FindStringSubmatch(line); m != nil {
					n, _ := strconv.Atoi(m[1])
					findings = append(findings, map[string]any{"kind": "pain", "text": "боль: " + m[2], "importance": "high",
						"quotes": []map[string]any{{"text": m[2], "line": n}}})
				}
			}
			b, _ := json.Marshal(map[string]any{"respondent": map[string]string{"role": "менеджер", "summary": "s"}, "findings": findings})
			reply = string(b)
		case discovery.MarkerSynthesis:
			var syn struct {
				Interviews []struct {
					ID       string              `json:"id"`
					Findings []discovery.Finding `json:"findings"`
				} `json:"interviews"`
			}
			_, after, _ := strings.Cut(prompt, "# Конспекты интервью")
			_, block, _ := strings.Cut(after, "```json\n")
			body, _, _ := strings.Cut(block, "\n```")
			_ = json.Unmarshal([]byte(body), &syn)
			var personas, needs []map[string]any
			var ids []string
			for i, iv := range syn.Interviews {
				pid := fmt.Sprintf("p%d", i+1)
				personas = append(personas, map[string]any{"id": pid, "name": "Персона", "description": "d", "interviews": []string{iv.ID}})
				for _, f := range iv.Findings {
					id := fmt.Sprintf("n%d", len(needs)+1)
					ids = append(ids, id)
					needs = append(needs, map[string]any{"id": id, "title": f.Text, "kind": "pain", "persona": pid, "importance": "high",
						"evidence": []map[string]any{{"interview": iv.ID, "quote": f.Quotes[0].Text}}})
				}
			}
			b, _ := json.Marshal(map[string]any{"personas": personas, "needs": needs, "epics": []map[string]any{
				{"key": "e1", "title": "Сделки и воронка", "goal": "Менеджер видит этап каждой сделки и не теряет клиентов в воронке.",
					"needs": ids[:1], "priority": "must", "size": "M", "depends_on": []string{}, "existing_epic": nil},
				{"key": "e2", "title": "Дашборд продаж", "goal": "Руководитель видит выручку и конверсию воронки за период на одном экране.",
					"needs": ids[1:], "priority": "should", "size": "S", "depends_on": []string{"e1"}, "existing_epic": nil},
			}})
			reply = string(b)
		default:
			t.Errorf("неизвестное задание: %.80s", prompt)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 100},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type discFixture struct {
	opsFixture
	project string
	member  string
}

func seedDiscovery(t *testing.T, llmURL string) discFixture {
	t.Helper()
	f := seedOps(t, false)
	ctx := context.Background()
	p, err := f.st.CreateProject(ctx, "CRM", "o/crm", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	login := fmt.Sprintf("member-%d", time.Now().UnixNano())
	if _, err := f.st.CreateUser(ctx, login, "", "pw-testpass", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddMember(ctx, p.ID, login, "member"); err != nil {
		t.Fatal(err)
	}
	if llmURL != "" {
		if _, err := f.st.Pool.Exec(ctx, `INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by)
			VALUES ('fake', 'Fake', 'local', 'openai', $1,
			'[{"id":"fake-model","label":"Fake","source":"manual","input_price":3000000,"output_price":15000000}]', $2)`, llmURL, f.uid.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='fake', model='fake-model' WHERE id='vera'`); err != nil {
			t.Fatal(err)
		}
	}
	return discFixture{opsFixture: f, project: p.ID, member: loginSession(t, f.srv, login, "pw-testpass")}
}

func interviewText(name string) string {
	return "**И:** Как вы ведёте сделки?\n**Р:** Сделки клиента " + name + " веду в таблице, этапы путаются.\n" +
		"**И:** Что ещё мешает?\n**Р:** Забываю перезвонить клиенту " + name + " после встречи."
}

func multipartBody(t *testing.T, fields map[string]string, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func postMultipart(t *testing.T, url, session string, body *bytes.Buffer, ct string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp, out.Bytes()
}

type apiRun struct {
	ID         string `json:"id"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	Outdated   bool   `json:"outdated"`
	Error      string `json:"error"`
	BacklogRev int    `json:"backlog_rev"`
	Agent      *struct {
		Name string `json:"name"`
	} `json:"agent"`
	Needs   []discovery.Need `json:"needs"`
	Backlog []struct {
		discovery.BacklogEpic
		EpicID    *string               `json:"epic_id"`
		Decompose *store.DecomposeState `json:"decompose"`
	} `json:"backlog"`
	Usage map[string]any `json:"usage"`
}

func (f discFixture) waitRun(t *testing.T, rid string, done func(apiRun) bool) apiRun {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, body := call(t, "GET", f.srv.URL+"/api/v1/projects/"+f.project+"/discovery/analyses/"+rid, f.member, "", nil)
		mustStatus(t, resp, http.StatusOK, "версия исследования")
		var run apiRun
		if err := json.Unmarshal(body, &run); err != nil {
			t.Fatal(err)
		}
		if done(run) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("версия не дошла до нужного состояния: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func errCode(body []byte) (code, field string, raw map[string]any) {
	var out struct {
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	code, _ = out.Error["code"].(string)
	field, _ = out.Error["field"].(string)
	return code, field, out.Error
}

// Сквозной сценарий API исследования: интервью, анализ, бэклог,
// утверждение с декомпозицией, зависимости, права и ошибки по контракту.
func TestDiscoveryAPI(t *testing.T) {
	gate := make(chan struct{})
	llmSrv := fakeAnalystLLM(t, gate)
	f := seedDiscovery(t, llmSrv.URL)
	ctx := context.Background()
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"

	// Интервью: права, JSON, файл, ошибки.
	resp, _ := call(t, "POST", base+"/interviews", f.member, "", map[string]any{"title": "x", "text": "y"})
	mustStatus(t, resp, http.StatusForbidden, "интервью участником")
	resp, body := call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "Анна", "respondent_role": "Менеджер", "held_at": "2026-10-01", "text": interviewText("Анна")})
	mustStatus(t, resp, http.StatusCreated, "интервью JSON")
	var anna store.DiscoveryInterview
	_ = json.Unmarshal(body, &anna)
	mb, ct := multipartBody(t, map[string]string{"respondent_role": "Руководитель"}, "boris.md", []byte(interviewText("Борис")))
	resp, body = postMultipart(t, base+"/interviews", f.user, mb, ct)
	mustStatus(t, resp, http.StatusCreated, "интервью файлом")
	if !jsonContains(body, `"title":"boris"`) || !jsonContains(body, `"lines":4`) {
		t.Fatalf("интервью из файла: %s", body)
	}
	if got := defaultInterviewTitle("\ufeff# Интервью 1. Менеджер\n\n**И:** ...", "01-manager.md"); got != "Интервью 1. Менеджер" {
		t.Fatalf("название из заголовка: %q", got)
	}
	mb, ct = multipartBody(t, map[string]string{"title": "bad"}, "bad.md", []byte{0xff, 0xfe, 'a'})
	resp, body = postMultipart(t, base+"/interviews", f.user, mb, ct)
	if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || field != "file" {
		t.Fatalf("не UTF-8: %d %s", resp.StatusCode, body)
	}
	resp, body = call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "Пусто", "text": "  \n "})
	if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || field != "text" {
		t.Fatalf("пустое интервью: %d %s", resp.StatusCode, body)
	}
	resp, body = call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "Длинное", "text": strings.Repeat("а", store.MaxInterviewChars+1)})
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusRequestEntityTooLarge || code != "interview_too_large" {
		t.Fatalf("длинное интервью: %d %s", resp.StatusCode, body)
	}

	// Анализ: участник не запускает, второй запуск — конфликт с run_id.
	resp, _ = call(t, "POST", base+"/analyses", f.member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "анализ участником")
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "запуск анализа")
	var started apiRun
	_ = json.Unmarshal(body, &started)
	if started.Status != store.RunRunning || started.Agent == nil || started.Agent.Name != "Вера" {
		t.Fatalf("запущенная версия: %s", body)
	}
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, e := errCode(body); resp.StatusCode != http.StatusConflict || code != "analysis_running" || e["run_id"] != started.ID {
		t.Fatalf("второй анализ: %d %s", resp.StatusCode, body)
	}
	close(gate)
	run := f.waitRun(t, started.ID, func(r apiRun) bool { return r.Status != store.RunRunning })
	if run.Status != store.RunReady || len(run.Needs) != 4 || len(run.Backlog) != 2 || run.BacklogRev != 1 {
		t.Fatalf("готовая версия: %+v", run)
	}
	if q := run.Needs[0].Evidence[0]; q.InterviewTitle == "" || q.Line == 0 || q.RespondentRole == "" {
		t.Fatalf("цитата без интервью и строки: %+v", q)
	}
	if run.Usage["tokens_in"] == nil || run.Usage["cost_usd"] == nil {
		t.Fatalf("usage версии: %+v", run.Usage)
	}
	var analystUsage int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_records WHERE project_id=$1 AND role='analyst' AND model='fake-model'`, f.project).Scan(&analystUsage)
	if analystUsage != 3 {
		t.Fatalf("usage Аналитика: %d записей", analystUsage)
	}
	// Текст интервью — участнику; состояние исследования — участнику.
	resp, body = call(t, "GET", base+"/interviews/"+anna.ID, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "текст интервью")
	if !jsonContains(body, `"text":"**И:**`) {
		t.Fatalf("текст интервью: %s", body)
	}
	resp, body = call(t, "GET", base, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "состояние исследования")
	if !jsonContains(body, `"current":{`) || !jsonContains(body, `"versions":[{`) {
		t.Fatalf("состояние: %s", body)
	}
	// Администратор без членства — 404.
	resp, _ = call(t, "GET", base, f.admin, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "исследование администратору без членства")

	// Бэклог: устаревшая ревизия, цикл, правка.
	epics := []discovery.BacklogEpic{run.Backlog[0].BacklogEpic, run.Backlog[1].BacklogEpic}
	resp, body = call(t, "PUT", base+"/analyses/"+run.ID+"/backlog", f.user, "", map[string]any{"backlog_rev": 7, "epics": epics})
	if code, _, e := errCode(body); resp.StatusCode != http.StatusConflict || code != "backlog_conflict" || e["backlog_rev"] != float64(1) {
		t.Fatalf("конфликт ревизии: %d %s", resp.StatusCode, body)
	}
	cyclic := append([]discovery.BacklogEpic{}, epics...)
	cyclic[0].DependsOn = []string{"e2"}
	resp, body = call(t, "PUT", base+"/analyses/"+run.ID+"/backlog", f.user, "", map[string]any{"backlog_rev": 1, "epics": cyclic})
	if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || !strings.Contains(field, "depends_on") {
		t.Fatalf("цикл в бэклоге: %d %s", resp.StatusCode, body)
	}
	epics[1].Priority = "must"
	resp, _ = call(t, "PUT", base+"/analyses/"+run.ID+"/backlog", f.member, "", map[string]any{"backlog_rev": 1, "epics": epics})
	mustStatus(t, resp, http.StatusForbidden, "правка бэклога участником")
	resp, body = call(t, "PUT", base+"/analyses/"+run.ID+"/backlog", f.user, "", map[string]any{"backlog_rev": 1, "epics": epics})
	mustStatus(t, resp, http.StatusOK, "правка бэклога")
	if !jsonContains(body, `"backlog_rev":2`) {
		t.Fatalf("ревизия после правки: %s", body)
	}

	// Утверждение с декомпозицией: Тимлид получает потребности.
	var plannerMu sync.Mutex
	var prompts []string
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(_ context.Context, prompt string) (string, error) {
		plannerMu.Lock()
		prompts = append(prompts, prompt)
		plannerMu.Unlock()
		return `[{"title":"A","description":"d","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	resp, body = call(t, "POST", base+"/analyses/"+run.ID+"/approve", f.user, "", map[string]any{"backlog_rev": 2, "decompose": true})
	mustStatus(t, resp, http.StatusOK, "утверждение")
	var approved struct {
		Epics []store.ApprovedEpic `json:"epics"`
	}
	_ = json.Unmarshal(body, &approved)
	if len(approved.Epics) != 2 || approved.Epics[0].Action != "created" {
		t.Fatalf("утверждение: %s", body)
	}
	resp, body = call(t, "POST", base+"/analyses/"+run.ID+"/approve", f.user, "", map[string]any{"backlog_rev": 2, "decompose": false})
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusConflict || code != "run_not_approvable" {
		t.Fatalf("повторное утверждение: %d %s", resp.StatusCode, body)
	}
	run = f.waitRun(t, run.ID, func(r apiRun) bool {
		for _, row := range r.Backlog {
			if row.Decompose == nil || row.Decompose.Status != "done" {
				return false
			}
		}
		return true
	})
	if run.Backlog[0].EpicID == nil {
		t.Fatalf("строка бэклога без Epic'а: %+v", run.Backlog[0])
	}
	plannerMu.Lock()
	if len(prompts) != 2 || !strings.Contains(prompts[0], "# Потребности пользователей") || !strings.Contains(prompts[0], "Сделки и воронка") {
		t.Fatalf("задания Тимлиду: %d", len(prompts))
	}
	plannerMu.Unlock()
	e1, e2 := *run.Backlog[0].EpicID, *run.Backlog[1].EpicID
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/epics/"+e2, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "Epic из исследования")
	if !jsonContains(body, `"Priority":"must"`) || !jsonContains(body, `"DependsOn":["`+e1+`"]`) ||
		!jsonContains(body, `"WaitingFor":[{"id":"`+e1+`"`) || !jsonContains(body, `"run_id":"`+run.ID+`"`) ||
		!jsonContains(body, `"interview_title"`) {
		t.Fatalf("DTO Epic: %s", body)
	}

	// Зависимости Epic'а: цикл, чужой проект, права, снятие.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/epics/"+e1+"/deps", f.user, "", map[string]any{"depends_on": []string{e2}})
	if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || field != "depends_on" || !strings.Contains(string(body), "цикл") {
		t.Fatalf("цикл зависимостей: %d %s", resp.StatusCode, body)
	}
	other, _ := f.st.CreateProject(ctx, "Другой", "o/other", nil, f.uid.ID)
	foreign, _ := f.st.CreateEpic(ctx, other.ID, "Чужой", "")
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/epics/"+e1+"/deps", f.user, "", map[string]any{"depends_on": []string{foreign.ID}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Epic другого проекта") {
		t.Fatalf("чужой проект: %d %s", resp.StatusCode, body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/epics/"+e2+"/deps", f.member, "", map[string]any{"depends_on": []string{}})
	mustStatus(t, resp, http.StatusForbidden, "зависимости участником")
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/epics/"+e2+"/deps", f.user, "", map[string]any{"depends_on": []string{}})
	mustStatus(t, resp, http.StatusOK, "снятие зависимостей")
	if !jsonContains(body, `"DependsOn":[]`) {
		t.Fatalf("зависимости сняты: %s", body)
	}
	if evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: e2, Type: "epic.deps_changed"}); len(evs) != 1 {
		t.Fatalf("событие изменения зависимостей: %d", len(evs))
	}

	// Удаление интервью: версия устарела, цитаты остаются.
	resp, _ = call(t, "DELETE", base+"/interviews/"+anna.ID, f.member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "удаление участником")
	resp, _ = call(t, "DELETE", base+"/interviews/"+anna.ID, f.user, "", nil)
	mustStatus(t, resp, http.StatusNoContent, "удаление интервью")
	run = f.waitRun(t, run.ID, func(apiRun) bool { return true })
	if !run.Outdated || len(run.Needs) != 4 {
		t.Fatalf("версия после удаления: outdated=%v needs=%d", run.Outdated, len(run.Needs))
	}
	resp, _ = call(t, "GET", base+"/interviews/"+anna.ID, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "удалённое интервью для цитат")

	evs, _ := f.st.Events(ctx, store.EventFilter{ProjectID: f.project, Limit: 500})
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Type] = true
	}
	for _, typ := range []string{"discovery.interview_added", "discovery.interview_deleted", "discovery.analysis_started",
		"discovery.analysis_progress", "discovery.analysis_ready", "discovery.backlog_edited", "discovery.approved",
		"epic.decomposing", "epic.decomposed", "epic.deps_changed"} {
		if !seen[typ] {
			t.Errorf("нет события %s", typ)
		}
	}
}

// Ошибки запуска анализа: нет интервью, шаблон отключён, модель не настроена.
func TestDiscoveryStartErrors(t *testing.T) {
	f := seedDiscovery(t, "")
	ctx := context.Background()
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	resp, body := call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "no_interviews" {
		t.Fatalf("без интервью: %d %s", resp.StatusCode, body)
	}
	resp, _ = call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "Анна", "text": interviewText("Анна")})
	mustStatus(t, resp, http.StatusCreated, "интервью")
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusPreconditionFailed || code != "model_not_configured" {
		t.Fatalf("без модели: %d %s", resp.StatusCode, body)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='gone', model='m' WHERE id='vera'`); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusPreconditionFailed || code != "model_invalid" {
		t.Fatalf("удалённое подключение: %d %s", resp.StatusCode, body)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=false WHERE id='analyst'`); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, _ := errCode(body); resp.StatusCode != http.StatusConflict || code != "analyst_disabled" {
		t.Fatalf("шаблон отключён: %d %s", resp.StatusCode, body)
	}
	// Исчерпанный дневной бюджет проекта: анализ не запускается.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agent_roles SET enabled=true WHERE id='analyst'`); err != nil {
		t.Fatal(err)
	}
	budget := int64(100)
	if _, err := f.st.SaveProjectPolicy(ctx, f.project, policy.Overrides{DailyTokenBudget: &budget}, "owner"); err != nil {
		t.Fatal(err)
	}
	in, out := int64(90), int64(20)
	if err := f.st.RecordUsage(ctx, store.UsageInput{ProjectID: f.project, Model: "m", Role: "analyst", TokensIn: &in, TokensOut: &out}); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	if code, _, e := errCode(body); resp.StatusCode != http.StatusConflict || code != "budget_exceeded" || e["paused_until"] == nil {
		t.Fatalf("исчерпанный бюджет: %d %s", resp.StatusCode, body)
	}

	// Нулевой символ в тексте — ошибка поля, а не 500.
	resp, body = call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "NUL", "text": "текст\x00интервью"})
	if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || field != "text" {
		t.Fatalf("нулевой символ: %d %s", resp.StatusCode, body)
	}

	// Новый Epic отдаётся в той же форме DTO, что и чтение.
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/projects/"+f.project+"/epics", f.user, "", map[string]any{"title": "Новый", "goal": "g"})
	mustStatus(t, resp, http.StatusCreated, "новый Epic")
	if !jsonContains(body, `"DependsOn":[]`) || !jsonContains(body, `"WaitingFor":[]`) {
		t.Fatalf("DTO нового Epic'а: %s", body)
	}

	// Прерванный перезапуском анализ становится failed с событием, в
	// событии агент и модель версии.
	run, err := f.st.CreateRun(ctx, f.project, store.NewRun{Interviews: []store.RunInterview{{ID: "x", Title: "x"}},
		Model: "m", AgentID: "vera", AgentName: "Вера", RoleTitle: "Аналитик"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.api.RecoverDiscovery(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.GetRun(ctx, f.project, run.ID)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "перезапуском") {
		t.Fatalf("прерванный анализ: %s %q", got.Status, got.Error)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{ProjectID: f.project, Type: "discovery.analysis_failed"})
	if len(evs) != 1 || evs[0].Payload["model"] != "m" {
		t.Fatalf("событие прерванного анализа: %+v", evs)
	}
	if a, _ := evs[0].Payload["agent"].(map[string]any); a["name"] != "Вера" {
		t.Fatalf("агент в событии прерванного анализа: %+v", evs[0].Payload)
	}
}
