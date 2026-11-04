package analyst

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("RIVET_DATABASE_URL")
	if base == "" {
		base = "postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("postgres недоступен: %v", err)
	}
	name := fmt.Sprintf("rivet_analyst_%d_%x", time.Now().UnixNano(), rand.Uint32())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(base)
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

var numberedRe = regexp.MustCompile(`^(\d+)\| \*\*Р:\*\* (.*)$`)

// fakeModel отвечает на задания Аналитика по их содержимому: находка на
// каждую реплику респондента, потребность на находку, один Epic. Хуки
// позволяют испортить ответ.
type fakeModel struct {
	mu        sync.Mutex
	summaries int
	syntheses int
	// synth — подмена ответа синтеза по номеру попытки (с 1).
	synth func(n int, prompt string) (string, error, bool)
	// summary — подмена ответа конспекта по номеру запроса (с 1).
	summary               func(n int, prompt string) (string, error, bool)
	inFlight, maxInFlight atomic.Int32
	// limits — лимиты ответа запросов по порядку.
	limits []int
}

func ptr(v int64) *int64 { return &v }

func (f *fakeModel) CompleteWithUsage(ctx context.Context, model, prompt string, maxTokens int) (string, llm.Usage, error) {
	f.mu.Lock()
	f.limits = append(f.limits, maxTokens)
	f.mu.Unlock()
	cur := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxInFlight.Load()
		if cur <= m || f.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	usage := llm.Usage{InputTokens: ptr(1000), OutputTokens: ptr(200)}
	first, _, _ := strings.Cut(prompt, "\n")
	switch first {
	case discovery.MarkerSummary:
		f.mu.Lock()
		f.summaries++
		n := f.summaries
		f.mu.Unlock()
		if f.summary != nil {
			if raw, err, ok := f.summary(n, prompt); ok {
				return raw, usage, err
			}
		}
		return summaryReply(prompt), usage, nil
	case discovery.MarkerSynthesis:
		f.mu.Lock()
		f.syntheses++
		n := f.syntheses
		f.mu.Unlock()
		if f.synth != nil {
			if raw, err, ok := f.synth(n, prompt); ok {
				return raw, usage, err
			}
		}
		return synthesisReply(prompt), usage, nil
	}
	return "", usage, fmt.Errorf("неизвестное задание")
}

func summaryReply(prompt string) string {
	var findings []map[string]any
	for _, line := range strings.Split(prompt, "\n") {
		m := numberedRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		findings = append(findings, map[string]any{"kind": "pain", "text": "боль: " + m[2], "importance": "high",
			"quotes": []map[string]any{{"text": m[2], "line": n}}})
	}
	out, _ := json.Marshal(map[string]any{"respondent": map[string]string{"role": "менеджер", "summary": "конспект"}, "findings": findings})
	return string(out)
}

func synthesisReply(prompt string) string {
	var in struct {
		Interviews []struct {
			ID       string              `json:"id"`
			Findings []discovery.Finding `json:"findings"`
		} `json:"interviews"`
	}
	_, after, _ := strings.Cut(prompt, "# Конспекты интервью")
	_, block, _ := strings.Cut(after, "```json\n")
	body, _, _ := strings.Cut(block, "\n```")
	_ = json.Unmarshal([]byte(body), &in)
	var personas, needs []map[string]any
	var ids []string
	for i, iv := range in.Interviews {
		pid := fmt.Sprintf("p%d", i+1)
		personas = append(personas, map[string]any{"id": pid, "name": "Персона " + pid, "description": "d", "interviews": []string{iv.ID}})
		for _, f := range iv.Findings {
			id := fmt.Sprintf("n%d", len(needs)+1)
			ids = append(ids, id)
			needs = append(needs, map[string]any{"id": id, "title": f.Text, "kind": "pain", "persona": pid, "importance": "high",
				"evidence": []map[string]any{{"interview": iv.ID, "quote": f.Quotes[0].Text}}})
		}
	}
	out, _ := json.Marshal(map[string]any{"personas": personas, "needs": needs, "epics": []map[string]any{{
		"key": "e1", "title": "Сделки и воронка", "goal": "Менеджер видит этап каждой сделки и не теряет клиентов в воронке продаж.",
		"needs": ids, "priority": "must", "size": "M", "depends_on": []string{}, "existing_epic": nil,
	}}})
	return string(out)
}

