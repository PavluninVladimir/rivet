package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// mustOwner — владелец проектов в тестах конвейера (членство обязательно).
func mustOwner(t *testing.T, st *store.Store) domain.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), fmt.Sprintf("owner-%d", time.Now().UnixNano()), "", "pw", false)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)

	p, err := st.CreateProject(ctx, "demo", "owner/repo", []domain.Check{{Name: "tests", Cmd: "true"}}, mustOwner(t, st).ID)
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
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: taskA.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, taskA.ID); got != domain.TaskTesting {
		t.Fatalf("want testing, got %s", got)
	}
	if as := out.lastAssign(t); as.Stage != pb.StageResult_TESTING || len(as.Checks) != 1 {
		t.Fatalf("ожидали TESTING с checks, got %v", as)
	}

	// TESTING ok → PR создан → review, исполнитель освобождён.
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: taskA.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
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
	if err := e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: taskA.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_REVIEW, Ok: true}); err != nil {
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
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "c"}}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if err := e.OnBlocked(ctx, "worker", &pb.BlockedQuestion{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Question: "fail-closed или кэш?"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskBlocked {
		t.Fatalf("want blocked, got %s", got)
	}
	atts, _ := st.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttBlocked {
		t.Fatalf("ожидали эскалацию BLOCKED: %+v", atts)
	}
	// Runner освобождён и может брать другие задачи.
	runners, _ := st.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}
}

func (c *capture) countStage(stage pb.StageResult_Stage) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if a := m.GetAssign(); a != nil && a.Stage == stage {
			n++
		}
	}
	return n
}

// Провал автопроверок расходует попытку; исчерпание лимита — failed + TEST_FAILED
// (спеки task-pipeline «Автоматические проверки», orchestration «Лимит попыток»).
func TestTestFailureConsumesAttempt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", []domain.Check{{Name: "tests", Cmd: "true"}}, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", AttemptLimit: 2})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}

	// Провал #1: попытка израсходована, исправление тем же runner'ом.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: false, Detail: "2 failed"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskFixing || got.AttemptUsed != 1 || got.RunnerID != "worker" {
		t.Fatalf("после провала #1: want fixing/1/worker, got %s/%d/%q", got.Status, got.AttemptUsed, got.RunnerID)
	}
	if as := out.lastAssign(t); as.Stage != pb.StageResult_FIXING || as.ExtraContext == "" {
		t.Fatalf("ожидали FIXING с выводом проверок, got %+v", as)
	}

	// Исправление готово → проверки снова; провал #2 исчерпывает лимит.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_FIXING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: false, Detail: "still failing"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskFailed {
		t.Fatalf("want failed, got %s", got)
	}
	atts, _ := st.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttTestFailed {
		t.Fatalf("ожидали эскалацию TEST_FAILED: %+v", atts)
	}
	runners, _ := st.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}
}

// Пауза Epic останавливает конвейер на границе стадии, resume продолжает
// с той же точки (спека orchestration «Пауза и возобновление Epic»).
func TestPauseParksAtStageBoundary(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "o/r", []domain.Check{{Name: "tests", Cmd: "true"}}, mustOwner(t, st).ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	// Пауза во время coding: стадия дорабатывает, testing не стартует, runner свободен.
	pauseEv := store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicPaused, pauseEv); err != nil {
		t.Fatal(err)
	}
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskTesting || got.RunnerID != "" {
		t.Fatalf("на паузе: want testing без runner'а, got %s/%q", got.Status, got.RunnerID)
	}
	if n := out.countStage(pb.StageResult_TESTING); n != 0 {
		t.Fatalf("TESTING не должен диспатчиться на паузе, назначен %d раз", n)
	}
	if runners, _ := st.ListRunners(ctx); runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}
	// Tick на паузе ничего не назначает.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := out.countStage(pb.StageResult_TESTING); n != 0 {
		t.Fatal("Tick на паузе не должен назначать TESTING")
	}

	// Resume: конвейер продолжается со стадии testing.
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, pauseEv); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := out.countStage(pb.StageResult_TESTING); n != 1 {
		t.Fatalf("после resume ожидали одно назначение TESTING, got %d", n)
	}

	// Пауза во время testing-провала: fixing зафиксирован, диспатча нет.
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicPaused, pauseEv); err != nil {
		t.Fatal(err)
	}
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: false, Detail: "1 failed"}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskFixing || got.AttemptUsed != 1 || got.RunnerID != "" {
		t.Fatalf("на паузе: want fixing/1 без runner'а, got %s/%d/%q", got.Status, got.AttemptUsed, got.RunnerID)
	}
	if n := out.countStage(pb.StageResult_FIXING); n != 0 {
		t.Fatal("FIXING не должен диспатчиться на паузе")
	}

	// Resume: исправление назначается планировщиком.
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, pauseEv); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := out.countStage(pb.StageResult_FIXING); n != 1 {
		t.Fatalf("после resume ожидали одно назначение FIXING, got %d", n)
	}
	// Вывод упавших проверок не потерялся за время паузы.
	if as := out.lastAssign(t); as.Stage != pb.StageResult_FIXING || !strings.Contains(as.ExtraContext, "Вывод проверок") {
		t.Fatalf("fixing-агент должен получить вывод проверок, got %v %q", as.Stage, as.ExtraContext)
	}
}

