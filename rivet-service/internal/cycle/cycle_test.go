package cycle

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var (
	t0      = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	process = []Step{{"code", "code"}, {"test", "test"}, {"review", "review"}, {"migrations", "prompt"}, {"merge", "merge"}, {"deploy", "deploy"}}
)

func ts(d time.Duration) *time.Time { v := t0.Add(d); return &v }

func TestPhasesOrder(t *testing.T) {
	want := []string{"discovery", "architecture", "planning", "development", "testing", "review", "release", "operations"}
	got := Phases()
	if len(got) != 8 {
		t.Fatalf("фаз %d", len(got))
	}
	for i, p := range got {
		if string(p.ID) != want[i] || p.Title == "" || p.Role == "" {
			t.Fatalf("фаза %d: %+v", i, p)
		}
	}
	if Phases()[0].Role != "analyst" || Phases()[7].Role != "sre" {
		t.Fatal("шаблоны ролей фаз")
	}
}

// Правила спеки «Отнесение к фазе» для задач.
func TestPhaseOfTask(t *testing.T) {
	cut := ReleaseCut{AutoEnv: true, LastCreated: ts(0)}
	cases := []struct {
		name string
		task Task
		cut  ReleaseCut
		want Placement
	}{
		{"шаг code", Task{Status: "running", StepID: "code", Process: process, EpicRunning: true}, cut,
			Placement{Development, StateActive, ""}},
		{"исправление", Task{Status: "fixing", StepID: "code", Process: process}, cut,
			Placement{Development, StateActive, ""}},
		{"шаг test", Task{Status: "testing", StepID: "test", Process: process}, cut,
			Placement{Testing, StateActive, ""}},
		{"шаг review", Task{Status: "review", StepID: "review", Process: process}, cut,
			Placement{Review, StateActive, ""}},
		{"prompt после review", Task{Status: "review", StepID: "migrations", Process: process}, cut,
			Placement{Review, StateActive, ""}},
		{"prompt первым шагом", Task{Status: "running", StepID: "p", Process: []Step{{"p", "prompt"}, {"code", "code"}}}, cut,
			Placement{Development, StateActive, ""}},
		{"ожидание merge", Task{Status: "review", StepID: "merge", Process: process, AwaitingMerge: true}, cut,
			Placement{Review, StateWaiting, ""}},
		{"шаг merge без флага", Task{Status: "review", StepID: "merge", Process: process}, cut,
			Placement{Review, StateActive, ""}},
		{"эскалированная задача на code", Task{Status: "running", StepID: "code", Process: process, NeedsHuman: true}, cut,
			Placement{Development, StateNeedsHuman, ""}},
		{"заблокированная", Task{Status: "blocked", StepID: "test", Process: process}, cut,
			Placement{Testing, StateNeedsHuman, ""}},
		{"queued в запущенном Epic", Task{Status: "queued", EpicRunning: true}, cut,
			Placement{Development, StateWaiting, ""}},
		{"ready в не запущенном Epic", Task{Status: "ready"}, cut, Placement{}},
		{"шаг не найден, запасной по статусу", Task{Status: "testing", StepID: "old"}, cut,
			Placement{Testing, StateActive, ""}},
		{"задача-баг", Task{Kind: KindBug, Status: "running", StepID: "code", Process: process}, cut,
			Placement{Development, StateActive, BadgeBug}},
		{"задача вида architecture", Task{Kind: KindArchitecture, Status: "review", StepID: "review", Process: process}, cut,
			Placement{Architecture, StateActive, ""}},
		// Задача архитектуры входит в состав релиза, значит и ждёт его.
		{"архитектура ожидает релиза", Task{Kind: KindArchitecture, Status: "done", MergedAt: ts(time.Minute)}, cut,
			Placement{Release, StateWaiting, BadgeAwaitingRelease}},
		{"ожидает релиза", Task{Status: "done", MergedAt: ts(time.Minute)}, cut,
			Placement{Release, StateWaiting, BadgeAwaitingRelease}},
		{"баг ожидает релиза", Task{Kind: KindBug, Status: "done", MergedAt: ts(time.Minute)}, cut,
			Placement{Release, StateWaiting, BadgeAwaitingRelease}},
		{"вошла в публикацию на границе", Task{Status: "done", MergedAt: ts(0)}, cut, Placement{}},
		{"вошла в публикацию", Task{Status: "done", MergedAt: ts(-time.Minute)}, cut, Placement{}},
		{"нет окружения с автозапуском", Task{Status: "done", MergedAt: ts(time.Minute)}, ReleaseCut{}, Placement{}},
		{"публикаций ещё не было", Task{Status: "done", MergedAt: ts(time.Minute)}, ReleaseCut{AutoEnv: true},
			Placement{Release, StateWaiting, BadgeAwaitingRelease}},
		{"отменена", Task{Status: "cancelled"}, cut, Placement{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PhaseOfTask(c.task, c.cut); got != c.want {
				t.Fatalf("получено %+v, ожидалось %+v", got, c.want)
			}
		})
	}
}

