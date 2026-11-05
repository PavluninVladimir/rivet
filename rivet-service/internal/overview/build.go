package overview

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/PavluninVladimir/rivet/internal/signals"
)

// calc — входные данные с индексами, общие для разделов.
type calc struct {
	in        Input
	now       time.Time
	loc       *time.Location
	th        Thresholds
	tasksBy   map[string][]Task // по Epic'у
	openEsc   map[string]bool   // задачи с открытой или взятой эскалацией
	mergeWait map[string]bool   // задачи в ожидании подтверждения merge
	roleBy    map[string]Role
	epicStat  map[string]string // статус Epic'а по id
	// noLive — состояние задач «сейчас» к моменту расчёта не относится
	// (отчёт за прошедший период): попытки, последнее событие и ожидание
	// runner'а не восстановить, поэтому last_attempt, stalled, no_capacity
	// и списки застрявших задач не считаются.
	noLive bool

	projectV  float64
	projectOK bool
}

func newCalc(in Input, noLive bool) *calc {
	c := &calc{in: in, now: in.Now, loc: in.Location, th: in.Thresholds, noLive: noLive}
	if c.loc == nil {
		c.loc = time.UTC
	}
	if c.th == (Thresholds{}) {
		c.th = DefaultThresholds()
	}
	c.tasksBy = map[string][]Task{}
	for _, t := range in.Tasks {
		c.tasksBy[t.EpicID] = append(c.tasksBy[t.EpicID], t)
	}
	c.openEsc = map[string]bool{}
	for _, e := range in.Escalations {
		if e.TaskID != "" && escalationOpen(e) {
			c.openEsc[e.TaskID] = true
		}
	}
	c.mergeWait = map[string]bool{}
	for _, w := range in.MergeWaits {
		c.mergeWait[w.TaskID] = true
	}
	c.roleBy = map[string]Role{}
	for _, r := range in.Roles {
		c.roleBy[r.ID] = r
	}
	c.epicStat = map[string]string{}
	for _, e := range in.Epics {
		c.epicStat[e.ID] = e.Status
	}
	c.projectV, c.projectOK = c.projectVelocity()
	return c
}

// Build считает обзор проекта на момент in.Now за период in.Period.
func Build(in Input) Overview { return newCalc(in, false).overview() }

func (c *calc) overview() Overview {
	in := c.in
	ov := Overview{
		ProjectID:  in.ProjectID,
		Period:     c.period(),
		ComputedAt: c.now.In(c.loc),
		Thresholds: c.th,
	}
	ov.Epics = c.epics()
	ov.Decisions = c.decisions()
	ov.Team = c.team()
	ov.Money = c.money()
	ov.Delivery = c.delivery(ov.Epics)
	active := 0
	for _, e := range in.Epics {
		if (e.Status == EpicRunning || e.Status == EpicPaused) && !e.System() {
			active++
		}
	}
	ov.Summary = Summary{
		ActiveEpics:         active,
		TasksDone:           ov.Team.Throughput.TasksDone,
		ThroughputPerDay:    ov.Team.Throughput.PerDay,
		Spend:               ov.Money.Period,
		OpenDecisions:       len(ov.Decisions.Items),
		FirstPassReviewRate: ov.Team.Quality.FirstPassReviewRate,
	}
	return ov
}

func (c *calc) period() Period {
	p := c.in.Period
	p.From, p.To = p.From.In(c.loc), p.To.In(c.loc)
	if p.TZ == "" {
		p.TZ = c.loc.String()
	}
	return p
}

func escalationOpen(e Escalation) bool { return e.Status == "open" || e.Status == "claimed" }

func unfinished(t Task) bool { return t.Status != TaskDone && t.Status != TaskCancelled }

// doneAt — момент завершения задачи, если она завершена.
func doneAt(t Task) (time.Time, bool) {
	if t.Status != TaskDone || t.DoneAt == nil {
		return time.Time{}, false
	}
	return *t.DoneAt, true
}

