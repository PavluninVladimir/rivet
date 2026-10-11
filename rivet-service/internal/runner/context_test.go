package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Обратный канал целиком: контекст, пришедший во время запуска, доезжает
// до агента через stderr хука на PostToolUse; событие Stop контекст не
// потребляет (спека agent-integration «Обратный канал контекста»).
func TestClaudeAdapterDeliversContext(t *testing.T) {
	// Скрипт ждёт файл-флаг, затем шлёт Stop (контекст не выдаётся) и
	// PostToolUse: полученный stderr печатается текстом ассистента, как
	// это делает fake-claude.sh на стенде.
	cfg, ws := fakeClaude(t, `
hook() {
  ctx=$(printf '%s' "$1" | sh -c "$RIVET_HOOK_CMD" 2>&1 >/dev/null || true)
  [ -z "$ctx" ] || printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$ctx"
}
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"claude-test-1"}'
while [ ! -f "$PWD/go.flag" ]; do sleep 0.05; done
hook '{"hook_event_name":"Stop"}'
hook '{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}'
echo '{"type":"result","subtype":"success","result":"готово"}'
`)
	hub := newContextHub()
	c := &stepCollector{}
	sink := c.sink()
	sink.session, sink.contexts = "s-1", hub

	done := make(chan error, 1)
	go func() {
		_, err := (&claudeAdapter{cfg: cfg}).Run(t.Context(), ws, "промпт", sink)
		done <- err
	}()

	// Очередь заводится внутри Run — ждём её появления и кладём контекст.
	deadline := time.Now().Add(10 * time.Second)
	for !hub.push("s-1", "Предупреждение Rivet: общие файлы с task-7") {
		if time.Now().After(deadline) {
			t.Fatal("очередь контекста не появилась")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.flag"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("запуск: %v", err)
	}

	out := c.buf.String()
	if !strings.Contains(out, "Предупреждение Rivet: общие файлы с task-7") {
		t.Fatalf("контекст не доехал до агента: %q", out)
	}
	// Stop не потребил контекст: предупреждение пришло ровно один раз.
	if n := strings.Count(out, "Предупреждение Rivet"); n != 1 {
		t.Fatalf("контекст должен доставляться один раз, доставлен %d", n)
	}
	// Очередь снята после запуска.
	if hub.push("s-1", "поздний контекст") {
		t.Fatal("очередь должна закрываться вместе с запуском")
	}
}
