package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Шаблоны ролей и команда (add-agent-roles, api-contract): чтение всем,
// правка администратору, POST создаёт, PUT меняет, 409 с кодами и
// ссылками по шагам, оптимистическая проверка версии.
func TestRolesAndTeamAPI(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()

	resp, body := call(t, "GET", f.srv.URL+"/api/v1/roles", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "шаблоны участнику")
	var roles struct {
		Roles []struct {
			ID           string `json:"id"`
			Agents       int    `json:"agents"`
			ControlPlane bool   `json:"control_plane"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(body, &roles); err != nil || len(roles.Roles) != 8 || roles.Roles[0].ID != "analyst" || !roles.Roles[0].ControlPlane {
		t.Fatalf("шаблоны: %s", body)
	}
	if roles.Roles[3].ID != "developer" || roles.Roles[3].Agents != 2 {
		t.Fatalf("два разработчика в команде по умолчанию: %+v", roles.Roles[3])
	}

	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/roles", f.user, "", map[string]any{"id": "qa"})
	mustStatus(t, resp, http.StatusForbidden, "шаблон не-админом")
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/roles", f.admin, "", map[string]any{
		"id": "tester", "title": "QA", "instruction": "x", "color": "teal", "capability": "coding"})
	mustStatus(t, resp, http.StatusConflict, "занятый id шаблона")
	if !jsonContains(body, `"code":"role_exists"`) {
		t.Fatalf("код занятого id: %s", body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/roles/ghost", f.admin, "", map[string]any{
		"title": "x", "instruction": "x", "color": "teal", "capability": "coding"})
	mustStatus(t, resp, http.StatusNotFound, "изменение несуществующего шаблона")
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/roles", f.admin, "", map[string]any{
		"id": "security", "title": "Безопасник", "instruction": "Ищи уязвимости.", "color": "pink", "capability": "review"})
	mustStatus(t, resp, http.StatusUnprocessableEntity, "цвет вне палитры")
	if !jsonContains(body, `"field":"color"`) {
		t.Fatalf("поле ошибки: %s", body)
	}
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/roles", f.admin, "", map[string]any{
		"id": "security", "title": "Безопасник", "instruction": "Ищи уязвимости.", "color": "slate", "capability": "review"})
	mustStatus(t, resp, http.StatusCreated, "собственный шаблон")

	// Процесс проекта ссылается на шаблон: отказ удаления со ссылкой на шаг.
	p, err := f.st.CreateProject(ctx, "Бюджет", "o/r", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	proc := policy.DefaultProcess()
	proc.Steps[2].Participants = []policy.Participant{{Agent: &policy.AgentRef{Role: "security"}}}
	if _, err := f.st.SaveProjectPolicy(ctx, p.ID, policy.Overrides{Process: &proc}, "root"); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/roles/security", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "шаблон в процессе")
	if !jsonContains(body, `"code":"role_in_use"`) || !jsonContains(body, `"step":"review"`) || !jsonContains(body, `"project_name":"Бюджет"`) {
		t.Fatalf("ссылки по шагам: %s", body)
	}
	resp, body = call(t, "DELETE", f.srv.URL+"/api/v1/system/roles/tester", f.admin, "", nil)
	mustStatus(t, resp, http.StatusConflict, "встроенный шаблон")
	if !jsonContains(body, `"code":"role_builtin"`) {
		t.Fatalf("код встроенного: %s", body)
	}

	// Оптимистическая проверка: устаревшая версия — 409 stale с актуальной.
	resp, body = call(t, "PUT", f.srv.URL+"/api/v1/system/roles/reviewer", f.admin, "", map[string]any{
		"title": "Ревьюер", "instruction": "Будь строг.", "color": "rose", "capability": "review",
		"updated_at": "2020-01-01T00:00:00Z"})
	mustStatus(t, resp, http.StatusConflict, "устаревшая версия шаблона")
	if !jsonContains(body, `"code":"stale"`) || !jsonContains(body, `"current"`) {
		t.Fatalf("stale с актуальной версией: %s", body)
	}

	// Команда: читают все, агент из шаблона создаёт администратор.
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/team", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "команда участнику")
	if !jsonContains(body, `"name":"Тимур"`) || !jsonContains(body, `"role_title":"Тимлид"`) {
		t.Fatalf("команда: %s", body)
	}
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/team", f.user, "", map[string]any{"id": "yan", "name": "Ян", "role_id": "developer"})
	mustStatus(t, resp, http.StatusForbidden, "агент не-админом")
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/system/team", f.admin, "", map[string]any{"id": "yan", "name": "Ян", "role_id": "developer"})
	mustStatus(t, resp, http.StatusCreated, "новый агент")
	resp, body = call(t, "POST", f.srv.URL+"/api/v1/system/team", f.admin, "", map[string]any{"id": "yan", "name": "Ян", "role_id": "developer"})
	mustStatus(t, resp, http.StatusConflict, "занятый id агента")
	if !jsonContains(body, `"code":"agent_exists"`) {
		t.Fatalf("код занятого id агента: %s", body)
	}
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/team/yan", f.admin, "", map[string]any{"name": "Ян", "role_id": "developer", "instruction_extra": "Пиши короче."})
	mustStatus(t, resp, http.StatusOK, "правка агента")
	resp, _ = call(t, "DELETE", f.srv.URL+"/api/v1/system/team/yan", f.admin, "", nil)
	mustStatus(t, resp, http.StatusNoContent, "удаление агента")
	resp, _ = call(t, "PUT", f.srv.URL+"/api/v1/system/team/yan", f.admin, "", map[string]any{"name": "Ян", "role_id": "developer"})
	mustStatus(t, resp, http.StatusNotFound, "правка удалённого")

	resp, body = call(t, "GET", f.srv.URL+"/api/v1/models/choices", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "выбор моделей")
	if !jsonContains(body, `"choices"`) {
		t.Fatalf("выбор моделей: %s", body)
	}

	// Имя runner'а: администратор, длинное имя — ошибка поля.
	if err := f.st.UpsertRunner(ctx, domainRunner("claude-sonnet-1")); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "PATCH", f.srv.URL+"/api/v1/system/runners/claude-sonnet-1", f.user, "", map[string]any{"display_name": "Макс"})
	mustStatus(t, resp, http.StatusForbidden, "имя runner'а не-админом")
	resp, body = call(t, "PATCH", f.srv.URL+"/api/v1/system/runners/claude-sonnet-1", f.admin, "", map[string]any{"display_name": "Сервер 1"})
	mustStatus(t, resp, http.StatusOK, "имя runner'а")
	if !jsonContains(body, `"DisplayName":"Сервер 1"`) || !jsonContains(body, `"DisplayNameLocked":true`) {
		t.Fatalf("runner с именем: %s", body)
	}
	resp, body = call(t, "GET", f.srv.URL+"/api/v1/runners", f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "runner'ы")
	if !jsonContains(body, `"Roles":["developer","tester"]`) {
		t.Fatalf("роли runner'а по capability: %s", body)
	}
}

// domainRunner — runner-исполнитель с capability coding.
func domainRunner(id string) domain.Runner {
	return domain.Runner{ID: id, Agent: "claude-code", Capabilities: []string{"coding"}}
}

// completerFunc — провайдер планировщика из функции.
type completerFunc func(ctx context.Context, prompt string) (string, error)

func (f completerFunc) Complete(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// Тимлид составляет план: событие старта, занятость в команде на время
// декомпозиции, событие провала (add-agent-roles, живой API консоли).
func TestDecomposingLeadSignal(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	p, err := f.st.CreateProject(ctx, "Бюджет", "o/r", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := f.st.CreateEpic(ctx, p.ID, "Категории", "цель")
	if err != nil {
		t.Fatal(err)
	}
	var during []byte
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(ctx context.Context, prompt string) (string, error) {
		_, during = call(t, "GET", f.srv.URL+"/api/v1/team", f.user, "", nil)
		if !strings.Contains(prompt, "# Роль Тимлида") {
			t.Errorf("инструкция Тимлида в задании: %s", prompt)
		}
		return `[{"title":"A","description":"d","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	resp, body := call(t, "POST", f.srv.URL+"/api/v1/epics/"+epic.ID+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusCreated, "декомпозиция")
	_ = body
	var team struct {
		Agents []struct {
			ID     string `json:"id"`
			Busy   bool   `json:"busy"`
			Step   string `json:"step"`
			EpicID string `json:"epic_id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(during, &team); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range team.Agents {
		if a.ID == "timur" {
			found = a.Busy && a.Step == "decompose" && a.EpicID == epic.ID
		}
	}
	if !found {
		t.Fatalf("Тимлид составляет план: %s", during)
	}
	_, after := call(t, "GET", f.srv.URL+"/api/v1/team", f.user, "", nil)
	if jsonContains(after, `"step":"decompose"`) {
		t.Fatalf("после декомпозиции признак снят: %s", after)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: epic.ID})
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
		if e.Type == "epic.decomposing" {
			a, _ := e.Payload["agent"].(map[string]any)
			if a["name"] != "Тимур" || e.Payload["epic_id"] != epic.ID || !strings.Contains(e.Text, "Тимлид Тимур составляет план") {
				t.Fatalf("событие старта: %q %+v", e.Text, e.Payload)
			}
		}
	}
	if strings.Join(types, ",") != "epic.decomposing,epic.decomposed" {
		t.Fatalf("события декомпозиции: %v", types)
	}
	// Занятость Тимлида видна командам всех проектов: события установки
	// без проекта, в payload только id и busy.
	leadWork := func() string {
		var out []string
		for _, typ := range []string{store.EventAgentBusy, store.EventAgentFree} {
			evs, _ := f.st.Events(ctx, store.EventFilter{Installation: true, Type: typ})
			for _, e := range evs {
				if e.ProjectID != "" || e.EpicID != "" || len(e.Payload) != 2 || e.Payload["id"] != "timur" {
					t.Fatalf("событие занятости Тимлида: %+v", e)
				}
				out = append(out, typ)
			}
		}
		return strings.Join(out, ",")
	}
	if got := leadWork(); got != "team.agent_busy,team.agent_free" {
		t.Fatalf("занятость Тимлида: %s", got)
	}

	// Провал: epic.plan_failed с агентом и причиной.
	epic2, _ := f.st.CreateEpic(ctx, p.ID, "Второй", "цель")
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(context.Context, string) (string, error) {
		return "", errors.New("модель недоступна")
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	resp, _ = call(t, "POST", f.srv.URL+"/api/v1/epics/"+epic2.ID+"/decompose", f.user, "", nil)
	mustStatus(t, resp, http.StatusUnprocessableEntity, "провал декомпозиции")
	evs, _ = f.st.Events(ctx, store.EventFilter{EpicID: epic2.ID, Type: "epic.plan_failed"})
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "модель недоступна") {
		t.Fatalf("событие провала: %+v", evs)
	}
	if got := leadWork(); got != "team.agent_busy,team.agent_busy,team.agent_free,team.agent_free" {
		t.Fatalf("провал декомпозиции освобождает Тимлида: %s", got)
	}
}

// Запуск шага в деталке задачи несёт агента команды.
func TestStepRunViewCarriesAgent(t *testing.T) {
	v := runView(store.StepRun{ID: 1, Participant: "p1", Role: "tester", AgentID: "lev", AgentName: "Лев", Model: "m"})
	if v.AgentID != "lev" || v.AgentName != "Лев" || v.Role != "tester" || v.Model != "m" {
		t.Fatalf("запуск шага: %+v", v)
	}
}

// Две декомпозиции одного Тимлида: окончание первой не снимает признак
// второй, повторная по тому же Epic считается отдельно (круг 2, P3-5).
// Сигнал занятости один на серию: занят с первой, свободен после последней.
func TestParallelDecompositionsOfLead(t *testing.T) {
	s := &Server{}
	lead := &domain.TeamAgent{ID: "timur"}
	var signals []bool
	signal := func(busy bool) { signals = append(signals, busy) }
	doneA := s.startDecomposition(lead, domain.Epic{ID: "a", ProjectID: "p"}, signal)
	doneB := s.startDecomposition(lead, domain.Epic{ID: "b", ProjectID: "p"}, signal)
	if d, ok := s.decompositionOf("timur"); !ok || d.EpicID != "b" {
		t.Fatalf("показывается последняя начатая: %+v %v", d, ok)
	}
	doneB2 := s.startDecomposition(lead, domain.Epic{ID: "b", ProjectID: "p"}, signal)
	doneB()
	if d, ok := s.decompositionOf("timur"); !ok || d.EpicID != "b" {
		t.Fatalf("повторная декомпозиция Epic b ещё идёт: %+v %v", d, ok)
	}
	doneB2()
	if d, ok := s.decompositionOf("timur"); !ok || d.EpicID != "a" {
		t.Fatalf("окончание b не снимает a: %+v %v", d, ok)
	}
	if fmt.Sprint(signals) != "[true]" {
		t.Fatalf("пока идёт декомпозиция a, сигнал только о занятости: %v", signals)
	}
	doneA()
	if _, ok := s.decompositionOf("timur"); ok {
		t.Fatal("все декомпозиции закончены")
	}
	if fmt.Sprint(signals) != "[true false]" {
		t.Fatalf("сигналы занятости: %v", signals)
	}
	// Новая серия снова начинается с сигнала занятости.
	s.startDecomposition(lead, domain.Epic{ID: "c", ProjectID: "p"}, signal)()
	if fmt.Sprint(signals) != "[true false true false]" {
		t.Fatalf("сигналы второй серии: %v", signals)
	}
}

// Обрыв запроса клиентом не теряет epic.plan_failed (круг 2, P3-5).
func TestPlanFailedSurvivesClientCancel(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	p, err := f.st.CreateProject(ctx, "Бюджет", "o/r", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := f.st.CreateEpic(ctx, p.ID, "Категории", "цель")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	reqCtx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(reqCtx, "POST", f.srv.URL+"/api/v1/epics/"+epic.ID+"/decompose", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.user})
	go func() {
		<-started
		cancel()
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: epic.ID, Type: "epic.plan_failed"})
		if len(evs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("epic.plan_failed после обрыва запроса")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Текст 409 in_use называет объект правки и виды ссылок (круг 2, P3-6).
func TestInUseMessage(t *testing.T) {
	refs := []store.Ref{{Kind: "team_agent", ID: "maks"}, {Kind: "team_agent", ID: "eva"}, {Kind: "project", ID: "p"}}
	if got := inUseMessage("профиль агента", refs); got != "профиль агента используется (агенты команды, процессы и политики проектов): сначала уберите эти ссылки" {
		t.Fatalf("профиль: %q", got)
	}
	if got := inUseMessage("подключение", []store.Ref{{Kind: "planner"}}); got != "подключение используется (модель декомпозиции): сначала уберите эти ссылки" {
		t.Fatalf("подключение: %q", got)
	}
}

// Отказ смены профиля шаблона объясняет, что делать с агентами команды
// (круг 3, P3-5).
func TestRoleInUseProfileMessage(t *testing.T) {
	for reason, want := range map[string]string{
		"":                     "шаблон используется",
		store.RoleInUseProfile: "переведите этих агентов на модель, которая есть у нового профиля",
	} {
		rec := httptest.NewRecorder()
		writeRoleErr(rec, &store.ErrRoleInUse{Reason: reason, Refs: []store.RoleRef{{Kind: "agent", AgentID: "maks"}}}, nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), `"role_in_use"`) {
			t.Fatalf("причина %q: %d %s", reason, rec.Code, rec.Body.String())
		}
	}
}
