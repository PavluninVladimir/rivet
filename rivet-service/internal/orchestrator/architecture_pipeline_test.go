package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// reviewArchitectureTask — задача описания в review с PR #7.
func reviewArchitectureTask(t *testing.T, sc *fakeSCM) (*Engine, domain.User, string) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	e.Adapters.Force = true
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, taskID, err := e.DescribeArchitecture(ctx, p.ID, "manual", owner.Login)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE tasks SET status='review', step_id='review', pr_url='https://github.com/o/r/pull/7', branch='agent/task-1' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	return e, owner, taskID
}

// Граница задачи описания на control plane (ревью Opus, круги 2 и 3): PR с
// файлом вне docs/architecture или с неполным списком файлов не мержится,
// задача уходит в failed с причиной. В событии не больше 50 путей.
func TestArchitectureMergeRejectsOutsideFiles(t *testing.T) {
	many := []string{"docs/architecture/architecture.yaml"}
	for i := 0; i < 60; i++ {
		many = append(many, "src/f"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+".go")
	}
	for _, tc := range []struct {
		name   string
		files  []string
		err    error
		reason string
		want   string
	}{
		{"outside", []string{"docs/architecture/architecture.yaml", "src/main.go"}, nil, "architecture_outside_docs", "src/main.go"},
		{"many", many, nil, "architecture_outside_docs", "и ещё"},
		{"truncated", nil, scm.ErrPRFilesTruncated, "architecture_files_truncated", "неполный"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sc := &fakeSCM{prFiles: tc.files, prFilesErr: tc.err}
			e, owner, taskID := reviewArchitectureTask(t, sc)
			if err := e.MergeTask(ctx, taskID, owner.ID); !errors.Is(err, ErrArchitecturePRRejected) {
				t.Fatalf("merge задачи описания: %v", err)
			}
			if len(sc.merged) != 0 {
				t.Fatalf("PR смержен вопреки границе: %v", sc.merged)
			}
			task, _ := e.St.GetTask(ctx, taskID)
			if task.Status != domain.TaskFailed {
				t.Fatalf("статус задачи: %s", task.Status)
			}
			evs := eventsOfType(t, e.St, taskID, "task.status")
			last := evs[len(evs)-1]
			if !strings.Contains(last.Text, tc.want) || last.Payload["reason"] != tc.reason {
				t.Fatalf("причина: %q %+v", last.Text, last.Payload)
			}
			f, err := e.St.TaskFailure(ctx, taskID)
			if err != nil || f == nil || f.Reason != tc.reason || len(f.Files) > 50 {
				t.Fatalf("причина провала задачи: %+v %v", f, err)
			}
			if tc.name == "many" && (len(f.Files) != 50 || f.FilesTotal != 60) {
				t.Fatalf("путей в причине: %d из %d", len(f.Files), f.FilesTotal)
			}
		})
	}
}

