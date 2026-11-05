package runner

import (
	"strings"
	"testing"
)

// Очередь контекста: порядок сохраняется, лишние сообщения вытесняют
// старые, take очищает очередь (design add-context-channel).
func TestContextQueueLimits(t *testing.T) {
	hub := newContextHub()
	if hub.push("нет-такой-сессии", "текст") {
		t.Fatal("контекст без активной стадии не должен приниматься")
	}
	q := hub.open("s-1")
	for i := 0; i < maxContextMessages+2; i++ {
		hub.push("s-1", string(rune('a'+i)))
	}
	hub.push("s-1", strings.Repeat("я", maxContextRunes+50))
	items := q.take()
	if len(items) != maxContextMessages {
		t.Fatalf("глубина очереди: %d", len(items))
	}
	// Первые сообщения вытеснены, порядок остальных сохранён.
	if items[0] != "d" || items[len(items)-2] != "l" {
		t.Fatalf("порядок очереди: %+v", items)
	}
	if last := []rune(items[len(items)-1]); len(last) != maxContextRunes+1 {
		t.Fatalf("длинное сообщение должно обрезаться: %d рун", len(last))
	}
	if rest := q.take(); len(rest) != 0 {
		t.Fatalf("take должен очищать очередь: %+v", rest)
	}
	hub.close("s-1")
	if hub.push("s-1", "после стадии") {
		t.Fatal("после закрытия очереди контекст не принимается")
	}
}