// Отклонённый review возвращает задачу в fixing, и планировщик назначает
// исправление; ревьюер получает diff и отчёт автопроверок.
func TestReviewRejectionReassignsFixing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "owner/repo", []domain.Check{{Name: "tests", Cmd: "true"}}, mustOwner(t, st).ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"coding", "review"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	workerID := mustTaskRunner(t, st, task.ID)
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true, Detail: "все проверки прошли"}); err != nil {
		t.Fatal(err)
	}

	// Review назначается с diff и отчётом автопроверок (спека task-pipeline).
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("ожидали REVIEW, got %v", as.Stage)
	}
	if !strings.Contains(as.ExtraContext, "diff --git") || !strings.Contains(as.ExtraContext, "Результаты автопроверок") {
		t.Fatalf("ревьюеру не передан diff с отчётом проверок: %q", as.ExtraContext)
	}

	// Review отклонён → fixing, планировщик назначает исправление с замечаниями.
	if err := e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_REVIEW, Ok: false, Detail: "нет тестов"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskFixing || got.AttemptUsed != 1 {
		t.Fatalf("want fixing/1, got %s/%d", got.Status, got.AttemptUsed)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = out.lastAssign(t)
	if as.Stage != pb.StageResult_FIXING || !strings.Contains(as.ExtraContext, "Замечания review") {
		t.Fatalf("ожидали FIXING с замечаниями review, got %v %q", as.Stage, as.ExtraContext)
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

// Replay после reconnect: StageResult прошлой сессии не закрывает текущую
// и не расходует попытку (design add-session-visibility, решение 4).
func TestStaleSessionResultDropped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "o/r", []domain.Check{{Name: "tests", Cmd: "true"}}, mustOwner(t, st).ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s1 := out.lastAssign(t).SessionId
	if s1 == "" {
		t.Fatal("Assignment без session_id")
	}

	// CODING ok закрывает сессию s1 и открывает s2 (TESTING).
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: s1, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	s2 := out.lastAssign(t).SessionId
	if s2 == s1 {
		t.Fatal("новая стадия должна открыть новую сессию")
	}

	// Replay провала со старой сессией s1: отброшен без изменений задачи.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: s1, Stage: pb.StageResult_TESTING, Ok: false, Detail: "stale"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskTesting || got.AttemptUsed != 0 {
		t.Fatalf("stale-результат изменил задачу: %s/%d", got.Status, got.AttemptUsed)
	}
	// Пустой session_id тоже не принимается.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, Stage: pb.StageResult_TESTING, Ok: false}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskTesting {
		t.Fatalf("результат без session_id изменил задачу: %s", got)
	}

	// Актуальная сессия обрабатывается как обычно.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: s2, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskReview {
		t.Fatalf("want review, got %s", got)
	}
}

// После рестарта rivetd (карта сессий пуста) результат открытой в БД сессии
// принимается: SessionMatches поднимает её через store.OpenSession.
func TestSessionRecoveredAfterRestart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, mustOwner(t, st).ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s1 := out.lastAssign(t).SessionId

	// «Рестарт»: новый Engine с тем же store, память пуста.
	e2 := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	if err := e2.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: s1, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, st, task.ID); got != domain.TaskTesting {
		t.Fatalf("результат после рестарта потерян: %s", got)
	}
}

// Секрет в результате стадии маскируется до записи в event log и эскалацию
// (сценарий team-visibility «Секрет в результате стадии»).
func TestStageResultDetailMasked(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	// Невосстановимая ошибка с секретом в detail → событие и эскалация с маской.
	secret := "ghp_leakedFromAgent0123456789"
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{
		TaskId: task.ID, SessionId: out.lastAssign(t).SessionId,
		Stage: pb.StageResult_CODING, Ok: false, Detail: "push failed: token " + secret,
	}); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.EventFilter{TaskID: task.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Text, secret) {
			t.Fatalf("секрет утёк в event log: %q", ev.Text)
		}
	}
	atts, _ := st.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || strings.Contains(atts[0].Message, secret) {
		t.Fatalf("секрет утёк в эскалацию: %+v", atts)
	}
	if !strings.Contains(atts[0].Message, "***") {
		t.Fatalf("в эскалации нет маски: %q", atts[0].Message)
	}
}

// После отмены задачи (сессии закрыты в БД, кеш Engine ещё не чищен)
// поздний StageResult отменённой стадии игнорируется: без CreatePR и переходов.
func TestCancelledTaskStaleResultIgnored(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, mustOwner(t, st).ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s1 := out.lastAssign(t).SessionId

	// Отмена: сессии закрыты в БД (ResolveTask). Кеш Engine намеренно НЕ
	// инвалидирован — атомарный захват в EndSession обязан отбросить
	// результат даже в окне до DropSession.
	if err := st.ResolveTask(ctx, task.ID, "", "human", true); err != nil {
		t.Fatal(err)
	}

	// Поздний «tests ok» отменённой сессии не создаёт PR и не двигает задачу.
	if err := e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: task.ID, SessionId: s1, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(ctx, task.ID)
	if got.Status != domain.TaskCancelled || got.PRURL != "" {
		t.Fatalf("stale-результат после отмены изменил задачу: %s PR=%q", got.Status, got.PRURL)
	}
}