func TestPhaseOfOtherEntities(t *testing.T) {
	cases := []struct {
		name string
		got  Placement
		want Placement
	}{
		{"Epic exploring", PhaseOfEpic(Epic{Status: "exploring"}), Placement{Architecture, StateActive, ""}},
		{"Epic planning", PhaseOfEpic(Epic{Kind: KindWork, Status: "planning"}), Placement{Planning, StateActive, ""}},
		{"Epic planned ждёт человека", PhaseOfEpic(Epic{Status: "planned"}), Placement{Planning, StateNeedsHuman, ""}},
		{"Epic running", PhaseOfEpic(Epic{Status: "running"}), Placement{}},
		{"Epic сопровождения", PhaseOfEpic(Epic{Kind: KindMaintenance, Status: "planned"}), Placement{}},
		{"публикация в очереди", PhaseOfDeployment(Deployment{Status: "queued"}), Placement{Release, StateWaiting, ""}},
		{"публикация идёт", PhaseOfDeployment(Deployment{Status: "verifying"}), Placement{Release, StateActive, ""}},
		{"откат", PhaseOfDeployment(Deployment{Status: "deploying", Rollback: true}), Placement{Release, StateActive, BadgeRollback}},
		{"публикация завершена", PhaseOfDeployment(Deployment{Status: "done"}), Placement{}},
		{"инцидент открыт", PhaseOfIncident(Incident{Status: "open"}), Placement{Operations, StateActive, ""}},
		{"инцидент с эскалацией", PhaseOfIncident(Incident{Status: "open", NeedsHuman: true}), Placement{Operations, StateNeedsHuman, ""}},
		{"инцидент смягчён", PhaseOfIncident(Incident{Status: "mitigated"}), Placement{Operations, StateWaiting, ""}},
		{"инцидент закрыт", PhaseOfIncident(Incident{Status: "resolved"}), Placement{}},
		{"неучтённое интервью", PhaseOfMaterial(Material{ID: "m2"}, LastRun{Included: map[string]bool{"m1": true}}), Placement{Discovery, StateWaiting, ""}},
		{"учтённое интервью", PhaseOfMaterial(Material{ID: "m1"}, LastRun{Included: map[string]bool{"m1": true}}), Placement{}},
		{"интервью без версий", PhaseOfMaterial(Material{ID: "m1"}, LastRun{}), Placement{Discovery, StateWaiting, ""}},
		{"идёт анализ", PhaseOfAnalysis(Analysis{Status: "running"}), Placement{Discovery, StateActive, ""}},
		{"бэклог на утверждении", PhaseOfAnalysis(Analysis{Status: "ready"}), Placement{Discovery, StateNeedsHuman, ""}},
		{"анализ упал", PhaseOfAnalysis(Analysis{Status: "failed"}), Placement{}},
		{"бэклог утверждён", PhaseOfAnalysis(Analysis{Status: "approved"}), Placement{}},
		{"проверка упала", PhaseOfEnv(EnvMonitor{State: "fail"}), Placement{Operations, StateActive, ""}},
		{"проверка unknown", PhaseOfEnv(EnvMonitor{State: "unknown"}), Placement{}},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: получено %+v, ожидалось %+v", c.name, c.got, c.want)
		}
	}
}

