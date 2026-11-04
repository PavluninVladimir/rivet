// Package signals — общий детектор «завис» и «ждёт» для экранов, которые
// судят о застрявшей работе: обзор проекта (add-project-lead-view) и карта
// работы (add-work-board). Один набор порогов и предикатов, чтобы экраны не
// расходились в том, что считать застрявшим.
//
// Пакет чистый: ни базы, ни времени «сейчас» — всё приходит аргументами.
package signals

import (
	"os"
	"strconv"
	"time"
)

// Пороги системы (спеки project-overview «Статус риска Epic» и work-board
// «Общий детектор завис»).
const (
	// SilentTask — задача в работе без событий так долго считается застрявшей.
	SilentTask = 30 * time.Minute
	// LongWait — задача ждёт runner'а или агента дольше этого.
	LongWait = 15 * time.Minute
	// StalledEpic — у работающего Epic'а нет событий по задачам так долго.
	StalledEpic = 60 * time.Minute
	// LastAttemptLeft — сколько попыток до лимита считается «последней».
	LastAttemptLeft = 1
)

// Thresholds — действующие пороги. Нулевые значения означают пороги
// системы (Default).
type Thresholds struct {
	SilentTask      time.Duration
	LongWait        time.Duration
	StalledEpic     time.Duration
	LastAttemptLeft int
}

// Default — пороги системы.
func Default() Thresholds {
	return Thresholds{SilentTask: SilentTask, LongWait: LongWait, StalledEpic: StalledEpic, LastAttemptLeft: LastAttemptLeft}
}

// ScaleEnv — переменная окружения с множителем порогов. Её выставляет только
// тестовый стенд e2e, чтобы проверять «завис» за секунды, а не за минуты.
const ScaleEnv = "RIVET_SIGNALS_SCALE"

// FromEnv — пороги системы с множителем из ScaleEnv. Пустое, неверное или
// неположительное значение означает множитель 1.
func FromEnv() Thresholds {
	t := Default()
	raw := os.Getenv(ScaleEnv)
	if raw == "" {
		return t
	}
	k, err := strconv.ParseFloat(raw, 64)
	if err != nil || k <= 0 {
		return t
	}
	return t.Scaled(k)
}

// Scaled — пороги времени, умноженные на k (лимит попыток не меняется).
func (t Thresholds) Scaled(k float64) Thresholds {
	scale := func(d time.Duration) time.Duration { return time.Duration(float64(d) * k) }
	t.SilentTask, t.LongWait, t.StalledEpic = scale(t.SilentTask), scale(t.LongWait), scale(t.StalledEpic)
	return t
}

func (t Thresholds) orDefault() Thresholds {
	d := Default()
	if t.SilentTask <= 0 {
		t.SilentTask = d.SilentTask
	}
	if t.LongWait <= 0 {
		t.LongWait = d.LongWait
	}
	if t.StalledEpic <= 0 {
		t.StalledEpic = d.StalledEpic
	}
	if t.LastAttemptLeft <= 0 {
		t.LastAttemptLeft = d.LastAttemptLeft
	}
	return t
}

// Silent — от последнего события прошло не меньше after: задача в работе
// молчит. Граница включается (ровно after уже «молчит»).
func Silent(last, now time.Time, after time.Duration) bool {
	return !last.IsZero() && now.Sub(last) >= after
}

// Waiting — ожидание длится строго дольше after.
func Waiting(since, now time.Time, after time.Duration) bool {
	return !since.IsZero() && now.Sub(since) > after
}

// AtLastAttempt — использовано столько попыток, что осталось не больше
// left. Лимит 0 и меньше означает «без лимита».
func AtLastAttempt(used, limit, left int) bool {
	return limit > 0 && used >= limit-left
}

// TaskSilent — задача в работе без событий дольше SilentTask.
func (t Thresholds) TaskSilent(last, now time.Time) bool {
	return Silent(last, now, t.orDefault().SilentTask)
}

// LongWaiting — задача ждёт runner'а или агента дольше LongWait.
func (t Thresholds) LongWaiting(since, now time.Time) bool {
	return Waiting(since, now, t.orDefault().LongWait)
}

// EpicStalled — у работающего Epic'а нет событий дольше StalledEpic.
func (t Thresholds) EpicStalled(last, now time.Time) bool {
	return Silent(last, now, t.orDefault().StalledEpic)
}

// LastAttempt — задача на последней попытке по LastAttemptLeft.
func (t Thresholds) LastAttempt(used, limit int) bool {
	return AtLastAttempt(used, limit, t.orDefault().LastAttemptLeft)
}
