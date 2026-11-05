package native

import (
	"sync"
	"time"
)

// Отслеживание потомков команды там, где нет PID namespace (macOS, Linux
// без user namespaces): фоновый опрос таблицы процессов собирает всех
// потомков лидера, после завершения команды выжившие добиваются. Процесс
// опознаётся по паре (pid, время старта), поэтому переиспользованный pid
// чужого процесса не будет убит. Потомок, который успел уйти к init между
// опросами, не ловится: полную гарантию даёт только песочница Linux.

type procInfo struct {
	pid, ppid int
	start     uint64
}

type procKey struct {
	pid   int
	start uint64
}

type tracker struct {
	mu      sync.Mutex
	leader  int
	tracked map[procKey]bool
	stop    chan struct{}
	done    chan struct{}
}

// trackDescendants начинает опрос потомков pid с интервалом every.
// Без поддержки снимков процессов (snapshotProcs == nil) ничего не делает.
func trackDescendants(pid int, every time.Duration) *tracker {
	t := &tracker{leader: pid, tracked: map[procKey]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	if snapshotProcs == nil {
		close(t.done)
		return t
	}
	go func() {
		defer close(t.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			t.poll()
			select {
			case <-t.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return t
}

// poll добавляет к отслеживаемым всех потомков лидера и уже известных.
func (t *tracker) poll() {
	procs := snapshotProcs()
	t.mu.Lock()
	defer t.mu.Unlock()
	parents := map[int]bool{t.leader: true}
	for k := range t.tracked {
		parents[k.pid] = true
	}
	for changed := true; changed; {
		changed = false
		for _, p := range procs {
			if parents[p.ppid] && !parents[p.pid] {
				parents[p.pid] = true
				t.tracked[procKey{p.pid, p.start}] = true
				changed = true
			}
		}
	}
}

// finish останавливает опрос, делает последний снимок и возвращает живых
// отслеживаемых потомков (кроме лидера).
func (t *tracker) finish() []procKey {
	if snapshotProcs == nil {
		return nil
	}
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	<-t.done
	t.poll()
	alive := map[procKey]bool{}
	for _, p := range snapshotProcs() {
		alive[procKey{p.pid, p.start}] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []procKey
	for k := range t.tracked {
		if k.pid != t.leader && alive[k] {
			out = append(out, k)
		}
	}
	return out
}
