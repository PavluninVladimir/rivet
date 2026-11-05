package overview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Момент расчёта тестов: 10 ноября 2026, 12:00 UTC.
var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return now.Add(d) }
func tp(t time.Time) *time.Time    { return &t }
func i64(n int64) *int64           { return &n }
func f64(x float64) *float64       { return &x }

func mustPeriod(t *testing.T, name string, loc *time.Location) Period {
	t.Helper()
	p, err := NewPeriod(name, now, loc)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func doneTask(id, epic string, num, est int, done time.Time) Task {
	return Task{ID: id, EpicID: epic, Num: num, Title: id, Status: TaskDone, Estimate: est, DoneAt: tp(done)}
}

func session(epic string, start, end time.Time) Session {
	return Session{ID: epic + start.String(), EpicID: epic, Start: start, End: tp(end)}
}

// ─── интервалы ───────────────────────────────────────────────────────────

func TestUnionDuration(t *testing.T) {
	h := time.Hour
	cases := []struct {
		name string
		iv   []Interval
		from time.Time
		to   time.Time
		want time.Duration
	}{
		{"пусто", nil, at(-10 * h), now, 0},
		{"два раздельных", []Interval{{at(-5 * h), at(-4 * h)}, {at(-2 * h), at(-1 * h)}}, at(-10 * h), now, 2 * h},
		{"параллельные не суммируются", []Interval{{at(-3 * h), at(-1 * h)}, {at(-3 * h), at(-1 * h)}}, at(-10 * h), now, 2 * h},
		{"перекрытие", []Interval{{at(-4 * h), at(-2 * h)}, {at(-3 * h), at(-1 * h)}}, at(-10 * h), now, 3 * h},
		{"вложенный", []Interval{{at(-4 * h), at(-1 * h)}, {at(-3 * h), at(-2 * h)}}, at(-10 * h), now, 3 * h},
		{"стык без зазора", []Interval{{at(-4 * h), at(-3 * h)}, {at(-3 * h), at(-2 * h)}}, at(-10 * h), now, 2 * h},
		{"обрезка окном", []Interval{{at(-5 * h), at(-1 * h)}}, at(-3 * h), at(-2 * h), h},
		{"вне окна", []Interval{{at(-5 * h), at(-4 * h)}}, at(-3 * h), now, 0},
		{"порядок не важен", []Interval{{at(-2 * h), at(-1 * h)}, {at(-6 * h), at(-5 * h)}}, at(-10 * h), now, 2 * h},
	}
	for _, c := range cases {
		if got := UnionDuration(c.iv, c.from, c.to); got != c.want {
			t.Errorf("%s: %v, ожидалось %v", c.name, got, c.want)
		}
	}
}

// ─── прогноз ─────────────────────────────────────────────────────────────

// Сценарий «Прогноз по темпу проекта»: остаток 6 единиц, за 14 суток 8
// задач на 12 единиц за 4 часа работы конвейера: темп 3, прогноз +2 часа.
func TestForecastProjectVelocity(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{{ID: "E", Title: "Сделки", Status: EpicRunning, StartedAt: tp(at(-48 * time.Hour))}}
	for i := 0; i < 8; i++ {
		est := 1
		if i < 4 {
			est = 2
		}
		in.Tasks = append(in.Tasks, doneTask("d"+string(rune('a'+i)), "P", i+1, est, at(-time.Duration(i+1)*time.Hour)))
	}
	in.Tasks = append(in.Tasks,
		Task{ID: "r1", EpicID: "E", Num: 20, Status: TaskRunning, Estimate: 4},
		Task{ID: "r2", EpicID: "E", Num: 21, Status: TaskQueued, Estimate: 2},
		Task{ID: "c", EpicID: "E", Num: 22, Status: TaskCancelled, Estimate: 9},
	)
	// Две параллельные сессии по 4 часа: работа конвейера 4 часа, не 8.
	in.Sessions = []Session{session("P", at(-10*time.Hour), at(-6*time.Hour)), session("P", at(-10*time.Hour), at(-6*time.Hour))}
	ov := Build(in)
	f := ov.Epics[0].Forecast
	if !f.Available || f.RemainingUnits != 6 || *f.VelocityUnitsPerHour != 3 || *f.VelocitySource != SourceProject {
		t.Fatalf("прогноз: %+v", f)
	}
	if !f.At.Equal(at(2 * time.Hour)) {
		t.Fatalf("момент прогноза %v, ожидалось +2ч", f.At)
	}
	if p := ov.Epics[0].Progress; p.TotalUnits != 6 || p.TotalTasks != 2 {
		t.Fatalf("отменённая задача в объёме: %+v", p)
	}
}

// Сценарий «Мало истории, темп Epic'а»: 2 задачи за окно, в Epic'е 2 задачи
// на 4 единицы за 1 час: темп 4, источник epic.
func TestForecastEpicVelocity(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{{ID: "E", Status: EpicRunning, StartedAt: tp(at(-30 * 24 * time.Hour))}}
	in.Tasks = []Task{
		doneTask("a", "E", 1, 2, at(-20*24*time.Hour)),
		doneTask("b", "E", 2, 2, at(-20*24*time.Hour)),
		doneTask("x", "O", 3, 1, at(-time.Hour)),
		doneTask("y", "O", 4, 1, at(-time.Hour)),
		{ID: "r", EpicID: "E", Num: 5, Status: TaskRunning, Estimate: 2},
	}
	in.Sessions = []Session{session("E", at(-21*24*time.Hour), at(-21*24*time.Hour+time.Hour))}
	f := Build(in).Epics[0].Forecast
	if !f.Available || *f.VelocityUnitsPerHour != 4 || *f.VelocitySource != SourceEpic {
		t.Fatalf("прогноз: %+v", f)
	}
	if !f.At.Equal(at(30 * time.Minute)) {
		t.Fatalf("2 ед. при темпе 4: +30 мин, получено %v", f.At)
	}
}

// Сценарий «Прогноз недоступен» и округление вверх до минуты.
func TestForecastUnavailableAndRounding(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{{ID: "E", Status: EpicRunning, StartedAt: tp(at(-time.Hour))}}
	in.Tasks = []Task{{ID: "r", EpicID: "E", Num: 1, Status: TaskRunning, Estimate: 3}}
	f := Build(in).Epics[0].Forecast
	if f.Available || f.Reason != "нет завершённых задач для расчёта темпа" || f.At != nil || f.VelocitySource != nil {
		t.Fatalf("прогноз без истории: %+v", f)
	}

	// Темп 3 ед./ч, остаток 1 единица: 20 минут ровно; остаток 0,5 — 10 мин.
	// Сдвиг момента на 30 секунд даёт округление вверх до минуты.
	in.Now = now.Add(30 * time.Second)
	in.Tasks = []Task{
		doneTask("a", "E", 1, 1, at(-time.Hour)), doneTask("b", "E", 2, 1, at(-time.Hour)), doneTask("c", "E", 3, 1, at(-time.Hour)),
		{ID: "r", EpicID: "E", Num: 4, Status: TaskRunning, Estimate: 1},
	}
	in.Sessions = []Session{session("E", at(-2*time.Hour), at(-time.Hour))}
	f = Build(in).Epics[0].Forecast
	if !f.Available || !f.At.Equal(at(21*time.Minute)) {
		t.Fatalf("округление вверх: %v", f.At)
	}
}

func TestForecastPausedAndDone(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{
		{ID: "P", Status: EpicPaused, StartedAt: tp(at(-time.Hour)), PauseReason: "остановлен человеком"},
		{ID: "D", Status: EpicDone, StartedAt: tp(at(-48 * time.Hour)), DoneAt: tp(at(-time.Hour))},
		{ID: "X", Status: EpicDone, DoneAt: tp(at(-30 * 24 * time.Hour))},
	}
	ov := Build(in)
	if len(ov.Epics) != 2 {
		t.Fatalf("Epic, завершённый вне периода, не входит: %d", len(ov.Epics))
	}
	if f := ov.Epics[0].Forecast; f.Available || f.Reason != "Epic на паузе: остановлен человеком" {
		t.Fatalf("пауза: %+v", f)
	}
	if r := ov.Epics[1].Risk; r.Status != RiskDone {
		t.Fatalf("завершённый Epic: %+v", r)
	}
}

// ─── риск ────────────────────────────────────────────────────────────────

// riskInput — работающий Epic с прогнозом +2 часа (как в прогнозе по темпу
// проекта) и свежими событиями.
func riskInput(t *testing.T) Input {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{{ID: "E", Title: "Сделки", Status: EpicRunning, StartedAt: tp(at(-48 * time.Hour))}}
	for i := 0; i < 4; i++ {
		in.Tasks = append(in.Tasks, doneTask("d"+string(rune('a'+i)), "E", i+1, 1, at(-time.Duration(i+1)*time.Hour)))
	}
	in.Tasks = append(in.Tasks, Task{ID: "r", EpicID: "E", Num: 10, Title: "Воронка", Status: TaskRunning, Estimate: 2,
		AttemptLimit: 3, LastEventAt: tp(at(-5 * time.Minute))})
	in.Sessions = []Session{session("E", at(-10*time.Hour), at(-6*time.Hour))} // темп 1 ед./ч
	return in
}

func codes(r Risk) string { return reasonCodes(r.Reasons) }

func TestRiskReasons(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(in *Input)
		status string
		codes  string
	}{
		{"всё по плану", func(in *Input) { in.Epics[0].DueAt = tp(at(24 * time.Hour)) }, RiskOnTrack, ""},
		{"late", func(in *Input) { in.Epics[0].DueAt = tp(at(time.Hour)) }, RiskAtRisk, ReasonLate},
		{"escalation важнее late", func(in *Input) {
			in.Epics[0].DueAt = tp(at(time.Hour))
			in.Escalations = []Escalation{{ID: "a1", TaskID: "r", EpicID: "E", TaskNum: 10, Reason: "ATTEMPTS_EXHAUSTED", Status: "open", OpenedAt: at(-time.Hour)}}
		}, RiskNeedsHuman, "escalation, late"},
		{"взятая эскалация тоже открыта", func(in *Input) {
			in.Escalations = []Escalation{{ID: "a1", TaskID: "r", EpicID: "E", TaskNum: 10, Status: "claimed", ClaimedBy: "admin", OpenedAt: at(-time.Hour)}}
		}, RiskNeedsHuman, ReasonEscalation},
		{"закрытая эскалация не считается", func(in *Input) {
			in.Escalations = []Escalation{{ID: "a1", TaskID: "r", EpicID: "E", Status: "resolved", OpenedAt: at(-time.Hour), ResolvedAt: tp(at(-time.Minute))}}
		}, RiskOnTrack, ""},
		{"budget_exhausted", func(in *Input) { in.Epics[0].BudgetExhausted = true }, RiskNeedsHuman, ReasonBudgetExhausted},
		{"budget_forecast", func(in *Input) {
			// Сценарий «Бюджет уходит» в масштабе: 4 из 10 единиц, 600 тыс.
			// токенов, бюджет 1 млн: прогноз 1,5 млн.
			in.Tasks[4].Estimate = 6
			in.Epics[0].TokenBudget = i64(1_000_000)
			in.Epics[0].Spend = Totals{Records: 3, TokensIn: 500_000, TokensOut: 100_000}
		}, RiskAtRisk, ReasonBudgetForecast},
		{"бюджет без превышения", func(in *Input) {
			in.Epics[0].TokenBudget = i64(2_000_000)
			in.Epics[0].Spend = Totals{Records: 1, TokensIn: 600_000}
		}, RiskOnTrack, ""},
		{"last_attempt", func(in *Input) { in.Tasks[4].AttemptUsed = 2 }, RiskAtRisk, ReasonLastAttempt},
		{"попытки ещё есть", func(in *Input) { in.Tasks[4].AttemptUsed = 1 }, RiskOnTrack, ""},
		{"stalled", func(in *Input) { in.Tasks[4].LastEventAt = tp(at(-61 * time.Minute)) }, RiskAtRisk, ReasonStalled},
		{"stalled ровно 60 минут", func(in *Input) { in.Tasks[4].LastEventAt = tp(at(-60 * time.Minute)) }, RiskAtRisk, ReasonStalled},
		{"тишина при эскалации не stalled", func(in *Input) {
			in.Tasks[4].LastEventAt = tp(at(-90 * time.Minute))
			in.Escalations = []Escalation{{ID: "a1", TaskID: "r", EpicID: "E", TaskNum: 10, Status: "open", OpenedAt: at(-time.Hour)}}
		}, RiskNeedsHuman, ReasonEscalation},
		{"no_capacity", func(in *Input) { in.Tasks[4].WaitingRunnerSince = tp(at(-16 * time.Minute)) }, RiskAtRisk, ReasonNoCapacity},
		{"ожидание 15 минут ещё не риск", func(in *Input) { in.Tasks[4].WaitingRunnerSince = tp(at(-15 * time.Minute)) }, RiskOnTrack, ""},
		{"пауза: без stalled и no_capacity", func(in *Input) {
			in.Epics[0].Status = EpicPaused
			in.Tasks[4].LastEventAt = tp(at(-5 * time.Hour))
			in.Tasks[4].WaitingRunnerSince = tp(at(-time.Hour))
		}, RiskOnTrack, ""},
	}
	for _, c := range cases {
		in := riskInput(t)
		c.mod(&in)
		r := Build(in).Epics[0].Risk
		if r.Status != c.status || codes(r) != c.codes {
			t.Errorf("%s: %s [%s], ожидалось %s [%s]", c.name, r.Status, codes(r), c.status, c.codes)
		}
	}
}

