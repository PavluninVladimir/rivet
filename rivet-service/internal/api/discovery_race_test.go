package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Две декомпозиции одного Epic'а одновременно (фоновая после утверждения и
// ручная кнопка): план один, вторая получает отказ (репро ревью Opus).
func TestConcurrentDecomposeSinglePlan(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	p, _ := f.st.CreateProject(ctx, "Бюджет", "o/r", nil, f.uid.ID)
	epic, _ := f.st.CreateEpic(ctx, p.ID, "Категории", "цель")
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(ctx context.Context, prompt string) (string, error) {
		time.Sleep(300 * time.Millisecond)
		return `[{"title":"A","description":"d","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep, _ := f.st.GetEpic(ctx, epic.ID)
			_, errs[i] = f.api.decomposeEpic(ctx, ep)
		}()
	}
	wg.Wait()
	tasks, _ := f.st.ListEpicTasks(ctx, epic.ID)
	if len(tasks) != 1 {
		t.Fatalf("дубль плана: %d задач вместо 1 (ошибки %v)", len(tasks), errs)
	}
	failed := 0
	for _, err := range errs {
		if errors.Is(err, errDecomposeRunning) || errors.Is(err, errEpicHasTasks) {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("ровно одна декомпозиция должна получить отказ: %v", errs)
	}
}

// Фоновая декомпозиция бэклога натыкается на идущую ручную или на готовый
// план: это пропуск, а не провал, epic.plan_failed не пишется.
func TestBacklogDecomposeSkipsBusyEpic(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	p, _ := f.st.CreateProject(ctx, "Бюджет", "o/r", nil, f.uid.ID)
	epic, _ := f.st.CreateEpic(ctx, p.ID, "Категории", "цель")
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(context.Context, string) (string, error) {
		return `[{"title":"A","description":"d","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	release, ok := f.api.claimDecompose(epic.ID)
	if !ok {
		t.Fatal("claim")
	}
	f.api.decomposeBacklogEpic(ctx, "00000000-0000-0000-0000-000000000000", "e1", epic.ID)
	release()
	if evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: epic.ID, Type: "epic.plan_failed"}); len(evs) != 0 {
		t.Fatalf("ложный провал при идущей ручной декомпозиции: %+v", evs)
	}
	if _, err := f.st.CreateTask(ctx, epic.ID, store.NewTask{Title: "ручная"}); err != nil {
		t.Fatal(err)
	}
	f.api.decomposeBacklogEpic(ctx, "00000000-0000-0000-0000-000000000000", "e1", epic.ID)
	if evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: epic.ID, Type: "epic.plan_failed"}); len(evs) != 0 {
		t.Fatalf("ложный провал при готовом плане: %+v", evs)
	}
}

// seedNeedEpic — Epic, связанный с исследованием, с одной потребностью и
// цитатой респондента «Кирилл Л.».
func seedNeedEpic(t *testing.T, f opsFixture, quote string) domain.Epic {
	t.Helper()
	ctx := context.Background()
	p, _ := f.st.CreateProject(ctx, "CRM", "o/r", nil, f.uid.ID)
	epic, _ := f.st.CreateEpic(ctx, p.ID, "Напоминания", "цель")
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO discovery_runs (id, project_id, version, status) VALUES (gen_random_uuid(), $1, 1, 'approved')`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE epics SET discovery_run_id=(SELECT id FROM discovery_runs WHERE project_id=$2) WHERE id=$1`, epic.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	need := `{"id":"n1","title":"Напоминания","kind":"pain","persona":"p1","importance":"high","evidence":[{"interview":"i","interview_title":"Кирилл Л., менеджер ООО «ТехноПривод»","respondent_role":"Менеджер","line":3,"text":"` + quote + `"}]}`
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO epic_needs (epic_id, run_id, need_id, need)
		VALUES ($1, (SELECT id FROM discovery_runs WHERE project_id=$2), 'n1', $3)`, epic.ID, p.ID, need); err != nil {
		t.Fatal(err)
	}
	ep, _ := f.st.GetEpic(ctx, epic.ID)
	return ep
}

// Дословный пересказ цитаты в задаче — предупреждение в событии, план
// создаётся без повтора.
func TestDecomposeWarnsOnQuoteInTasks(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	quote := "клиент просил набрать во вторник после совещания, я набрал в четверг"
	epic := seedNeedEpic(t, f, quote)
	calls := 0
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(context.Context, string) (string, error) {
		calls++
		return `[{"title":"Напоминания","description":"Менеджер: ` + quote + `","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	if _, err := f.api.decomposeEpic(ctx, epic); err != nil || calls != 1 {
		t.Fatalf("цитата в задаче не отклоняет план: err=%v, запросов %d", err, calls)
	}
	evs, _ := f.st.Events(ctx, store.EventFilter{EpicID: epic.ID, Type: "epic.decomposed"})
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "Предупреждение") || evs[0].Payload["warnings"] == nil {
		t.Fatalf("предупреждение в событии: %+v", evs)
	}
}

