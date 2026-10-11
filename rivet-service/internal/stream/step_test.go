package stream

import (
	"fmt"
	"strings"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// stepPayload: маскирование, лимиты, пустой kind (спека agent-integration
// «Шаги сессии и затронутые файлы», api-contract session.step).
func TestStepPayload(t *testing.T) {
	// Простой текстовый шаг обёртки: payload несёт только session_id.
	p := stepPayload(&pb.AgentEvent{SessionId: "s1", Text: "проверка: tests"})
	if len(p) != 1 || p["session_id"] != "s1" {
		t.Fatalf("payload обёртки: %+v", p)
	}
	// Секрет в detail маскируется, ok/kind/tool/files доезжают.
	files := make([]string, 60)
	for i := range files {
		files[i] = fmt.Sprintf("dir/f%02d.go", i)
	}
	p = stepPayload(&pb.AgentEvent{SessionId: "s1", Kind: "tool", Tool: "Bash", Ok: false,
		Detail: "export GH_TOKEN=ghp_0123456789abcdef0123456789abcdef1234", Files: files})
	if p["kind"] != "tool" || p["tool"] != "Bash" || p["ok"] != false {
		t.Fatalf("поля шага: %+v", p)
	}
	if d := p["detail"].(string); strings.Contains(d, "ghp_0123456789") {
		t.Fatalf("секрет не замаскирован: %q", d)
	}
	if got := p["files"].([]string); len(got) != 50 || got[0] != "dir/f00.go" {
		t.Fatalf("лимит files: %d", len(got))
	}
	// Длинный detail обрезается по границе руны.
	long := strings.Repeat("я", 600)
	p = stepPayload(&pb.AgentEvent{Kind: "tool", Tool: "Bash", Detail: long})
	if d := p["detail"].(string); len([]rune(strings.TrimSuffix(d, "…"))) > 500 {
		t.Fatalf("detail не обрезан: %d рун", len([]rune(d)))
	}
}