// ─── прогноз ─────────────────────────────────────────────────────────────

// projectVelocity — темп проекта: единицы задач, завершённых за окно,
// делённые на часы работы конвейера проекта за то же окно. Меньше
// минимума задач — темп проекта не считается.
func (c *calc) projectVelocity() (float64, bool) {
	from := c.now.Add(-time.Duration(c.th.VelocityWindowDays) * 24 * time.Hour)
	var count int
	var units float64
	for _, t := range c.in.Tasks {
		if d, ok := doneAt(t); ok && in(d, from, c.now) {
			count++
			units += float64(t.Estimate)
		}
	}
	if count < c.th.VelocityMinTasks {
		return 0, false
	}
	hours := UnionDuration(agentIntervals(c.in.Sessions, c.now, nil), from, c.now).Hours()
	if hours <= 0 || units <= 0 {
		return 0, false
	}
	return units / hours, true
}

// epicVelocity — темп самого Epic'а: единицы его завершённых задач на часы
// работы конвейера Epic'а с момента запуска.
func (c *calc) epicVelocity(e Epic) (float64, bool) {
	var units float64
	var count int
	for _, t := range c.tasksBy[e.ID] {
		if _, ok := doneAt(t); ok {
			count++
			units += float64(t.Estimate)
		}
	}
	if count == 0 || units <= 0 {
		return 0, false
	}
	from := c.epicStart(e)
	hours := UnionDuration(agentIntervals(c.in.Sessions, c.now, func(s Session) bool { return s.EpicID == e.ID }), from, c.now).Hours()
	if hours <= 0 {
		return 0, false
	}
	return units / hours, true
}

// epicStart — момент запуска Epic'а; без него начало первой сессии Epic'а.
func (c *calc) epicStart(e Epic) time.Time {
	if e.StartedAt != nil {
		return *e.StartedAt
	}
	start := c.now
	for _, s := range c.in.Sessions {
		if s.EpicID == e.ID && s.Start.Before(start) {
			start = s.Start
		}
	}
	return start
}

func (c *calc) progress(e Epic) Progress {
	var p Progress
	for _, t := range c.tasksBy[e.ID] {
		if t.Status == TaskCancelled {
			continue
		}
		p.TotalTasks++
		p.TotalUnits += float64(t.Estimate)
		if t.Status == TaskDone {
			p.DoneTasks++
			p.DoneUnits += float64(t.Estimate)
		}
	}
	return p
}

// forecast — прогноз завершения: сейчас плюс R/V часов, вверх до минуты.
func (c *calc) forecast(e Epic, p Progress) Forecast {
	f := Forecast{RemainingUnits: p.TotalUnits - p.DoneUnits}
	if e.System() && e.Status != EpicDone {
		f.Reason = "системный Epic: прогноз не считается"
		return f
	}
	switch e.Status {
	case EpicDone:
		f.Reason = "Epic завершён"
		return f
	case EpicPaused:
		f.Reason = "Epic на паузе"
		if e.PauseReason != "" {
			f.Reason += ": " + e.PauseReason
		}
		return f
	case EpicRunning:
	default:
		f.Reason = "Epic не запущен"
		return f
	}
	v, src := c.projectV, SourceProject
	if !c.projectOK {
		var ok bool
		v, ok = c.epicVelocity(e)
		src = SourceEpic
		if !ok {
			f.Reason = "нет завершённых задач для расчёта темпа"
			return f
		}
	}
	rv := round(v, 4)
	f.VelocityUnitsPerHour = &rv
	f.VelocitySource = &src
	secs := math.Ceil(f.RemainingUnits / v * 3600)
	at := ceilMinute(c.now.Add(time.Duration(secs) * time.Second)).In(c.loc)
	f.At = &at
	f.Available = true
	return f
}

