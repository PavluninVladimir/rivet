package overview

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Периоды обзора (api-contract) и период отчёта.
const (
	PeriodToday  = "today"
	Period7d     = "7d"
	Period30d    = "30d"
	PeriodMonth  = "month"
	PeriodCustom = "custom"
)

// MaxReportDays — самый длинный период отчёта.
const MaxReportDays = 92

// ErrPeriod — неверный период; API отвечает 422 с полем period.
var ErrPeriod = errors.New("неверный период")

// Period — границы периода, полуинтервал [From, To) в поясе TZ.
type Period struct {
	Name string    `json:"name"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	TZ   string    `json:"tz"`
}

// NewPeriod — период обзора на момент now. Пустое имя означает 7d. Границы
// дня и месяца считаются в поясе loc.
func NewPeriod(name string, now time.Time, loc *time.Location) (Period, error) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	p := Period{Name: name, To: now, TZ: loc.String()}
	switch name {
	case "", Period7d:
		p.Name, p.From = Period7d, now.Add(-7*24*time.Hour)
	case Period30d:
		p.From = now.Add(-30 * 24 * time.Hour)
	case PeriodToday:
		p.From = startOfDay(now, loc)
	case PeriodMonth:
		p.From = startOfMonth(now, loc)
	default:
		return Period{}, fmt.Errorf("%w: %q, допустимы today, 7d, 30d, month", ErrPeriod, name)
	}
	return p, nil
}

// CustomPeriod — период отчёта [from, to): from раньше to, не длиннее
// MaxReportDays суток.
func CustomPeriod(from, to time.Time, loc *time.Location) (Period, error) {
	if loc == nil {
		loc = time.UTC
	}
	if !from.Before(to) {
		return Period{}, fmt.Errorf("%w: начало периода должно быть раньше конца", ErrPeriod)
	}
	if to.Sub(from) > MaxReportDays*24*time.Hour {
		return Period{}, fmt.Errorf("%w: период длиннее %d суток", ErrPeriod, MaxReportDays)
	}
	return Period{Name: PeriodCustom, From: from.In(loc), To: to.In(loc), TZ: loc.String()}, nil
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func startOfMonth(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

// daysInMonth — число суток месяца момента t в поясе loc.
func daysInMonth(t time.Time, loc *time.Location) int {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, loc).Day()
}

// in — момент в полуинтервале [from, to).
func in(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }

// ceilMinute округляет момент вверх до минуты.
func ceilMinute(t time.Time) time.Time {
	tr := t.Truncate(time.Minute)
	if tr.Equal(t) {
		return t
	}
	return tr.Add(time.Minute)
}

func round(x float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(x*p) / p
}

// ─── интервалы ───────────────────────────────────────────────────────────

// Interval — отрезок работы [Start, End).
type Interval struct {
	Start time.Time
	End   time.Time
}

// UnionDuration — длительность объединения интервалов внутри окна
// [from, to). Пересечения не суммируются: два параллельных агента за час
// дают один час работы конвейера (design, решение 4).
func UnionDuration(intervals []Interval, from, to time.Time) time.Duration {
	clipped := make([]Interval, 0, len(intervals))
	for _, iv := range intervals {
		s, e := iv.Start, iv.End
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if e.After(s) {
			clipped = append(clipped, Interval{s, e})
		}
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].Start.Before(clipped[j].Start) })
	var total time.Duration
	var cur Interval
	open := false
	for _, iv := range clipped {
		switch {
		case !open:
			cur, open = iv, true
		case !iv.Start.After(cur.End):
			if iv.End.After(cur.End) {
				cur.End = iv.End
			}
		default:
			total += cur.End.Sub(cur.Start)
			cur = iv
		}
	}
	if open {
		total += cur.End.Sub(cur.Start)
	}
	return total
}

// clippedDuration — длительность одного интервала внутри окна.
func clippedDuration(iv Interval, from, to time.Time) time.Duration {
	s, e := iv.Start, iv.End
	if s.Before(from) {
		s = from
	}
	if e.After(to) {
		e = to
	}
	if e.After(s) {
		return e.Sub(s)
	}
	return 0
}

// sessionInterval — интервал сессии; идущая сессия длится до now.
func sessionInterval(s Session, now time.Time) Interval {
	end := now
	if s.End != nil {
		end = *s.End
	}
	return Interval{Start: s.Start, End: end}
}

// agentIntervals — интервалы агентских сессий, подходящих фильтру.
func agentIntervals(sessions []Session, now time.Time, keep func(Session) bool) []Interval {
	var out []Interval
	for _, s := range sessions {
		if s.Human || (keep != nil && !keep(s)) {
			continue
		}
		out = append(out, sessionInterval(s, now))
	}
	return out
}

// ─── расход ──────────────────────────────────────────────────────────────

// Totals — сумма записей usage. Store может отдать её готовой (расход
// Epic'а и задачи за всё время) или пакет соберёт её из записей.
type Totals struct {
	Records     int
	CostRecords int
	TokensIn    int64
	TokensOut   int64
	CostUSD     float64
	DurationS   int64
}

// Add добавляет запись. Стоимость без цены в денежную сумму не входит.
func (t *Totals) Add(r UsageRecord) {
	t.Records++
	t.TokensIn += r.TokensIn
	t.TokensOut += r.TokensOut
	t.DurationS += r.DurationS
	if r.CostKnown {
		t.CostRecords++
		t.CostUSD += r.CostUSD
	}
}

// Tokens — все токены.
func (t Totals) Tokens() int64 { return t.TokensIn + t.TokensOut }

// Amount — расход в форме контракта: без записей токены и деньги
// недоступны, без записей со стоимостью недоступны деньги, часть записей
// без стоимости даёт пометку partial_cost.
func (t Totals) Amount() Amount {
	a := Amount{DurationS: t.DurationS}
	if t.Records == 0 {
		return a
	}
	tin, tout := t.TokensIn, t.TokensOut
	a.TokensIn, a.TokensOut = &tin, &tout
	if t.CostRecords > 0 {
		c := round(t.CostUSD, 6)
		a.CostUSD = &c
		a.PartialCost = t.CostRecords < t.Records
	}
	return a
}

// sumUsage — сумма записей в окне [from, to).
func sumUsage(records []UsageRecord, from, to time.Time) Totals {
	var t Totals
	for _, r := range records {
		if in(r.TS, from, to) {
			t.Add(r)
		}
	}
	return t
}