func TestTaskTransition(t *testing.T) {
	cut := ReleaseCut{AutoEnv: true, LastCreated: ts(0)}
	onTest := Task{Status: "testing", StepID: "test", Process: process}
	onReview := Task{Status: "review", StepID: "review", Process: process}
	fixing := Task{Status: "fixing", StepID: "code", Process: process}
	merged := Task{Status: "done", MergedAt: ts(time.Minute)}
	cases := []struct {
		before, after Task
		from, to      Phase
		changed       bool
	}{
		{onTest, onReview, Testing, Review, true},
		{onReview, fixing, Review, Development, true},
		{Task{Status: "review", StepID: "merge", Process: process, AwaitingMerge: true}, merged, Review, Release, true},
		{onReview, Task{Status: "review", StepID: "migrations", Process: process}, Review, Review, false},
		{merged, Task{Status: "done", MergedAt: ts(-time.Minute)}, Release, "", true},
	}
	for i, c := range cases {
		from, to, ch := TaskTransition(c.before, c.after, cut)
		if from != c.from || to != c.to || ch != c.changed {
			t.Errorf("%d: %q → %q (%v), ожидалось %q → %q (%v)", i, from, to, ch, c.from, c.to, c.changed)
		}
	}
	if PhasePtr("") != nil || *PhasePtr(Review) != "review" {
		t.Fatal("PhasePtr")
	}
}

func TestRolePhaseAndAgentState(t *testing.T) {
	for role, want := range map[string]Phase{"analyst": Discovery, "tester": Testing, "devops": Release, "sre": Operations} {
		if got, ok := RolePhase(role, "coding"); !ok || got != want {
			t.Errorf("%s: %q", role, got)
		}
	}
	for capb, want := range map[string]Phase{"coding": Development, "review": Review, "explore": Architecture} {
		if got, ok := RolePhase("security", capb); !ok || got != want {
			t.Errorf("собственная роль с %s: %q", capb, got)
		}
	}
	if _, ok := RolePhase("pm", ""); ok {
		t.Fatal("собственная роль без capability не относится к фазе")
	}
	cases := []struct {
		a    Agent
		want string
	}{
		{Agent{Enabled: false, Current: &Work{}}, AgentDisabled},
		{Agent{Enabled: true, Current: &Work{}}, AgentWorking},
		{Agent{Enabled: true, Capability: "review"}, AgentNoRunner},
		{Agent{Enabled: true, Capability: "review", RunnerOnline: true}, AgentIdle},
		{Agent{Enabled: true}, AgentIdle}, // control plane: no_runner не бывает
	}
	for i, c := range cases {
		if got := AgentState(c.a); got != c.want {
			t.Errorf("%d: %s, ожидалось %s", i, got, c.want)
		}
	}
}

func TestPeriod(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 11, 4, 22, 30, 0, 0, time.UTC) // 01:30 следующих суток по MSK
	got, err := PeriodStart(PeriodToday, now, msk)
	if err != nil || !got.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, msk)) {
		t.Fatalf("today: %v %v", got, err)
	}
	if got, _ := PeriodStart(Period7d, now, nil); !got.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Fatal("7d")
	}
	if _, err := PeriodStart("week", now, nil); err == nil || ValidPeriod("week") {
		t.Fatal("неверный период должен отклоняться")
	}
}