// budgetForecast — прогноз расхода токенов Epic'а: израсходованное на
// единицу сделанного, умноженное на объём W. Без сделанного или без
// записей usage не считается.
func budgetForecast(e Epic, p Progress) (int64, bool) {
	if e.System() {
		return 0, false
	}
	if p.DoneUnits <= 0 || e.Spend.Records == 0 {
		return 0, false
	}
	return int64(math.Round(float64(e.Spend.Tokens()) / p.DoneUnits * p.TotalUnits)), true
}

// ─── риск ────────────────────────────────────────────────────────────────

var reasonOrder = map[string]int{
	ReasonEscalation: 0, ReasonBudgetExhausted: 1, ReasonLate: 2, ReasonBudgetForecast: 3,
	ReasonLastAttempt: 4, ReasonStalled: 5, ReasonNoCapacity: 6,
}

func strp(s string) *string { return &s }

// started — задача начата: была попытка, назначение или статус дальше ready.
func started(t Task) bool {
	return t.AttemptUsed >= 1 || t.StartedAt != nil || activeStatus(t.Status) || t.Status == TaskFailed
}

// atLastAttempt — начатая незавершённая задача на последней попытке. Без
// условия «начата» лимит в одну попытку помечал бы все задачи плана.
func (c *calc) atLastAttempt(t Task) bool {
	return !c.noLive && unfinished(t) && started(t) &&
		signals.AtLastAttempt(t.AttemptUsed, t.AttemptLimit, c.th.LastAttemptLeft)
}

// risk — статус и причины Epic'а в running или paused (спека «Статус риска
// Epic»).
func (c *calc) risk(e Epic, p Progress, f Forecast) Risk {
	var rs []Reason
	add := func(code, text, taskID string) {
		r := Reason{Code: code, Text: text, EpicID: e.ID}
		if taskID != "" {
			r.TaskID = strp(taskID)
		}
		rs = append(rs, r)
	}
	tasks := c.tasksBy[e.ID]
	numBy := map[string]int{}
	for _, t := range tasks {
		numBy[t.ID] = t.Num
	}
	epicEsc := false
	seenEsc := map[string]bool{}
	for _, esc := range c.in.Escalations {
		if esc.EpicID != e.ID || !escalationOpen(esc) || seenEsc[esc.TaskID] {
			continue
		}
		seenEsc[esc.TaskID] = true
		epicEsc = true
		add(ReasonEscalation, fmt.Sprintf("эскалация по задаче #%d: %s", esc.TaskNum, esc.Reason), esc.TaskID)
	}
	if e.BudgetExhausted {
		add(ReasonBudgetExhausted, "Epic остановлен: исчерпан бюджет токенов", "")
	}
	if f.Available && e.DueAt != nil && f.At.After(*e.DueAt) {
		add(ReasonLate, fmt.Sprintf("прогноз %s позже целевой даты %s", fmtTime(*f.At, c.loc), fmtTime(*e.DueAt, c.loc)), "")
	}
	if e.TokenBudget != nil {
		if bf, ok := budgetForecast(e, p); ok && bf > *e.TokenBudget {
			add(ReasonBudgetForecast, fmt.Sprintf("прогноз расхода %s токенов больше бюджета %s", fmtInt(bf), fmtInt(*e.TokenBudget)), "")
		}
	}
	for _, t := range tasks {
		if c.atLastAttempt(t) {
			add(ReasonLastAttempt, fmt.Sprintf("задача #%d на последней попытке (%d из %d)", t.Num, t.AttemptUsed, t.AttemptLimit), t.ID)
		}
	}
	if e.Status == EpicRunning && !c.noLive {
		if !epicEsc {
			if last, ok := c.lastEpicEvent(e, tasks); ok {
				if signals.Silent(last, c.now, minutes(c.th.StalledMinutes)) {
					add(ReasonStalled, fmt.Sprintf("нет событий по задачам %s", fmtDuration(c.now.Sub(last))), "")
				}
			}
		}
		for _, t := range tasks {
			if t.WaitingRunnerSince != nil && unfinished(t) {
				if signals.Waiting(*t.WaitingRunnerSince, c.now, minutes(c.th.NoCapacityMinutes)) {
					add(ReasonNoCapacity, fmt.Sprintf("задача #%d ждёт runner'а %s", t.Num, fmtDuration(c.now.Sub(*t.WaitingRunnerSince))), t.ID)
				}
			}
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if reasonOrder[rs[i].Code] != reasonOrder[rs[j].Code] {
			return reasonOrder[rs[i].Code] < reasonOrder[rs[j].Code]
		}
		return numBy[deref(rs[i].TaskID)] < numBy[deref(rs[j].TaskID)]
	})
	status := RiskOnTrack
	for _, r := range rs {
		switch r.Code {
		case ReasonEscalation, ReasonBudgetExhausted:
			status = RiskNeedsHuman
		default:
			if status == RiskOnTrack {
				status = RiskAtRisk
			}
		}
	}
	if rs == nil {
		rs = []Reason{}
	}
	return Risk{Status: status, Reasons: rs}
}

