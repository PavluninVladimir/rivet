package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/ops"
)

// sdlcFixture — проект с окружением под наблюдением и Epic'ом.
type sdlcFixture struct {
	s       *Store
	env     domain.Environment
	project string
	epic    string
}

func newSDLC(t *testing.T) sdlcFixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	env := seedEnv(t, s, "auto")
	env.Config.Monitor = &domain.MonitorConfig{Enabled: true, CheckCmd: "sh scripts/smoke.sh", IntervalMin: 1}
	if _, err := s.UpdateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	ep, err := s.CreateEpic(ctx, env.ProjectID, "CRM", "контакты")
	if err != nil {
		t.Fatal(err)
	}
	return sdlcFixture{s: s, env: env, project: env.ProjectID, epic: ep.ID}
}

// merged — смерженная задача с моментом merge.
func (f sdlcFixture) merged(t *testing.T, title string, at time.Time) domain.Task {
	t.Helper()
	task := mustTask(t, f.s, f.epic, NewTask{Title: title})
	if _, err := f.s.Pool.Exec(context.Background(),
		`UPDATE tasks SET status='done', merged_at=$2, pr_url='https://example.test/pr/1' WHERE id=$1`, task.ID, at); err != nil {
		t.Fatal(err)
	}
	return task
}

// deploy — публикация, созданная в момент created и завершённая успехом.
func (f sdlcFixture) deploy(t *testing.T, version string, created time.Time) string {
	t.Helper()
	ctx := context.Background()
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, version, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='verifying', created_at=$2, version_at=$2, started_at=$2 WHERE id=$1`,
		d.ID, created); err != nil {
		t.Fatal(err)
	}
	ok, err := f.s.CompleteDeployment(ctx, d.ID, "")
	if err != nil || !ok {
		t.Fatalf("CompleteDeployment: %v %v", ok, err)
	}
	return d.ID
}

// check — результат проверки наблюдения от runner'а r1.
func (f sdlcFixture) check(t *testing.T, ok bool, output, logs string) MonitorOutcome {
	t.Helper()
	ctx := context.Background()
	if err := f.s.syncMonitorStates(ctx); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("chk-%d", time.Now().UnixNano())
	if _, err := f.s.Pool.Exec(ctx, `UPDATE monitor_state SET pending_check_id='', pending_since=NULL WHERE env_id=$1`, f.env.ID); err != nil {
		t.Fatal(err)
	}
	started, err := f.s.StartMonitorCheck(ctx, f.env.ID, id, "r1")
	if err != nil || !started {
		t.Fatalf("StartMonitorCheck: %v %v", started, err)
	}
	out, err := f.s.RecordMonitorResult(ctx, MonitorResultInput{EnvID: f.env.ID, CheckID: id, OK: ok, Output: output, Logs: logs, DurationMs: 12})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f sdlcFixture) incident(t *testing.T, id string) Incident {
	t.Helper()
	in, err := f.s.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// describe — готовое описание инцидента (как от Инженера сопровождения).
func (f sdlcFixture) describe(t *testing.T, id string) string {
	t.Helper()
	inc, err := f.s.IncidentForJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	d := ops.TemplateDescription(inc.EnvName, inc.ErrorLine, inc.EvidenceText())
	bug, _, err := f.s.SetIncidentDescription(context.Background(), id, d, "ready", "gleb")
	if err != nil {
		t.Fatal(err)
	}
	return bug
}

func countEvents(t *testing.T, s *Store, projectID, typ string) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE project_id=$1 AND type=$2`, projectID, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Релиз после двух merge: состав по времени merge, прошлая версия, merge
// после создания публикации в состав не входит.
func TestReleaseComposition(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	f.merged(t, "старое", t0.Add(-time.Minute))
	f.deploy(t, "aaaaaaaaaaaaaaaa", t0)
	t7 := f.merged(t, "Импорт контактов", t0.Add(5*time.Minute))
	t9 := f.merged(t, "Падение списка", t0.Add(6*time.Minute))
	late := f.merged(t, "после публикации", t0.Add(20*time.Minute))
	dep := f.deploy(t, "bbbbbbbbbbbbbbbb", t0.Add(10*time.Minute))

	rel, err := f.s.ReleaseByDeployment(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "bbbbbbbbbbbbbbbb" || rel.PrevVersion != "aaaaaaaaaaaaaaaa" || rel.NotesStatus != "pending" {
		t.Fatalf("релиз: %+v", rel)
	}
	ids := map[string]bool{}
	for _, it := range rel.Composition {
		ids[it.TaskID] = true
	}
	if len(rel.Composition) != 2 || !ids[t7.ID] || !ids[t9.ID] || ids[late.ID] {
		t.Fatalf("состав: %+v", rel.Composition)
	}
	if n := countEvents(t, f.s, f.project, "release.created"); n != 2 {
		t.Fatalf("release.created: %d", n)
	}
	list, next, err := f.s.ListReleases(ctx, f.project, "", "", 1)
	if err != nil || len(list) != 1 || next == "" || list[0].ID != rel.ID || list[0].NotesMD != "" {
		t.Fatalf("список релизов: %+v next=%q err=%v", list, next, err)
	}
	if err := f.s.SetReleaseNotes(ctx, rel.ID, "## Новое", "ready", "dina", "m", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RequestReleaseNotes(ctx, rel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RequestReleaseNotes(ctx, rel.ID); err != ErrNotesPending {
		t.Fatalf("повторная перегенерация: %v", err)
	}
}

// Откат релиза не создаёт.
func TestRollbackNoRelease(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='verifying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FailDeployment(ctx, d.ID, "", "rolled_back", "verify: 500"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReleaseByDeployment(ctx, d.ID); err != ErrNotFound {
		t.Fatalf("релиз у отката: %v", err)
	}
}

// Первый сбой, повтор с другим временем, high после трёх подряд; токен
// в логах замаскирован; баг создаётся после описания.
func TestIncidentLifecycle(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	out := f.check(t, false, "2026-11-04T10:00:01Z GET /api/contacts?page=2 → 500", "Authorization: Bearer "+token)
	if out.State != MonitorFail || out.Failure == nil || out.Failure.Action != ops.ActionNew || !out.Failure.Describe {
		t.Fatalf("первый сбой: %+v", out)
	}
	id := out.Failure.IncidentID
	in := f.incident(t, id)
	if in.Status != "open" || in.Severity != "medium" || in.Occurrences != 1 || in.BugTask != nil {
		t.Fatalf("инцидент: %+v", in)
	}
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), token) {
		t.Fatal("токен в доказательствах инцидента")
	}
	if countEvents(t, f.s, f.project, "incident.opened") != 1 || countEvents(t, f.s, f.project, "monitor.state") != 1 {
		t.Fatal("события открытия")
	}
	out = f.check(t, false, "2026-11-04T10:05:42Z GET /api/contacts?page=2 → 500", "")
	if out.Failure.IncidentID != id || out.Failure.Action != ops.ActionRepeat {
		t.Fatalf("повтор: %+v", out.Failure)
	}
	f.check(t, false, "2026-11-04T10:06:13Z GET /api/contacts?page=2 → 500", "")
	in = f.incident(t, id)
	if in.Occurrences != 3 || in.Severity != "high" {
		t.Fatalf("после трёх провалов: %+v", in)
	}
	if countEvents(t, f.s, f.project, "monitor.state") != 1 {
		t.Fatal("monitor.state на повторах того же результата")
	}
	bugID := f.describe(t, id)
	if bugID == "" {
		t.Fatal("баг не создан после описания")
	}
	bug, err := f.s.GetTask(ctx, bugID)
	if err != nil {
		t.Fatal(err)
	}
	if bug.Kind != domain.TaskBug || bug.IncidentID != id {
		t.Fatalf("задача-баг: %+v", bug)
	}
	if strings.Contains(bug.Description, token) {
		t.Fatal("токен в описании бага")
	}
	hasMonitor := false
	for _, c := range bug.Criteria {
		if c.Text == ops.MonitorCriterion(f.env.Name) {
			hasMonitor = true
		}
	}
	if !hasMonitor {
		t.Fatalf("нет критерия наблюдения: %+v", bug.Criteria)
	}
	ep, err := f.s.GetEpic(ctx, bug.EpicID)
	if err != nil || ep.Kind != domain.EpicMaintenance || ep.Status != domain.EpicRunning {
		t.Fatalf("Epic «Сопровождение»: %+v %v", ep, err)
	}
	// Повтор при незавершённом баге второй не создаёт.
	f.check(t, false, "GET /api/contacts?page=2 → 500", "")
	var bugs int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE incident_id=$1`, id).Scan(&bugs)
	if bugs != 1 {
		t.Fatalf("багов у инцидента: %d", bugs)
	}
}

// Цикл замкнулся: баг в релизе даёт mitigated, три успеха — resolved.
func TestIncidentClosedByRelease(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "TypeError: cursor of undefined", "").Failure.IncidentID
	bugID := f.describe(t, id)
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='done', merged_at=now() - interval '1 minute' WHERE id=$1`, bugID); err != nil {
		t.Fatal(err)
	}
	dep := f.deploy(t, "cccccccccccccccc", time.Now())
	rel, err := f.s.ReleaseByDeployment(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	in := f.incident(t, id)
	if in.Status != "mitigated" || in.MitigatedReleaseID != rel.ID {
		t.Fatalf("после релиза: %+v", in)
	}
	var recheck *time.Time
	_ = f.s.Pool.QueryRow(ctx, `SELECT recheck_at FROM monitor_state WHERE env_id=$1`, f.env.ID).Scan(&recheck)
	if recheck == nil {
		t.Fatal("нет повторной проверки после публикации")
	}
	for i := 0; i < 2; i++ {
		f.check(t, true, "ok", "")
	}
	if f.incident(t, id).Status != "mitigated" {
		t.Fatal("закрыт раньше трёх успехов")
	}
	f.check(t, true, "ok", "")
	in = f.incident(t, id)
	if in.Status != "resolved" || in.Resolution != "fixed" {
		t.Fatalf("после трёх успехов: %+v", in)
	}
	if countEvents(t, f.s, f.project, "incident.resolved") != 1 {
		t.Fatal("нет incident.resolved")
	}
	// Повтор через час после закрытия переоткрывает тот же инцидент.
	out := f.check(t, false, "TypeError: cursor of undefined", "")
	if out.Failure.IncidentID != id || out.Failure.Action != ops.ActionReopen {
		t.Fatalf("повтор после закрытия: %+v", out.Failure)
	}
}

// Самоустранение: три успеха закрывают открытый инцидент, не начатый баг
// отменяется системой.
func TestIncidentSelfResolved(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "error: connection refused", "").Failure.IncidentID
	bugID := f.describe(t, id)
	for i := 0; i < 3; i++ {
		f.check(t, true, "ok", "")
	}
	in := f.incident(t, id)
	if in.Status != "resolved" || in.Resolution != "self" {
		t.Fatalf("самоустранение: %+v", in)
	}
	if st := status(t, f.s, bugID); st != domain.TaskCancelled {
		t.Fatalf("не начатый баг: %s", st)
	}
	_ = ctx
}