func fullInput() Input {
	in := Input{
		ProjectID: "p1", Period: "7d", GeneratedAt: t0,
		Cut: ReleaseCut{AutoEnv: true, LastCreated: ts(-time.Hour)},
		Tasks: []Task{
			{ID: "t3", Num: 3, Title: "Сделки", Status: "running", StepID: "code", Process: process, Since: t0.Add(-30 * time.Minute)},
			{ID: "t5", Num: 5, Title: "Импорт", Status: "fixing", StepID: "code", Process: process, Since: t0.Add(-50 * time.Minute)},
			{ID: "t6", Num: 6, Title: "Права", Status: "running", StepID: "code", Process: process, NeedsHuman: true, Since: t0.Add(-5 * time.Minute)},
			{ID: "t7", Num: 7, Title: "Воронка", Status: "review", StepID: "review", Process: process, Since: t0.Add(-10 * time.Minute)},
			{ID: "t8", Num: 8, Title: "Дашборд", Status: "done", MergedAt: ts(-10 * time.Minute)},
			{ID: "t9", Num: 9, Title: "Старое", Status: "done", MergedAt: ts(-2 * time.Hour)},
		},
		Epics:       []Epic{{ID: "e2", Title: "Источники лидов", Status: "planned", Since: t0.Add(-time.Hour)}},
		Deployments: []Deployment{{ID: "d1", Title: "Демо · abc123", Status: "verifying", Since: t0}},
		Incidents: []Incident{
			{ID: "i1", Title: "Список контактов отвечает 500", EnvID: "env1", Status: "open", Since: t0.Add(-3 * time.Minute)},
			{ID: "i0", Title: "Старый", EnvID: "env1", Status: "resolved"},
		},
		Envs:      []EnvMonitor{{EnvID: "env1", Name: "Демо", State: "fail"}, {EnvID: "env2", Name: "Стейдж", State: "fail", Since: t0}},
		Materials: []Material{{ID: "m1", Title: "Интервью: РОП"}, {ID: "m2", Title: "Отзыв"}},
		LastRun:   LastRun{Included: map[string]bool{"m1": true}},
		Agents: []Agent{
			{ID: "a1", Name: "Макс", RoleID: "developer", RoleTitle: "Разработчик", Model: "sonnet", Enabled: true, Capability: "coding", RunnerOnline: true,
				Current: &Work{Kind: "task", ID: "t3", Num: 3, Title: "Сделки", Action: "пишет код", Phase: Development}, RunnerID: "r1"},
			{ID: "a2", Name: "Ева", RoleID: "developer", RoleTitle: "Разработчик", Enabled: true, Capability: "coding", RunnerOnline: true},
			{ID: "a3", Name: "Ирина", RoleID: "reviewer", RoleTitle: "Ревьюер", Model: "gpt-5", Enabled: true, Capability: "review"},
			{ID: "a4", Name: "Глеб", RoleID: "sre", RoleTitle: "Инженер сопровождения", Enabled: true,
				Current: &Work{Kind: "incident", ID: "i1", Title: "Список контактов отвечает 500", Action: "описывает инцидент", Phase: Operations}},
			{ID: "a5", Name: "Лев", RoleID: "tester", RoleTitle: "Тестировщик", Enabled: false, Capability: "coding"},
			{ID: "a6", Name: "Вера", RoleID: "pm", RoleTitle: "Менеджер", Enabled: true},
		},
		Artifacts: []Artifact{
			{Phase: Release, Kind: "release", ID: "rel1", Title: "Демо abc", At: t0.Add(-time.Hour)},
			{Phase: Development, Kind: "pr", ID: "pr1", Title: "PR 1", At: t0.Add(-6 * time.Hour)},
		},
		DonePeriod:  map[Phase]int{Development: 4, Review: 3},
		Available:   map[Phase]bool{Discovery: true, Operations: true},
		LastRelease: &LastRelease{ID: "rel1", EnvID: "env1", EnvName: "Демо", Version: "abc123", CreatedAt: t0.Add(-time.Hour)},
		Tokens:      120000,
	}
	for i := 0; i < 7; i++ {
		in.Artifacts = append(in.Artifacts, Artifact{Phase: Development, Kind: "pr", ID: "pr" + string(rune('a'+i)), At: t0.Add(-time.Duration(i) * time.Minute)})
	}
	return in
}

func phaseOf(v View, id Phase) PhaseView {
	for _, p := range v.Phases {
		if p.ID == id {
			return p
		}
	}
	return PhaseView{}
}

