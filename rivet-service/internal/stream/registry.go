// Package stream — живые соединения: реестр runner'ов (gRPC) и hub
// live-потоков для SSE-клиентов.
package stream

import (
	"sync"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Registry — активные каналы отправки к подключённым runner'ам.
type Registry struct {
	mu    sync.RWMutex
	conns map[string]chan *pb.PlaneMsg
}

func NewRegistry() *Registry {
	return &Registry{conns: map[string]chan *pb.PlaneMsg{}}
}

// Attach регистрирует соединение runner'а; прежнее (если было) вытесняется.
func (r *Registry) Attach(runnerID string) chan *pb.PlaneMsg {
	ch := make(chan *pb.PlaneMsg, 64)
	r.mu.Lock()
	if old, ok := r.conns[runnerID]; ok {
		close(old)
	}
	r.conns[runnerID] = ch
	r.mu.Unlock()
	return ch
}

func (r *Registry) Detach(runnerID string, ch chan *pb.PlaneMsg) {
	r.mu.Lock()
	if cur, ok := r.conns[runnerID]; ok && cur == ch {
		delete(r.conns, runnerID)
		close(cur)
	}
	r.mu.Unlock()
}

// Send — неблокирующая доставка; false, если runner не подключён или канал полон.
func (r *Registry) Send(runnerID string, msg *pb.PlaneMsg) bool {
	r.mu.RLock()
	ch, ok := r.conns[runnerID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}
