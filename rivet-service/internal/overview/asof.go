package overview

import "time"

// Статус выкатки, которая на момент среза ещё шла.
const deployInProgress = "deploying"

// cutOf — момент среза отчёта: конец периода, но не позже момента снимка.
// Отчёт за неделю, которая ещё идёт, строится на текущий момент, а не на
// будущий конец периода.
func cutOf(in Input) time.Time {
	cut := in.Period.To
	if !in.Now.IsZero() && in.Now.Before(cut) {
		cut = in.Now
	}
	return cut
}

// asOf приводит снимок к моменту среза cutOf(in): отчёт показывает
// состояние на конец периода, данные, появившиеся позже, на текст не
// влияют. Возвращает срез и сам момент.
//
// Восстановление идёт по таймстампам: задача сделана, если DoneAt < cut;
// Epic активен, если StartedAt < cut, а DoneAt пуст или не раньше cut;
// эскалация открыта, если OpenedAt < cut, а ResolvedAt пуст или не раньше
// cut; выкатка, закончившаяся после cut, на cut ещё шла. Расход Epic'ов и
// задач пересчитывается из записей usage до cut.
//
// Без in.Live (отчёт за прошедший период) значения, известные только на
// момент формирования, не используются: попытки, последнее событие и
// ожидание runner'а задач, остановка Epic'а по бюджету, ожидания
// подтверждения merge.
func asOf(in Input) (Input, time.Time) {
	cut := cutOf(in)
	past := !in.Live
	before := func(t time.Time) bool { return t.Before(cut) }
	after := func(t *time.Time) bool { return t != nil && !before(*t) }
	out := in
	out.Now = cut
	// Отчёт общий для всех участников: «мои шаги» в нём не участвуют.
	out.MySteps = nil

	out.Usage = nil
	epicSpend, taskSpend := map[string]*Totals{}, map[string]*Totals{}
	add := func(m map[string]*Totals, id string, r UsageRecord) {
		if id == "" {
			return
		}
		if m[id] == nil {
			m[id] = &Totals{}
		}
		m[id].Add(r)
	}
	for _, r := range in.Usage {
		if !before(r.TS) {
			continue
		}
		out.Usage = append(out.Usage, r)
		add(epicSpend, r.EpicID, r)
		add(taskSpend, r.TaskID, r)
	}
	spend := func(m map[string]*Totals, id string) Totals {
		if t := m[id]; t != nil {
			return *t
		}
		return Totals{}
	}

	out.Tasks = nil
	for _, t := range in.Tasks {
		if !t.CreatedAt.IsZero() && !before(t.CreatedAt) {
			continue
		}
		if after(t.StartedAt) {
			t.StartedAt = nil
		}
		if after(t.DoneAt) {
			// Завершена после среза: на срез была в работе или ждала.
			t.DoneAt = nil
			t.Status = TaskQueued
			if t.StartedAt != nil {
				t.Status = TaskRunning
			}
		}
		if past {
			t.AttemptUsed, t.LastEventAt, t.WaitingRunnerSince = 0, nil, nil
		}
		t.Spend = spend(taskSpend, t.ID)
		out.Tasks = append(out.Tasks, t)
	}

	out.Epics = nil
	for _, e := range in.Epics {
		if !e.CreatedAt.IsZero() && !before(e.CreatedAt) {
			continue
		}
		if after(e.PlannedAt) {
			e.PlannedAt = nil
		}
		startedAfter, doneAfter := after(e.StartedAt), after(e.DoneAt)
		if startedAfter {
			e.StartedAt = nil
		}
		if doneAfter {
			e.DoneAt = nil
		}
		switch {
		case doneAfter && (e.Status == EpicDone || e.Status == EpicArchived):
			e.Status = EpicPlanned
			if e.StartedAt != nil {
				e.Status = EpicRunning
			}
		case e.Status == EpicArchived && e.DoneAt != nil:
			// Завершён до среза, архивирован неизвестно когда: на срез
			// показывается завершённым.
			e.Status = EpicDone
		case startedAfter && (e.Status == EpicRunning || e.Status == EpicPaused):
			e.Status = EpicPlanned
		}
		if past {
			e.BudgetExhausted = false
		}
		e.Spend = spend(epicSpend, e.ID)
		out.Epics = append(out.Epics, e)
	}

	out.Sessions = nil
	for _, s := range in.Sessions {
		if !before(s.Start) {
			continue
		}
		if after(s.End) {
			s.End = nil
		}
		out.Sessions = append(out.Sessions, s)
	}
	out.StepRuns = nil
	for _, r := range in.StepRuns {
		if before(r.At) {
			out.StepRuns = append(out.StepRuns, r)
		}
	}
	out.MergeConflicts = nil
	for _, m := range in.MergeConflicts {
		if before(m.At) {
			out.MergeConflicts = append(out.MergeConflicts, m)
		}
	}
	out.Escalations = nil
	for _, e := range in.Escalations {
		if !before(e.OpenedAt) {
			continue
		}
		if after(e.ResolvedAt) {
			e.ResolvedAt, e.Status, e.ClaimedBy = nil, "open", ""
		}
		out.Escalations = append(out.Escalations, e)
	}
	// Ожидание merge, подтверждённое после среза, из снимка уже пропало:
	// для прошедшего периода список неизвестен целиком.
	out.MergeWaits = nil
	if !past {
		for _, w := range in.MergeWaits {
			if before(w.Since) {
				out.MergeWaits = append(out.MergeWaits, w)
			}
		}
	}

	// Выкатка, закончившаяся после среза, на срез ещё шла. Без Live так же
	// и незакрытая: её статус и откат текущие и изменятся, когда она
	// закончится, а отчёт за прошедший период меняться не должен.
	atCut := func(d Deployment) Deployment {
		if after(d.EndedAt) || (past && d.EndedAt == nil) {
			d.EndedAt, d.Status, d.Rollback = nil, deployInProgress, false
		}
		return d
	}
	out.Deployments = nil
	for _, d := range in.Deployments {
		if before(d.CreatedAt) {
			out.Deployments = append(out.Deployments, atCut(d))
		}
	}
	out.Environments = nil
	for _, env := range in.Environments {
		var last *Deployment
		consider := func(d Deployment) {
			if d.EnvID != env.ID || !before(d.CreatedAt) {
				return
			}
			if last == nil || d.CreatedAt.After(last.CreatedAt) || (d.CreatedAt.Equal(last.CreatedAt) && d.ID > last.ID) {
				v := atCut(d)
				last = &v
			}
		}
		if env.Last != nil {
			consider(*env.Last)
		}
		for _, d := range out.Deployments {
			consider(d)
		}
		env.Last = last
		out.Environments = append(out.Environments, env)
	}
	return out, cut
}
