package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
)

func cyclePhase(t *testing.T, v cycle.View, id cycle.Phase) cycle.PhaseView {
	t.Helper()
	for _, p := range v.Phases {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("нет фазы %s", id)
	return cycle.PhaseView{}
}

func cycleAgent(p cycle.PhaseView, id string) *cycle.AgentView {
	for i := range p.Agents {
		if p.Agents[i].AgentID == id {
			return &p.Agents[i]
		}
	}
	return nil
}

func artifactKinds(p cycle.PhaseView) []string {
	var out []string
	for _, a := range p.Artifacts {
		out = append(out, a.Kind+":"+a.ID)
	}
	return out
}

// seedDiscoveryRun — версия исследования с набором материалов и
// результатом (nil — без результата, идущий или упавший анализ).
func seedDiscoveryRun(t *testing.T, s *Store, projectID string, version int, status string, mats []string, needs []map[string]string, approved bool) string {
	t.Helper()
	ivs := []RunInterview{}
	for _, m := range mats {
		ivs = append(ivs, RunInterview{ID: m, Title: m})
	}
	ij, _ := json.Marshal(ivs)
	var result any
	if needs != nil {
		b, _ := json.Marshal(map[string]any{"personas": []any{}, "needs": needs, "epics": []any{}})
		result = b
	}
	var id string
	if err := s.Pool.QueryRow(context.Background(), `
		INSERT INTO discovery_runs (project_id, version, status, stage, progress_done, progress_total, interviews,
			agent_id, agent_name, role_title, result, finished_at, approved_at)
		VALUES ($1, $2, $3, CASE WHEN $3 = 'running' THEN 'summaries' ELSE 'done' END, 1, $4, $5,
			'vera', 'Вера', 'Аналитик', $6, CASE WHEN $3 = 'running' THEN NULL ELSE now() END,
			CASE WHEN $7 THEN now() END)
		RETURNING id`, projectID, version, status, len(mats), ij, result, approved).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Агрегат с данными всех восьми фаз (fix-cycle-phase-data), подробно
// «Исследование» и «Архитектура»: материалы, не учтённые последней
// готовой версией, идущий анализ и Аналитик за работой, бэклог на
// утверждении, снимок архитектуры с решениями ADR, задача описания,
// завершённые за период.
func TestLoadCycleDiscoveryArchitecture(t *testing.T) {
	f := newSDLC(t)
	s, ctx := f.s, context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, f.epic); err != nil {
		t.Fatal(err)
	}
	anna, err := s.AddInterview(ctx, f.project, "", NewInterview{Title: "Анна", RespondentRole: "Менеджер", Text: "**Р:** Сделки теряются."})
	if err != nil {
		t.Fatal(err)
	}
	oleg, err := s.AddInterview(ctx, f.project, "", NewInterview{Title: "Олег", RespondentRole: "РОП", Text: "**Р:** Нет сводки по воронке."})
	if err != nil {
		t.Fatal(err)
	}
	needs := []map[string]string{
		{"id": "n1", "title": "Напоминания о звонках", "importance": "low"},
		{"id": "n2", "title": "Этап каждой сделки", "importance": "high"},
		{"id": "n3", "title": "Сводка по воронке", "importance": "medium"},
		{"id": "n4", "title": "Импорт из Excel", "importance": "high"},
	}
	// Утверждённая версия 1 учла только Анну, версия 2 идёт по обоим.
	v1 := seedDiscoveryRun(t, s, f.project, 1, "approved", []string{anna.ID}, needs, true)
	v2 := seedDiscoveryRun(t, s, f.project, 2, "running", []string{anna.ID, oleg.ID}, nil, false)

	// Архитектура: снимок с пятью решениями, задача описания в работе и
	// завершённая за период задача прошлого описания.
	decisions := []map[string]any{}
	for n := 1; n <= 5; n++ {
		st := "accepted"
		if n == 4 {
			st = "proposed"
		}
		decisions = append(decisions, map[string]any{"number": n, "title": "Решение " + string(rune('A'+n-1)), "status": st, "date": "2026-11-0" + string(rune('0'+n))})
	}
	data, _ := json.Marshal(map[string]any{"system": map[string]string{"name": "CRM"}, "elements": []any{}, "decisions": decisions})
	if _, _, err := s.SaveProjectArchitecture(ctx, f.project, ArchitectureSave{Status: "ok", SHA: "0123456789abcdef", Data: data}); err != nil {
		t.Fatal(err)
	}
	_, archTask, err := s.CreateArchitectureTask(ctx, architectureInput(f.project))
	if err != nil {
		t.Fatal(err)
	}
	var oldEpic string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO epics (project_id, title, goal, kind, status) VALUES ($1, 'Архитектура проекта', 'g', 'architecture', 'done') RETURNING id`,
		f.project).Scan(&oldEpic); err != nil {
		t.Fatal(err)
	}
	// У задачи описания есть PR и вердикты шагов проверки и ревью: в
	// артефакты разработки, тестирования и ревью они не входят.
	var oldTask string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO tasks (epic_id, title, kind, status, done_at, merged_at, pr_url)
		VALUES ($1, 'Описать архитектуру', 'architecture', 'done', now(), now(), 'https://example.test/pr/arch') RETURNING id`,
		oldEpic).Scan(&oldTask); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"task.verdict", "task.review_passed"} {
		if _, err := s.AppendEvent(ctx, EventInput{ActorKind: "runner", Type: typ, ProjectID: f.project, EpicID: oldEpic, TaskID: oldTask,
			Text: "вердикт описания"}); err != nil {
			t.Fatal(err)
		}
	}
	f.merged(t, "обычная задача", time.Now())

	// Остальные фазы: план на утверждении, код, тесты, ревью, смерженная
	// задача ждёт релиза, проверка наблюдения упала.
	planned, err := s.CreateEpic(ctx, f.project, "Сделки", "воронка")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='planned' WHERE id=$1`, planned.ID); err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct{ status, step string }{{"running", "code"}, {"testing", "test"}, {"review", "review"}} {
		task := mustTask(t, s, f.epic, NewTask{Title: "на шаге " + st.step})
		if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status=$2, step_id=$3,
			process='{"steps":[{"id":"code","kind":"code"},{"id":"test","kind":"test"},{"id":"review","kind":"review"}]}' WHERE id=$1`,
			task.ID, st.status, st.step); err != nil {
			t.Fatal(err)
		}
	}
	f.check(t, false, "error: 500", "")

	v, err := s.LoadCycle(ctx, f.project, "7d", time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range v.Phases {
		if c := p.Counters; p.NoData || c.Active+c.Waiting+c.NeedsHuman == 0 {
			t.Errorf("фаза %s без данных: %+v %+v", p.ID, c, p.Items)
		}
	}
	disc := cyclePhase(t, v, cycle.Discovery)
	if disc.NoData {
		t.Fatal("исследование с интервью отдано без данных")
	}
	if c := disc.Counters; c.Active != 1 || c.Waiting != 1 || c.NeedsHuman != 0 || c.DonePeriod != 1 {
		t.Fatalf("счётчики исследования: %+v", c)
	}
	items := map[string]cycle.Item{}
	for _, it := range disc.Items {
		items[it.Kind] = it
	}
	if m := items["material"]; m.ID != oleg.ID || m.State != cycle.StateWaiting || m.Title != "Интервью · Олег" {
		t.Fatalf("неучтённое интервью: %+v", disc.Items)
	}
	if a := items["analysis"]; a.ID != v2 || a.State != cycle.StateActive || !strings.Contains(a.Title, "версия 2") {
		t.Fatalf("идущий анализ: %+v", disc.Items)
	}
	// Артефакты: два интервью и три потребности версии 1, важные первыми.
	got := strings.Join(artifactKinds(disc), " ")
	// Потребности одной версии идут по важности: n2 и n4 важные, n3 заметная.
	if !strings.Contains(got, "need:"+v1+"/n2 need:"+v1+"/n4 need:"+v1+"/n3") {
		t.Fatalf("порядок потребностей: %s", got)
	}
	for _, want := range []string{"interview:" + anna.ID, "interview:" + oleg.ID, "need:" + v1 + "/n2", "need:" + v1 + "/n4", "need:" + v1 + "/n3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("артефакты исследования без %s: %s", want, got)
		}
	}
	if strings.Contains(got, "/n1") {
		t.Fatalf("неважная потребность вытеснила важные: %s", got)
	}
	for _, a := range disc.Artifacts {
		if a.Kind == "need" && (a.Ref.View != "discovery" || a.Ref.ID != v1) {
			t.Fatalf("ссылка потребности: %+v", a)
		}
	}
	vera := cycleAgent(disc, "vera")
	if vera == nil || vera.State != cycle.AgentWorking || vera.Current == nil || vera.Current.Kind != "analysis" || vera.Current.ID != v2 {
		t.Fatalf("Аналитик за анализом: %+v", vera)
	}

	arch := cyclePhase(t, v, cycle.Architecture)
	if arch.NoData {
		t.Fatal("архитектура со снимком отдана без данных")
	}
	if c := arch.Counters; c.Active != 1 || c.DonePeriod != 1 {
		t.Fatalf("счётчики архитектуры: %+v", c)
	}
	if len(arch.Items) != 1 || arch.Items[0].ID != archTask {
		t.Fatalf("задача описания: %+v", arch.Items)
	}
	if len(arch.Artifacts) != 5 || arch.Artifacts[0].Kind != "architecture" || arch.Artifacts[0].Ref.View != "architecture" ||
		!strings.Contains(arch.Artifacts[0].Title, "CRM") || !strings.Contains(arch.Artifacts[0].Title, "0123456") {
		t.Fatalf("снимок первым: %+v", arch.Artifacts)
	}
	// Решения 5, 4, 3, 2 — последние по номеру, ссылка на номер.
	adrs := map[string]cycle.Artifact{}
	for _, a := range arch.Artifacts[1:] {
		if a.Kind != "adr" || a.Ref.View != "adr" || a.Ref.ID != a.ID {
			t.Fatalf("решение: %+v", a)
		}
		adrs[a.ID] = a
	}
	if _, ok := adrs["1"]; ok || len(adrs) != 4 {
		t.Fatalf("последние решения: %+v", arch.Artifacts)
	}
	if adrs["4"].Title != "ADR-0004 · Решение D (предложено)" || adrs["5"].Title != "ADR-0005 · Решение E" {
		t.Fatalf("названия решений: %q %q", adrs["4"].Title, adrs["5"].Title)
	}
	if d := cyclePhase(t, v, cycle.Development).Counters.DonePeriod; d != 1 {
		t.Fatalf("описание архитектуры попало в разработку: %d", d)
	}
	for _, ph := range []cycle.Phase{cycle.Development, cycle.Testing, cycle.Review} {
		for _, a := range cyclePhase(t, v, ph).Artifacts {
			if a.ID == oldTask || a.Ref.ID == oldTask || strings.Contains(a.Ref.URL, "/pr/arch") {
				t.Fatalf("артефакт задачи описания в фазе %s: %+v", ph, a)
			}
		}
	}
	if n := len(cyclePhase(t, v, cycle.Development).Artifacts); n != 1 {
		t.Fatalf("PR обычной задачи в разработке: %d", n)
	}

	// Анализ завершён: бэклог ждёт утверждения, оба интервью учтены,
	// потребности берутся из новой версии, Аналитик свободен.
	if _, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET status='ready', stage='done', finished_at=now(),
		result='{"needs":[{"id":"n9","title":"Новая потребность","importance":"high"}]}' WHERE id=$1`, v2); err != nil {
		t.Fatal(err)
	}
	// Описание сломали: снимок invalid, решения последней корректной версии.
	if _, _, err := s.SaveProjectArchitecture(ctx, f.project, ArchitectureSave{Status: "invalid", SHA: "fedcba9876543210"}); err != nil {
		t.Fatal(err)
	}
	v, err = s.LoadCycle(ctx, f.project, "7d", time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	disc = cyclePhase(t, v, cycle.Discovery)
	if c := disc.Counters; c.Active != 0 || c.Waiting != 0 || c.NeedsHuman != 1 {
		t.Fatalf("бэклог на утверждении: %+v %+v", c, disc.Items)
	}
	if got := strings.Join(artifactKinds(disc), " "); !strings.Contains(got, "need:"+v2+"/n9") || strings.Contains(got, "need:"+v1) {
		t.Fatalf("потребности последней готовой версии: %s", got)
	}
	if vera := cycleAgent(disc, "vera"); vera == nil || vera.State != cycle.AgentIdle {
		t.Fatalf("Аналитик после анализа: %+v", vera)
	}
	arch = cyclePhase(t, v, cycle.Architecture)
	if len(arch.Artifacts) != 5 || !strings.Contains(arch.Artifacts[0].Title, "есть ошибки проверки") || !strings.Contains(arch.Artifacts[0].Title, "fedcba9") {
		t.Fatalf("снимок с ошибками: %+v", arch.Artifacts)
	}

	// Интервью версии удалено: версия устарела, утвердить её нельзя.
	if _, err := s.DeleteInterview(ctx, f.project, oleg.ID); err != nil {
		t.Fatal(err)
	}
	v, err = s.LoadCycle(ctx, f.project, "7d", time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	disc = cyclePhase(t, v, cycle.Discovery)
	if len(disc.Items) != 1 || disc.Items[0].State != cycle.StateNeedsHuman ||
		disc.Items[0].Title != "Бэклог исследования · версия 2 устарела, нужен новый анализ" {
		t.Fatalf("устаревшая версия: %+v", disc.Items)
	}
}

// Анализ Аналитика в другом проекте: агент занят, но работа и её ход
// скрыты. Анализы в двух проектах сразу: каждый проект видит свой.
func TestLoadCycleAnalysisElsewhere(t *testing.T) {
	f := newSDLC(t)
	s, ctx := f.s, context.Background()
	other := seedEnv(t, s, "manual")
	otherRun := seedDiscoveryRun(t, s, other.ProjectID, 1, "running", []string{"x", "y", "z"}, nil, false)
	vera := func(project string) *cycle.AgentView {
		t.Helper()
		v, err := s.LoadCycle(ctx, project, "7d", time.UTC, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cycleAgent(cyclePhase(t, v, cycle.Discovery), "vera")
	}
	a := vera(f.project)
	if a == nil || a.State != cycle.AgentWorking || a.Current == nil || a.Current.Kind != "other" || a.Current.ID != "" ||
		a.Current.Action != "анализирует интервью" {
		t.Fatalf("Аналитик в другом проекте: %+v %+v", a, a.Current)
	}

	// Свой анализ запущен позже чужого: в каждом проекте своя работа.
	own := seedDiscoveryRun(t, s, f.project, 1, "running", []string{"a"}, nil, false)
	for i := 0; i < 5; i++ {
		if a := vera(f.project); a.Current == nil || a.Current.Kind != "analysis" || a.Current.ID != own ||
			a.Current.Action != "конспектирует интервью 1 из 1" {
			t.Fatalf("свой анализ: %+v", a.Current)
		}
		if a := vera(other.ProjectID); a.Current == nil || a.Current.ID != otherRun {
			t.Fatalf("анализ другого проекта: %+v", a.Current)
		}
	}
}