// Человек отменил баг: повтор баг не пересоздаёт, «создать баг заново»
// создаёт.
func TestBugCancelledByHuman(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	id := f.check(t, false, "error: 502 bad gateway", "").Failure.IncidentID
	bugID := f.describe(t, id)
	if err := f.s.ResolveTask(ctx, bugID, "", "vova", true); err != nil {
		t.Fatal(err)
	}
	in := f.incident(t, id)
	if !in.NeedsHuman || in.BugCancelledBy != "vova" {
		t.Fatalf("отмена человеком: %+v", in)
	}
	out := f.check(t, false, "error: 502 bad gateway", "")
	if out.Failure.BugTaskID != "" {
		t.Fatal("баг пересоздан после отмены человеком")
	}
	newBug, err := f.s.RebugIncident(ctx, id, "vova")
	if err != nil || newBug == "" || newBug == bugID {
		t.Fatalf("создать баг заново: %q %v", newBug, err)
	}
	if f.incident(t, id).NeedsHuman {
		t.Fatal("отметка «нужен человек» не снята")
	}
	if err := f.s.ResolveIncident(ctx, id, "", "vova"); err == nil {
		t.Fatal("пустой комментарий принят")
	}
	if err := f.s.ResolveIncident(ctx, id, "исправлено вручную", "vova"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ResolveIncident(ctx, id, "ещё раз", "vova"); err != ErrIncidentResolved {
		t.Fatalf("повторное закрытие: %v", err)
	}
	if in := f.incident(t, id); in.Resolution != "manual" || len(in.History) < 3 {
		t.Fatalf("ручное закрытие: %+v", in)
	}
}

// Шквал сбоев: шесть разных инцидентов, багов три, одна эскалация.
func TestOpsLimitFlurry(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		out := f.check(t, false, fmt.Sprintf("error: сбой модуля %c", 'a'+rune(i)), "")
		if out.Failure.Describe {
			f.describe(t, out.Failure.IncidentID)
		}
	}
	var bugs, incidents, esc int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind='bug'`).Scan(&bugs)
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE project_id=$1`, f.project).Scan(&incidents)
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE project_id=$1 AND reason='OPS_LIMIT' AND status <> 'resolved'`, f.project).Scan(&esc)
	if bugs != 3 || incidents != 6 || esc != 1 {
		t.Fatalf("багов %d, инцидентов %d, эскалаций %d", bugs, incidents, esc)
	}
}

// Повтор не обходит суточный лимит: шестой за сутки инцидент без бага, и
// повтор при свободном лимите багов баг не создаёт.
func TestDailyLimitSticky(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := f.s.Pool.Exec(ctx, `
			INSERT INTO incidents (project_id, env_id, source, fingerprint, status, resolved_at, first_seen)
			VALUES ($1,$2,'monitor',$3,'resolved',now(),now())`, f.project, f.env.ID, fmt.Sprintf("fp-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	out := f.check(t, false, "error: шестой", "")
	id := out.Failure.IncidentID
	if f.describe(t, id) != "" {
		t.Fatal("баг сверх суточного лимита")
	}
	if !f.incident(t, id).SkippedDaily {
		t.Fatal("нет отметки skipped_daily")
	}
	out = f.check(t, false, "error: шестой", "")
	if out.Failure.BugTaskID != "" {
		t.Fatal("повтор обошёл суточный лимит")
	}
}

// Гонка двух проверок с одним отпечатком: один инцидент.
func TestIncidentRace(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.s.RecordFailure(ctx, FailureInput{EnvID: f.env.ID, Source: ops.SourceMonitor,
				Output: "error: одновременно", ConsecutiveFail: 1}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n, occ int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*), max(occurrences) FROM incidents WHERE project_id=$1`, f.project).Scan(&n, &occ)
	if n != 1 || occ != 4 {
		t.Fatalf("инцидентов %d, повторов %d", n, occ)
	}
}

