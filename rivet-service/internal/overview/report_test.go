package overview

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "перезаписать golden-файлы отчёта")

// reportFixture — неделя проекта CRM: Epic «Сделки» в работе с целевой
// датой раньше прогноза, Epic «Контакты» завершён в периоде, эскалации,
// конфликт merge, возвраты тестировщика и ревьюера, выкатки с откатом.
func reportFixture(t *testing.T) Input {
	t.Helper()
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	p, err := CustomPeriod(end.Add(-7*24*time.Hour), end, msk)
	if err != nil {
		t.Fatal(err)
	}
	h := func(d float64) time.Time { return end.Add(time.Duration(d * float64(time.Hour))) }
	in := Input{
		ProjectID: "p1", ProjectName: "CRM", Period: p, Location: msk,
		// Снимок сразу после конца периода: отчёт «текущий».
		Now: end.Add(30 * time.Second), Live: true,
		Roles: []Role{
			{ID: "analyst", Title: "Аналитик", Color: "slate"},
			{ID: "developer", Title: "Разработчик", Color: "blue"},
			{ID: "tester", Title: "Тестировщик", Color: "teal"},
			{ID: "reviewer", Title: "Ревьюер", Color: "rose"},
		},
		DailyTokenBudget: i64(5_000_000),
		MonthlyBudgetUSD: f64(40),
	}
	in.Epics = []Epic{
		{ID: "e-deals", Title: "Сделки и воронка", Status: EpicRunning, StartedAt: tp(h(-60)), DueAt: tp(h(1)),
			TokenBudget: i64(3_000_000), Spend: Totals{Records: 6, CostRecords: 6, TokensIn: 1_400_000, TokensOut: 90_000, CostUSD: 9.4, DurationS: 2400}},
		{ID: "e-contacts", Title: "Компании | контакты", Status: EpicDone, StartedAt: tp(h(-150)), DoneAt: tp(h(-70)),
			Spend: Totals{Records: 5, CostRecords: 4, TokensIn: 1_100_000, TokensOut: 70_000, CostUSD: 7.25, DurationS: 1800}},
		{ID: "e-dash", Title: "Дашборд продаж", Status: EpicPlanned, CreatedAt: h(-3), PlannedAt: tp(h(-2))},
	}
	task := func(id, epic string, num, est int, status string, done float64) Task {
		tk := Task{ID: id, EpicID: epic, Num: num, Title: "Задача " + id, Status: status, Estimate: est, AttemptLimit: 3}
		if status == TaskDone {
			tk.DoneAt = tp(h(done))
			tk.Spend = Totals{Records: 2, CostRecords: 2, CostUSD: float64(est) * 1.1}
		}
		return tk
	}
	in.Tasks = []Task{
		task("c1", "e-contacts", 1, 2, TaskDone, -140),
		task("c2", "e-contacts", 2, 3, TaskDone, -100),
		task("c3", "e-contacts", 3, 1, TaskDone, -71),
		task("d1", "e-deals", 4, 2, TaskDone, -40),
		task("d2", "e-deals", 5, 3, TaskDone, -20),
		task("d3", "e-deals", 6, 3, TaskReview, 0),
		task("d4", "e-deals", 7, 2, TaskFixing, 0),
		task("p1", "e-dash", 8, 2, TaskQueued, 0),
	}
	in.Tasks[5].LastEventAt = tp(h(-0.75))
	in.Tasks[6].AttemptUsed, in.Tasks[6].LastEventAt = 2, tp(h(-0.2))
	sess := func(role string, from, to float64, epic string) Session {
		return Session{RoleID: role, EpicID: epic, Start: h(from), End: tp(h(to))}
	}
	in.Sessions = []Session{
		sess("developer", -141, -140.5, "e-contacts"), sess("developer", -101, -100.2, "e-contacts"),
		sess("developer", -72, -71.5, "e-contacts"), sess("tester", -71.5, -71.2, "e-contacts"),
		sess("developer", -41, -40.2, "e-deals"), sess("reviewer", -40.2, -40, "e-deals"),
		sess("developer", -21, -20.5, "e-deals"), sess("tester", -20.6, -20.3, "e-deals"),
		sess("reviewer", -20.3, -20, "e-deals"),
		{RoleID: "developer", EpicID: "e-deals", Start: h(-0.4)},
	}
	u := func(d float64, epic, task, role, model string, tin int64, cost float64, known bool) UsageRecord {
		return UsageRecord{TS: h(d), EpicID: epic, TaskID: task, RoleID: role, Model: model,
			TokensIn: tin, TokensOut: tin / 15, CostUSD: cost, CostKnown: known, DurationS: 300}
	}
	in.Usage = []UsageRecord{
		u(-140, "e-contacts", "c1", "developer", "claude-sonnet-5", 300_000, 1.8, true),
		u(-100, "e-contacts", "c2", "developer", "claude-sonnet-5", 400_000, 2.4, true),
		u(-71.3, "e-contacts", "c3", "tester", "claude-sonnet-5", 200_000, 1.2, true),
		u(-40.1, "e-deals", "d1", "reviewer", "claude-opus-5", 250_000, 3.75, true),
		u(-20.4, "e-deals", "d2", "developer", "claude-sonnet-5", 350_000, 2.1, true),
		u(-20.1, "e-deals", "d2", "reviewer", "claude-opus-5", 240_000, 3.6, true),
		u(-0.3, "e-deals", "d4", "", "local-model", 50_000, 0, false),
		u(-200, "", "", "developer", "claude-sonnet-5", 999_000, 9.9, true),
	}
	in.StepRuns = []StepRun{
		{TaskID: "c2", StepKind: StepKindReview, RoleID: RoleReviewer, Verdict: VerdictChanges, At: h(-101.5)},
		{TaskID: "d1", StepKind: "test", RoleID: RoleTester, Verdict: VerdictChanges, At: h(-40.5)},
		{TaskID: "d2", StepKind: StepKindReview, RoleID: RoleReviewer, Verdict: "ok", At: h(-20)},
	}
	in.MergeConflicts = []MergeConflict{{TaskID: "d2", At: h(-20.05)}}
	in.Escalations = []Escalation{
		{ID: "a1", TaskID: "c2", EpicID: "e-contacts", TaskNum: 2, Reason: "BLOCKED", Status: "resolved", OpenedAt: h(-102), ResolvedAt: tp(h(-101.25))},
		{ID: "a2", TaskID: "d4", EpicID: "e-deals", TaskNum: 7, Title: "Задача d4", Reason: "ATTEMPTS_EXHAUSTED", Status: "claimed", ClaimedBy: "admin", OpenedAt: h(-0.5)},
	}
	rolled := Deployment{ID: "dep2", EnvID: "demo", Version: "9f3c2a1", Status: "rolled_back", Initiator: "auto", CreatedAt: h(-19.5), Rollback: true}
	in.Environments = []Environment{{ID: "demo", Name: "Демо", Last: &rolled}}
	in.Deployments = []Deployment{
		{ID: "dep1", EnvID: "demo", Version: "41ab0de", Status: "done", Initiator: "auto", CreatedAt: h(-70)},
		rolled,
	}
	return in
}

