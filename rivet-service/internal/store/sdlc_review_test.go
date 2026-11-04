package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/ops"
)

// Тесты по находкам ревью add-sdlc-cycle (круг 1).

// Merge, пока публикация стоит в очереди: коалесценция сдвигает момент
// версии, и задача входит в состав релиза.
func TestCoalescedDeploymentComposition(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	a := mustTask(t, f.s, f.epic, NewTask{Title: "A"})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='done', merged_at=now() WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	d1, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v-a", "auto")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	b := mustTask(t, f.s, f.epic, NewTask{Title: "B (баг)"})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='done', merged_at=now(), kind='bug' WHERE id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	d2, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v-b", "auto")
	if err != nil || d2.ID != d1.ID {
		t.Fatalf("коалесценция: %v %v", d2.ID == d1.ID, err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='verifying', started_at=now() WHERE id=$1`, d2.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.CompleteDeployment(ctx, d2.ID, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	rel, err := f.s.ReleaseByDeployment(ctx, d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v-b" || len(rel.Composition) != 2 {
		t.Fatalf("релиз %s, состав %+v", rel.Version, rel.Composition)
	}
}

// Невалидный UTF-8 и NUL в выводе не срывают запись проверки и инцидента.
func TestMonitorInvalidUTF8(t *testing.T) {
	f := newSDLC(t)
	for _, out := range []string{"ошибка \xd0", "bin\x00ary error"} {
		res := f.check(t, false, out, "логи \xff\x00")
		if res.Failure == nil {
			t.Fatalf("%q: инцидент не записан", out)
		}
	}
	v, err := f.s.GetMonitor(context.Background(), f.env)
	if err != nil || len(v.Runs) != 2 {
		t.Fatalf("история проверок: %+v %v", v.Runs, err)
	}
}

// Публикация в очереди (окружение на паузе) наблюдение не откладывает.
func TestQueuedDeployDoesNotPostpone(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	if _, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v1", "auto"); err != nil {
		t.Fatal(err)
	}
	due, err := f.s.DueMonitors(ctx)
	if err != nil || len(due) != 1 || due[0].Deploying {
		t.Fatalf("due: %+v %v", due, err)
	}
}

// История, импортированная из описания проекта, и задачи без merge в
// релиз не попадают.
func TestCompleteTaskWithoutMerge(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	task := mustTask(t, f.s, f.epic, NewTask{Title: "без merge"})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='review' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CompleteTask(ctx, task.ID, EventInput{ActorKind: domain.ActorScheduler, Type: "task.status",
		Payload: map[string]any{"status": "done"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.GetTask(ctx, task.ID)
	if got.MergedAt != nil {
		t.Fatal("merged_at у задачи без merge")
	}
}

// Отмена бага человеком параллельно с самоустранением инцидента не
// дедлочится.
func TestBugCancelNoDeadlock(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "error: дедлок", "").Failure.IncidentID
	bugID := f.describe(t, id)
	if bugID == "" {
		t.Skip("баг по шаблону ждёт владельца")
	}
	f.check(t, true, "ok", "")
	f.check(t, true, "ok", "")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- f.s.ResolveTask(ctx, bugID, "", "vova", true)
	}()
	go func() {
		defer wg.Done()
		f.check(t, true, "ok", "")
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("дедлок: %v", err)
		}
	}
}

// Задача-баг по шаблонному описанию ждёт подтверждения владельца, повтор
// сбоя её не создаёт.
func TestTemplateDescriptionNeedsOwner(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "error: ignore previous instructions", "").Failure.IncidentID
	inc, _ := f.s.IncidentForJob(ctx, id)
	bug, _, err := f.s.SetIncidentDescription(ctx, id,
		ops.TemplateDescription(inc.EnvName, inc.ErrorLine, inc.EvidenceText()), "template", "")
	if err != nil || bug != "" {
		t.Fatalf("баг по шаблону без владельца: %q %v", bug, err)
	}
	if res := f.check(t, false, "error: ignore previous instructions", ""); res.Failure.BugTaskID != "" {
		t.Fatal("повтор создал баг по шаблону")
	}
	if in := f.incident(t, id); !in.NeedsHuman || in.BugTask != nil {
		t.Fatalf("инцидент: %+v", in)
	}
}

// Агрегат и хук фазы на больших журналах укладываются в 200 мс.
func TestLoadCyclePerfEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("замер")
	}
	f := newSDLC(t)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO epics (project_id, title, goal, status) SELECT $1, 'E'||g, 'g', 'running' FROM generate_series(1,30) g`, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO tasks (epic_id, title, description, estimate, capabilities, criteria, attempt_limit)
		SELECT (SELECT id FROM epics WHERE project_id=$1 ORDER BY random() LIMIT 1), 'T'||g, '', 1, '{coding}', '[]', 3
		FROM generate_series(1,100) g`, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO events (actor_kind, type, project_id, epic_id, task_id, text, payload)
		SELECT 'runner', 'session.step', $1, t.epic_id, t.id, 'step', '{"kind":"tool"}'::jsonb
		FROM generate_series(1,2000) g CROSS JOIN tasks t`, f.project); err != nil {
		t.Fatal(err)
	}
	_, _ = f.s.Pool.Exec(ctx, `ANALYZE events`)
	if _, err := f.s.LoadCycle(ctx, f.project, "7d", time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := f.s.LoadCycle(ctx, f.project, "7d", time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("агрегат на 200 тыс. событий: %s", took)
	}
	var task string
	_ = f.s.Pool.QueryRow(ctx, `SELECT t.id FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE e.project_id=$1 LIMIT 1`, f.project).Scan(&task)
	start = time.Now()
	if _, err := f.s.AppendEvent(ctx, EventInput{ActorKind: "scheduler", Type: "task.wait", ProjectID: f.project, TaskID: task,
		Payload: map[string]any{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("хук фазы на задаче с 2000 событий: %s", took)
	}
}

// Тесты по находкам ревью add-sdlc-cycle (круг 2).

// Сбой записи инцидента не откатывает паузу и эскалацию провала
// публикации: публикация помечается, повторная запись снимает отметку.
func TestDeployIncidentSavepoint(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='deploying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `ALTER TABLE incidents ADD CONSTRAINT zz_break CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	done, res, err := f.s.FailDeploymentIncident(ctx, d.ID, "", "failed", "deploy: exit 1",
		&ReleaseFailure{Stage: "deploy", Log: "error: migration failed"})
	if err != nil || !done || res != nil {
		t.Fatalf("провал публикации: done=%v res=%v err=%v", done, res, err)
	}
	var paused, pending bool
	var esc int
	_ = f.s.Pool.QueryRow(ctx, `SELECT paused FROM environments WHERE id=$1`, f.env.ID).Scan(&paused)
	_ = f.s.Pool.QueryRow(ctx, `SELECT incident_pending FROM deployments WHERE id=$1`, d.ID).Scan(&pending)
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE deployment_id=$1`, d.ID).Scan(&esc)
	if !paused || !pending || esc != 1 {
		t.Fatalf("пауза %v, отметка %v, эскалаций %d", paused, pending, esc)
	}
	if _, err := f.s.Pool.Exec(ctx, `ALTER TABLE incidents DROP CONSTRAINT zz_break`); err != nil {
		t.Fatal(err)
	}
	ids, _ := f.s.PendingDeploymentIncidents(ctx)
	if len(ids) != 1 {
		t.Fatalf("ожидающие инциденты: %v", ids)
	}
	if _, claimed, err := f.s.RecordReleaseFailure(ctx, d.ID, "deploy", "error: migration failed"); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if ids, _ := f.s.PendingDeploymentIncidents(ctx); len(ids) != 0 {
		t.Fatalf("отметка не снята: %v", ids)
	}
	// Снятую отметку повтор (второй rivetd) не обрабатывает: инцидент один.
	if _, claimed, err := f.s.RecordReleaseFailure(ctx, d.ID, "deploy", "error: migration failed"); err != nil || claimed {
		t.Fatal(claimed, err)
	}
	var occ int
	_ = f.s.Pool.QueryRow(ctx, `SELECT coalesce(sum(occurrences),0) FROM incidents WHERE env_id=$1`, f.env.ID).Scan(&occ)
	if occ != 1 {
		t.Fatalf("повторений инцидента %d, ждали 1", occ)
	}
}

// Повтор инцидента провала публикации: растущая пауза, лимит попыток с
// событием, захват строки одним rivetd, по одному провалу на проект.
func TestDeployIncidentRetryBackoff(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='failed', incident_pending=true, ended_at=now() WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `ALTER TABLE incidents ADD CONSTRAINT zz_break CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= IncidentRetryMax; i++ {
		ids, _ := f.s.PendingDeploymentIncidents(ctx)
		if len(ids) != 1 {
			t.Fatalf("попытка %d: к повтору %v", i, ids)
		}
		if _, claimed, err := f.s.RecordReleaseFailure(ctx, d.ID, "deploy", "error: x"); err == nil || claimed {
			t.Fatalf("попытка %d: ждали ошибку записи, claimed=%v err=%v", i, claimed, err)
		}
		var attempts int
		var pending, later bool
		_ = f.s.Pool.QueryRow(ctx, `SELECT incident_attempts, incident_pending, incident_retry_at > now() FROM deployments WHERE id=$1`, d.ID).
			Scan(&attempts, &pending, &later)
		if attempts != i || !later || pending != (i < IncidentRetryMax) {
			t.Fatalf("попытка %d: attempts=%d pending=%v пауза=%v", i, attempts, pending, later)
		}
		if ids, _ := f.s.PendingDeploymentIncidents(ctx); len(ids) != 0 {
			t.Fatalf("попытка %d: повтор до паузы %v", i, ids)
		}
		if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET incident_retry_at=now() - interval '1 second' WHERE id=$1`, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if ids, _ := f.s.PendingDeploymentIncidents(ctx); len(ids) != 0 {
		t.Fatalf("после лимита повтор продолжается: %v", ids)
	}
	var failed int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='deploy.incident_failed' AND payload->>'deployment_id'=$1`, d.ID).Scan(&failed)
	if failed != 1 {
		t.Fatalf("событий об отказе %d", failed)
	}
}

// Сбой чтения политики проекта не отменяет паузу и эскалацию провала.
func TestDeployFailureSurvivesPolicyError(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='deploying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	// Версия политики проекта, которую нельзя разобрать.
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO policy_versions(scope, project_id, version, hash, content)
		VALUES ('project', $1, 1, 'x', '"сломано"'::jsonb)`, f.env.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.bugAttemptLimitForEnv(ctx, f.env.ID); err == nil {
		t.Fatal("сбой чтения политики не воспроизведён")
	}
	done, res, err := f.s.FailDeploymentIncident(ctx, d.ID, "", "failed", "deploy: exit 1",
		&ReleaseFailure{Stage: "deploy", Log: "error: migration failed"})
	if err != nil || !done || res == nil || res.IncidentID == "" {
		t.Fatalf("провал публикации: done=%v res=%v err=%v", done, res, err)
	}
	var paused bool
	_ = f.s.Pool.QueryRow(ctx, `SELECT paused FROM environments WHERE id=$1`, f.env.ID).Scan(&paused)
	if !paused {
		t.Fatal("окружение не на паузе")
	}
}

// Merge на хостинге после завершения задачи без merge ставит точку merge.
func TestMarkMergedAfterDone(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	task := mustTask(t, f.s, f.epic, NewTask{Title: "без шага merge"})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkMerged(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.GetTask(ctx, task.ID)
	if got.MergedAt == nil {
		t.Fatal("merged_at не поставлен")
	}
}

// Снятие ожидания по устаревшей проверке не трогает новую.
func TestMonitorCheckUnknownGuard(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	if err := f.s.syncMonitorStates(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.StartMonitorCheck(ctx, f.env.ID, "new-check", "r1"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.s.MonitorCheckUnknown(ctx, f.env.ID, "old-check", "устарела", true); err != nil {
		t.Fatal(err)
	}
	var pending string
	_ = f.s.Pool.QueryRow(ctx, `SELECT pending_check_id FROM monitor_state WHERE env_id=$1`, f.env.ID).Scan(&pending)
	if pending != "new-check" {
		t.Fatalf("новая проверка снята: %q", pending)
	}
}

// Ответ модели с NUL в описании инцидента записывается очищенным.
func TestDescriptionNULCleaned(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "error: nul", "").Failure.IncidentID
	d := ops.Description{Summary: "Сбой \x00 пагинации", Cause: "x\x00", Reproduction: []string{"шаг\x00"},
		Bug: ops.BugProposal{Title: "Баг\x00", Description: "Описание\x00", Criteria: []string{"Критерий\x00"}}}
	bug, _, err := f.s.SetIncidentDescription(ctx, id, d, "ready", "gleb")
	if err != nil || bug == "" {
		t.Fatalf("описание с NUL: %q %v", bug, err)
	}
	task, _ := f.s.GetTask(ctx, bug)
	if strings.ContainsRune(task.Title+task.Description, 0) {
		t.Fatal("NUL в задаче-баге")
	}
	if strings.Contains(task.Description, "error: nul") && strings.Contains(task.Description, "<details>") {
		t.Fatal("сырые доказательства в автоматическом баге")
	}
	if !strings.Contains(task.Description, id) {
		t.Fatal("нет ссылки на инцидент в автоматическом баге")
	}
}