// Гонка двух окружений одного проекта у лимита: багов не больше трёх и одна
// эскалация.
func TestOpsLimitRaceTwoEnvs(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	env2, err := f.s.CreateEnvironment(ctx, domain.Environment{ProjectID: f.project, Name: "prod", ExecType: "ssh",
		Trigger: "manual", Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := f.env.ID
			if i%2 == 1 {
				env = env2.ID
			}
			res, err := f.s.RecordFailure(ctx, FailureInput{EnvID: env, Source: ops.SourceMonitor,
				Output: fmt.Sprintf("error: сбой модуля %c", 'a'+rune(i)), ConsecutiveFail: 1})
			if err != nil {
				t.Error(err)
				return
			}
			if res.Describe {
				f.describe(t, res.IncidentID)
			}
		}(i)
	}
	wg.Wait()
	var bugs, esc int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind='bug'`).Scan(&bugs)
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE project_id=$1 AND reason='OPS_LIMIT'`, f.project).Scan(&esc)
	if bugs != 3 || esc != 1 {
		t.Fatalf("багов %d, эскалаций %d", bugs, esc)
	}
}

// Провал публикации: инцидент release; новая версия без бага закрывает
// как самоустранившийся, выкатка прежней версии — нет.
func TestReleaseIncident(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	f.deploy(t, "good-version-0001", t0)
	d, err := f.s.EnqueueDeployment(ctx, f.env.ID, "bad-version-00002", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET status='verifying', created_at=$2, version_at=$2 WHERE id=$1`, d.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FailDeployment(ctx, d.ID, "", "rolled_back", "провал: verify: HTTP 500"); err != nil {
		t.Fatal(err)
	}
	// Инцидент записывается повтором по отметке incident_pending.
	if _, err := f.s.Pool.Exec(ctx, `UPDATE deployments SET incident_pending=true WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	res, claimed, err := f.s.RecordReleaseFailure(ctx, d.ID, "verify", "error: migration failed")
	if err != nil || !claimed || !res.Describe {
		t.Fatalf("инцидент публикации: %+v %v %v", res, claimed, err)
	}
	in := f.incident(t, res.IncidentID)
	if in.Source != "release" || in.Evidence.DeployLog == "" || in.DeploymentID != d.ID {
		t.Fatalf("инцидент: %+v", in)
	}
	// Ручная выкатка той же прежней версии провал не исправляет.
	f.deploy(t, "good-version-0001", time.Now().Add(-10*time.Minute))
	if f.incident(t, res.IncidentID).Status != "open" {
		t.Fatal("выкатка старой версии закрыла инцидент публикации")
	}
	// Три успешные проверки наблюдения его тоже не закрывают.
	for i := 0; i < 3; i++ {
		f.check(t, true, "ok", "")
	}
	if f.incident(t, res.IncidentID).Status != "open" {
		t.Fatal("проверки закрыли инцидент публикации")
	}
	f.deploy(t, "new-version-00003", time.Now())
	in = f.incident(t, res.IncidentID)
	if in.Status != "resolved" || in.Resolution != "self" {
		t.Fatalf("новая версия: %+v", in)
	}
}

// Фаза в событиях: test → review, review → development (исправление),
// merge → release (ожидает релиза).
func TestPhaseEvents(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, f.epic); err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, f.s, f.epic, NewTask{Title: "фаза"})
	proc := `{"steps":[{"id":"code","kind":"code"},{"id":"test","kind":"test"},{"id":"review","kind":"review"},{"id":"merge","kind":"merge"}]}`
	set := func(status, step string) {
		t.Helper()
		if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status=$2, step_id=$3, process=$4 WHERE id=$1`,
			task.ID, status, step, proc); err != nil {
			t.Fatal(err)
		}
	}
	ev := func(typ string) map[string]any {
		t.Helper()
		id, err := f.s.AppendEvent(ctx, EventInput{ActorKind: domain.ActorScheduler, Type: typ, ProjectID: f.project,
			EpicID: f.epic, TaskID: task.ID, Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		_ = f.s.Pool.QueryRow(ctx, `SELECT payload FROM events WHERE id=$1`, id).Scan(&raw)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		return p
	}
	set("testing", "test")
	if p := ev("task.step"); p["phase_from"] != nil || p["phase_to"] != "testing" {
		t.Fatalf("вход в тестирование: %v", p)
	}
	set("review", "review")
	if p := ev("task.step"); p["phase_from"] != "testing" || p["phase_to"] != "review" {
		t.Fatalf("test → review: %v", p)
	}
	set("review", "review")
	if p := ev("task.step"); p["phase_to"] != nil {
		t.Fatalf("фаза не менялась, а поля есть: %v", p)
	}
	set("fixing", "code")
	if p := ev("task.status"); p["phase_from"] != "review" || p["phase_to"] != "development" {
		t.Fatalf("review → development: %v", p)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='done', merged_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if p := ev("task.status"); p["phase_from"] != "development" || p["phase_to"] != "release" {
		t.Fatalf("merge → release: %v", p)
	}
}

// Агрегат: восемь фаз, задача на тестировании, инцидент в сопровождении,
// исследование без данных, Дина и Глеб в своих фазах.
func TestLoadCycle(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, f.epic); err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, f.s, f.epic, NewTask{Title: "на тестировании"})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE tasks SET status='testing', step_id='test',
		process='{"steps":[{"id":"code","kind":"code"},{"id":"test","kind":"test"}]}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, false, "error: 500", "")
	v, err := f.s.LoadCycle(ctx, f.project, "7d", time.UTC, CycleBusy{"dina": {Kind: "release", ID: "r", Title: "релиз", Action: "пишет release notes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Phases) != 8 {
		t.Fatalf("фаз: %d", len(v.Phases))
	}
	by := map[cycle.Phase]cycle.PhaseView{}
	for _, p := range v.Phases {
		by[p.ID] = p
	}
	if !by[cycle.Discovery].NoData || !by[cycle.Architecture].NoData {
		t.Fatal("исследование и архитектура без данных")
	}
	if len(by[cycle.Testing].Items) != 1 || by[cycle.Testing].Items[0].ID != task.ID {
		t.Fatalf("тестирование: %+v", by[cycle.Testing].Items)
	}
	if by[cycle.Operations].Counters.Active != 1 || v.Summary.IncidentsOpen != 1 {
		t.Fatalf("сопровождение: %+v", by[cycle.Operations].Counters)
	}
	agent := func(ph cycle.Phase, id string) *cycle.AgentView {
		for i := range by[ph].Agents {
			if by[ph].Agents[i].AgentID == id {
				return &by[ph].Agents[i]
			}
		}
		return nil
	}
	if a := agent(cycle.Release, "dina"); a == nil || a.State != cycle.AgentWorking {
		t.Fatalf("Дина в релизе: %+v", a)
	}
	if a := agent(cycle.Operations, "gleb"); a == nil || a.State != cycle.AgentIdle {
		t.Fatalf("Глеб в сопровождении: %+v", a)
	}
	if a := agent(cycle.Development, "maks"); a == nil || a.State != cycle.AgentNoRunner {
		t.Fatalf("Макс без runner'а: %+v", a)
	}
	if v.Summary.AgentsTotal != 9 || v.Summary.AgentsWorking != 1 {
		t.Fatalf("сводка: %+v", v.Summary)
	}
}

// Замер агрегата на 1000 задачах (цель меньше 200 мс).
func TestLoadCycle1000(t *testing.T) {
	if testing.Short() {
		t.Skip("замер")
	}
	f := newSDLC(t)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, f.epic); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO tasks (epic_id, title, status, step_id, process, kind)
		SELECT $1, 'задача ' || g, (ARRAY['ready','running','testing','review','done'])[1 + g % 5],
			(ARRAY['','code','test','review',''])[1 + g % 5],
			'{"steps":[{"id":"code","kind":"code"},{"id":"test","kind":"test"},{"id":"review","kind":"review"}]}', 'work'
		FROM generate_series(1, 1000) g`, f.epic); err != nil {
		t.Fatal(err)
	}
	// Исследование и архитектура на пределах спеки (fix-cycle-phase-data):
	// 50 интервью, версия с 60 потребностями, снимок с 300 решениями и
	// большим HLD.
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO discovery_interviews (project_id, title, text, sha256, chars, lines)
		SELECT $1, 'Интервью ' || g, repeat('**Р:** реплика. ', 2000), md5(g::text), 32000, 1
		FROM generate_series(1, 50) g`, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `
		INSERT INTO discovery_runs (project_id, version, status, stage, interviews, result, finished_at, approved_at)
		SELECT $1, 1, 'approved', 'done',
			(SELECT jsonb_agg(jsonb_build_object('id', id::text, 'title', title)) FROM (SELECT id, title FROM discovery_interviews WHERE project_id=$1 LIMIT 40) i),
			jsonb_build_object('needs', (SELECT jsonb_agg(jsonb_build_object('id', 'n' || g, 'title', 'Потребность ' || g, 'importance', 'high'))
				FROM generate_series(1, 60) g)), now(), now()`, f.project); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"system": map[string]string{"name": "CRM"}, "hld": strings.Repeat("HLD ", 50000),
		"decisions": func() []map[string]any {
			var out []map[string]any
			for n := 1; n <= 300; n++ {
				out = append(out, map[string]any{"number": n, "title": fmt.Sprintf("Решение %d", n), "status": "accepted",
					"date": "2026-11-01", "body": strings.Repeat("текст ", 200)})
			}
			return out
		}()})
	if _, _, err := f.s.SaveProjectArchitecture(ctx, f.project, ArchitectureSave{Status: "ok", SHA: "abc", Data: data}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.LoadCycle(ctx, f.project, "7d", time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := f.s.LoadCycle(ctx, f.project, "7d", time.UTC, nil); err != nil {
			t.Fatal(err)
		}
	}
	avg := time.Since(start) / n
	t.Logf("агрегат цикла на 1000 задачах, 50 интервью и 300 решениях: %s", avg)
	if avg > 200*time.Millisecond {
		t.Fatalf("агрегат дольше 200 мс: %s", avg)
	}
}

// Нет runner'а: unknown с причиной, инцидент не открывается.
func TestMonitorUnknown(t *testing.T) {
	f := newSDLC(t)
	ctx := context.Background()
	due, err := f.s.DueMonitors(ctx)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %+v %v", due, err)
	}
	if _, ok, err := f.s.MonitorRunner(ctx, f.env, false); ok || err != nil {
		t.Fatalf("runner без feature monitor: %v %v", ok, err)
	}
	if err := f.s.MonitorUnknown(ctx, f.env.ID, "нет runner'а с поддержкой наблюдения"); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.GetMonitor(ctx, f.env)
	if err != nil || v.State != MonitorUnknown || v.Reason == "" {
		t.Fatalf("наблюдение: %+v %v", v, err)
	}
	var n int
	_ = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM incidents`).Scan(&n)
	if n != 0 {
		t.Fatal("unknown открыл инцидент")
	}
	if due, _ := f.s.DueMonitors(ctx); len(due) != 0 {
		t.Fatal("после unknown проверка не перенесена на интервал")
	}
}