// Временный сбой хостинга при чтении файлов PR: повторы, затем задача
// остаётся в review с эскалацией, а не падает (ревью Opus, круг 3).
func TestArchitectureMergeFilesUnavailable(t *testing.T) {
	old := guardRetryDelays
	guardRetryDelays = []time.Duration{0, 0}
	defer func() { guardRetryDelays = old }()
	ctx := context.Background()
	sc := &fakeSCM{prFilesErr: errors.New("502 bad gateway")}
	e, owner, taskID := reviewArchitectureTask(t, sc)
	if err := e.MergeTask(ctx, taskID, owner.ID); !errors.Is(err, ErrArchitectureFilesUnavailable) {
		t.Fatalf("merge при сбое хостинга: %v", err)
	}
	if sc.prFilesCalls != 3 || len(sc.merged) != 0 {
		t.Fatalf("обращений %d, merge %v", sc.prFilesCalls, sc.merged)
	}
	if task, _ := e.St.GetTask(ctx, taskID); task.Status != domain.TaskReview {
		t.Fatalf("задача после сбоя хостинга: %s", task.Status)
	}
	var open int
	if err := e.St.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE task_id=$1 AND status <> 'resolved'`, taskID).Scan(&open); err != nil || open != 1 {
		t.Fatalf("эскалация: %d %v", open, err)
	}
}

// Ветка PR сдвинулась между проверкой файлов и merge: merge идёт с sha
// проверенной вершины, хостинг отказывает, задача возвращается на шаг
// проверки (ревью Opus, круг 3).
func TestArchitectureMergeHeadMoved(t *testing.T) {
	ctx := context.Background()
	sc := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}, prHead: "head-1", headMoved: true}
	e, owner, taskID := reviewArchitectureTask(t, sc)
	if err := e.MergeTask(ctx, taskID, owner.ID); !errors.Is(err, ErrArchitectureHeadMoved) {
		t.Fatalf("merge сдвинутой ветки: %v", err)
	}
	if sc.mergedAt != "head-1" || len(sc.merged) != 0 {
		t.Fatalf("merge: вершина %q, смержено %v", sc.mergedAt, sc.merged)
	}
	task, _ := e.St.GetTask(ctx, taskID)
	if task.StepID != archStepCheck || task.Status == domain.TaskFailed || task.Status == domain.TaskDone {
		t.Fatalf("задача после сдвига ветки: шаг %s, статус %s", task.StepID, task.Status)
	}
	// Ветка на месте: merge той же вершины проходит.
	sc2 := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}, prHead: "head-2"}
	e2, owner2, task2 := reviewArchitectureTask(t, sc2)
	if err := e2.MergeTask(ctx, task2, owner2.ID); err != nil {
		t.Fatal(err)
	}
	if sc2.mergedAt != "head-2" || len(sc2.merged) != 1 {
		t.Fatalf("merge проверенной вершины: %q %v", sc2.mergedAt, sc2.merged)
	}
	// Задача описания не входит в состав релиза: точки merge у неё нет
	// (add-sdlc-cycle).
	if got, _ := e2.St.GetTask(ctx, task2); got.Status != domain.TaskDone || got.MergedAt != nil {
		t.Fatalf("задача описания после merge: %s, точка merge %v", got.Status, got.MergedAt)
	}
}

// Сдвиг вершины больше трёх раз подряд: эскалация вместо нового круга
// проверки, повторный отказ эскалацию не дублирует (ревью Opus, финальная
// проверка).
func TestArchitectureHeadMovedLimit(t *testing.T) {
	ctx := context.Background()
	sc := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}, prHead: "head-1", headMoved: true}
	e, owner, taskID := reviewArchitectureTask(t, sc)
	task, _ := e.St.GetTask(ctx, taskID)
	for i := 0; i < headMovedLimit; i++ {
		if _, err := e.St.AppendEvent(ctx, store.EventInput{ActorKind: domain.ActorSystem, Type: "task.step",
			TaskID: taskID, EpicID: task.EpicID, Text: "возврат", Payload: map[string]any{"reason": "architecture_head_moved"}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := e.MergeTask(ctx, taskID, owner.ID); !errors.Is(err, ErrArchitectureHeadMoved) {
			t.Fatalf("merge после лимита возвратов: %v", err)
		}
	}
	got, _ := e.St.GetTask(ctx, taskID)
	if got.StepID == archStepCheck || got.Status != domain.TaskReview {
		t.Fatalf("после лимита задача ушла на проверку: шаг %s, статус %s", got.StepID, got.Status)
	}
	var open int
	if err := e.St.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE task_id=$1 AND status <> 'resolved'`, taskID).Scan(&open); err != nil || open != 1 {
		t.Fatalf("эскалаций: %d %v", open, err)
	}
}

// Merge привязан к вершине, прошедшей шаг проверки: push после проверки
// возвращает задачу на проверку без merge, даже если файлы в порядке.
func TestArchitectureMergeBoundToCheckedSHA(t *testing.T) {
	ctx := context.Background()
	sc := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}, prHead: "head-new"}
	e, owner, taskID := reviewArchitectureTask(t, sc)
	if err := e.St.SetTaskCheckedSHA(ctx, taskID, "head-checked"); err != nil {
		t.Fatal(err)
	}
	if err := e.MergeTask(ctx, taskID, owner.ID); !errors.Is(err, ErrArchitectureHeadMoved) {
		t.Fatalf("merge непроверенной вершины: %v", err)
	}
	if sc.mergedAt != "" || len(sc.merged) != 0 {
		t.Fatalf("смержено непроверенное: %q %v", sc.mergedAt, sc.merged)
	}
	if got, _ := e.St.GetTask(ctx, taskID); got.StepID != archStepCheck {
		t.Fatalf("задача не вернулась на проверку: %s", got.StepID)
	}
	// Проверенная вершина совпадает с вершиной PR: merge именно её.
	sc2 := &fakeSCM{prFiles: []string{"docs/architecture/architecture.yaml"}, prHead: "head-ok"}
	e2, owner2, task2 := reviewArchitectureTask(t, sc2)
	if err := e2.St.SetTaskCheckedSHA(ctx, task2, "head-ok"); err != nil {
		t.Fatal(err)
	}
	if err := e2.MergeTask(ctx, task2, owner2.ID); err != nil || sc2.mergedAt != "head-ok" {
		t.Fatalf("merge проверенной вершины: %v %q", err, sc2.mergedAt)
	}
}

// Внешний merge при сбое хостинга: сверка файлов повторяется в фоне и
// доводит задачу до done (файлы в порядке) или failed с merged=true
// (ревью Opus, финальная проверка).
func TestExternalMergeReverify(t *testing.T) {
	oldGuard, oldExt := guardRetryDelays, externalVerifyDelays
	guardRetryDelays, externalVerifyDelays = []time.Duration{0, 0}, []time.Duration{0, 0}
	defer func() { guardRetryDelays, externalVerifyDelays = oldGuard, oldExt }()
	for _, tc := range []struct {
		name   string
		files  []string
		status domain.TaskStatus
	}{
		{"ok", []string{"docs/architecture/architecture.yaml"}, domain.TaskDone},
		{"outside", []string{"docs/architecture/architecture.yaml", "src/main.go"}, domain.TaskFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			// Три попытки при приёме webhook'а падают, фоновая сверка проходит.
			sc := &fakeSCM{prFiles: tc.files, prFilesErr: errors.New("502"), prFilesFailN: 3}
			e, _, taskID := reviewArchitectureTask(t, sc)
			task, _ := e.St.GetTask(ctx, taskID)
			p, _, _ := e.projectOf(ctx, task)
			if err := e.GuardExternalArchitectureMerge(ctx, task, p); !errors.Is(err, ErrArchitectureFilesUnavailable) {
				t.Fatalf("приём внешнего merge при сбое: %v", err)
			}
			archInflight.Wait()
			got, _ := e.St.GetTask(ctx, taskID)
			if got.Status != tc.status {
				t.Fatalf("итог фоновой сверки: %s, ожидался %s", got.Status, tc.status)
			}
			if tc.status == domain.TaskFailed {
				if f, _ := e.St.TaskFailure(ctx, taskID); f == nil || !f.Merged || f.Reason != "architecture_outside_docs" {
					t.Fatalf("причина: %+v", f)
				}
			}
		})
	}
}

// assigns — число отправленных назначений.
func (c *capture) assigns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if m.GetAssign() != nil {
			n++
		}
	}
	return n
}

