package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// testStore поднимает изолированную БД на локальном Postgres и накатывает миграции.
func testStore(t *testing.T) *Store {
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
	name := fmt.Sprintf("rivet_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(base)
	testURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func mustTask(t *testing.T, s *Store, epicID string, in NewTask) domain.Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), epicID, in)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func status(t *testing.T, s *Store, taskID string) domain.TaskStatus {
	t.Helper()
	task, err := s.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task.Status
}

func TestSchedulerFlow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "Epic", "цель")
	if err != nil {
		t.Fatal(err)
	}
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	b := mustTask(t, s, e.ID, NewTask{Title: "B", Deps: []string{a.ID}})
	c := mustTask(t, s, e.ID, NewTask{Title: "C", Deps: []string{b.ID}, Capabilities: []string{"coding", "frontend"}})

	// До старта Epic пересчёт ничего не двигает (пауза/план).
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, a.ID); got != domain.TaskQueued {
		t.Fatalf("до старта Epic задача должна остаться queued, got %s", got)
	}

	// Старт Epic → A становится ready.
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, ActorID: "vladimir", Type: "epic.status", Text: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, a.ID); got != domain.TaskReady {
		t.Fatalf("A: want ready, got %s", got)
	}
	if got := status(t, s, b.ID); got != domain.TaskQueued {
		t.Fatalf("B: want queued, got %s", got)
	}

	// Runner без нужной capability не получает задачу C; для A хватает coding.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	asg, ok, err := s.AssignNext(ctx)
	if err != nil || !ok {
		t.Fatalf("ожидали назначение A: ok=%v err=%v", ok, err)
	}
	if asg.Task.ID != a.ID || asg.Runner.ID != "r1" {
		t.Fatalf("назначено не то: task=%s runner=%s", asg.Task.Title, asg.Runner.ID)
	}
	if asg.Task.Branch == "" {
		t.Fatal("ветка agent/task-N не проставлена")
	}
	// Второго назначения нет: runner занят, других ready-задач нет.
	if _, ok, _ := s.AssignNext(ctx); ok {
		t.Fatal("второе назначение не должно случиться")
	}

	// A: running → testing → review → done; B становится ready.
	for _, to := range []domain.TaskStatus{domain.TaskTesting, domain.TaskReview} {
		if err := s.TransitionTask(ctx, a.ID, to,
			EventInput{ActorKind: domain.ActorScheduler, Type: "task.status"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TransitionWithRunnerRelease(ctx, a.ID, domain.TaskDone,
		EventInput{ActorKind: domain.ActorScheduler, Type: "task.status", Text: "merged"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, b.ID); got != domain.TaskReady {
		t.Fatalf("B после A=done: want ready, got %s", got)
	}

	// Недопустимый переход: done → running — ошибка + событие transition_denied.
	err = s.TransitionTask(ctx, a.ID, domain.TaskRunning,
		EventInput{ActorKind: domain.ActorSystem, Type: "task.status"}, nil)
	var bad domain.ErrBadTransition
	if !errors.As(err, &bad) {
		t.Fatalf("ожидали ErrBadTransition, got %v", err)
	}
	evs, err := s.Events(ctx, EventFilter{TaskID: a.ID, Type: "task.transition_denied"})
	if err != nil || len(evs) == 0 {
		t.Fatalf("событие о недопустимом переходе не записано: %v", err)
	}

	// B: назначение → попытки review до лимита → failed + эскалация REVIEW_LIMIT.
	asg, ok, err = s.AssignNext(ctx)
	if err != nil || !ok || asg.Task.ID != b.ID {
		t.Fatalf("ожидали назначение B: %+v ok=%v err=%v", asg.Task.Title, ok, err)
	}
	for i := 0; i < 3; i++ {
		for _, to := range []domain.TaskStatus{domain.TaskTesting, domain.TaskReview} {
			if err := s.TransitionTask(ctx, b.ID, to,
				EventInput{ActorKind: domain.ActorScheduler, Type: "task.status"}, nil); err != nil {
				t.Fatal(err)
			}
		}
		failed, err := s.ConsumeAttempt(ctx, b.ID, "issues found")
		if err != nil {
			t.Fatal(err)
		}
		wantFailed := i == 2
		if failed != wantFailed {
			t.Fatalf("попытка %d: failed=%v, want %v", i+1, failed, wantFailed)
		}
		// после ConsumeAttempt задача в fixing; следующая итерация цикла
		// проходит fixing→testing→review — оба перехода допустимы по матрице
	}
	if got := status(t, s, b.ID); got != domain.TaskFailed {
		t.Fatalf("B: want failed, got %s", got)
	}
	atts, err := s.ListAttention(ctx)
	if err != nil || len(atts) != 1 || atts[0].Reason != domain.AttReviewLimit {
		t.Fatalf("ожидали одну эскалацию REVIEW_LIMIT: %+v err=%v", atts, err)
	}

	// Каскад: C blocked из-за failed-зависимости.
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, c.ID); got != domain.TaskBlocked {
		t.Fatalf("C: want blocked (каскад), got %s", got)
	}

	// Решение человека: повтор B со сбросом счётчика, эскалация закрыта.
	if err := s.ResolveTask(ctx, b.ID, "критерии уточнены", "vladimir", false); err != nil {
		t.Fatal(err)
	}
	bTask, _ := s.GetTask(ctx, b.ID)
	if bTask.Status != domain.TaskQueued || bTask.AttemptUsed != 0 {
		t.Fatalf("B после ответа: want queued/0, got %s/%d", bTask.Status, bTask.AttemptUsed)
	}
	if atts, _ := s.ListAttention(ctx); len(atts) != 0 {
		t.Fatalf("эскалации должны быть закрыты: %+v", atts)
	}
}

func TestValidateDAG(t *testing.T) {
	ok := map[string][]string{"a": {}, "b": {"a"}, "c": {"a", "b"}}
	if err := ValidateDAG(ok); err != nil {
		t.Fatalf("валидный DAG отклонён: %v", err)
	}
	cycle := map[string][]string{"a": {"c"}, "b": {"a"}, "c": {"b"}}
	if err := ValidateDAG(cycle); err == nil {
		t.Fatal("цикл не обнаружен")
	}
	unknown := map[string][]string{"a": {"zzz"}}
	if err := ValidateDAG(unknown); err == nil {
		t.Fatal("зависимость вне плана не обнаружена")
	}
}

func TestStaleRunnerRestartsAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx); !ok || err != nil {
		t.Fatalf("назначение: ok=%v err=%v", ok, err)
	}
	// «Тишина»: сдвигаем last_seen в прошлое и помечаем протухших.
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStaleRunnersOffline(ctx, 90); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, a.ID); got != domain.TaskReady {
		t.Fatalf("задача offline-runner'а: want ready, got %s", got)
	}
	runners, _ := s.ListRunners(ctx)
	if len(runners) != 1 || runners[0].Status != domain.RunnerOffline {
		t.Fatalf("runner должен быть offline: %+v", runners)
	}
}