// lastEpicEvent — последнее событие задач Epic'а при незавершённых задачах;
// без событий отсчёт идёт от запуска Epic'а.
func (c *calc) lastEpicEvent(e Epic, tasks []Task) (time.Time, bool) {
	open := false
	var last time.Time
	for _, t := range tasks {
		if unfinished(t) {
			open = true
		}
		if t.LastEventAt != nil && t.LastEventAt.After(last) {
			last = *t.LastEventAt
		}
	}
	if !open {
		return time.Time{}, false
	}
	if last.IsZero() {
		if e.StartedAt == nil {
			return time.Time{}, false
		}
		last = *e.StartedAt
	}
	return last, true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── Epic'и ──────────────────────────────────────────────────────────────

// epics — активные Epic'и и завершённые в периоде. Epic, завершённый в
// периоде и затем архивированный, показывается завершённым.
func (c *calc) epics() []EpicOverview {
	var active, done []Epic
	for _, e := range c.in.Epics {
		switch {
		case e.Status == EpicRunning || e.Status == EpicPaused:
			active = append(active, e)
		case (e.Status == EpicDone || e.Status == EpicArchived) && e.DoneAt != nil && in(*e.DoneAt, c.in.Period.From, c.in.Period.To):
			e.Status = EpicDone
			done = append(done, e)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		a, b := c.epicStart(active[i]), c.epicStart(active[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return active[i].ID < active[j].ID
	})
	sort.SliceStable(done, func(i, j int) bool {
		if !done[i].DoneAt.Equal(*done[j].DoneAt) {
			return done[i].DoneAt.After(*done[j].DoneAt)
		}
		return done[i].ID < done[j].ID
	})
	out := make([]EpicOverview, 0, len(active)+len(done))
	for _, e := range append(active, done...) {
		p := c.progress(e)
		f := c.forecast(e, p)
		eo := EpicOverview{
			ID: e.ID, Title: e.Title, Status: e.Status, Progress: p,
			DueAt: inLoc(e.DueAt, c.loc), Forecast: f, Spend: e.Spend.Amount(),
			TokenBudget: e.TokenBudget, System: e.System(),
		}
		if bf, ok := budgetForecast(e, p); ok {
			eo.BudgetForecastTokens = &bf
		}
		if e.Status == EpicDone {
			eo.Risk = Risk{Status: RiskDone, Reasons: []Reason{}}
		} else {
			eo.Risk = c.risk(e, p, f)
		}
		out = append(out, eo)
	}
	return out
}

func inLoc(t *time.Time, loc *time.Location) *time.Time {
	if t == nil {
		return nil
	}
	v := t.In(loc)
	return &v
}

// ─── решения ─────────────────────────────────────────────────────────────

var decisionOrder = map[string]int{
	DecisionPlanApproval: 0, DecisionEscalation: 1, DecisionMergeConfirmation: 2, DecisionMyStep: 3,
}

func intp(n int) *int { return &n }

// decisions — то, что ждёт людей, самые давние сверху.
func (c *calc) decisions() Decisions {
	var items []Decision
	mk := func(kind, title string, since time.Time) Decision {
		w := int64(c.now.Sub(since) / time.Second)
		if w < 0 {
			w = 0
		}
		return Decision{Kind: kind, Title: title, Since: since.In(c.loc), WaitingSeconds: w}
	}
	for _, e := range c.in.Epics {
		if e.Status != EpicPlanned || len(c.tasksBy[e.ID]) == 0 {
			continue
		}
		since := e.CreatedAt
		if e.PlannedAt != nil {
			since = *e.PlannedAt
		}
		d := mk(DecisionPlanApproval, "Утвердить план: "+e.Title, since)
		d.EpicID = strp(e.ID)
		items = append(items, d)
	}
	for _, esc := range c.in.Escalations {
		if !escalationOpen(esc) {
			continue
		}
		title := "Эскалация: " + esc.Reason
		if esc.TaskID != "" {
			title = fmt.Sprintf("Эскалация по задаче #%d %s", esc.TaskNum, esc.Title)
		}
		d := mk(DecisionEscalation, title, esc.OpenedAt)
		d.AttentionID = strp(esc.ID)
		d.Reason = strp(esc.Reason)
		if esc.EpicID != "" {
			d.EpicID = strp(esc.EpicID)
		}
		if esc.TaskID != "" {
			d.TaskID, d.TaskNum = strp(esc.TaskID), intp(esc.TaskNum)
		}
		if esc.ClaimedBy != "" {
			d.ClaimedBy = strp(esc.ClaimedBy)
		}
		items = append(items, d)
	}
	for _, w := range c.in.MergeWaits {
		d := mk(DecisionMergeConfirmation, fmt.Sprintf("Подтвердить merge: #%d %s", w.TaskNum, w.Title), w.Since)
		d.EpicID, d.TaskID, d.TaskNum = strp(w.EpicID), strp(w.TaskID), intp(w.TaskNum)
		items = append(items, d)
	}
	for _, s := range c.in.MySteps {
		d := mk(DecisionMyStep, fmt.Sprintf("Мой шаг: #%d %s", s.TaskNum, s.Title), s.Since)
		d.EpicID, d.TaskID, d.TaskNum, d.RunID = strp(s.EpicID), strp(s.TaskID), intp(s.TaskNum), strp(s.RunID)
		items = append(items, d)
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		if decisionOrder[a.Kind] != decisionOrder[b.Kind] {
			return decisionOrder[a.Kind] < decisionOrder[b.Kind]
		}
		return a.Title < b.Title
	})
	var counts DecisionCounts
	for _, d := range items {
		switch d.Kind {
		case DecisionPlanApproval:
			counts.PlanApproval++
		case DecisionEscalation:
			counts.Escalation++
		case DecisionMergeConfirmation:
			counts.MergeConfirmation++
		case DecisionMyStep:
			counts.MyStep++
		}
	}
	if items == nil {
		items = []Decision{}
	}
	return Decisions{Counts: counts, Items: items}
}

// ─── команда ─────────────────────────────────────────────────────────────

// periodEnd — конец периода, не позже момента расчёта.
func (c *calc) periodEnd() time.Time {
	if c.in.Period.To.After(c.now) {
		return c.now
	}
	return c.in.Period.To
}

// perDay — пересчёт на сутки: период короче суток считается за сутки.
func (c *calc) perDay(x float64) float64 {
	// Делитель — прошедшая часть периода: у недели, которая ещё идёт,
	// конец периода в будущем.
	days := c.periodEnd().Sub(c.in.Period.From).Hours() / 24
	if days < 1 {
		days = 1
	}
	return round(x/days, 2)
}

func (c *calc) team() Team {
	from, to := c.in.Period.From, c.periodEnd()
	periodSecs := to.Sub(from).Seconds()
	loads := map[string]*RoleLoad{}
	get := func(id string) *RoleLoad {
		if l, ok := loads[id]; ok {
			return l
		}
		l := &RoleLoad{Role: id, Title: "без роли", Color: "slate"}
		if r, ok := c.roleBy[id]; ok {
			l.Title, l.Color = r.Title, r.Color
		} else if id != "" {
			l.Title = id
		}
		loads[id] = l
		return l
	}
	for _, r := range c.in.Roles {
		get(r.ID)
	}
	for _, s := range c.in.Sessions {
		if s.Human {
			continue
		}
		iv := sessionInterval(s, c.now)
		busy := clippedDuration(iv, from, to)
		running := s.End == nil
		started := in(s.Start, from, to)
		if busy == 0 && !running && !started {
			continue
		}
		l := get(s.RoleID)
		if running {
			l.BusyNow++
		}
		if started {
			l.Sessions++
		}
		l.BusySeconds += int64(busy / time.Second)
	}
	var roles []RoleLoad
	for _, r := range c.in.Roles {
		roles = append(roles, *loads[r.ID])
	}
	var extra []string
	for id := range loads {
		if _, ok := c.roleBy[id]; !ok {
			extra = append(extra, id)
		}
	}
	// Роли вне каталога по id, группа «без роли» последней.
	sort.Slice(extra, func(i, j int) bool {
		if (extra[i] == "") != (extra[j] == "") {
			return extra[j] == ""
		}
		return extra[i] < extra[j]
	})
	for _, id := range extra {
		roles = append(roles, *loads[id])
	}
	for i := range roles {
		if periodSecs > 0 {
			roles[i].AvgBusy = round(float64(roles[i].BusySeconds)/periodSecs, 3)
		}
	}
	if roles == nil {
		roles = []RoleLoad{}
	}
	return Team{Roles: roles, Throughput: c.throughput(), Quality: c.quality()}
}

// doneInPeriod — задачи, завершённые в периоде.
func (c *calc) doneInPeriod() []Task {
	var out []Task
	for _, t := range c.in.Tasks {
		if d, ok := doneAt(t); ok && in(d, c.in.Period.From, c.in.Period.To) {
			out = append(out, t)
		}
	}
	return out
}

func (c *calc) throughput() Throughput {
	var th Throughput
	for _, t := range c.doneInPeriod() {
		th.TasksDone++
		th.UnitsDone += float64(t.Estimate)
	}
	th.PerDay = PerDay{Tasks: c.perDay(float64(th.TasksDone)), Units: c.perDay(th.UnitsDone)}
	return th
}

func (c *calc) quality() Quality {
	q := Quality{Escalations: []ReasonCount{}}
	returned := map[string]bool{}
	for _, r := range c.in.StepRuns {
		if r.StepKind == StepKindReview && r.Verdict == VerdictChanges {
			returned[r.TaskID] = true
		}
		if r.Verdict != VerdictChanges || !in(r.At, c.in.Period.From, c.in.Period.To) {
			continue
		}
		switch r.RoleID {
		case RoleTester:
			q.TesterReturns++
		case RoleReviewer:
			q.ReviewerReturns++
		}
	}
	done := c.doneInPeriod()
	if len(done) > 0 {
		pass := 0
		for _, t := range done {
			if !returned[t.ID] {
				pass++
			}
		}
		rate := round(float64(pass)/float64(len(done)), 4)
		q.FirstPassReviewRate = &rate
	}
	taskBy := map[string]Task{}
	for _, t := range c.in.Tasks {
		taskBy[t.ID] = t
	}
	firstConflict := map[string]time.Time{}
	for _, mc := range c.in.MergeConflicts {
		if mc.Exhausted || !in(mc.At, c.in.Period.From, c.in.Period.To) {
			continue
		}
		if f, ok := firstConflict[mc.TaskID]; !ok || mc.At.Before(f) {
			firstConflict[mc.TaskID] = mc.At
		}
	}
	q.MergeConflicts.Returned = len(firstConflict)
	for id, at := range firstConflict {
		if d, ok := doneAt(taskBy[id]); ok && d.After(at) {
			q.MergeConflicts.Done++
		}
	}
	byReason := map[string]int{}
	for _, e := range c.in.Escalations {
		if in(e.OpenedAt, c.in.Period.From, c.in.Period.To) {
			byReason[e.Reason]++
		}
	}
	for r, n := range byReason {
		q.Escalations = append(q.Escalations, ReasonCount{Reason: r, Count: n})
	}
	sort.Slice(q.Escalations, func(i, j int) bool {
		if q.Escalations[i].Count != q.Escalations[j].Count {
			return q.Escalations[i].Count > q.Escalations[j].Count
		}
		return q.Escalations[i].Reason < q.Escalations[j].Reason
	})
	return q
}

// ─── деньги ──────────────────────────────────────────────────────────────

func (c *calc) money() Money {
	now := c.now
	// Месяц берётся от момента на наносекунду раньше: полуинтервал [.., now)
	// с now ровно в 00:00 первого числа целиком лежит в прошлом месяце.
	ref := now.Add(-time.Nanosecond)
	m := Money{
		Today:       sumUsage(c.in.Usage, startOfDay(now, c.loc), now).Amount(),
		Last7d:      sumUsage(c.in.Usage, now.Add(-7*24*time.Hour), now).Amount(),
		MonthToDate: sumUsage(c.in.Usage, startOfMonth(ref, c.loc), now).Amount(),
	}
	period := sumUsage(c.in.Usage, c.in.Period.From, c.in.Period.To)
	m.Period = period.Amount()
	m.ByRole = c.shares(period, func(r UsageRecord) string { return r.RoleID }, func(k string) string {
		if r, ok := c.roleBy[k]; ok {
			return r.Title
		}
		if k == "" {
			return "без роли"
		}
		return k
	})
	m.ByModel = c.shares(period, func(r UsageRecord) string { return r.Model }, func(k string) string {
		if k == "" {
			return "модель неизвестна"
		}
		return k
	})
	var sum float64
	var n int
	for _, t := range c.doneInPeriod() {
		if t.Spend.CostRecords > 0 {
			sum += t.Spend.CostUSD
			n++
		}
	}
	if n > 0 {
		avg := round(sum/float64(n), 2)
		m.AvgCostPerTask = &avg
	}
	if c.in.DailyTokenBudget != nil {
		m.DailyTokenBudget = &DailyBudget{Budget: *c.in.DailyTokenBudget, UsedToday: sumUsage(c.in.Usage, startOfDay(now, c.loc), now).Tokens()}
	}
	m.MonthlyBudgetUSD = c.in.MonthlyBudgetUSD
	if mtd := m.MonthToDate.CostUSD; mtd != nil {
		day := ref.In(c.loc).Day()
		f := round(*mtd/float64(day)*float64(daysInMonth(ref, c.loc)), 2)
		m.MonthForecastUSD = &f
		if b := c.in.MonthlyBudgetUSD; b != nil && f > *b {
			over := round(f-*b, 2)
			m.MonthOverBudgetUSD = &over
		}
	}
	return m
}

// shares — разрез расхода периода по ключу с долями: по деньгам, если
// стоимость известна, иначе по токенам.
func (c *calc) shares(total Totals, key func(UsageRecord) string, title func(string) string) []Share {
	groups := map[string]*Totals{}
	for _, r := range c.in.Usage {
		if !in(r.TS, c.in.Period.From, c.in.Period.To) {
			continue
		}
		k := key(r)
		if groups[k] == nil {
			groups[k] = &Totals{}
		}
		groups[k].Add(r)
	}
	byCost := total.CostRecords > 0 && total.CostUSD > 0
	out := make([]Share, 0, len(groups))
	for k, g := range groups {
		s := Share{Key: k, Title: title(k), Amount: g.Amount()}
		switch {
		case byCost && g.CostRecords > 0:
			v := round(g.CostUSD/total.CostUSD, 4)
			s.Share = &v
		case byCost:
			// Группа без стоимости в денежном разрезе: доля неизвестна.
		case total.Tokens() > 0:
			v := round(float64(g.Tokens())/float64(total.Tokens()), 4)
			s.Share = &v
		default:
			v := 0.0
			s.Share = &v
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Share, out[j].Share
		if (a == nil) != (b == nil) {
			return b == nil
		}
		if a != nil && *a != *b {
			return *a > *b
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ─── поставки и риски ────────────────────────────────────────────────────

func activeStatus(s string) bool {
	return s == TaskRunning || s == TaskTesting || s == TaskReview || s == TaskFixing
}

func (c *calc) delivery(epics []EpicOverview) Delivery {
	d := Delivery{
		Environments: []EnvStatus{}, Deployments: []Deployment{}, Stuck: []StuckTask{},
		LastAttempt: []AttemptTask{}, BudgetRisks: []BudgetRisk{},
	}
	for _, e := range c.in.Environments {
		d.Environments = append(d.Environments, EnvStatus{ID: e.ID, Name: e.Name, Last: c.deployInLoc(e.Last)})
	}
	for _, dep := range c.in.Deployments {
		if in(dep.CreatedAt, c.in.Period.From, c.in.Period.To) {
			d.Deployments = append(d.Deployments, *c.deployInLoc(&dep))
		}
	}
	sort.SliceStable(d.Deployments, func(i, j int) bool {
		a, b := d.Deployments[i], d.Deployments[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	stuckAfter := minutes(c.th.StuckMinutes)
	waitAfter := minutes(c.th.NoCapacityMinutes)
	tasks := append([]Task{}, c.in.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Num < tasks[j].Num })
	for _, t := range tasks {
		es := c.epicStat[t.EpicID]
		if !unfinished(t) || (es != EpicRunning && es != EpicPaused) {
			continue
		}
		live := es == EpicRunning && !c.noLive
		waiting := live && t.WaitingRunnerSince != nil && signals.Waiting(*t.WaitingRunnerSince, c.now, waitAfter)
		silent := live && activeStatus(t.Status) && !c.openEsc[t.ID] && !c.mergeWait[t.ID] &&
			t.LastEventAt != nil && signals.Silent(*t.LastEventAt, c.now, stuckAfter)
		if waiting || silent {
			d.Stuck = append(d.Stuck, StuckTask{
				TaskID: t.ID, TaskNum: t.Num, Title: t.Title, Status: t.Status,
				LastEventAt: inLoc(t.LastEventAt, c.loc), WaitingRunner: waiting,
			})
		}
		if c.atLastAttempt(t) {
			d.LastAttempt = append(d.LastAttempt, AttemptTask{
				TaskID: t.ID, TaskNum: t.Num, Title: t.Title, AttemptUsed: t.AttemptUsed, AttemptLimit: t.AttemptLimit,
			})
		}
	}
	for _, e := range epics {
		for _, r := range e.Risk.Reasons {
			if r.Code == ReasonBudgetForecast && e.TokenBudget != nil && e.BudgetForecastTokens != nil {
				d.BudgetRisks = append(d.BudgetRisks, BudgetRisk{
					EpicID: e.ID, Title: e.Title, TokenBudget: *e.TokenBudget, BudgetForecastTokens: *e.BudgetForecastTokens,
				})
			}
		}
	}
	return d
}

func (c *calc) deployInLoc(d *Deployment) *Deployment {
	if d == nil {
		return nil
	}
	v := *d
	v.CreatedAt = v.CreatedAt.In(c.loc)
	v.EndedAt = inLoc(v.EndedAt, c.loc)
	return &v
}

// minutes — порог в минутах как длительность.
func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }
