package overview

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Разделитель тысяч и пробел перед единицами: узкий неразрывный пробел.
// Формат не зависит от локали (design, решение 7).
const nbsp = " "

// fmtInt — целое с разделителем тысяч.
func fmtInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteString(nbsp)
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtNum — число с фиксированным числом знаков после запятой.
func fmtNum(x float64, digits int) string {
	x = round(x, digits)
	neg := x < 0
	if neg {
		x = -x
	}
	whole := math.Floor(x)
	out := fmtInt(int64(whole))
	if digits > 0 {
		frac := int64(math.Round((x - whole) * math.Pow(10, float64(digits))))
		out += "," + fmt.Sprintf("%0*d", digits, frac)
	}
	if neg && out != "0" {
		return "-" + out
	}
	return out
}

// fmtUnits — единицы оценки: целые без дробной части.
func fmtUnits(x float64) string {
	if x == math.Trunc(x) {
		return fmtInt(int64(x))
	}
	return fmtNum(x, 2)
}

func fmtUSD(x float64) string { return fmtNum(x, 2) + nbsp + "$" }

func fmtPct(r float64) string { return fmtNum(r*100, 0) + nbsp + "%" }

// fmtTime — момент в поясе запроса.
func fmtTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// fmtDuration — длительность в минутах, часах и сутках.
func fmtDuration(d time.Duration) string {
	m := int64(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%d мин", m)
	case m < 24*60:
		return fmt.Sprintf("%d ч %02d мин", m/60, m%60)
	default:
		return fmt.Sprintf("%d сут %d ч", m/(24*60), (m%(24*60))/60)
	}
}

// mdCell — значение для ячейки Markdown-таблицы.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

func amountText(a Amount) string {
	tokens := "токены: нет данных"
	if a.TokensIn != nil && a.TokensOut != nil {
		tokens = "токены: " + fmtInt(*a.TokensIn) + " входящих, " + fmtInt(*a.TokensOut) + " исходящих"
	}
	cost := "стоимость неизвестна"
	if a.CostUSD != nil {
		cost = fmtUSD(*a.CostUSD)
		if a.PartialCost {
			cost += " (часть расхода без стоимости)"
		}
	}
	return cost + ", " + tokens
}

func costText(a Amount) string {
	if a.CostUSD == nil {
		return "нет данных"
	}
	s := fmtUSD(*a.CostUSD)
	if a.PartialCost {
		s += "*"
	}
	return s
}

var riskTitle = map[string]string{
	RiskOnTrack: "в графике", RiskAtRisk: "под риском", RiskNeedsHuman: "нужен человек", RiskDone: "завершён",
}

var epicStatusTitle = map[string]string{
	EpicRunning: "в работе", EpicPaused: "на паузе", EpicDone: "завершён", EpicPlanned: "план", EpicArchived: "в архиве",
}

func reasonCodes(rs []Reason) string {
	codes := make([]string, 0, len(rs))
	seen := map[string]bool{}
	for _, r := range rs {
		if !seen[r.Code] {
			seen[r.Code] = true
			codes = append(codes, r.Code)
		}
	}
	return strings.Join(codes, ", ")
}