type fixture struct {
	st      *store.Store
	project string
	user    string
}

func seed(t *testing.T, st *store.Store, interviews ...string) fixture {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, fmt.Sprintf("owner-%d", time.Now().UnixNano()), "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "CRM", "o/crm", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{st: st, project: p.ID, user: u.ID}
	for _, title := range interviews {
		f.add(t, title)
	}
	return f
}

func (f fixture) add(t *testing.T, title string) {
	t.Helper()
	text := "**И:** Как вы ведёте сделки?\n**Р:** Сделки веду в таблице " + title + ", этапы путаются.\n**И:** Что мешает?\n**Р:** Забываю перезвонить клиенту после встречи."
	if _, err := f.st.AddInterview(context.Background(), f.project, f.user, store.NewInterview{Title: title, RespondentRole: "Менеджер", Text: text}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) job(t *testing.T, m Completer) Job {
	t.Helper()
	ctx := context.Background()
	ivs, err := f.st.InterviewsForAnalysis(ctx, f.project)
	if err != nil {
		t.Fatal(err)
	}
	var set []store.RunInterview
	for _, iv := range ivs {
		set = append(set, store.RunInterview{ID: iv.ID, Title: iv.Title, SHA256: iv.SHA256})
	}
	an := discovery.Analyst{Title: "Аналитик", Name: "Вера", Instruction: "Ищи боли."}
	run, err := f.st.CreateRun(ctx, f.project, store.NewRun{Interviews: set, AgentID: "vera", AgentName: "Вера",
		RoleTitle: "Аналитик", Model: "fake-model", InstructionSHA: an.InstructionSHA()})
	if err != nil {
		t.Fatal(err)
	}
	return Job{Run: run, AgentID: "vera", Analyst: an, Interviews: ivs,
		Model: Model{Client: m, Model: "fake-model", InputPrice: ptr(3_000_000), OutputPrice: ptr(15_000_000)}}
}

func eventTypes(t *testing.T, st *store.Store, project string) []string {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{ProjectID: project, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if strings.HasPrefix(e.Type, "discovery.") {
			out = append(out, e.Type)
		}
	}
	return out
}

// Анализ пяти интервью: конспекты параллельно не больше трёх, ход по
// этапам, ready с персонами, потребностями и бэклогом, usage с ролью
// analyst и стоимостью по ценам каталога.
func TestAnalysisReady(t *testing.T) {
	st := testStore(t)
	f := seed(t, st, "Анна", "Борис", "Вика", "Глеб", "Дина")
	m := &fakeModel{}
	job := f.job(t, m)
	(&Runner{St: st}).Run(context.Background(), job)

	ctx := context.Background()
	run, err := st.GetRun(ctx, f.project, job.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunReady || run.Result == nil {
		t.Fatalf("версия: %s %q %v", run.Status, run.Error, run.Defects)
	}
	if len(run.Result.Personas) != 5 || len(run.Result.Needs) != 10 || len(run.Backlog) != 1 || run.BacklogRev != 1 {
		t.Fatalf("результат: %+v", run.Result)
	}
	if m.maxInFlight.Load() > DefaultParallel {
		t.Fatalf("параллельных запросов %d, лимит %d", m.maxInFlight.Load(), DefaultParallel)
	}
	if run.TokensIn == nil || *run.TokensIn != 6000 || run.CostUSD == nil {
		t.Fatalf("usage версии: %v %v", run.TokensIn, run.CostUSD)
	}
	// 6 запросов × (1000×3$ + 200×15$)/1M = 6 × 0.006.
	if d := *run.CostUSD - 0.036; d > 1e-9 || d < -1e-9 {
		t.Fatalf("стоимость: %v", *run.CostUSD)
	}
	var n int
	var cost float64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(cost_usd),0)::float8 FROM usage_records
		WHERE project_id=$1 AND role='analyst' AND model='fake-model' AND task_id IS NULL AND epic_id IS NULL`, f.project).Scan(&n, &cost); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("usage Аналитика: %d записей", n)
	}
	types := eventTypes(t, st, f.project)
	progress := 0
	for _, ty := range types {
		if ty == "discovery.analysis_progress" {
			progress++
		}
	}
	if progress != 6 || types[len(types)-1] != "discovery.analysis_ready" {
		t.Fatalf("события: %v", types)
	}
	evs, _ := st.Events(ctx, store.EventFilter{ProjectID: f.project, Type: "discovery.analysis_ready"})
	if a, _ := evs[0].Payload["agent"].(map[string]any); a["name"] != "Вера" || evs[0].Payload["model"] != "fake-model" {
		t.Fatalf("payload готовности: %+v", evs[0].Payload)
	}
	// Лимиты ответа по этапам: конспекты и синтез.
	for i, l := range m.limits {
		want := SummaryMaxTokens
		if i == len(m.limits)-1 {
			want = SynthesisMaxTokens
		}
		if l != want {
			t.Fatalf("лимит запроса %d: %d, ждали %d", i, l, want)
		}
	}
	// Ход «N из M» не откатывается.
	progEvs, _ := st.Events(ctx, store.EventFilter{ProjectID: f.project, Type: "discovery.analysis_progress", Limit: 500})
	last := 0.0
	for _, e := range progEvs {
		done, _ := e.Payload["done"].(float64)
		if done < last {
			t.Fatalf("ход откатился: %v после %v", done, last)
		}
		last = done
	}

	// Повторный анализ после шестого интервью: конспекты пяти прежних из
	// кэша, новая версия переводит прежнюю ready в superseded.
	f.add(t, "Ефим")
	m2 := &fakeModel{}
	job2 := f.job(t, m2)
	(&Runner{St: st}).Run(ctx, job2)
	if m2.summaries != 1 || m2.syntheses != 1 {
		t.Fatalf("кэш конспектов: конспектов %d, синтезов %d", m2.summaries, m2.syntheses)
	}
	old, _ := st.GetRun(ctx, f.project, job.Run.ID)
	cur, _ := st.GetRun(ctx, f.project, job2.Run.ID)
	if old.Status != store.RunSuperseded || cur.Status != store.RunReady || cur.Version != 2 {
		t.Fatalf("версии: %s, %s v%d", old.Status, cur.Status, cur.Version)
	}
}

// Кэш конспектов общий для установки: в отброшенном версии стоит название
// интервью своего проекта, а не того, где конспект построен впервые.
func TestSummaryCacheRefs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	bad := func(_ int, prompt string) (string, error, bool) {
		var v map[string]any
		_ = json.Unmarshal([]byte(summaryReply(prompt)), &v)
		fs := v["findings"].([]any)
		v["findings"] = append(fs, map[string]any{"kind": "pain", "text": "выдумка", "importance": "low",
			"quotes": []map[string]any{{"text": "этого нет в интервью совсем", "line": 1}}})
		b, _ := json.Marshal(v)
		return string(b), nil, true
	}
	a := seed(t, st, "Секретный клиент")
	ja := a.job(t, &fakeModel{summary: bad})
	(&Runner{St: st}).Run(ctx, ja)

	b := seed(t, st)
	ivs, _ := st.InterviewsForAnalysis(ctx, a.project)
	if _, err := st.AddInterview(ctx, b.project, b.user, store.NewInterview{Title: "Интервью Б", Text: ivs[0].Text}); err != nil {
		t.Fatal(err)
	}
	mb := &fakeModel{}
	jb := b.job(t, mb)
	(&Runner{St: st}).Run(ctx, jb)
	if mb.summaries != 0 {
		t.Fatalf("конспект должен прийти из кэша: %d запросов", mb.summaries)
	}
	ra, _ := st.GetRun(ctx, a.project, ja.Run.ID)
	rb, _ := st.GetRun(ctx, b.project, jb.Run.ID)
	if len(ra.Dropped) == 0 || !strings.Contains(ra.Dropped[0].Ref, "«Секретный клиент»") {
		t.Fatalf("отброшенное версии A: %+v", ra.Dropped)
	}
	if len(rb.Dropped) == 0 || !strings.Contains(rb.Dropped[0].Ref, "«Интервью Б»") || strings.Contains(rb.Dropped[0].Ref, "Секретный") {
		t.Fatalf("отброшенное версии B с чужим названием: %+v", rb.Dropped)
	}
}

// Цикл в зависимостях: один повтор с дефектами, второй цикл — failed с
// перечнем дефектов. Оборванный ответ конспекта повторяется.
func TestAnalysisRetryAndFail(t *testing.T) {
	st := testStore(t)
	f := seed(t, st, "Анна")
	var synthPrompts []string
	m := &fakeModel{
		summary: func(n int, prompt string) (string, error, bool) {
			if n == 1 {
				return `{"findings": [`, llm.ErrTruncated, true
			}
			if !strings.Contains(prompt, "оборван") {
				t.Errorf("повтор конспекта без дефекта обрыва")
			}
			return "", nil, false
		},
		synth: func(n int, prompt string) (string, error, bool) {
			synthPrompts = append(synthPrompts, prompt)
			var v map[string]any
			_ = json.Unmarshal([]byte(synthesisReply(prompt)), &v)
			epics := v["epics"].([]any)
			epics[0].(map[string]any)["depends_on"] = []string{"e2"}
			v["epics"] = append(epics, map[string]any{"key": "e2", "title": "Второй",
				"goal": "Цель второго Epic'а достаточно длинная для проверки кодом.", "needs": []string{"n1"},
				"priority": "should", "size": "S", "depends_on": []string{"e1"}, "existing_epic": nil})
			cyclic, _ := json.Marshal(v)
			return string(cyclic), nil, true
		},
	}
	job := f.job(t, m)
	(&Runner{St: st}).Run(context.Background(), job)
	run, _ := st.GetRun(context.Background(), f.project, job.Run.ID)
	if run.Status != store.RunFailed || len(run.Defects) == 0 || !strings.Contains(strings.Join(run.Defects, ";"), "цикл") {
		t.Fatalf("ждали failed с дефектом цикла: %s %q %v", run.Status, run.Error, run.Defects)
	}
	if len(synthPrompts) != 2 || !strings.Contains(synthPrompts[1], "цикл") {
		t.Fatalf("повтор синтеза с перечнем дефектов: %d заданий", len(synthPrompts))
	}
	if m.summaries != 2 {
		t.Fatalf("оборванный конспект повторяется один раз: %d", m.summaries)
	}
	types := eventTypes(t, st, f.project)
	if types[len(types)-1] != "discovery.analysis_failed" {
		t.Fatalf("события: %v", types)
	}
	// Токены оборванного ответа учтены.
	var n int
	_ = st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_records WHERE project_id=$1 AND role='analyst'`, f.project).Scan(&n)
	if n != 4 {
		t.Fatalf("usage: %d записей, ждали 4", n)
	}
}

// Общий таймаут анализа: версия failed с причиной.
func TestAnalysisTimeout(t *testing.T) {
	st := testStore(t)
	f := seed(t, st, "Анна")
	m := &fakeModel{summary: func(int, string) (string, error, bool) {
		time.Sleep(300 * time.Millisecond)
		return "", context.DeadlineExceeded, true
	}}
	job := f.job(t, m)
	(&Runner{St: st, TotalTimeout: 100 * time.Millisecond}).Run(context.Background(), job)
	run, _ := st.GetRun(context.Background(), f.project, job.Run.ID)
	if run.Status != store.RunFailed || !strings.Contains(run.Error, "не уложился") {
		t.Fatalf("таймаут: %s %q", run.Status, run.Error)
	}
}