func TestRiskLateTextNamesBothDates(t *testing.T) {
	in := riskInput(t)
	msk, _ := time.LoadLocation("Europe/Moscow")
	in.Location = msk
	in.Epics[0].DueAt = tp(at(time.Hour))
	r := Build(in).Epics[0].Risk.Reasons[0]
	if r.Code != ReasonLate || r.Text != "прогноз 2026-11-10 17:00 позже целевой даты 2026-11-10 16:00" {
		t.Fatalf("текст late: %q", r.Text)
	}
}

// ─── решения ─────────────────────────────────────────────────────────────

// Сценарий «Смешанный список»: план 2 часа, эскалация 30 минут, merge 5
// минут, в этом порядке, со счётчиками.
func TestDecisions(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{
		{ID: "P", Title: "Воронка", Status: EpicPlanned, CreatedAt: at(-5 * time.Hour), PlannedAt: tp(at(-2 * time.Hour))},
		{ID: "Empty", Title: "Без плана", Status: EpicPlanned, CreatedAt: at(-9 * time.Hour)},
	}
	in.Tasks = []Task{{ID: "p1", EpicID: "P", Num: 1, Status: TaskQueued}}
	in.Escalations = []Escalation{
		{ID: "a1", TaskID: "t7", EpicID: "E", TaskNum: 7, Title: "Импорт", Reason: "BLOCKED", Status: "claimed", ClaimedBy: "admin", OpenedAt: at(-30 * time.Minute)},
		{ID: "a0", TaskID: "t6", Reason: "X", Status: "resolved", OpenedAt: at(-3 * time.Hour)},
	}
	in.MergeWaits = []MergeWait{{TaskID: "t9", EpicID: "E", TaskNum: 9, Title: "Канбан", Since: at(-5 * time.Minute)}}
	d := Build(in).Decisions
	if len(d.Items) != 3 {
		t.Fatalf("пункты: %+v", d.Items)
	}
	kinds := []string{d.Items[0].Kind, d.Items[1].Kind, d.Items[2].Kind}
	if strings.Join(kinds, ",") != "plan_approval,escalation,merge_confirmation" {
		t.Fatalf("порядок: %v", kinds)
	}
	if d.Items[0].WaitingSeconds != 7200 || *d.Items[1].ClaimedBy != "admin" || *d.Items[1].AttentionID != "a1" || *d.Items[2].TaskNum != 9 {
		t.Fatalf("поля: %+v", d.Items)
	}
	if d.Counts != (DecisionCounts{PlanApproval: 1, Escalation: 1, MergeConfirmation: 1}) {
		t.Fatalf("счётчики: %+v", d.Counts)
	}

	// Сценарий «Свои шаги»: пункт есть только у того, кому адресован.
	in.MySteps = []MyStep{{RunID: "run1", TaskID: "t3", EpicID: "E", TaskNum: 3, Title: "Review", Since: at(-time.Minute)}}
	if d := Build(in).Decisions; d.Counts.MyStep != 1 || *d.Items[3].RunID != "run1" {
		t.Fatalf("мой шаг: %+v", d)
	}
}

