package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// opsFixture — проект с окружением, Релиз-инженер и Инженер сопровождения
// с моделью.
type opsFixture struct {
	st      *store.Store
	e       *Engine
	out     *capture
	env     domain.Environment
	project string
	epic    string
}

func newOpsFixture(t *testing.T) opsFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "Демо", ExecType: "ssh", Trigger: "auto",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true",
			Monitor: &domain.MonitorConfig{Enabled: true, CheckCmd: "sh scripts/smoke.sh", LogsCmd: "tail log", IntervalMin: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := st.CreateEpic(ctx, p.ID, "CRM", "контакты")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		  ('dina', 'Дина', 'devops', 'c1', 'm-devops', 80), ('gleb', 'Глеб', 'sre', 'c1', 'm-sre', 90)`); err != nil {
		t.Fatal(err)
	}
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	return opsFixture{st: st, e: e, out: out, env: env, project: p.ID, epic: ep.ID}
}

// release — смерженная задача и успешная публикация через оркестратор.
func (f opsFixture) release(t *testing.T, title, version string) store.Release {
	t.Helper()
	ctx := context.Background()
	task, err := f.st.CreateTask(ctx, f.epic, store.NewTask{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE tasks SET status='done', merged_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	d, err := f.st.EnqueueDeployment(ctx, f.env.ID, version, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE deployments SET status='verifying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.e.finishDeploy(ctx, d.ID, "", "done", ""); err != nil {
		t.Fatal(err)
	}
	rel, err := f.st.ReleaseByDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func countOpsEvents(t *testing.T, st *store.Store, projectID, state string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE project_id=$1 AND type='ops.job' AND payload->>'state'=$2`, projectID, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Очередь агента: одно задание одновременно, модель агента, usage с ролью,
// release notes без пункта не из состава.
func TestOpsQueueReleaseNotes(t *testing.T) {
	f := newOpsFixture(t)
	ctx := context.Background()
	var running, maxRunning int32
	var models sync.Map
	f.e.OpsComplete = func(ctx context.Context, connID, model, prompt string) (string, error) {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		models.Store(model, true)
		time.Sleep(50 * time.Millisecond)
		if !strings.Contains(prompt, "[rivet:release_notes]") {
			return "", errors.New("не то задание")
		}
		// Номер 999 не из состава: пункт отбрасывается.
		num := "1"
		if strings.Contains(prompt, "task-2") {
			num = "2"
		}
		return fmt.Sprintf(`{"new":[{"task":%s,"text":"Новая функция"},{"task":999,"text":"лишнее"}],"fixes":[],"other":[]}`, num), nil
	}
	r1 := f.release(t, "Импорт контактов", "aaaaaaaaaaaaaaa1")
	r2 := f.release(t, "Экспорт контактов", "aaaaaaaaaaaaaaa2")
	f.e.WaitOps()
	if maxRunning != 1 {
		t.Fatalf("у агента одновременно %d заданий", maxRunning)
	}
	if _, ok := models.Load("m-devops"); !ok {
		t.Fatal("задание шло не на модели агента")
	}
	for _, id := range []string{r1.ID, r2.ID} {
		rel, err := f.st.GetRelease(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if rel.NotesStatus != "ready" || rel.DroppedItems != 1 || rel.NotesAgent == nil || rel.NotesAgent.ID != "dina" {
			t.Fatalf("release notes: %+v", rel)
		}
		if strings.Contains(rel.NotesMD, "лишнее") {
			t.Fatal("пункт не из состава остался")
		}
	}
	var usage int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_records WHERE project_id=$1 AND role='devops' AND runner_id IS NULL`, f.project).Scan(&usage)
	if usage != 2 {
		t.Fatalf("usage с ролью devops: %d", usage)
	}
	if countOpsEvents(t, f.st, f.project, "started") != 2 || countOpsEvents(t, f.st, f.project, "done") != 2 {
		t.Fatal("события ops.job")
	}
}

// Модель отвечает ошибкой: release notes по шаблону, публикация не ждёт.
func TestReleaseNotesTemplateOnError(t *testing.T) {
	f := newOpsFixture(t)
	f.e.OpsComplete = func(context.Context, string, string, string) (string, error) {
		return "", errors.New("HTTP 500")
	}
	rel := f.release(t, "Импорт контактов", "bbbbbbbbbbbbbbb1")
	f.e.WaitOps()
	got, err := f.st.GetRelease(context.Background(), rel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NotesStatus != "template" || !strings.Contains(got.NotesMD, "Импорт контактов") {
		t.Fatalf("шаблон: %+v", got)
	}
}

// Сбой проверки от runner'а: инцидент, описание Глебом, задача-баг в
// Epic'е «Сопровождение» готова к назначению.
func TestMonitorFailureToBug(t *testing.T) {
	f := newOpsFixture(t)
	ctx := context.Background()
	f.e.OpsComplete = func(_ context.Context, _, model, prompt string) (string, error) {
		if model != "m-sre" || !strings.Contains(prompt, "[rivet:incident_description]") {
			return "", errors.New("не то задание")
		}
		return `{"summary":"Вторая страница контактов отдаёт 500","cause":"курсор берётся у пустой страницы",
			"reproduction":["импортировать 60 контактов","открыть вторую страницу"],
			"bug":{"title":"Пагинация контактов падает за границей списка","description":"Список контактов отдаёт 500 на второй странице.",
			"criteria":["Вторая страница при 60 контактах отдаёт 200"]}}`, nil
	}
	// Runner с feature monitor.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Agent: "none", Capabilities: []string{"deploy"}, Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET features='{roles,monitor}' WHERE id='deployer'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.tickMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	var job *pb.MonitorJob
	f.out.mu.Lock()
	for _, m := range f.out.sent {
		if j := m.GetMonitor(); j != nil {
			job = j
		}
	}
	f.out.mu.Unlock()
	if job == nil || job.CheckCmd != "sh scripts/smoke.sh" || job.LogsCmd != "tail log" || job.TimeoutS != 60 {
		t.Fatalf("MonitorJob: %+v", job)
	}
	// Чужой runner результат не записывает.
	if err := f.e.OnMonitorResult(ctx, "other", &pb.MonitorResult{CheckId: job.CheckId, Ok: false, Output: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnMonitorResult(ctx, "deployer", &pb.MonitorResult{CheckId: job.CheckId, Ok: false,
		Output: "GET /api/contacts?page=2 → 500", Logs: "TypeError: Cannot read properties of undefined", DurationMs: 40}); err != nil {
		t.Fatal(err)
	}
	f.e.WaitOps()
	list, _, err := f.st.ListIncidents(ctx, f.project, "active", "", "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("инциденты: %+v %v", list, err)
	}
	in, err := f.st.GetIncident(ctx, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if in.Occurrences != 1 || in.Description.Status != "ready" || in.Description.Agent == nil || in.Description.Agent.Name != "Глеб" {
		t.Fatalf("описание: %+v", in.Description)
	}
	if in.BugTask == nil || in.BugTask.Status != string(domain.TaskReady) {
		t.Fatalf("задача-баг: %+v", in.BugTask)
	}
	bug, err := f.st.GetTask(ctx, in.BugTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bug.Title != "Пагинация контактов падает за границей списка" || len(bug.Criteria) != 2 {
		t.Fatalf("баг: %q %+v", bug.Title, bug.Criteria)
	}
}

// Нет runner'а с feature monitor: unknown, инцидента нет.
func TestMonitorNoRunner(t *testing.T) {
	f := newOpsFixture(t)
	ctx := context.Background()
	if err := f.e.tickMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := f.st.GetMonitor(ctx, f.env)
	if err != nil || v.State != store.MonitorUnknown || v.Reason != noMonitorRunner {
		t.Fatalf("наблюдение без runner'а: %+v %v", v, err)
	}
}

// Публикация в окружение откладывает проверку.
func TestMonitorPostponedByDeploy(t *testing.T) {
	f := newOpsFixture(t)
	ctx := context.Background()
	d, err := f.st.EnqueueDeployment(ctx, f.env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE deployments SET status='deploying', started_at=now() WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.e.tickMonitors(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	_ = f.st.Pool.QueryRow(ctx, `SELECT next_at FROM monitor_state WHERE env_id=$1`, f.env.ID).Scan(&next)
	if time.Until(next) < 20*time.Second {
		t.Fatalf("проверка не перенесена: %s", time.Until(next))
	}
}

// Провал публикации без модели: инцидент release с хвостом лога вместо
// разбора, откат и эскалация как раньше.
func TestDeployFailureIncident(t *testing.T) {
	f := newOpsFixture(t)
	ctx := context.Background()
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET model='', connection_id='' WHERE id='dina'`); err != nil {
		t.Fatal(err)
	}
	d, err := f.st.EnqueueDeployment(ctx, f.env.ID, "ddddddddddddddd1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE deployments SET status='deploying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.e.failDeployNow(ctx, d.ID, "", "deploy: exit status 1\nerror: migration 0042 failed"); err != nil {
		t.Fatal(err)
	}
	f.e.WaitOps()
	list, _, err := f.st.ListIncidents(ctx, f.project, "active", "", "", 10)
	if err != nil || len(list) != 1 || list[0].Source != "release" {
		t.Fatalf("инцидент публикации: %+v %v", list, err)
	}
	in, err := f.st.GetIncident(ctx, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if in.Description.Status != "template" || !strings.Contains(in.Description.Bug.Description, "migration 0042") {
		t.Fatalf("описание без модели: %+v", in.Description)
	}
	var esc int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE deployment_id=$1`, d.ID).Scan(&esc)
	if esc != 1 {
		t.Fatal("эскалация провала публикации")
	}
}