func TestReportGolden(t *testing.T) {
	in := reportFixture(t)
	got := Report(in)
	path := filepath.Join("testdata", "report.golden.md")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden: %v (запустите с -update)", err)
	}
	if got != string(want) {
		t.Fatalf("отчёт отличается от golden:\n%s", got)
	}
	// Повторный запрос даёт побайтно тот же текст, момент вызова не влияет.
	in.Now = in.Period.To.Add(45 * time.Second)
	if again := Report(in); again != got {
		t.Fatal("повторный отчёт отличается")
	}
}

// Отчёт содержит семь разделов в фиксированном порядке, цифры совпадают с
// обзором за тот же период на его конец.
func TestReportSectionsMatchOverview(t *testing.T) {
	in := reportFixture(t)
	rep := Report(in)
	last := -1
	for _, h := range []string{"## 1. Итог периода", "## 2. Epic'и", "## 3. Пропускная способность и качество",
		"## 4. Деньги", "## 5. Эскалации и решения людей", "## 6. Поставки", "## 7. Риски на конец периода"} {
		i := strings.Index(rep, h)
		if i <= last {
			t.Fatalf("раздел %q не на месте", h)
		}
		last = i
	}
	in.Now = in.Period.To
	ov := Build(in)
	for _, want := range []string{
		"Завершено задач: " + fmtInt(int64(ov.Team.Throughput.TasksDone)),
		"Расход за период: " + amountText(ov.Money.Period),
		"Возвраты Тестировщика: 1, Ревьюера: 1.",
		"Конфликты merge, возвращённые агентам: 1, из них задач завершено: 1.",
		"| Компании \\| контакты | завершён |",
		"нужен человек: escalation, late, last_attempt",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("нет %q в отчёте:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "—") {
		t.Error("в отчёте длинное тире")
	}
}

// Данные, появившиеся после конца периода, не меняют отчёт за прошедший
// период; причины, зависящие от состояния «сейчас», в нём не считаются.
func TestReportIgnoresDataAfterPeriod(t *testing.T) {
	base := reportFixture(t)
	base.Now, base.Live = base.Period.To.Add(24*time.Hour), false
	// На конец периода: ждёт подтверждения merge задача d3, на «Демо» идёт
	// выкатка dep4. На момент запроса она ещё не закрыта, но уже
	// откатывается: текущие статус и откат в прошедший отчёт не попадают.
	to0 := base.Period.To
	base.MergeWaits = []MergeWait{{TaskID: "d3", EpicID: "e-deals", TaskNum: 6, Title: "Задача d3", Since: to0.Add(-time.Hour)}}
	running := Deployment{ID: "dep4", EnvID: "demo", Version: "aa11bb2", Status: "verifying", Rollback: true, Initiator: "auto", CreatedAt: to0.Add(-10 * time.Minute)}
	base.Deployments = append(base.Deployments, running)
	base.Environments[0].Last = &running
	want := Report(base)
	for _, code := range []string{"last_attempt", "stalled", "no_capacity", "застряла"} {
		if strings.Contains(want, code) {
			t.Errorf("отчёт за прошедший период содержит %q", code)
		}
	}

	later := reportFixture(t)
	later.Now, later.Live = later.Period.To.Add(48*time.Hour), false
	to := later.Period.To
	h := func(d float64) time.Time { return to.Add(time.Duration(d * float64(time.Hour))) }
	// Задача d3 завершена после конца периода, d4 получила ещё попытку.
	later.Tasks[5].Status, later.Tasks[5].DoneAt = TaskDone, tp(h(2))
	later.Tasks[6].AttemptUsed = 3
	later.Tasks = append(later.Tasks, Task{ID: "n1", EpicID: "e-deals", Num: 9, Status: TaskQueued, Estimate: 5, CreatedAt: h(1)})
	// Эскалация закрыта после конца периода, открыта новая.
	later.Escalations[1].Status, later.Escalations[1].ResolvedAt = "resolved", tp(h(3))
	later.Escalations = append(later.Escalations, Escalation{ID: "a3", TaskID: "d3", EpicID: "e-deals", TaskNum: 6, Reason: "BLOCKED", Status: "open", OpenedAt: h(5)})
	// Epic «Сделки» завершён после конца периода, новый Epic создан.
	later.Epics[0].Status, later.Epics[0].DoneAt = EpicDone, tp(h(6))
	later.Epics = append(later.Epics, Epic{ID: "e-new", Title: "Новый", Status: EpicRunning, CreatedAt: h(1), StartedAt: tp(h(2))})
	// Расход, сессии, вердикты, конфликты, выкатки после конца периода.
	later.Usage = append(later.Usage, UsageRecord{TS: h(1), EpicID: "e-deals", TaskID: "d3", RoleID: "developer", Model: "claude-sonnet-5", TokensIn: 9_000_000, CostUSD: 90, CostKnown: true})
	later.Sessions = append(later.Sessions, Session{RoleID: "developer", EpicID: "e-deals", Start: h(1), End: tp(h(2))})
	later.Sessions[len(later.Sessions)-2].End = tp(h(0.5)) // шла на конец периода, закончилась после
	later.StepRuns = append(later.StepRuns, StepRun{TaskID: "d3", StepKind: StepKindReview, RoleID: RoleReviewer, Verdict: VerdictChanges, At: h(1)})
	later.MergeConflicts = append(later.MergeConflicts, MergeConflict{TaskID: "d3", At: h(1)})
	later.MergeWaits = append(later.MergeWaits, MergeWait{TaskID: "d3", EpicID: "e-deals", TaskNum: 6, Since: h(1)})
	newer := Deployment{ID: "dep3", EnvID: "demo", Version: "ffff000", Status: "done", Initiator: "auto", CreatedAt: h(4)}
	later.Deployments = append(later.Deployments, newer)
	later.Environments[0].Last = &newer
	later.MySteps = []MyStep{{RunID: "r", TaskID: "d3", TaskNum: 6, Since: h(-1)}}
	// Epic «Контакты», завершённый в периоде, архивирован после его конца.
	later.Epics[1].Status = EpicArchived
	// Ожидание merge подтверждено после конца периода и из снимка пропало,
	// выкатка dep4 закончилась после конца периода.
	later.MergeWaits = nil
	finished := Deployment{ID: "dep4", EnvID: "demo", Version: "aa11bb2", Status: "done", Initiator: "auto",
		CreatedAt: to.Add(-10 * time.Minute), EndedAt: tp(h(0.5))}
	later.Deployments = append(later.Deployments, finished)

	if got := Report(later); got != want {
		t.Fatalf("данные после конца периода изменили отчёт:\n%s\n---- ожидалось ----\n%s", got, want)
	}
	for _, line := range []string{
		"| Компании \\| контакты | завершён |",
		"подтверждения merge нет данных",
		"без подтверждений merge (по ним нет данных)",
		"| Демо | aa11bb2 | deploying |",
		"- Демо: aa11bb2, deploying,",
	} {
		if !strings.Contains(want, line) {
			t.Errorf("нет %q в отчёте за прошедший период:\n%s", line, want)
		}
	}
	if strings.Contains(want, "budget_exhausted") {
		t.Error("budget_exhausted в прошедшем отчёте")
	}
}

// Неделя ещё идёт: срез на текущий момент, без ложных late, stalled и
// застрявших задач, пересчёт на сутки по прошедшей части периода.
func TestReportPeriodInProgress(t *testing.T) {
	in := riskInput(t)
	in.ProjectName, in.Live = "CRM", true
	in.Epics[0].DueAt = tp(at(3 * time.Hour)) // от «сейчас» в графике, от конца недели было бы late
	p, err := CustomPeriod(at(-2*24*time.Hour), at(5*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	in.Period = p
	rep := Report(in)
	for _, bad := range []string{"late", "stalled", "no_capacity", "застряла", "под риском: 1"} {
		if strings.Contains(rep, bad) {
			t.Errorf("ложное %q в отчёте за идущую неделю:\n%s", bad, rep)
		}
	}
	for _, want := range []string{"данные на 2026-11-10 12:00: период ещё идёт", "В графике: 1", "В сутки: 2,00 задачи"} {
		if !strings.Contains(rep, want) {
			t.Errorf("нет %q:\n%s", want, rep)
		}
	}
}

// «Мои шаги» в общий отчёт не попадают.
func TestReportWithoutMySteps(t *testing.T) {
	in := reportFixture(t)
	want := Report(in)
	in.MySteps = []MyStep{{RunID: "r", TaskID: "d3", EpicID: "e-deals", TaskNum: 6, Title: "Review", Since: in.Period.To.Add(-time.Hour)}}
	if got := Report(in); got != want {
		t.Fatal("мои шаги изменили отчёт")
	}
}

// Конец периода ровно в 00:00 первого числа: месяц берётся прошлый.
func TestReportMonthBoundary(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	to := time.Date(2026, 12, 1, 0, 0, 0, 0, msk)
	p, err := CustomPeriod(to.Add(-7*24*time.Hour), to, msk)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{ProjectName: "CRM", Period: p, Location: msk, Now: to, MonthlyBudgetUSD: f64(100),
		Usage: []UsageRecord{{TS: time.Date(2026, 11, 20, 12, 0, 0, 0, msk), TokensIn: 10, CostUSD: 60, CostKnown: true}}}
	rep := Report(in)
	for _, want := range []string{"С начала месяца на конец периода: 60,00", "Прогноз расхода месяца: 60,00"} {
		if !strings.Contains(rep, want) {
			t.Errorf("нет %q:\n%s", want, rep)
		}
	}
	in.Now = to
	in.Period = Period{Name: PeriodMonth, From: time.Date(2026, 11, 1, 0, 0, 0, 0, msk), To: to, TZ: "Europe/Moscow"}
	if m := Build(in).Money; m.MonthToDate.CostUSD == nil || *m.MonthToDate.CostUSD != 60 || *m.MonthForecastUSD != 60 {
		t.Fatalf("обзор на 00:00 первого числа: %+v", m.MonthToDate)
	}
}
