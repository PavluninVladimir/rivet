package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ревью add-agent-roles, круг 2: недоставка не останавливает назначения
// установки, причина до отправки откладывает запуск.

// deadSender — runner dead не подключён: назначения ему не доставляются.
type deadSender struct {
	mu   sync.Mutex
	dead string
	sent map[string]int
}

func (d *deadSender) Send(runnerID string, msg *pb.PlaneMsg) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if runnerID == d.dead {
		return false
	}
	if a := msg.GetAssign(); a != nil {
		d.sent[a.TaskId]++
	}
	return true
}

func (d *deadSender) Connected(runnerID string) bool { return runnerID != d.dead }

// Головная задача на недоступном runner'е не держит остальные назначения;
// недоступный runner уходит в offline (круг 2, P2-1).
func TestUndeliveredHeadDoesNotBlockTick(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &deadSender{dead: "dead", sent: map[string]int{}}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Epic", "")
	t1, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "special", Capabilities: []string{"special"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "dead", Agent: "wrap", Capabilities: []string{"coding", "special"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "alive", Agent: "wrap", Capabilities: []string{"coding"}})
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if out.sent[t2.ID] != 1 {
		t.Fatalf("вторая задача назначена живому runner'у: sent=%d", out.sent[t2.ID])
	}
	if r, _ := st.GetRunner(ctx, "dead"); r.Status != domain.RunnerOffline {
		t.Fatalf("недоступный runner в offline: %s", r.Status)
	}
	// Недоставленное назначение не пишет «назначена» и возвращает задачу в
	// ready (ревью, круг 4).
	for _, ev := range eventsOfType(t, st, t1.ID, "task.status") {
		if ev.Payload["status"] == "running" {
			t.Fatalf("событие назначения без доставки: %+v", ev)
		}
	}
	if tk, _ := st.GetTask(ctx, t1.ID); tk.Status != domain.TaskReady {
		t.Fatalf("недоставленная задача в ready: %s", tk.Status)
	}
	if tk, _ := st.GetTask(ctx, t2.ID); tk.Status != domain.TaskRunning {
		t.Fatalf("доставленная задача в работе: %s", tk.Status)
	}
	if n := len(eventsOfType(t, st, t2.ID, "task.status")); n != 2 {
		t.Fatalf("у доставленной задачи «готова» и «назначена»: %d", n)
	}
}

// Причина до отправки (шага нет в процессе) откладывает запуск с причиной
// ожидания; до паузы он не назначается (круг 2, P2-1 и P3-4).
func TestStartErrorDefersRun(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	run, err := f.st.RunBySession(ctx, as.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	// Запуск ссылается на шаг, которого нет в снимке процесса задачи.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE task_step_runs SET step_id='ghost', runner_id=NULL, session_id=NULL WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL`); err != nil {
		t.Fatal(err)
	}
	assignsBefore := len(eventsOfType(t, f.st, f.task.ID, "task.assign"))
	a, ok, err := f.st.AssignRun(ctx, nil, nil)
	if err != nil || !ok {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	if f.e.dispatchAssigned(ctx, a) {
		t.Fatal("стадия без шага не стартует")
	}
	// «Назначен исполнитель» пишется только после доставки (круг 3, P3-6).
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.assign")); n != assignsBefore {
		t.Fatalf("событие назначения без доставки: %d → %d", assignsBefore, n)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if !strings.Contains(task.WaitReason, "шага ghost нет") {
		t.Fatalf("причина ожидания: %q", task.WaitReason)
	}
	if _, ok, err := f.st.AssignRun(ctx, nil, nil); err != nil || ok {
		t.Fatalf("отложенный запуск не назначается до паузы: %v %v", ok, err)
	}
	waits := eventsOfType(t, f.st, f.task.ID, "task.wait")
	if len(waits) != 1 || waits[0].Payload["deferred"] != true || waits[0].Payload["retry_in_s"] != float64(30) {
		t.Fatalf("событие ожидания отложенного запуска: %+v", waits)
	}
	if r, _ := f.st.GetRunner(ctx, a.Runner.ID); r.TaskID != "" {
		t.Fatal("runner свободен")
	}
}