func TestBuild(t *testing.T) {
	v := Build(fullInput())
	if len(v.Phases) != 8 || v.Phases[0].ID != Discovery || v.Phases[7].ID != Operations {
		t.Fatalf("фазы: %+v", v.Phases)
	}
	dev := phaseOf(v, Development)
	if dev.Counters != (Counters{Active: 2, NeedsHuman: 1, DonePeriod: 4}) {
		t.Fatalf("счётчики разработки: %+v", dev.Counters)
	}
	if dev.Items[0].ID != "t6" || dev.Items[1].ID != "t5" || dev.Items[2].ID != "t3" {
		t.Fatalf("порядок: сначала нужен человек, затем дольше ждущие: %+v", dev.Items)
	}
	// Агенты фазы по имени: Ева, затем Макс.
	if len(dev.Agents) != 2 || dev.Agents[0].Name != "Ева" || dev.Agents[0].State != AgentIdle || dev.Agents[1].State != AgentWorking {
		t.Fatalf("агенты разработки: %+v", dev.Agents)
	}
	if len(dev.Artifacts) != MaxArtifacts || dev.Artifacts[0].ID != "pra" {
		t.Fatalf("артефакты: свежие первыми, не больше 5: %+v", dev.Artifacts)
	}
	rev := phaseOf(v, Review)
	if len(rev.Agents) != 1 || rev.Agents[0].State != AgentNoRunner || rev.Counters.Active != 1 {
		t.Fatalf("ревью: %+v", rev)
	}
	rel := phaseOf(v, Release)
	if rel.Counters.Active != 1 || rel.Counters.Waiting != 1 {
		t.Fatalf("релиз: %+v", rel.Counters)
	}
	ops := phaseOf(v, Operations)
	// Окружение env1 представлено инцидентом, env2 без инцидента элементом окружения.
	if ops.Counters.Active != 2 || ops.Items[0].ID != "i1" || ops.Items[1].Kind != "environment" {
		t.Fatalf("сопровождение: %+v", ops.Items)
	}
	if len(ops.Agents) != 1 || ops.Agents[0].Name != "Глеб" {
		t.Fatalf("агенты сопровождения: %+v", ops.Agents)
	}
	// Единственный тестировщик отключён: фаза работает по шаблону, а
	// отключённый агент виден.
	test := phaseOf(v, Testing)
	if !test.TemplateOnly || len(test.Agents) != 1 || test.Agents[0].State != AgentDisabled {
		t.Fatalf("тестирование: %+v", test)
	}
	if !phaseOf(v, Release).TemplateOnly || !phaseOf(v, Discovery).TemplateOnly {
		t.Fatal("фазы без агентов роли работают по шаблону")
	}
	disc := phaseOf(v, Discovery)
	if disc.NoData || disc.Counters.Waiting != 1 {
		t.Fatalf("исследование: %+v", disc)
	}
	arch := phaseOf(v, Architecture)
	if !arch.NoData || arch.Hint == "" || arch.Counters != (Counters{}) {
		t.Fatalf("архитектура без данных: %+v", arch)
	}
	plan := phaseOf(v, Planning)
	if plan.Counters.NeedsHuman != 1 {
		t.Fatalf("планирование: %+v", plan.Counters)
	}
	// Все агенты, включая отключённого; активные инциденты (resolved не в счёт).
	if v.Summary.AgentsTotal != 6 || v.Summary.AgentsWorking != 2 || v.Summary.IncidentsOpen != 1 || v.Summary.LastRelease == nil {
		t.Fatalf("сводка: %+v", v.Summary)
	}
}

