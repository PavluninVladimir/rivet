package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// fastReverify — сверка и повторы без пауз на время теста.
func fastReverify(t *testing.T, delays ...time.Duration) {
	t.Helper()
	oldGuard, oldExt := guardRetryDelays, externalVerifyDelays
	guardRetryDelays = []time.Duration{0, 0}
	externalVerifyDelays = delays
	t.Cleanup(func() { guardRetryDelays, externalVerifyDelays = oldGuard, oldExt })
}

// openEscalations — открытые эскалации задачи: код и текст.
func openEscalations(t *testing.T, st *store.Store, taskID string) map[string]string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(),
		`SELECT code, message FROM attention WHERE task_id=$1 AND status <> 'resolved'`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var code, msg string
		if err := rows.Scan(&code, &msg); err != nil {
			t.Fatal(err)
		}
		out[code] = msg
	}
	return out
}

// Хостинг так и не отдал файлы смерженного PR: после последней попытки
// задача в review, событие итога, та же эскалация с понятным текстом
// вместо прежнего «сверка повторится». После рестарта сверка не
// поднимается (ревью ограничителя, P3-1 и P3-2).
func TestExternalMergeReverifyGivesUp(t *testing.T) {
	fastReverify(t, 0, 0)
	ctx := context.Background()
	sc := &fakeSCM{prFilesErr: errors.New("502 bad gateway")}
	e, _, taskID := reviewArchitectureTask(t, sc)
	task, _ := e.St.GetTask(ctx, taskID)
	p, _, _ := e.projectOf(ctx, task)
	if err := e.GuardExternalArchitectureMerge(ctx, task, p); !errors.Is(err, ErrArchitectureFilesUnavailable) {
		t.Fatalf("приём внешнего merge при сбое: %v", err)
	}
	archInflight.Wait()
	if got, _ := e.St.GetTask(ctx, taskID); got.Status != domain.TaskReview {
		t.Fatalf("задача после неудачной сверки: %s", got.Status)
	}
	esc := openEscalations(t, e.St, taskID)
	if len(esc) != 1 || !strings.Contains(esc[escFilesUnavailable], "не удалась") || !strings.Contains(esc[escFilesUnavailable], "Redeliver") {
		t.Fatalf("эскалация после последней попытки: %v", esc)
	}
	if evs := eventsOfType(t, e.St, taskID, "task.merge_failed"); evs[len(evs)-1].Payload["reason"] != "architecture_files_unverified" {
		t.Fatalf("событие итога сверки: %+v", evs[len(evs)-1].Payload)
	}
	if ids, err := e.St.PendingExternalReverify(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("сверка после итога снова в очереди: %v %v", ids, err)
	}
	// Повтор доставки на хостинге заводит сверку заново.
	sc.mu.Lock()
	sc.prFilesErr, sc.prFiles = nil, []string{"docs/architecture/architecture.yaml"}
	sc.mu.Unlock()
	fresh, _ := e.St.GetTask(ctx, taskID)
	if err := e.GuardExternalArchitectureMerge(ctx, fresh, p); err != nil {
		t.Fatalf("повтор доставки: %v", err)
	}
}

// Рестарт rivetd во время фоновой сверки: на старте она поднимается
// заново и доводит задачу до итога (ревью ограничителя, P3-1).
func TestExternalMergeReverifyResumesAfterRestart(t *testing.T) {
	fastReverify(t, 0)
	ctx := context.Background()
	sc := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}}
	e, _, taskID := reviewArchitectureTask(t, sc)
	task, _ := e.St.GetTask(ctx, taskID)
	// Приём webhook'а записал «список не получен», а горутина сверки
	// пропала вместе с процессом.
	if _, err := e.St.AppendEvent(ctx, store.EventInput{ActorKind: domain.ActorSystem, ActorID: "architecture",
		Type: "task.merge_failed", TaskID: taskID, EpicID: task.EpicID, Text: "файлы не получены",
		Payload: map[string]any{"reason": "architecture_files_unavailable", "merged": true}}); err != nil {
		t.Fatal(err)
	}
	ids, err := e.St.PendingExternalReverify(ctx)
	if err != nil || len(ids) != 1 || ids[0] != taskID {
		t.Fatalf("сверки к подъёму: %v %v", ids, err)
	}
	restarted := New(e.St, sc, nil, &capture{}, 90*time.Second)
	restarted.Adapters.Force = true
	restarted.ResumeArchitecture(ctx)
	archInflight.Wait()
	if got, _ := e.St.GetTask(ctx, taskID); got.Status != domain.TaskDone {
		t.Fatalf("задача после сверки на старте: %s", got.Status)
	}
}