// Спека «Задача описания»: Архитектор описывает, проверка гоняет только
// гейт описания, Ревьюер принимает, merge без выкатки обновляет снимок.
// Runner без capability architecture задачу не получает, попытки не тратятся.
func TestArchitectureTaskPipeline(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)
	e.Adapters.Force = true
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", []domain.Check{{Name: "unit", Cmd: "go test ./..."}}, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range [][3]string{{"ada", "Ада", "architect"}, {"irina", "Ирина", "reviewer"}} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO team_agents (id, name, role_id, position) VALUES ($1,$2,$3,$4)`,
			a[0], a[1], a[2], (i+1)*10); err != nil {
			t.Fatal(err)
		}
	}
	enableAutoMerge(t, st, p.ID, nil)
	// Ни runner кода без architecture, ни runner ревьюера с architecture
	// описание не пишут.
	for _, r := range []domain.Runner{
		{ID: "plain", Agent: "wrap", Capabilities: []string{"coding"}},
		{ID: "revarch", Agent: "wrap", Capabilities: []string{"review", "architecture"}},
	} {
		if err := st.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	epicID, taskID, err := e.DescribeArchitecture(ctx, p.ID, "manual", owner.Login)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, taskID)
	// Runner ревьюера объявляет architecture: подсказка «обновите
	// rivet-runner» была бы неверной, причина ожидания остаётся настоящей.
	if out.assigns() != 0 || task.AttemptUsed != 0 || task.WaitReason == "" || strings.Contains(task.WaitReason, "обновите") {
		t.Fatalf("без runner'а с architecture: назначений %d, попыток %d, причина %q", out.assigns(), task.AttemptUsed, task.WaitReason)
	}

	for _, r := range []domain.Runner{
		{ID: "arch", Agent: "wrap", Capabilities: []string{"coding", "architecture"}},
		{ID: "rev", Agent: "wrap", Capabilities: []string{"review"}},
	} {
		if err := st.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Следующую стадию движок отправляет сразу по исходу (тому же runner'у)
	// или на тике планировщика (другой роли).
	stages := []string{}
	processed := 0
	for i := 0; i < 8; i++ {
		if out.assigns() == processed {
			if err := e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if out.assigns() == processed {
				break
			}
		}
		processed = out.assigns()
		as := out.lastAssign(t)
		run, err := st.RunBySession(ctx, as.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		stages = append(stages, as.Stage.String()+":"+as.GetRole().GetName()+"@"+run.RunnerID)
		if len(as.Checks) != 1 || as.Checks[0].Name != "Архитектура" || !strings.Contains(as.Checks[0].Cmd, "-force -only-docs") {
			t.Fatalf("проверки задачи описания: %+v", as.Checks)
		}
		switch as.Stage {
		case pb.StageResult_CODING:
			if as.GetRole().GetName() != "Ада" || run.RunnerID != "arch" || !strings.Contains(as.Description, "docs/architecture") {
				t.Fatalf("описание ведёт Архитектор Ада на runner'е с architecture: %+v %s", as.GetRole(), run.RunnerID)
			}
		case pb.StageResult_REVIEW:
			if as.GetRole().GetName() != "Ирина" || (run.RunnerID != "rev" && run.RunnerID != "revarch") {
				t.Fatalf("review ведёт Ирина на runner'е ревьюера: %+v %s", as.GetRole(), run.RunnerID)
			}
		}
		res := &pb.StageResult{TaskId: taskID, SessionId: as.SessionId, Stage: as.Stage, Ok: true, Verdict: "ok"}
		if err := e.OnStageResult(ctx, run.RunnerID, res); err != nil {
			t.Fatal(err)
		}
	}
	task, _ = st.GetTask(ctx, taskID)
	if task.Status != domain.TaskDone || len(sc.merged) != 1 {
		t.Fatalf("задача описания после review: %s, merge %v, стадии %v", task.Status, sc.merged, stages)
	}
	if epic, _ := st.GetEpic(ctx, epicID); epic.Status != domain.EpicDone {
		t.Fatalf("системный Epic после merge: %s", epic.Status)
	}
	out.mu.Lock()
	for _, m := range out.sent {
		if m.GetDeploy() != nil {
			t.Error("задача описания выкатывается")
		}
	}
	out.mu.Unlock()
	e.WaitArchitecture()
	if _, ok, _ := st.GetProjectArchitecture(ctx, p.ID); !ok {
		t.Fatal("merge не обновил снимок")
	}
	t.Logf("стадии: %v", stages)
}
