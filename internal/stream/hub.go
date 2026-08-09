package stream

import "sync"

// LogChunk — живой кусок терминального вывода (SSE session.log).
// По контракту не реплеится: полный транскрипт — по transcript_ref сессии.
type LogChunk struct {
	ProjectID string
	TaskID    string
	Data      []byte
}

// Hub раздаёт live-чанки подписчикам проекта.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan LogChunk]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan LogChunk]struct{}{}}
}

func (h *Hub) Subscribe(projectID string) (chan LogChunk, func()) {
	ch := make(chan LogChunk, 128)
	h.mu.Lock()
	if h.subs[projectID] == nil {
		h.subs[projectID] = map[chan LogChunk]struct{}{}
	}
	h.subs[projectID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[projectID], ch)
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(c LogChunk) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[c.ProjectID] {
		select {
		case ch <- c:
		default: // отстающий подписчик теряет live-чанк, не блокируя runner
		}
	}
}
