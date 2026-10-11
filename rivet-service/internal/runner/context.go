package runner

import (
	"log/slog"
	"sync"
)

// Обратный канал контекста (change add-context-channel): control plane шлёт
// работающему агенту контекст стадии (пока — предупреждения о пересечении
// работ). Runner копит его в очереди запуска, а адаптер с обратным каналом
// отдаёт агенту. Очередь живёт ровно на время запуска: контекст завершённой
// стадии никому не нужен.

const (
	// maxContextMessages — глубина очереди: агенту нужно последнее
	// состояние дел, а не вся история предупреждений.
	maxContextMessages = 10
	// maxContextRunes — предел одного сообщения: текст приходит от plane,
	// но упирается в лимиты промпта агента.
	maxContextRunes = 2000
)

// contextHub — очереди контекста по сессиям стадий runner'а.
type contextHub struct {
	mu     sync.Mutex
	queues map[string]*contextQueue
}

func newContextHub() *contextHub {
	return &contextHub{queues: map[string]*contextQueue{}}
}

// open заводит очередь сессии на время запуска агента.
func (h *contextHub) open(session string) *contextQueue {
	if h == nil || session == "" {
		return nil
	}
	q := &contextQueue{}
	h.mu.Lock()
	h.queues[session] = q
	h.mu.Unlock()
	return q
}

// close снимает очередь сессии: контекст, пришедший после запуска, теряется
// осознанно (стадия уже закончилась).
func (h *contextHub) close(session string) {
	if h == nil || session == "" {
		return
	}
	h.mu.Lock()
	delete(h.queues, session)
	h.mu.Unlock()
}

// push кладёт контекст в очередь сессии; false — очереди нет (стадия
// не выполняется или адаптер без обратного канала).
func (h *contextHub) push(session, text string) bool {
	if h == nil || session == "" || text == "" {
		return false
	}
	h.mu.Lock()
	q := h.queues[session]
	h.mu.Unlock()
	if q == nil {
		return false
	}
	q.push(text)
	return true
}

// contextQueue — накопленный контекст одного запуска в порядке поступления.
type contextQueue struct {
	mu    sync.Mutex
	items []string
}

func (q *contextQueue) push(text string) {
	if q == nil {
		return
	}
	text = clipRunes(text, maxContextRunes)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, text)
	if len(q.items) > maxContextMessages {
		dropped := len(q.items) - maxContextMessages
		q.items = q.items[dropped:]
		slog.Warn("очередь контекста переполнена — старые сообщения отброшены", "dropped", dropped)
	}
}

// restore возвращает в начало очереди контекст, который не удалось отдать
// агенту (обрыв соединения с хуком): он уйдёт на следующем вызове
// инструмента, а не пропадёт. При переполнении действует то же правило,
// что и при push: остаётся самое свежее.
func (q *contextQueue) restore(items []string) {
	if q == nil || len(items) == 0 {
		return
	}
	q.mu.Lock()
	q.items = append(items, q.items...)
	if len(q.items) > maxContextMessages {
		q.items = q.items[len(q.items)-maxContextMessages:]
	}
	q.mu.Unlock()
}

// take забирает накопленный контекст и очищает очередь: одно и то же
// предупреждение не доставляется агенту дважды.
func (q *contextQueue) take() []string {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}
