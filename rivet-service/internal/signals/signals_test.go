package signals

import (
	"testing"
	"time"
)

func TestPredicates(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	th := Default()
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"молчит ровно 30 мин — граница включается", th.TaskSilent(now.Add(-30*time.Minute), now), true},
		{"молчит 29 мин", th.TaskSilent(now.Add(-29*time.Minute), now), false},
		{"нет событий вовсе — не молчит", th.TaskSilent(time.Time{}, now), false},
		{"ждёт ровно 15 мин — граница не включается", th.LongWaiting(now.Add(-15*time.Minute), now), false},
		{"ждёт 15 мин 1 с", th.LongWaiting(now.Add(-15*time.Minute-time.Second), now), true},
		{"Epic без событий 60 мин", th.EpicStalled(now.Add(-time.Hour), now), true},
		{"Epic без событий 59 мин", th.EpicStalled(now.Add(-59*time.Minute), now), false},
		{"3 из 3 — последняя", th.LastAttempt(3, 3), true},
		{"2 из 3 — последняя (осталась одна)", th.LastAttempt(2, 3), true},
		{"1 из 3", th.LastAttempt(1, 3), false},
		{"лимит 0 — без лимита", th.LastAttempt(5, 0), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %v, ожидалось %v", c.name, c.got, c.want)
		}
	}
}

func TestZeroThresholdsMeanDefault(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var zero Thresholds
	if !zero.TaskSilent(now.Add(-SilentTask), now) || zero.TaskSilent(now.Add(-SilentTask+time.Second), now) {
		t.Fatal("нулевые пороги должны означать пороги системы")
	}
	if !zero.LongWaiting(now.Add(-LongWait-time.Second), now) || !zero.EpicStalled(now.Add(-StalledEpic), now) {
		t.Fatal("нулевые пороги ожидания и застоя Epic'а — пороги системы")
	}
	// Нулевой запас попыток — тоже порог системы: 2 из 3 уже последняя.
	if zero.LastAttempt(2, 3) != Default().LastAttempt(2, 3) || !zero.LastAttempt(2, 3) {
		t.Fatal("нулевой LastAttemptLeft должен означать порог системы")
	}
}

func TestFromEnvScale(t *testing.T) {
	t.Setenv(ScaleEnv, "0.01")
	th := FromEnv()
	if th.SilentTask != 18*time.Second || th.LongWait != 9*time.Second || th.StalledEpic != 36*time.Second {
		t.Fatalf("масштаб: %+v", th)
	}
	if th.LastAttemptLeft != LastAttemptLeft {
		t.Fatal("лимит попыток не масштабируется")
	}
	for _, bad := range []string{"", "x", "0", "-1"} {
		t.Setenv(ScaleEnv, bad)
		if FromEnv() != Default() {
			t.Fatalf("значение %q должно давать пороги системы", bad)
		}
	}
}
