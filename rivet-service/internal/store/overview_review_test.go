package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/overview"
)

// Окна store для отчёта за прошедший период отсчитываются от среза, а не
// от момента запроса (ревью круг 1, P2-2).
func TestOverviewWindowStartAnchoredOnCut(t *testing.T) {
	th := overview.DefaultThresholds()
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	per, _ := overview.CustomPeriod(from, to, time.UTC)
	want := to.Add(-time.Duration(th.VelocityWindowDays) * 24 * time.Hour)
	for _, now := range []time.Time{to.Add(2 * time.Hour), to.Add(30 * 24 * time.Hour)} {
		got := overviewWindowStart(OverviewQuery{Now: now, Period: per, Loc: time.UTC, Report: true}, th)
		if !got.Equal(want) {
			t.Fatalf("now=%s: окно с %s, ждали начало окна темпа на срез %s", now, got, want)
		}
	}
	// Текущий обзор: срез — now.
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cur, _ := overview.NewPeriod("7d", now, time.UTC)
	if got := overviewWindowStart(OverviewQuery{Now: now, Period: cur, Loc: time.UTC}, th); !got.Equal(now.Add(-14 * 24 * time.Hour)) {
		t.Fatalf("окно текущего обзора: %s", got)
	}
}

// Отчёт за закрытый период побайтно одинаков, когда бы его ни запросили:
// окна от среза, сессии с запуска Epic'ов, работавших на срез (P2-2).
func TestOverviewPastReportStability(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "P", "o/r", nil, owner.ID)
	e, _ := s.CreateEpic(ctx, p.ID, "E", "")
	d := func(m time.Month, day, h int) time.Time { return time.Date(2026, m, day, h, 0, 0, 0, time.UTC) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`UPDATE epics SET status='done' WHERE id=$1`, e.ID)
	exec(`UPDATE epics SET created_at=$2, started_at=$3, done_at=$4 WHERE id=$1`, e.ID, d(9, 20, 0), d(9, 27, 0), d(10, 20, 0))
	var ids []string
	for i, at := range []time.Time{d(10, 2, 0), d(10, 6, 0), d(10, 8, 0), d(10, 19, 0)} {
		est := 2
		if i == 3 {
			est = 4
		}
		task := mustTask(t, s, e.ID, NewTask{Title: "T", Estimate: est})
		ids = append(ids, task.ID)
		exec(`UPDATE tasks SET status='done' WHERE id=$1`, task.ID)
		exec(`UPDATE tasks SET created_at=$2, started_at=$3, done_at=$4 WHERE id=$1`, task.ID, d(9, 20, 0), d(9, 28, 0), at)
	}
	for i, iv := range [][2]time.Time{{d(9, 28, 1), d(9, 28, 23)}, {d(10, 5, 10), d(10, 5, 12)}, {d(10, 7, 10), d(10, 7, 12)}} {
		exec(`INSERT INTO sessions (task_id, driver_kind, agent, started_at, ended_at) VALUES ($1,'scheduler','rivet',$2,$3)`, ids[i], iv[0], iv[1])
	}
	from, to := d(10, 5, 0), d(10, 12, 0)
	per, _ := overview.CustomPeriod(from, to, time.UTC)
	var reports []string
	for _, now := range []time.Time{to.Add(2 * time.Hour), to.Add(30 * 24 * time.Hour)} {
		in, err := s.OverviewInput(ctx, OverviewQuery{ProjectID: p.ID, Now: now, Period: per, Loc: time.UTC, Report: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(in.Sessions) != 3 {
			t.Fatalf("now=%s: сессий %d, ждали 3 (с запуска Epic'а, работавшего на срез)", now, len(in.Sessions))
		}
		reports = append(reports, overview.Report(in))
	}
	if reports[0] != reports[1] {
		a, b := strings.Split(reports[0], "\n"), strings.Split(reports[1], "\n")
		for i := range a {
			if i < len(b) && a[i] != b[i] {
				t.Errorf("строка %d:\n  1: %s\n  2: %s", i, a[i], b[i])
			}
		}
		t.Fatal("отчёт за закрытый период зависит от момента запроса")
	}
}

// Отчёт берёт полную историю usage только у Epic'ов, которые могут в него
// попасть: завершённый до начала периода Epic не тянет свои записи (P3-5).
func TestOverviewReportUsageScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "P", "o/r", nil, owner.ID)
	old, _ := s.CreateEpic(ctx, p.ID, "старый", "")
	cur, _ := s.CreateEpic(ctx, p.ID, "текущий", "")
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	// Статус отдельно: триггер ставит done_at на переходе в done.
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='done' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET done_at=$2 WHERE id=$1`, old.ID, from.Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	longAgo := from.Add(-60 * 24 * time.Hour)
	for i, ep := range []string{old.ID, cur.ID} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO usage_records (ts, project_id, epic_id, model, tokens_in, duration_s, source_msg_id)
			VALUES ($1,$2,$3,'m',100,1,$4)`, longAgo, p.ID, ep, "u"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	per, _ := overview.CustomPeriod(from, to, time.UTC)
	in, err := s.OverviewInput(ctx, OverviewQuery{ProjectID: p.ID, Now: to.Add(time.Hour), Period: per, Loc: time.UTC, Report: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Usage) != 1 || in.Usage[0].EpicID != cur.ID {
		t.Fatalf("записи usage отчёта: %+v", in.Usage)
	}
}