// Повтор доставки во время сверки новую не заводит: сверка одна на задачу.
func TestExternalMergeReverifySingle(t *testing.T) {
	fastReverify(t, 100*time.Millisecond)
	ctx := context.Background()
	sc := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}}
	e, _, taskID := reviewArchitectureTask(t, sc)
	for i := 0; i < 3; i++ {
		e.reverifyMergedArchitecture(taskID)
	}
	archInflight.Wait()
	if sc.prFilesCalls != 1 {
		t.Fatalf("сверок файлов: %d", sc.prFilesCalls)
	}
	if got, _ := e.St.GetTask(ctx, taskID); got.Status != domain.TaskDone {
		t.Fatalf("задача после сверки: %s", got.Status)
	}
}

// Эскалации задачи описания различаются кодом: сдвиг ветки и недоступный
// хостинг дают две открытые эскалации, повтор той же ситуации заменяет
// текст, а не добавляет запись (ревью ограничителя, P3-2).
func TestArchitectureEscalationCodes(t *testing.T) {
	ctx := context.Background()
	sc := &fakeSCM{}
	e, _, taskID := reviewArchitectureTask(t, sc)
	task, _ := e.St.GetTask(ctx, taskID)
	p, _, _ := e.projectOf(ctx, task)
	for _, c := range []struct{ code, msg string }{
		{escFilesUnavailable, "первое"},
		{escHeadMoved, "сдвиг"},
		{escFilesUnavailable, "уточнённое"},
	} {
		if err := e.St.EscalateOnce(ctx, p.ID, taskID, domain.AttTestFailed, c.code, c.msg); err != nil {
			t.Fatal(err)
		}
	}
	esc := openEscalations(t, e.St, taskID)
	if len(esc) != 2 || esc[escFilesUnavailable] != "уточнённое" || esc[escHeadMoved] != "сдвиг" {
		t.Fatalf("эскалации по кодам: %v", esc)
	}
}

// Хостинг не отдал вершину ветки при назначении шага проверки: шаг
// откладывается с причиной ожидания, а не уходит с пустой проверенной
// вершиной. Вершина вернулась — шаг назначается и запоминает её (ревью
// ограничителя, P3-3).
func TestArchitectureCheckDeferredWithoutHead(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)
	e.Adapters.Force = true
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO team_agents (id, name, role_id, position) VALUES ('ada','Ада','architect',10)`); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "arch", Agent: "wrap", Capabilities: []string{"coding", "architecture"}}); err != nil {
		t.Fatal(err)
	}
	_, taskID, err := e.DescribeArchitecture(ctx, p.ID, "manual", owner.Login)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING {
		t.Fatalf("первая стадия: %s", as.Stage)
	}
	sc.mu.Lock()
	sc.headErr = errors.New("502 bad gateway")
	sc.mu.Unlock()
	res := &pb.StageResult{TaskId: taskID, SessionId: as.SessionId, Stage: as.Stage, Ok: true, Verdict: "ok"}
	if err := e.OnStageResult(ctx, "arch", res); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, taskID)
	checked, _ := st.TaskCheckedSHA(ctx, taskID)
	if out.assigns() != 1 || checked != "" || !strings.Contains(task.WaitReason, "вершина") {
		t.Fatalf("шаг проверки без вершины: назначений %d, вершина %q, причина %q", out.assigns(), checked, task.WaitReason)
	}
	var open int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE task_id=$1 AND ended_at IS NULL`, taskID).Scan(&open); err != nil || open != 0 {
		t.Fatalf("незакрытых сессий: %d %v", open, err)
	}
	sc.mu.Lock()
	sc.headErr, sc.headSHA = nil, "head-checked"
	sc.mu.Unlock()
	if _, err := st.Pool.Exec(ctx, `UPDATE task_step_runs SET not_before = now() WHERE task_id=$1 AND verdict IS NULL`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	checked, _ = st.TaskCheckedSHA(ctx, taskID)
	if out.assigns() != 2 || out.lastAssign(t).Stage != pb.StageResult_TESTING || checked != "head-checked" {
		t.Fatalf("шаг проверки после возврата вершины: назначений %d, вершина %q", out.assigns(), checked)
	}
}