// ─── команда ─────────────────────────────────────────────────────────────

// Сценарий «Занятость ролей»: 42 часа сессий Разработчиков за 7 суток, два
// работают сейчас: занято 2, в среднем 0,25.
func TestTeamLoad(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Roles = []Role{{ID: "developer", Title: "Разработчик", Color: "blue"}, {ID: "reviewer", Title: "Ревьюер", Color: "rose"}}
	for d := 1; d <= 5; d++ {
		s := at(-time.Duration(d) * 24 * time.Hour)
		in.Sessions = append(in.Sessions, Session{RoleID: "developer", Start: s, End: tp(s.Add(8 * time.Hour))})
	}
	in.Sessions = append(in.Sessions,
		Session{RoleID: "developer", Start: at(-1 * time.Hour)}, // идёт
		Session{RoleID: "developer", Start: at(-1 * time.Hour)}, // идёт
		Session{RoleID: "developer", Human: true, Start: at(-30 * time.Hour), End: tp(at(-20 * time.Hour))},
		Session{RoleID: "", Start: at(-8 * 24 * time.Hour), End: tp(at(-7*24*time.Hour + time.Hour))}, // час внутри периода
	)
	team := Build(in).Team
	if len(team.Roles) != 3 || team.Roles[2].Role != "" || team.Roles[2].Title != "без роли" {
		t.Fatalf("роли: %+v", team.Roles)
	}
	dev := team.Roles[0]
	if dev.BusyNow != 2 || dev.AvgBusy != 0.25 || dev.Sessions != 7 || dev.BusySeconds != 42*3600 {
		t.Fatalf("разработчики: %+v", dev)
	}
	if rv := team.Roles[1]; rv.BusyNow != 0 || rv.AvgBusy != 0 {
		t.Fatalf("ревьюер без сессий: %+v", rv)
	}
	if nr := team.Roles[2]; nr.Sessions != 0 || nr.BusySeconds != 3600 {
		t.Fatalf("без роли, сессия до периода: %+v", nr)
	}
}

