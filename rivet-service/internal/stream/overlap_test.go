package stream

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Пересечение работ: событие session.overlap в timeline обеих задач,
// повтор той же пары не дублируется (спека team-visibility).
func TestEmitOverlaps(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "owner-ov", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	taskB, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B"})
	mk := func(taskID string) string {
		id, err := st.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "scheduler", Agent: "claude-code", Depth: domain.DepthFull, Scope: "CODING"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sesA, sesB := mk(taskA.ID), mk(taskB.ID)
	if err := st.AppendSessionFiles(ctx, sesA, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendSessionFiles(ctx, sesB, []string{"shared.go", "own.go"}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{St: st}
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go", "own.go"}); err != nil {
		t.Fatal(err)
	}
	// Повтор той же пары (ещё один шаг с тем же файлом) — без новых событий.
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{taskA.ID, taskB.ID} {
		evs, err := st.Events(ctx, store.EventFilter{TaskID: taskID, Type: "session.overlap", Limit: 10})
		if err != nil || len(evs) != 1 {
			t.Fatalf("task %s: %v %d событий", taskID, err, len(evs))
		}
		if files, _ := evs[0].Payload["files"].([]any); len(files) != 1 || files[0] != "shared.go" {
			t.Fatalf("общие файлы: %+v", evs[0].Payload)
		}
	}
	// Стороны ссылаются друг на друга.
	evsA, _ := st.Events(ctx, store.EventFilter{TaskID: taskA.ID, Type: "session.overlap", Limit: 10})
	if evsA[0].Payload["other_task_id"] != taskB.ID || evsA[0].Payload["session_id"] != sesA {
		t.Fatalf("payload стороны A: %+v", evsA[0].Payload)
	}
}

// Доставка предупреждения агенту: сторона с обратным каналом получает
// Context и отмечается как предупреждённая, сторона на обёртке — нет
// (спеки team-visibility «Агент предупреждён о пересечении»,
// agent-integration «Адаптер без обратного канала»).
func TestOverlapContextDelivery(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "owner-ovc", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	taskB, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B"})
	// Исполнитель A — нативный адаптер (обратный канал есть), исполнитель
	// B — универсальная обёртка (канала нет).
	for _, r := range []domain.Runner{
		{ID: "r-full", Agent: "claude-code", Capabilities: []string{"coding"},
			Adapter: "claude-code", Depth: domain.DepthFull, ContextChannel: true},
		{ID: "r-wrap", Agent: "cli", Capabilities: []string{"coding"},
			Adapter: "wrap", Depth: domain.DepthMinimal},
	} {
		if err := st.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range []struct{ task, runner string }{{taskA.ID, "r-full"}, {taskB.ID, "r-wrap"}} {
		if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET runner_id=$2 WHERE id=$1`, x.task, x.runner); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(taskID string) string {
		id, err := st.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "scheduler", Agent: "claude-code", Depth: domain.DepthFull, Scope: "CODING"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sesA, sesB := mk(taskA.ID), mk(taskB.ID)
	if err := st.AppendSessionFiles(ctx, sesA, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendSessionFiles(ctx, sesB, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	ch := reg.Attach("r-full")
	srv := &Server{St: st, Reg: reg}
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}

	// Runner с каналом получил сообщение Context с сессией и текстом.
	select {
	case msg := <-ch:
		c := msg.GetContext()
		if c == nil || c.SessionId != sesA || c.Kind != "overlap" || c.TaskId != taskA.ID {
			t.Fatalf("сообщение контекста: %+v", msg)
		}
		if !strings.Contains(c.Text, "shared.go") || !strings.Contains(c.Text, "не ошибка инструмента") {
			t.Fatalf("текст предупреждения: %q", c.Text)
		}
	default:
		t.Fatal("runner с обратным каналом не получил контекст")
	}

	delivery := func(taskID string) map[string]any {
		evs, err := st.Events(ctx, store.EventFilter{TaskID: taskID, Type: "session.overlap", Limit: 10})
		if err != nil || len(evs) != 1 {
			t.Fatalf("событие пересечения task %s: %v %d", taskID, err, len(evs))
		}
		return evs[0].Payload
	}
	if withChannel := delivery(taskA.ID); withChannel["delivered"] != true || withChannel["delivery_reason"] != nil {
		t.Fatalf("сторона с каналом должна быть предупреждена: %+v", withChannel)
	}
	// Обёртка контекст не получает: причина названа явно.
	if noChannel := delivery(taskB.ID); noChannel["delivered"] != false || noChannel["delivery_reason"] != "no_channel" {
		t.Fatalf("сторона без канала: %+v", noChannel)
	}
}

// Недоставка не мешает пересечению: без исполнителя стадии и при
// отключённом runner'е событие пишется с причиной (спека agent-integration
// «Обратный канал контекста»).
func TestOverlapDeliveryFailures(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "owner-ovf", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	taskB, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B"})
	// A выполняет ревьюер с каналом, который сейчас не подключён;
	// у B исполнителя нет вовсе.
	for _, r := range []domain.Runner{
		{ID: "r-off", Agent: "claude-code", Capabilities: []string{"review"},
			Adapter: "claude-code", Depth: domain.DepthFull, ContextChannel: true},
		// Прошлый исполнитель задачи остаётся в runner_id и на время
		// review: контекст review-сессии не должен уйти ему.
		{ID: "r-coder", Agent: "claude-code", Capabilities: []string{"coding"},
			Adapter: "claude-code", Depth: domain.DepthFull, ContextChannel: true},
	} {
		if err := st.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE tasks SET reviewer_id='r-off', runner_id='r-coder' WHERE id=$1`, taskA.ID); err != nil {
		t.Fatal(err)
	}
	mk := func(taskID string) string {
		id, err := st.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "scheduler", Agent: "claude-code", Depth: domain.DepthFull, Scope: "REVIEW"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sesA, sesB := mk(taskA.ID), mk(taskB.ID)
	for _, ses := range []string{sesA, sesB} {
		if err := st.AppendSessionFiles(ctx, ses, []string{"shared.go"}); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewRegistry()
	coderCh := reg.Attach("r-coder")
	srv := &Server{St: st, Reg: reg}
	if err := srv.emitOverlaps(ctx, sesB, p.ID, epic.ID, []string{"shared.go"}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-coderCh:
		t.Fatalf("контекст review-сессии ушёл прошлому исполнителю: %+v", msg)
	default:
	}
	reason := func(taskID string) any {
		evs, _ := st.Events(ctx, store.EventFilter{TaskID: taskID, Type: "session.overlap", Limit: 10})
		if len(evs) != 1 {
			t.Fatalf("событие пересечения task %s: %d", taskID, len(evs))
		}
		if evs[0].Payload["delivered"] != false {
			t.Fatalf("доставки быть не должно: %+v", evs[0].Payload)
		}
		return evs[0].Payload["delivery_reason"]
	}
	if r := reason(taskA.ID); r != "runner_offline" {
		t.Fatalf("ревьюер не подключён: %v", r)
	}
	if r := reason(taskB.ID); r != "no_runner" {
		t.Fatalf("исполнителя нет: %v", r)
	}
}