// «Нет данных» не прячет элементы: Epic в исследовании архитектуры и
// инцидент провала публикации видны, даже если смежный механизм не выкачен.
func TestNoDataDoesNotHideItems(t *testing.T) {
	v := Build(Input{
		ProjectID: "p",
		Epics:     []Epic{{ID: "e1", Title: "Архитектура CRM", Status: "exploring"}},
		Incidents: []Incident{
			{ID: "i1", Title: "Публикация упала", EnvID: "env1", Status: "open"},
			{ID: "i2", Title: "Смягчён", EnvID: "env2", Status: "mitigated"},
		},
		DonePeriod: map[Phase]int{Discovery: 2},
	})
	if a := phaseOf(v, Architecture); a.NoData || a.Counters.Active != 1 || len(a.Items) != 1 {
		t.Fatalf("архитектура: %+v", a)
	}
	if o := phaseOf(v, Operations); o.NoData || o.Counters.Active != 1 || o.Counters.Waiting != 1 {
		t.Fatalf("сопровождение: %+v", o)
	}
	if d := phaseOf(v, Discovery); d.NoData {
		t.Fatal("завершённое за период тоже данные")
	}
	// Артефакт без элементов тоже данные: снимок архитектуры виден.
	va := Build(Input{Artifacts: []Artifact{{Phase: Architecture, Kind: "architecture", ID: "snap1", Title: "Снимок", At: t0}}})
	if a := phaseOf(va, Architecture); a.NoData || len(a.Artifacts) != 1 {
		t.Fatalf("артефакт архитектуры: %+v", a)
	}
	if v.Summary.IncidentsOpen != 2 {
		t.Fatalf("активные инциденты: открытые и смягчённые: %d", v.Summary.IncidentsOpen)
	}
}

// Агент встроенного шаблона остаётся в фазе своей роли, даже если его
// задача на шаге prompt после review относится к ревью.
func TestBuiltinAgentStaysInRolePhase(t *testing.T) {
	proc := []Step{{"code", "code"}, {"review", "review"}, {"check", "prompt"}}
	task := Task{ID: "t1", Status: "review", StepID: "check", Process: proc, EpicRunning: true}
	ph := PhaseOfTask(task, ReleaseCut{}).Phase
	if ph != Review {
		t.Fatalf("задача на prompt после review: %s", ph)
	}
	v := Build(Input{
		Agents: []Agent{{ID: "a1", Name: "Макс", RoleID: "developer", Enabled: true, Capability: "coding", RunnerOnline: true,
			Current: &Work{Kind: "task", ID: "t1", Phase: ph}}},
		Available: map[Phase]bool{Discovery: true, Architecture: true, Operations: true},
	})
	if dev := phaseOf(v, Development); dev.TemplateOnly || len(dev.Agents) != 1 || dev.Agents[0].State != AgentWorking {
		t.Fatalf("разработка: %+v", dev)
	}
	if rev := phaseOf(v, Review); !rev.TemplateOnly || len(rev.Agents) != 0 {
		t.Fatalf("ревью без ревьюера: %+v", rev)
	}
	// Собственный шаблон следует за работой.
	v = Build(Input{Agents: []Agent{{ID: "a2", Name: "Сек", RoleID: "security", Enabled: true, Capability: "review",
		RunnerOnline: true, Current: &Work{Kind: "task", ID: "t1", Phase: Testing}}}})
	if test := phaseOf(v, Testing); len(test.Agents) != 1 {
		t.Fatalf("собственный шаблон в фазе работы: %+v", test)
	}
}

func TestBuildDeterministicAndTruncated(t *testing.T) {
	in := fullInput()
	for i := 0; i < 15; i++ {
		in.Tasks = append(in.Tasks, Task{ID: "x" + string(rune('a'+i)), Status: "testing", StepID: "test", Process: process, Since: t0})
	}
	a, _ := json.Marshal(Build(in))
	// Обратный порядок строк не меняет ответ.
	rev := in
	rev.Tasks = append([]Task(nil), in.Tasks...)
	for i, j := 0, len(rev.Tasks)-1; i < j; i, j = i+1, j-1 {
		rev.Tasks[i], rev.Tasks[j] = rev.Tasks[j], rev.Tasks[i]
	}
	b, _ := json.Marshal(Build(rev))
	if string(a) != string(b) {
		t.Fatal("агрегат зависит от порядка строк")
	}
	v := Build(in)
	if test := phaseOf(v, Testing); len(test.Items) != MaxItems || test.Counters.Active != 15 {
		t.Fatalf("обрезка до 10 при полном счётчике: %d, %+v", len(test.Items), test.Counters)
	}
}