// Сценарии «Качество review» и «Конфликты merge».
func TestQuality(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	for i := 0; i < 10; i++ {
		in.Tasks = append(in.Tasks, doneTask("t"+string(rune('0'+i)), "E", i+1, 1, at(-time.Duration(i+1)*time.Hour)))
	}
	for _, id := range []string{"t0", "t1", "t2"} {
		in.StepRuns = append(in.StepRuns, StepRun{TaskID: id, StepKind: StepKindReview, RoleID: RoleReviewer, Verdict: VerdictChanges, At: at(-20 * time.Hour)})
	}
	in.StepRuns = append(in.StepRuns,
		StepRun{TaskID: "t3", StepKind: "test", RoleID: RoleTester, Verdict: VerdictChanges, At: at(-20 * time.Hour)},
		StepRun{TaskID: "t3", StepKind: "test", RoleID: RoleTester, Verdict: VerdictChanges, At: at(-30 * 24 * time.Hour)}, // вне периода
		StepRun{TaskID: "t4", StepKind: StepKindReview, RoleID: RoleReviewer, Verdict: "ok", At: at(-20 * time.Hour)},
	)
	in.MergeConflicts = []MergeConflict{
		{TaskID: "t5", At: at(-30 * time.Hour)}, {TaskID: "t5", At: at(-29 * time.Hour)},
		{TaskID: "t6", At: at(-30 * time.Hour)},
		{TaskID: "t7", At: at(-30 * time.Hour), Exhausted: true},
	}
	in.Escalations = []Escalation{
		{Reason: "BLOCKED", OpenedAt: at(-time.Hour)}, {Reason: "BLOCKED", OpenedAt: at(-2 * time.Hour)},
		{Reason: "ATTEMPTS_EXHAUSTED", OpenedAt: at(-time.Hour)}, {Reason: "OLD", OpenedAt: at(-30 * 24 * time.Hour)},
	}
	q := Build(in).Team.Quality
	if *q.FirstPassReviewRate != 0.7 {
		t.Fatalf("review с первого раза: %v", *q.FirstPassReviewRate)
	}
	if q.TesterReturns != 1 || q.ReviewerReturns != 3 {
		t.Fatalf("возвраты: %+v", q)
	}
	if q.MergeConflicts != (MergeConflictStats{Returned: 2, Done: 2}) {
		t.Fatalf("конфликты: %+v", q.MergeConflicts)
	}
	if len(q.Escalations) != 2 || q.Escalations[0] != (ReasonCount{"BLOCKED", 2}) {
		t.Fatalf("эскалации: %+v", q.Escalations)
	}
	in.Tasks = nil
	if q := Build(in).Team.Quality; q.FirstPassReviewRate != nil {
		t.Fatal("без завершённых задач доля недоступна")
	}
}