// Персональные данные в задаче (email, имя респондента): один повтор с
// дефектом, затем план отклоняется, задачи не создаются.
func TestDecomposeRejectsPIIInTasks(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	epic := seedNeedEpic(t, f, "забываю перезвонить клиенту")
	var calls int
	var second string
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(_ context.Context, prompt string) (string, error) {
		calls++
		if calls == 2 {
			second = prompt
		}
		return `[{"title":"Напоминания","description":"Как просил Кирилл Л., писать на kirill@technoprivod.ru","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	_, err := f.api.decomposeEpic(ctx, epic)
	if err == nil || calls != 2 || !strings.Contains(second, "# Исправь прошлый ответ") {
		t.Fatalf("персональные данные в задаче: err=%v, запросов %d", err, calls)
	}
	if strings.Contains(err.Error(), "kirill@") {
		t.Fatalf("значение персональных данных в причине: %v", err)
	}
	if tasks, _ := f.st.ListEpicTasks(ctx, epic.ID); len(tasks) != 0 {
		t.Fatalf("план с персональными данными создан: %d задач", len(tasks))
	}
}

// Фоновая декомпозиция доходит до Epic'а, пока ручная держит его после
// плана: строка бэклога не остаётся «running» (репро ревью Opus, круг 3).
func TestBacklogRowNotStuckRunning(t *testing.T) {
	f := seedOps(t, true)
	ctx := context.Background()
	p, _ := f.st.CreateProject(ctx, "CRM", "o/r", nil, f.uid.ID)
	set := []store.RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r, err := f.st.CreateRun(ctx, p.ID, store.NewRun{Interviews: set, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	res := discovery.Synthesis{
		Personas: []discovery.Persona{{ID: "p1", Name: "Менеджер", Interviews: []string{"i1"}}},
		Needs: []discovery.Need{{ID: "n1", Title: "Нет дублей", Kind: "pain", Persona: "p1", Importance: "high",
			Evidence: []discovery.QuoteRef{{Interview: "i1", Line: 2, Text: "путаюсь в дублях"}}}},
		Epics: []discovery.BacklogEpic{{Key: "e1", Title: "Контакты", Goal: strings.Repeat("Цель про контакты. ", 4), Needs: []string{"n1"}, Priority: "must", Size: "M", DependsOn: []string{}}},
	}
	if _, err := f.st.FinishRunReady(ctx, r.ID, res, nil, nil); err != nil {
		t.Fatal(err)
	}
	ar, err := f.st.ApproveRun(ctx, p.ID, r.ID, f.uid.ID, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	epicID := ar.Epics[0].EpicID
	f.api.Planner.Set(planner.FromCompleter(completerFunc(func(context.Context, string) (string, error) {
		return `[{"title":"A","description":"d","criteria":["c"],"deps":[],"capabilities":["coding"],"estimate":1}]`, nil
	})), planner.Status{Source: planner.SourceCatalog, State: "ok"})
	release, ok := f.api.claimDecompose(epicID)
	if !ok {
		t.Fatal("claim")
	}
	if _, err := f.st.CreatePlan(ctx, epicID, []store.PlanTask{{NewTask: store.NewTask{Title: "A"}}}); err != nil {
		t.Fatal(err)
	}
	f.api.decomposeBacklogEpic(ctx, r.ID, "e1", epicID)
	release()
	got, _ := f.st.GetRun(ctx, p.ID, r.ID)
	if got.Decompose["e1"].Status != "done" {
		t.Fatalf("строка бэклога при готовом плане: %+v", got.Decompose["e1"])
	}
}
