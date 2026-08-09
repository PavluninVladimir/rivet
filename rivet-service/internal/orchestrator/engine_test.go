package orchestrator

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Интеграционный тест конвейера (задача 2.2): фейковый runner (перехват
// Assignment через Sender) + фейковый SCM; задача проходит
// queued → ready → running → testing → review → done; blocked даёт эскалацию.

type fakeSCM struct{ merged []int }

func (f *fakeSCM) CreatePR(ctx context.Context, repo, branch, base, title, body string) (scm.PR, error) {
	return scm.PR{Number: 42, URL: "https://github.com/" + repo + "/pull/42"}, nil
}
func (f *fakeSCM) Diff(ctx context.Context, repo string, number int) (string, error) {
	return "diff --git a/x b/x", nil
}
func (f *fakeSCM) Merge(ctx context.Context, repo string, number int) error {
	f.merged = append(f.merged, number)
	return nil
}

type capture struct {
	mu   sync.Mutex
	sent []*pb.PlaneMsg
}

func (c *capture) Send(runnerID string, msg *pb.PlaneMsg) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return true
}

func (c *capture) lastAssign(t *testing.T) *pb.Assignment {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if a := c.sent[i].GetAssign(); a != nil {
			return a
		}
	}
	t.Fatal("Assignment не отправлен")
	return nil
}

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
	name := fmt.Sprintf("rivet_engine_%d", time.Now().UnixNano())
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

func TestPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)

	p, err := st.CreateProject(ctx, "demo", "owner/repo", []domain.Check{{Name: "tests", Cmd: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Epic", "цель")
	taskA, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "работает"}}})
	if err != nil {
		t.Fatal(err)
	}

	// Два runner'а: исполнитель и ревьюер (review ≠ исполнитель).
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"coding", "review"}}); err != nil {
		t.Fatal(err)
	}

	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}

	// Tick: A → ready → назначена (CODING).
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := out.lastAssign(t)
	if as.TaskId != taskA.ID || as.Stage != pb.StageResult_CODING {
		t.Fatalf("ожидали CODING для A, got %v %v", as.TaskId, as.Stage)
	}
	workerID := mustTaskRunner(t, st, taskA.ID)

	// CODING ok → testing + Assignment TESTING тому же runner'у.
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: taskA.ID, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, taskA.ID); got != domain.TaskTesting {
		t.Fatalf("want testing, got %s", got)
	}
	if as := out.lastAssign(t); as.Stage != pb.StageResult_TESTING || len(as.Checks) != 1 {
		t.Fatalf("ожидали TESTING с checks, got %v", as)
	}

	// TESTING ok → PR создан → review, исполнитель освобождён.
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: taskA.ID, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, taskA.ID)
	if task.Status != domain.TaskReview || task.PRURL == "" {
		t.Fatalf("want review+PR, got %s %q", task.Status, task.PRURL)
	}

	// Tick: review назначается ревьюеру (не исполнителю), с diff.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW || as.ExtraContext == "" {
		t.Fatalf("ожидали REVIEW с diff, got %+v", as)
	}
	task, _ = st.GetTask(ctx, taskA.ID)

	// REVIEW ok → review_passed; merge кнопкой → done.
	if err := e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: taskA.ID, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
		t.Fatal(err)
	}
	// Регрессия: после пройденного review новый Tick не должен назначать review повторно.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	reviews := 0
	for _, m := range out.sent {
		if a := m.GetAssign(); a != nil && a.Stage == pb.StageResult_REVIEW {
			reviews++
		}
	}
	out.mu.Unlock()
	if reviews != 1 {
		t.Fatalf("review назначен %d раз, ожидали 1", reviews)
	}
	if err := e.MergeTask(ctx, taskA.ID, "vladimir"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, taskA.ID); got != domain.TaskDone {
		t.Fatalf("want done, got %s", got)
	}
	if len(sc.merged) != 1 || sc.merged[0] != 42 {
		t.Fatalf("PR не смержен через SCM: %v", sc.merged)
	}
	// Epic завершён (единственная задача done).
	ep, _ := st.GetEpic(ctx, epic.ID)
	if ep.Status != domain.EpicDone {
		t.Fatalf("Epic: want done, got %s", ep.Status)
	}
}

func TestBlockedEscalation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	e := New(st, &fakeSCM{}, nil, &capture{}, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "o/r", nil)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "c"}}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if err := e.OnBlocked(ctx, "worker", &pb.BlockedQuestion{TaskId: task.ID, Question: "fail-closed или кэш?"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskBlocked {
		t.Fatalf("want blocked, got %s", got)
	}
	atts, _ := st.ListAttention(ctx)
	if len(atts) != 1 || atts[0].Reason != domain.AttBlocked {
		t.Fatalf("ожидали эскалацию BLOCKED: %+v", atts)
	}
	// Runner освобождён и может брать другие задачи.
	runners, _ := st.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}
}

func taskStatus(t *testing.T, st *store.Store, id string) domain.TaskStatus {
	t.Helper()
	task, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task.Status
}

func mustTaskRunner(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	task, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if task.RunnerID == "" {
		t.Fatal("задача без runner'а")
	}
	return task.RunnerID
}