func TestThroughputPerDay(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	for i := 0; i < 14; i++ {
		in.Tasks = append(in.Tasks, doneTask("t"+string(rune('a'+i)), "E", i+1, 2, at(-time.Duration(i+1)*time.Hour)))
	}
	th := Build(in).Team.Throughput
	if th.TasksDone != 14 || th.UnitsDone != 28 || th.PerDay != (PerDay{Tasks: 2, Units: 4}) {
		t.Fatalf("пропускная способность: %+v", th)
	}
	// Период короче суток считается за сутки.
	in.Period = mustPeriod(t, PeriodToday, nil)
	if pd := Build(in).Team.Throughput.PerDay; pd.Tasks != 12 {
		t.Fatalf("сегодня: %+v", pd)
	}
}

// ─── деньги ──────────────────────────────────────────────────────────────

func usage(ts time.Time, role, model string, tokens int64, cost float64, known bool) UsageRecord {
	return UsageRecord{TS: ts, RoleID: role, Model: model, TokensIn: tokens, TokensOut: tokens / 10, CostUSD: cost, CostKnown: known}
}

// Сценарий «Прогноз расхода месяца»: 10 ноября, 50 долларов с начала
// месяца, план 120: прогноз 150, превышение 30.
func TestMonthForecast(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, PeriodMonth, nil), MonthlyBudgetUSD: f64(120)}
	in.Usage = []UsageRecord{
		usage(at(-24*time.Hour), "developer", "sonnet", 1000, 30, true),
		usage(at(-48*time.Hour), "reviewer", "opus", 1000, 20, true),
		usage(at(-40*24*time.Hour), "reviewer", "opus", 1000, 999, true), // прошлый месяц
	}
	m := Build(in).Money
	if *m.MonthToDate.CostUSD != 50 || *m.MonthForecastUSD != 150 || *m.MonthOverBudgetUSD != 30 {
		t.Fatalf("месяц: %+v %v %v", m.MonthToDate, *m.MonthForecastUSD, m.MonthOverBudgetUSD)
	}
	in.MonthlyBudgetUSD = f64(200)
	if m := Build(in).Money; m.MonthOverBudgetUSD != nil {
		t.Fatal("в пределах бюджета превышения нет")
	}
	// Первые сутки месяца: расход на сутки, умноженный на 30.
	in.Now = time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	in.Period = Period{Name: PeriodMonth, From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: in.Now}
	in.Usage = []UsageRecord{usage(in.Now.Add(-10*time.Minute), "developer", "sonnet", 10, 2, true)}
	if m := Build(in).Money; *m.MonthForecastUSD != 60 {
		t.Fatalf("начало месяца: %v", *m.MonthForecastUSD)
	}
	// Граница месяца в поясе запроса: в Москве уже 1 ноября 03:30.
	msk, _ := time.LoadLocation("Europe/Moscow")
	in.Location = msk
	in.Usage = append(in.Usage, usage(time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC), "developer", "sonnet", 10, 3, true))
	if m := Build(in).Money; *m.MonthToDate.CostUSD != 5 {
		t.Fatalf("месяц по Москве: %+v", m.MonthToDate)
	}
}

// Сценарий «Разрез по ролям»: Ревьюер на Opus 12, Разработчики на Sonnet
// 20; доли от итога.
func TestMoneyShares(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil), DailyTokenBudget: i64(50_000)}
	in.Roles = []Role{{ID: "developer", Title: "Разработчик"}, {ID: "reviewer", Title: "Ревьюер"}}
	in.Usage = []UsageRecord{
		usage(at(-time.Hour), "reviewer", "opus", 1000, 12, true),
		usage(at(-2*time.Hour), "developer", "sonnet", 3000, 15, true),
		usage(at(-26*time.Hour), "developer", "sonnet", 2000, 5, true),
	}
	m := Build(in).Money
	if len(m.ByRole) != 2 || m.ByRole[0].Key != "developer" || *m.ByRole[0].Amount.CostUSD != 20 || *m.ByRole[0].Share != 0.625 ||
		m.ByRole[1].Title != "Ревьюер" || *m.ByRole[1].Share != 0.375 {
		t.Fatalf("по ролям: %+v", m.ByRole)
	}
	if len(m.ByModel) != 2 || m.ByModel[0].Key != "sonnet" || m.ByModel[1].Key != "opus" {
		t.Fatalf("по моделям: %+v", m.ByModel)
	}
	if m.DailyTokenBudget == nil || m.DailyTokenBudget.UsedToday != 4400 {
		t.Fatalf("дневной бюджет: %+v", m.DailyTokenBudget)
	}
}

