package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

// ownerSeq — уникальные логины владельцев в пределах тестовой БД.
var ownerSeq int

// mustOwner создаёт пользователя-владельца проекта.
func mustOwner(t *testing.T, s *Store) domain.User {
	t.Helper()
	ownerSeq++
	u, err := s.CreateUser(context.Background(), fmt.Sprintf("owner-%d", ownerSeq), "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	return u
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

	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
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
	asg, ok, err := s.AssignNext(ctx, nil, nil)
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
	if _, ok, _ := s.AssignNext(ctx, nil, nil); ok {
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
	asg, ok, err = s.AssignNext(ctx, nil, nil)
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
		failed, err := s.ConsumeAttempt(ctx, b.ID, domain.AttReviewLimit, "issues found", false, 0, "")
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
	atts, err := s.ListAttention(ctx, owner.ID)
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
	if atts, _ := s.ListAttention(ctx, owner.ID); len(atts) != 0 {
		t.Fatalf("эскалации должны быть закрыты: %+v", atts)
	}
}

// Каскад блокировки транзитивен, называет первопричину, не создаёт эскалаций
// на потомков и снимается после решения первопричины (спеки orchestration
// «Сбой зависимости», human-escalation «Решение человека…»).
func TestCascadeTransitiveWithAutoUnblock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	b := mustTask(t, s, e.ID, NewTask{Title: "B", Deps: []string{a.ID}})
	c := mustTask(t, s, e.ID, NewTask{Title: "C", Deps: []string{b.ID}})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}

	// A падает: B и C блокируются транзитивно, с указанием первопричины A.
	for _, to := range []domain.TaskStatus{domain.TaskRunning, domain.TaskFailed} {
		if err := s.TransitionTask(ctx, a.ID, to,
			EventInput{ActorKind: domain.ActorScheduler, Type: "task.status"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{b.ID, c.ID} {
		task, _ := s.GetTask(ctx, id)
		if task.Status != domain.TaskBlocked || task.BlockedBy != a.ID {
			t.Fatalf("каскад: want blocked/first-cause=A, got %s blocked_by=%q", task.Status, task.BlockedBy)
		}
		if want := fmt.Sprintf("task-%d", a.Num); !strings.Contains(task.BlockReason, want) {
			t.Fatalf("причина не называет первопричину %s: %q", want, task.BlockReason)
		}
	}
	// Эскалаций на потомков нет (первопричина эскалируется своим путём).
	if atts, _ := s.ListAttention(ctx, owner.ID); len(atts) != 0 {
		t.Fatalf("каскад не должен создавать эскалаций: %+v", atts)
	}

	// Решение человека по A: потомки автоматически возвращаются в планирование.
	if err := s.ResolveTask(ctx, a.ID, "поправил условие", "vladimir", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, a.ID); got != domain.TaskReady {
		t.Fatalf("A после решения: want ready, got %s", got)
	}
	bTask, _ := s.GetTask(ctx, b.ID)
	if bTask.Status != domain.TaskQueued || bTask.BlockedBy != "" {
		t.Fatalf("B: want queued без blocked_by, got %s/%q", bTask.Status, bTask.BlockedBy)
	}
	if got := status(t, s, c.ID); got != domain.TaskQueued {
		t.Fatalf("C: want queued, got %s", got)
	}
}

// Отмена промежуточного звена рвёт каскад: потомок отменённой задачи не
// наследует failed-первопричину через неё и разблокируется.
func TestCancelBreaksCascadeChain(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil, mustOwner(t, s).ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	b := mustTask(t, s, e.ID, NewTask{Title: "B", Deps: []string{a.ID}})
	c := mustTask(t, s, e.ID, NewTask{Title: "C", Deps: []string{b.ID}})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	for _, to := range []domain.TaskStatus{domain.TaskRunning, domain.TaskFailed} {
		if err := s.TransitionTask(ctx, a.ID, to,
			EventInput{ActorKind: domain.ActorScheduler, Type: "task.status"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, c.ID); got != domain.TaskBlocked {
		t.Fatalf("C до отмены B: want blocked, got %s", got)
	}

	// Отмена B: C больше не потомок failed A, зависимость от B снята → ready.
	if err := s.ResolveTask(ctx, b.ID, "", "vladimir", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	cTask, _ := s.GetTask(ctx, c.ID)
	if cTask.Status != domain.TaskReady || cTask.BlockedBy != "" {
		t.Fatalf("C после отмены B: want ready без blocked_by, got %s/%q", cTask.Status, cTask.BlockedBy)
	}
}

// Отмена выполняющейся задачи освобождает её runner'а.
func TestCancelRunningTaskFreesRunner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil, mustOwner(t, s).ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	_ = s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "wrap", Capabilities: []string{"coding"}})
	if _, ok, err := s.AssignNext(ctx, nil, nil); !ok || err != nil {
		t.Fatalf("назначение: ok=%v err=%v", ok, err)
	}
	if err := s.ResolveTask(ctx, a.ID, "", "vladimir", true); err != nil {
		t.Fatal(err)
	}
	task, _ := s.GetTask(ctx, a.ID)
	if task.Status != domain.TaskCancelled || task.RunnerID != "" {
		t.Fatalf("want cancelled без runner'а, got %s/%q", task.Status, task.RunnerID)
	}
	runners, _ := s.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle || runners[0].TaskID != "" {
		t.Fatalf("runner должен быть idle без задачи: %+v", runners[0])
	}
}

// Отмена задачи исключает её из DAG: потомки выполняются, Epic доходит до done
// (спека human-escalation «Решение человека возвращает задачу в работу»).
func TestCancelledDepExcludedFromDAG(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil, mustOwner(t, s).ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A"})
	b := mustTask(t, s, e.ID, NewTask{Title: "B", Deps: []string{a.ID}})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.ResolveTask(ctx, a.ID, "", "vladimir", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, b.ID); got != domain.TaskReady {
		t.Fatalf("B после отмены A: want ready (зависимость снята), got %s", got)
	}

	// B доходит до done — Epic завершается, отменённая A не мешает.
	for _, to := range []domain.TaskStatus{domain.TaskRunning, domain.TaskTesting, domain.TaskReview, domain.TaskDone} {
		if err := s.TransitionTask(ctx, b.ID, to,
			EventInput{ActorKind: domain.ActorScheduler, Type: "task.status"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	epic, _ := s.GetEpic(ctx, e.ID)
	if epic.Status != domain.EpicDone {
		t.Fatalf("Epic: want done, got %s", epic.Status)
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
	dup := map[string][]string{"a": {}, "b": {"a", "a"}}
	if err := ValidateDAG(dup); err == nil {
		t.Fatal("дубль зависимости не обнаружен (упал бы на PK task_deps)")
	}
}

// Потеря runner'а расходует попытку; исчерпание лимита — failed + RUNNER_LOST
// (спеки orchestration «Лимит попыток», human-escalation «Причины эскалации»).
func TestStaleRunnerConsumesAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	a := mustTask(t, s, e.ID, NewTask{Title: "A", AttemptLimit: 2})
	if err := s.TransitionEpic(ctx, e.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx, nil, nil); !ok || err != nil {
		t.Fatalf("назначение: ok=%v err=%v", ok, err)
	}
	// «Тишина»: сдвигаем last_seen в прошлое и помечаем протухших.
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkStaleRunnersOffline(ctx, 90); err != nil {
		t.Fatal(err)
	}
	task, _ := s.GetTask(ctx, a.ID)
	if task.Status != domain.TaskReady || task.AttemptUsed != 1 {
		t.Fatalf("после потери runner'а: want ready/1, got %s/%d", task.Status, task.AttemptUsed)
	}
	runners, _ := s.ListRunners(ctx)
	if len(runners) != 1 || runners[0].Status != domain.RunnerOffline {
		t.Fatalf("runner должен быть offline: %+v", runners)
	}
	if atts, _ := s.ListAttention(ctx, owner.ID); len(atts) != 0 {
		t.Fatalf("попытки остались — эскалации быть не должно: %+v", atts)
	}

	// Вторая потеря исчерпывает лимит: failed + эскалация RUNNER_LOST.
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "wrap", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AssignNext(ctx, nil, nil); !ok || err != nil {
		t.Fatalf("повторное назначение: ok=%v err=%v", ok, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkStaleRunnersOffline(ctx, 90); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, a.ID); got != domain.TaskFailed {
		t.Fatalf("после исчерпания: want failed, got %s", got)
	}
	atts, _ := s.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttRunnerLost {
		t.Fatalf("ожидали эскалацию RUNNER_LOST: %+v", atts)
	}
}

// Находки ревью: деактивация обязана сериализоваться с правкой членства по
// проекту, причём по всем проектам пользователя, а не только там, где он уже
// владелец (иначе параллельное повышение до владельца пройдёт мимо проверки).
func TestOwnerDeactivationSerializesWithMembership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Админ установки отдельно: иначе сработала бы защита последнего админа
	// и тест проверял бы не то правило.
	if _, err := s.CreateUser(ctx, "root", "", "root-secret", true); err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob", "", "pw-bob-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "p", "o/r", nil, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	// alice пока обычный участник: её деактивация всё равно обязана ждать
	// правку членства этого проекта.
	if err := s.AddMember(ctx, p.ID, "alice", domain.RoleMember); err != nil {
		t.Fatal(err)
	}

	// Транзакция, имитирующая правку членства проекта (SetMemberRole,
	// RemoveMember берут ту же сериализацию).
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockProjects(ctx, tx, []string{p.ID}); err != nil {
		t.Fatal(err)
	}

	yes := true
	done := make(chan error, 1)
	go func() {
		_, err := s.SetUserState(ctx, alice.ID, nil, &yes, nil, "root")
		done <- err
	}()

	select {
	case err := <-done:
		_ = tx.Rollback(ctx)
		t.Fatalf("деактивация прошла мимо сериализации по проекту: %v", err)
	case <-time.After(700 * time.Millisecond):
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("деактивация после освобождения блокировки: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("деактивация не завершилась после освобождения блокировки")
	}

	// Инвариант цел: у проекта остался активный владелец.
	var activeOwners int
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM project_members m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=$1 AND m.role='owner' AND NOT u.disabled`, p.ID).Scan(&activeOwners); err != nil {
		t.Fatal(err)
	}
	if activeOwners == 0 {
		t.Fatal("проект остался без активного владельца")
	}
}

// Находка ревью: пути, делающие пользователя владельцем, обязаны ждать
// деактивацию (общий порядок: строка users, затем проект).
func TestDisabledUserCannotBecomeOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "root", "", "root-secret", true); err != nil {
		t.Fatal(err)
	}
	alice, err := s.CreateUser(ctx, "alice", "", "pw-alice-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob", "", "pw-bob-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "p", "o/r", nil, alice.ID)
	if err != nil {
		t.Fatal(err)
	}

	yes := true
	if _, err := s.SetUserState(ctx, bob.ID, nil, &yes, nil, "root"); err != nil {
		t.Fatal(err)
	}
	// Отключённого не берут в проект ни участником, ни владельцем.
	if err := s.AddMember(ctx, p.ID, "bob", domain.RoleMember); !errors.Is(err, ErrConflict) {
		t.Fatalf("ожидался отказ добавить отключённого, получено %v", err)
	}
	// Отключённый создатель не создаёт проект.
	if _, err := s.CreateProject(ctx, "p2", "o/r2", nil, bob.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("ожидался отказ создать проект отключённым, получено %v", err)
	}
	// Уже состоящего в проекте участника нельзя повысить после отключения.
	carol, err := s.CreateUser(ctx, "carol", "", "pw-carol-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, p.ID, "carol", domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserState(ctx, carol.ID, nil, &yes, nil, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberRole(ctx, p.ID, "carol", domain.RoleOwner); !errors.Is(err, ErrConflict) {
		t.Fatalf("ожидался отказ повысить отключённого, получено %v", err)
	}
}