func TestBuildEmptyJSON(t *testing.T) {
	raw, err := json.Marshal(Build(Input{ProjectID: "p"}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"period":"7d"`, `"agents":[]`, `"items":[]`, `"artifacts":[]`, `"last_release":null`, `"spend_usd":null`, `"no_data":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %s в %s", want, s)
		}
	}
	if strings.Contains(s, `"phase"`) {
		t.Error("служебное поле фазы не должно попадать в JSON")
	}
}

// Артефакты одного момента идут по Order, а не по строковому ID: решения
// одной даты новыми номерами (10 раньше 9), потребности по важности.
func TestArtifactOrderWithinMoment(t *testing.T) {
	in := Input{Available: map[Phase]bool{Architecture: true, Discovery: true}}
	for _, n := range []int{8, 9, 10, 7} {
		in.Artifacts = append(in.Artifacts, Artifact{Phase: Architecture, Kind: "adr", ID: fmt.Sprint(n), At: t0, Order: -n})
	}
	for i, id := range []string{"r/n9", "r/n2", "r/n10"} {
		in.Artifacts = append(in.Artifacts, Artifact{Phase: Discovery, Kind: "need", ID: id, At: t0, Order: i})
	}
	ids := func(p PhaseView) string {
		var out []string
		for _, a := range p.Artifacts {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	v := Build(in)
	if got := ids(phaseOf(v, Architecture)); got != "10,9,8,7" {
		t.Fatalf("решения одной даты: %s", got)
	}
	if got := ids(phaseOf(v, Discovery)); got != "r/n9,r/n2,r/n10" {
		t.Fatalf("потребности по важности: %s", got)
	}
}

// Неучтённые отзывы (add-user-feedback): счётчик фазы «Исследование»
// считает только отзывы, которых нет в последней готовой версии.
func TestFeedbackUnaccounted(t *testing.T) {
	in := Input{
		Materials: []Material{
			{ID: "iv1", Kind: MaterialInterview}, {ID: "fb1", Kind: MaterialFeedback},
			{ID: "fb2", Kind: MaterialFeedback}, {ID: "fb3", Kind: MaterialFeedback},
		},
		LastRun:   LastRun{Included: map[string]bool{"fb1": true}},
		Available: map[Phase]bool{Discovery: true},
	}
	v := Build(in)
	disc := phaseOf(v, Discovery)
	if disc.FeedbackUnaccounted != 2 || disc.Counters.Waiting != 3 {
		t.Fatalf("исследование: %+v", disc)
	}
	for _, p := range v.Phases {
		if p.ID != Discovery && p.FeedbackUnaccounted != 0 {
			t.Fatalf("счётчик отзывов у фазы %s", p.ID)
		}
	}
	b, _ := json.Marshal(phaseOf(Build(Input{}), Discovery))
	if strings.Contains(string(b), "feedback_unaccounted") {
		t.Fatalf("нулевой счётчик в JSON: %s", b)
	}
}

// Идущий анализ не выпадает из обрезанного списка, когда неучтённых
// материалов десять и больше: в работе раньше ждущих.
func TestActiveBeforeWaiting(t *testing.T) {
	in := Input{Available: map[Phase]bool{Discovery: true},
		Analyses: []Analysis{{ID: "run", Status: "running", Since: t0}}}
	for i := 0; i < 12; i++ {
		in.Materials = append(in.Materials, Material{ID: fmt.Sprintf("fb%02d", i), Kind: MaterialFeedback, Since: t0.Add(-time.Duration(i+1) * time.Minute)})
	}
	in.Analyses = append(in.Analyses, Analysis{ID: "old", Status: "ready", Since: t0.Add(-time.Hour)})
	disc := phaseOf(Build(in), Discovery)
	if len(disc.Items) != MaxItems || disc.Items[0].ID != "old" || disc.Items[1].ID != "run" || disc.Items[2].State != StateWaiting {
		t.Fatalf("порядок элементов: %+v", disc.Items[:3])
	}
	if disc.Counters.Waiting != 12 || disc.Counters.Active != 1 || disc.Counters.NeedsHuman != 1 {
		t.Fatalf("счётчики: %+v", disc.Counters)
	}
}