// Сценарий «Нет данных о стоимости» и пометка неполной стоимости.
func TestAmountAvailability(t *testing.T) {
	if a := (Totals{}).Amount(); a.TokensIn != nil || a.CostUSD != nil || a.PartialCost {
		t.Fatalf("без записей всё недоступно: %+v", a)
	}
	var none Totals
	none.Add(usage(now, "", "local", 100, 0, false))
	if a := none.Amount(); a.CostUSD != nil || *a.TokensIn != 100 || a.PartialCost {
		t.Fatalf("без стоимости: %+v", a)
	}
	var part Totals
	part.Add(usage(now, "", "opus", 100, 2.5, true))
	part.Add(usage(now, "", "local", 100, 0, false))
	if a := part.Amount(); *a.CostUSD != 2.5 || !a.PartialCost {
		t.Fatalf("часть без стоимости: %+v", a)
	}
	// Доли без стоимости считаются по токенам.
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Usage = []UsageRecord{usage(at(-time.Hour), "a", "m1", 300, 0, false), usage(at(-time.Hour), "b", "m1", 100, 0, false)}
	if s := Build(in).Money.ByRole; s[0].Key != "a" || *s[0].Share != 0.75 {
		t.Fatalf("доли по токенам: %+v", s)
	}
}

func TestAvgCostPerTask(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	a := doneTask("a", "E", 1, 1, at(-time.Hour))
	a.Spend = Totals{Records: 1, CostRecords: 1, CostUSD: 3}
	b := doneTask("b", "E", 2, 1, at(-time.Hour))
	b.Spend = Totals{Records: 1, CostRecords: 1, CostUSD: 5}
	c := doneTask("c", "E", 3, 1, at(-time.Hour))
	c.Spend = Totals{Records: 1}
	in.Tasks = []Task{a, b, c}
	if m := Build(in).Money; m.AvgCostPerTask == nil || *m.AvgCostPerTask != 4 {
		t.Fatalf("средняя стоимость: %v", m.AvgCostPerTask)
	}
}

// ─── поставки ────────────────────────────────────────────────────────────

// Сценарии «Проваленная выкатка» и «Застрявшая задача».
func TestDelivery(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	rolled := Deployment{ID: "d2", EnvID: "demo", Version: "abc", Status: "rolled_back", Initiator: "auto", CreatedAt: at(-time.Hour), Rollback: true}
	in.Environments = []Environment{{ID: "demo", Name: "Демо", Last: &rolled}, {ID: "prod", Name: "Прод"}}
	in.Deployments = []Deployment{
		{ID: "d1", EnvID: "demo", Version: "aaa", Status: "done", CreatedAt: at(-2 * time.Hour)},
		rolled,
		{ID: "d0", EnvID: "demo", Version: "old", Status: "done", CreatedAt: at(-30 * 24 * time.Hour)},
	}
	in.Epics = []Epic{{ID: "E", Status: EpicRunning, StartedAt: tp(at(-48 * time.Hour))}}
	in.Tasks = []Task{
		{ID: "s", EpicID: "E", Num: 2, Title: "Канбан", Status: TaskReview, LastEventAt: tp(at(-40 * time.Minute))},
		{ID: "f", EpicID: "E", Num: 1, Title: "Свежая", Status: TaskRunning, LastEventAt: tp(at(-10 * time.Minute))},
		{ID: "w", EpicID: "E", Num: 3, Title: "Ждёт", Status: TaskReady, WaitingRunnerSince: tp(at(-20 * time.Minute))},
		{ID: "e", EpicID: "E", Num: 4, Title: "Эскалация", Status: TaskReview, LastEventAt: tp(at(-time.Hour))},
		{ID: "m", EpicID: "E", Num: 5, Title: "Merge", Status: TaskReview, LastEventAt: tp(at(-time.Hour))},
		{ID: "l", EpicID: "E", Num: 6, Title: "Последняя", Status: TaskFixing, AttemptUsed: 2, AttemptLimit: 3, LastEventAt: tp(at(-time.Minute))},
	}
	in.Escalations = []Escalation{{ID: "a", TaskID: "e", Status: "open", OpenedAt: at(-time.Hour)}}
	in.MergeWaits = []MergeWait{{TaskID: "m", TaskNum: 5, Since: at(-time.Hour)}}
	d := Build(in).Delivery
	if d.Environments[0].Last.Status != "rolled_back" || d.Environments[1].Last != nil {
		t.Fatalf("окружения: %+v", d.Environments)
	}
	if len(d.Deployments) != 2 || d.Deployments[0].ID != "d2" || !d.Deployments[0].Rollback {
		t.Fatalf("выкатки периода: %+v", d.Deployments)
	}
	if len(d.Stuck) != 2 || d.Stuck[0].TaskID != "s" || d.Stuck[1].TaskID != "w" || !d.Stuck[1].WaitingRunner {
		t.Fatalf("застрявшие: %+v", d.Stuck)
	}
	if len(d.LastAttempt) != 1 || d.LastAttempt[0].TaskNum != 6 {
		t.Fatalf("последняя попытка: %+v", d.LastAttempt)
	}
}