// Недоставленное назначение снимает время начала, которое само поставило,
// а серия откладываний сохраняет начало ожидания runner'а (P3-4).
func TestUndoAssignAndDeferSeries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	var startedAt, waitSince *time.Time
	state := func(id string) (string, *time.Time, *time.Time) {
		t.Helper()
		var st string
		var started, since *time.Time
		if err := s.Pool.QueryRow(ctx, `SELECT status, started_at, wait_since FROM tasks WHERE id=$1`, id).Scan(&st, &started, &since); err != nil {
			t.Fatal(err)
		}
		return st, started, since
	}
	// Цикл: назначение, недоставка, откладывание с причиной. Пауза
	// откладывания 1 мс, чтобы следующий цикл мог назначить запуск сразу.
	cycle := func() (*PendingAssign, bool) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		a, ok, err := s.AssignRun(ctx, nil, nil)
		if err != nil || !ok || a.Pending == nil || a.Pending.To == "" {
			t.Fatalf("назначение из ready: %v %v %+v", ok, err, a.Pending)
		}
		if _, started, _ := state(a.Task.ID); started == nil {
			t.Fatal("назначение ставит started_at")
		}
		if err := s.UndoAssign(ctx, a.Task.ID, a.Pending); err != nil {
			t.Fatal(err)
		}
		first, _, err := s.DeferRun(ctx, a.Run.ID, a.Task.ID, time.Millisecond, time.Millisecond, "нет учётных данных")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FreeRunnerFor(ctx, a.Runner.ID, a.Task.ID); err != nil {
			t.Fatal(err)
		}
		return a.Pending, first
	}
	p1, first := cycle()
	if !p1.FirstStart || !first {
		t.Fatalf("первое назначение и первое откладывание: %+v %v", p1, first)
	}
	st, startedAt, seriesStart := state(tasks[0].ID)
	if st != string(domain.TaskReady) || startedAt != nil || seriesStart == nil {
		t.Fatalf("после недоставки: статус %s, started_at %v, wait_since %v", st, startedAt, seriesStart)
	}
	if _, first := cycle(); first {
		t.Fatal("второе откладывание — продолжение серии")
	}
	if _, _, waitSince = state(tasks[0].ID); waitSince == nil || !waitSince.Equal(*seriesStart) {
		t.Fatalf("начало ожидания серии: %v, ждали %v", waitSince, seriesStart)
	}
	// Задача, которая уже работала, после недоставки сохраняет started_at.
	earlier := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET started_at=$2 WHERE id=$1`, tasks[0].ID, earlier); err != nil {
		t.Fatal(err)
	}
	if p3, _ := cycle(); p3.FirstStart {
		t.Fatal("повторное назначение не первое")
	}
	if _, startedAt, _ = state(tasks[0].ID); startedAt == nil || !startedAt.Equal(earlier) {
		t.Fatalf("прежнее started_at: %v, ждали %v", startedAt, earlier)
	}
}

// Начало серии откладываний не зависит от событий (ревью круг 2, P3-2):
// событие первого откладывания пишется вне транзакции и может потеряться,
// тогда старое событие прошлой серии не должно сдвигать начало ожидания.
func TestDeferSeriesIgnoresOldEvents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 1)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "r1", Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	enterCode(t, s, tasks, nil)
	cycle := func() (RunAssignment, bool) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		a, ok, err := s.AssignRun(ctx, nil, nil)
		if err != nil || !ok {
			t.Fatalf("назначение: %v %v", ok, err)
		}
		if err := s.UndoAssign(ctx, a.Task.ID, a.Pending); err != nil {
			t.Fatal(err)
		}
		first, _, err := s.DeferRun(ctx, a.Run.ID, a.Task.ID, time.Millisecond, time.Millisecond, "нет учётных данных")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FreeRunnerFor(ctx, a.Runner.ID, a.Task.ID); err != nil {
			t.Fatal(err)
		}
		return a, first
	}
	waitSince := func() *time.Time {
		t.Helper()
		var v *time.Time
		if err := s.Pool.QueryRow(ctx, `SELECT wait_since FROM tasks WHERE id=$1`, tasks[0].ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Серия 1 двое суток назад: событие откладывания, затем доставка.
	a, first := cycle()
	if !first {
		t.Fatal("первое откладывание")
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO events (ts, actor_kind, type, task_id, payload)
		VALUES ($1,'scheduler','task.wait',$2,'{"deferred":true}')`, old, tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDelivered(ctx, a.Run.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Серия 2: событие первого откладывания потеряно.
	if _, first := cycle(); !first {
		t.Fatal("первое откладывание серии 2")
	}
	since1 := waitSince()
	if since1 == nil || since1.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("начало серии 2: %v", since1)
	}
	if _, first := cycle(); first {
		t.Fatal("второе откладывание не первое")
	}
	if since2 := waitSince(); since2 == nil || !since2.Equal(*since1) {
		t.Fatalf("ожидание серии 2 после второго откладывания с %v, ждали %v (событие серии 1: %v)", since2, since1, old)
	}
}