// Report — еженедельный отчёт за период data.Period в Markdown (спека
// «Еженедельный отчёт»). Снимок приводится к моменту среза (asOf): конец
// периода, но не позже data.Now. Данные после среза на текст не влияют,
// статус риска считается на срез. Без data.Live (прошедший период)
// значения, известные только на момент формирования, не используются.
// Одинаковые данные, период, пояс и Live дают побайтно одинаковый текст.
func Report(data Input) string {
	live := data.Live
	data, cut := asOf(data)
	c := newCalc(data, !live)
	ov := c.overview()
	loc := c.loc
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	name := data.ProjectName
	if name == "" {
		name = data.ProjectID
	}
	w("# Отчёт по проекту «%s»\n\n", name)
	w("Период: с %s по %s (%s)", fmtTime(data.Period.From, loc), fmtTime(data.Period.To, loc), loc.String())
	if cut.Before(data.Period.To) {
		w(", данные на %s: период ещё идёт", fmtTime(cut, loc))
	}
	w("\n\n")
	// Ожидания подтверждения merge за прошедший период неизвестны.
	mergeUnknown := !live

	// 1. Итог.
	var doneEpics, onTrack, atRisk, needsHuman int
	for _, e := range ov.Epics {
		// Системные Epic'и в итог не входят, как и в число активных.
		if e.System {
			continue
		}
		switch e.Risk.Status {
		case RiskDone:
			doneEpics++
		case RiskOnTrack:
			onTrack++
		case RiskAtRisk:
			atRisk++
		case RiskNeedsHuman:
			needsHuman++
		}
	}
	th := ov.Team.Throughput
	w("## 1. Итог периода\n\n")
	w("- Завершено задач: %d (%s ед. оценки), Epic'ов: %d.\n", th.TasksDone, fmtUnits(th.UnitsDone), doneEpics)
	w("- Активных Epic'ов на конец периода: %d. В графике: %d, под риском: %d, нужен человек: %d.\n",
		ov.Summary.ActiveEpics, onTrack, atRisk, needsHuman)
	w("- Расход за период: %s.\n", amountText(ov.Money.Period))
	if mergeUnknown {
		w("- Решений ждут на конец периода: %d, без подтверждений merge (по ним нет данных).\n", len(ov.Decisions.Items))
	} else {
		w("- Решений ждут на конец периода: %d.\n", len(ov.Decisions.Items))
	}
	if r := ov.Team.Quality.FirstPassReviewRate; r != nil {
		w("- Review с первого раза: %s.\n", fmtPct(*r))
	} else {
		w("- Review с первого раза: нет завершённых задач.\n")
	}

	// 2. Epic'и.
	w("\n## 2. Epic'и\n\n")
	if len(ov.Epics) == 0 {
		w("Активных и завершённых за период Epic'ов нет.\n")
	} else {
		w("| Epic | Статус | Прогресс | Прогноз | Риск |\n|---|---|---|---|---|\n")
		for _, e := range ov.Epics {
			p := e.Progress
			progress := fmt.Sprintf("%s из %s ед., %d из %d задач", fmtUnits(p.DoneUnits), fmtUnits(p.TotalUnits), p.DoneTasks, p.TotalTasks)
			forecast := e.Forecast.Reason
			if e.Forecast.Available {
				src := "проекта"
				if deref(e.Forecast.VelocitySource) == SourceEpic {
					src = "Epic'а"
				}
				forecast = fmt.Sprintf("%s (темп %s %s ед./ч)", fmtTime(*e.Forecast.At, loc), src, fmtNum(*e.Forecast.VelocityUnitsPerHour, 2))
			}
			risk := riskTitle[e.Risk.Status]
			if codes := reasonCodes(e.Risk.Reasons); codes != "" {
				risk += ": " + codes
			}
			title := e.Title
			if e.System {
				title += " (системный)"
			}
			w("| %s | %s | %s | %s | %s |\n", mdCell(title), epicStatusTitle[e.Status], progress, mdCell(forecast), risk)
		}
	}

	// 3. Пропускная способность и качество.
	q := ov.Team.Quality
	w("\n## 3. Пропускная способность и качество\n\n")
	w("- Завершено задач: %d, единиц оценки: %s. В сутки: %s задачи, %s единицы.\n",
		th.TasksDone, fmtUnits(th.UnitsDone), fmtNum(th.PerDay.Tasks, 2), fmtNum(th.PerDay.Units, 2))
	if q.FirstPassReviewRate != nil {
		w("- Review с первого раза: %s.\n", fmtPct(*q.FirstPassReviewRate))
	}
	w("- Возвраты Тестировщика: %d, Ревьюера: %d.\n", q.TesterReturns, q.ReviewerReturns)
	w("- Конфликты merge, возвращённые агентам: %d, из них задач завершено: %d.\n", q.MergeConflicts.Returned, q.MergeConflicts.Done)
	if len(q.Escalations) > 0 {
		parts := make([]string, 0, len(q.Escalations))
		for _, e := range q.Escalations {
			parts = append(parts, fmt.Sprintf("%s %d", e.Reason, e.Count))
		}
		w("- Эскалации по причинам: %s.\n", strings.Join(parts, ", "))
	}
	w("\n| Роль | Сессий | В среднем занято | Часы работы |\n|---|---|---|---|\n")
	for _, r := range ov.Team.Roles {
		w("| %s | %d | %s | %s |\n", mdCell(r.Title), r.Sessions, fmtNum(r.AvgBusy, 2), fmtNum(float64(r.BusySeconds)/3600, 1))
	}

	// 4. Деньги.
	m := ov.Money
	w("\n## 4. Деньги\n\n")
	w("- За период: %s.\n", amountText(m.Period))
	w("- С начала месяца на конец периода: %s.\n", amountText(m.MonthToDate))
	if m.AvgCostPerTask != nil {
		w("- Средняя стоимость завершённой задачи: %s.\n", fmtUSD(*m.AvgCostPerTask))
	}
	if m.MonthForecastUSD != nil {
		line := "- Прогноз расхода месяца: " + fmtUSD(*m.MonthForecastUSD)
		if m.MonthlyBudgetUSD != nil {
			line += ", плановый бюджет " + fmtUSD(*m.MonthlyBudgetUSD)
			if m.MonthOverBudgetUSD != nil {
				line += ", превышение " + fmtUSD(*m.MonthOverBudgetUSD)
			} else {
				line += ", в пределах бюджета"
			}
		}
		w("%s.\n", line)
	}
	for _, e := range ov.Epics {
		if e.TokenBudget == nil {
			continue
		}
		used := "нет данных"
		if e.Spend.TokensIn != nil && e.Spend.TokensOut != nil {
			used = fmtInt(*e.Spend.TokensIn + *e.Spend.TokensOut)
		}
		forecast := "нет данных"
		if e.BudgetForecastTokens != nil {
			forecast = fmtInt(*e.BudgetForecastTokens)
		}
		w("- Epic «%s»: израсходовано %s, прогноз %s из бюджета %s токенов.\n", e.Title, used, forecast, fmtInt(*e.TokenBudget))
	}
	writeShares := func(title string, shares []Share) {
		w("\n| %s | Стоимость | Доля |\n|---|---|---|\n", title)
		if len(shares) == 0 {
			w("| нет расхода | | |\n")
		}
		for _, s := range shares {
			share := "нет данных"
			if s.Share != nil {
				share = fmtPct(*s.Share)
			}
			w("| %s | %s | %s |\n", mdCell(s.Title), costText(s.Amount), share)
		}
	}
	writeShares("Роль", m.ByRole)
	writeShares("Модель", m.ByModel)

	// 5. Эскалации и решения людей.
	from, to := data.Period.From, data.Period.To
	var opened, closed, openAtEnd int
	var waitSum time.Duration
	for _, e := range data.Escalations {
		if in(e.OpenedAt, from, to) {
			opened++
		}
		if e.ResolvedAt != nil && in(*e.ResolvedAt, from, to) {
			closed++
			waitSum += e.ResolvedAt.Sub(e.OpenedAt)
		}
		if e.OpenedAt.Before(to) && (e.ResolvedAt == nil || !e.ResolvedAt.Before(to)) {
			openAtEnd++
		}
	}
	w("\n## 5. Эскалации и решения людей\n\n")
	w("- Открыто эскалаций за период: %d, закрыто: %d.\n", opened, closed)
	if closed > 0 {
		w("- Среднее время ответа: %s.\n", fmtDuration(waitSum/time.Duration(closed)))
	} else {
		w("- Среднее время ответа: закрытых эскалаций нет.\n")
	}
	w("- Открыто на конец периода: %d.\n", openAtEnd)
	dc := ov.Decisions.Counts
	merge := strconv.Itoa(dc.MergeConfirmation)
	if mergeUnknown {
		merge = "нет данных"
	}
	w("- Ждут решения: планы %d, эскалации %d, подтверждения merge %s.\n", dc.PlanApproval, dc.Escalation, merge)

	// 6. Поставки.
	w("\n## 6. Поставки\n\n")
	envName := map[string]string{}
	for _, e := range ov.Delivery.Environments {
		envName[e.ID] = e.Name
	}
	if len(ov.Delivery.Deployments) == 0 {
		w("Выкаток за период не было.\n")
	} else {
		w("| Окружение | Версия | Статус | Время | Инициатор |\n|---|---|---|---|---|\n")
		for _, d := range ov.Delivery.Deployments {
			status := d.Status
			if d.Rollback {
				status += ", откат"
			}
			w("| %s | %s | %s | %s | %s |\n", mdCell(envName[d.EnvID]), mdCell(d.Version), status, fmtTime(d.CreatedAt, loc), mdCell(d.Initiator))
		}
	}
	if len(ov.Delivery.Environments) > 0 {
		w("\nПоследние выкатки окружений:\n\n")
		for _, e := range ov.Delivery.Environments {
			if e.Last == nil {
				w("- %s: выкаток не было.\n", e.Name)
				continue
			}
			w("- %s: %s, %s, %s.\n", e.Name, e.Last.Version, e.Last.Status, fmtTime(e.Last.CreatedAt, loc))
		}
	}

	// 7. Риски.
	w("\n## 7. Риски на конец периода\n\n")
	var risks []string
	for _, e := range ov.Epics {
		for _, r := range e.Risk.Reasons {
			risks = append(risks, fmt.Sprintf("Epic «%s», %s: %s.", e.Title, r.Code, r.Text))
		}
	}
	for _, s := range ov.Delivery.Stuck {
		what := "нет событий"
		if s.LastEventAt != nil {
			what += " с " + fmtTime(*s.LastEventAt, loc)
		}
		if s.WaitingRunner {
			what = "ждёт runner'а"
		}
		risks = append(risks, fmt.Sprintf("Задача #%d «%s» застряла: %s.", s.TaskNum, s.Title, what))
	}
	if len(risks) == 0 {
		w("Рисков нет.\n")
	}
	for _, r := range risks {
		w("- %s\n", r)
	}
	return b.String()
}