func TestBudgetRisksInDelivery(t *testing.T) {
	in := riskInput(t)
	in.Tasks[4].Estimate = 6
	in.Epics[0].TokenBudget = i64(1_000_000)
	in.Epics[0].Spend = Totals{Records: 1, TokensIn: 600_000}
	d := Build(in).Delivery
	if len(d.BudgetRisks) != 1 || d.BudgetRisks[0].BudgetForecastTokens != 1_500_000 {
		t.Fatalf("риски бюджета: %+v", d.BudgetRisks)
	}
}

// ─── период и контракт ───────────────────────────────────────────────────

func TestPeriods(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	p := mustPeriod(t, PeriodToday, msk)
	if p.From.Format(time.RFC3339) != "2026-11-10T00:00:00+03:00" || p.TZ != "Europe/Moscow" {
		t.Fatalf("сегодня по Москве: %v", p.From)
	}
	if p := mustPeriod(t, "", nil); p.Name != Period7d || !p.From.Equal(at(-7*24*time.Hour)) {
		t.Fatalf("по умолчанию 7d: %+v", p)
	}
	if p := mustPeriod(t, PeriodMonth, nil); p.From.Day() != 1 {
		t.Fatalf("месяц: %+v", p)
	}
	if _, err := NewPeriod("year", now, nil); err == nil {
		t.Fatal("неизвестный период")
	}
	if _, err := CustomPeriod(now, now, nil); err == nil {
		t.Fatal("пустой период")
	}
	if _, err := CustomPeriod(at(-93*24*time.Hour), now, nil); err == nil {
		t.Fatal("период длиннее 92 суток")
	}
}

// Пустые списки отдаются массивами, недоступные значения null.
func TestJSONShape(t *testing.T) {
	raw, err := json.Marshal(Build(Input{Now: now, Period: mustPeriod(t, Period7d, nil)}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"epics":[]`, `"items":[]`, `"roles":[]`, `"by_role":[]`, `"stuck":[]`,
		`"escalations":[]`, `"cost_usd":null`, `"first_pass_review_rate":null`, `"velocity_window_days":14`} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %s в %s", want, s)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := [][2]string{
		{fmtInt(0), "0"}, {fmtInt(999), "999"}, {fmtInt(1000), "1\u202f000"}, {fmtInt(-1234567), "-1\u202f234\u202f567"},
		{fmtNum(1234.5, 2), "1\u202f234,50"}, {fmtNum(0.005, 2), "0,01"}, {fmtNum(-0.001, 2), "0,00"},
		{fmtUSD(12), "12,00\u202f$"}, {fmtPct(0.7), "70\u202f%"},
		{fmtUnits(3), "3"}, {fmtUnits(2.5), "2,50"},
		{fmtDuration(45 * time.Minute), "45 мин"}, {fmtDuration(65 * time.Minute), "1 ч 05 мин"}, {fmtDuration(50 * time.Hour), "2 сут 2 ч"},
		{mdCell("a|b\nc"), "a\\|b c"},
	}
	for i, c := range cases {
		if c[0] != c[1] {
			t.Errorf("%d: %q, ожидалось %q", i, c[0], c[1])
		}
	}
}

// Застрявшие задачи и ожидание runner'а считаются только у Epic'ов в работе,
// последняя попытка ещё и на паузе; задачи архивных и плановых Epic'ов не
// показываются.
func TestDeliveryByEpicStatus(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{
		{ID: "run", Status: EpicRunning, StartedAt: tp(at(-48 * time.Hour))},
		{ID: "pause", Status: EpicPaused, StartedAt: tp(at(-48 * time.Hour))},
		{ID: "arch", Status: EpicArchived},
		{ID: "plan", Status: EpicPlanned},
	}
	silent := func(id, epic string, num int) Task {
		return Task{ID: id, EpicID: epic, Num: num, Status: TaskReview, AttemptUsed: 2, AttemptLimit: 3,
			LastEventAt: tp(at(-2 * time.Hour)), WaitingRunnerSince: tp(at(-time.Hour))}
	}
	in.Tasks = []Task{silent("r", "run", 1), silent("p", "pause", 2), silent("a", "arch", 3), silent("q", "plan", 4)}
	d := Build(in).Delivery
	if len(d.Stuck) != 1 || d.Stuck[0].TaskID != "r" {
		t.Fatalf("застрявшие только у Epic'а в работе: %+v", d.Stuck)
	}
	if len(d.LastAttempt) != 2 || d.LastAttempt[0].TaskID != "r" || d.LastAttempt[1].TaskID != "p" {
		t.Fatalf("последняя попытка в работе и на паузе: %+v", d.LastAttempt)
	}
}

// Лимит в одну попытку: last_attempt только у начатых задач.
func TestLastAttemptOnlyStarted(t *testing.T) {
	in := riskInput(t)
	for i := range in.Tasks {
		in.Tasks[i].AttemptLimit = 1
	}
	in.Tasks = append(in.Tasks,
		Task{ID: "q", EpicID: "E", Num: 11, Status: TaskQueued, Estimate: 1, AttemptLimit: 1},
		Task{ID: "y", EpicID: "E", Num: 12, Status: TaskReady, Estimate: 1, AttemptLimit: 1},
	)
	ov := Build(in)
	r := ov.Epics[0].Risk
	if codes(r) != ReasonLastAttempt || len(r.Reasons) != 1 || *r.Reasons[0].TaskID != "r" {
		t.Fatalf("только начатая задача: %+v", r.Reasons)
	}
	if len(ov.Delivery.LastAttempt) != 1 || ov.Delivery.LastAttempt[0].TaskID != "r" {
		t.Fatalf("список последней попытки: %+v", ov.Delivery.LastAttempt)
	}
	in.Tasks[len(in.Tasks)-1].StartedAt = tp(at(-time.Minute))
	if r := Build(in).Epics[0].Risk; len(r.Reasons) != 2 {
		t.Fatalf("назначенная задача тоже начата: %+v", r.Reasons)
	}
}

// Прогноз расхода Epic'а без записей usage недоступен.
func TestBudgetForecastNeedsUsage(t *testing.T) {
	in := riskInput(t)
	in.Epics[0].TokenBudget = i64(10)
	ov := Build(in)
	if e := ov.Epics[0]; e.BudgetForecastTokens != nil || codes(e.Risk) != "" {
		t.Fatalf("без usage: %+v %+v", e.BudgetForecastTokens, e.Risk)
	}
}

// Группа без стоимости в денежном разрезе: доля null, в конце списка.
func TestShareUnknownCost(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Usage = []UsageRecord{usage(at(-time.Hour), "a", "opus", 100, 4, true), usage(at(-time.Hour), "b", "local", 900, 0, false)}
	s := Build(in).Money.ByRole
	if len(s) != 2 || s[0].Key != "a" || *s[0].Share != 1 || s[1].Key != "b" || s[1].Share != nil {
		t.Fatalf("доли: %+v", s)
	}
	raw, _ := json.Marshal(s[1])
	if !strings.Contains(string(raw), `"share":null`) {
		t.Fatalf("JSON: %s", raw)
	}
}

// Epic, завершённый в периоде и затем архивированный, в обзоре завершён.
func TestArchivedDoneEpicShown(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{
		{ID: "A", Title: "Контакты", Status: EpicArchived, DoneAt: tp(at(-time.Hour))},
		{ID: "Old", Status: EpicArchived, DoneAt: tp(at(-30 * 24 * time.Hour))},
		{ID: "Never", Status: EpicArchived},
	}
	ov := Build(in)
	if len(ov.Epics) != 1 || ov.Epics[0].ID != "A" || ov.Epics[0].Status != EpicDone || ov.Epics[0].Risk.Status != RiskDone {
		t.Fatalf("архивный Epic: %+v", ov.Epics)
	}
}

// Системные Epic'и («Архитектура проекта», «Сопровождение») видны в списке с
// пометкой, но в число активных, прогноз и итог отчёта не входят (ревью
// переноса архитектуры, P3-3).
func TestSystemEpicsOutOfForecast(t *testing.T) {
	in := Input{Now: now, Period: mustPeriod(t, Period7d, nil)}
	in.Epics = []Epic{
		{ID: "W", Title: "Сделки", Status: EpicRunning, StartedAt: tp(at(-48 * time.Hour))},
		{ID: "A", Title: "Архитектура проекта", Status: EpicRunning, Kind: "architecture", StartedAt: tp(at(-2 * time.Hour)), TokenBudget: i64(10)},
		{ID: "M", Title: "Сопровождение", Status: EpicRunning, Kind: "maintenance", StartedAt: tp(at(-time.Hour))},
	}
	in.Tasks = []Task{
		doneTask("w1", "W", 1, 2, at(-3*time.Hour)), doneTask("w2", "W", 2, 2, at(-2*time.Hour)),
		{ID: "a1", EpicID: "A", Num: 3, Status: TaskRunning, Estimate: 2},
	}
	ov := Build(in)
	if ov.Summary.ActiveEpics != 1 {
		t.Fatalf("активных Epic'ов: %d, ожидался один обычный", ov.Summary.ActiveEpics)
	}
	if len(ov.Epics) != 3 {
		t.Fatalf("Epic'ов в списке: %d", len(ov.Epics))
	}
	for _, e := range ov.Epics {
		if e.ID == "W" {
			if e.System {
				t.Fatal("обычный Epic помечен системным")
			}
			continue
		}
		if !e.System || e.Forecast.Available || !strings.Contains(e.Forecast.Reason, "системный") || e.BudgetForecastTokens != nil {
			t.Fatalf("системный Epic %s: %+v", e.ID, e)
		}
	}
	rep := Report(in)
	if !strings.Contains(rep, "Архитектура проекта (системный)") || !strings.Contains(rep, "Активных Epic'ов на конец периода: 1.") {
		t.Fatalf("отчёт о системных Epic'ах:\n%s", rep)
	}
}
