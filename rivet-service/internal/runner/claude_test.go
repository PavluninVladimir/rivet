package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Интеграционные тесты нативного адаптера Claude Code: вместо claude —
// shell-скрипт, печатающий stream-json и шлющий события хуков через
// $RIVET_HOOK_CMD (тот же путь, что fake-claude.sh на e2e-стенде).

// TestMain перехватывает «hook»: адаптер строит команду хука из
// os.Executable(), в тестах это тестовый бинарник.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(HookMain())
	}
	os.Exit(m.Run())
}

type stepCollector struct {
	mu    sync.Mutex
	steps []*pb.AgentEvent
	buf   strings.Builder
}

func (c *stepCollector) sink() runSink {
	return runSink{
		transcript: func(b []byte) { c.mu.Lock(); c.buf.Write(b); c.mu.Unlock() },
		step:       func(ev *pb.AgentEvent) { c.mu.Lock(); c.steps = append(c.steps, ev); c.mu.Unlock() },
	}
}

func fakeClaude(t *testing.T, script string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{Workdir: dir, Adapter: AdapterClaudeCode, ClaudeBin: bin}, ws
}

func TestClaudeAdapterFullDepth(t *testing.T) {
	cfg, ws := fakeClaude(t, `
hook() { printf '%s' "$1" | sh -c "$RIVET_HOOK_CMD"; }
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"claude-test-1"}'
echo '{"type":"assistant","message":{"model":"claude-test-1","usage":{"input_tokens":1000,"cache_read_input_tokens":50000,"output_tokens":100},"content":[{"type":"text","text":"Правлю файл."},{"type":"tool_use","name":"Edit","input":{"file_path":"'"$PWD"'/src/main.go"}}]}}'
hook "{\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$PWD/src/main.go\"}}"
hook '{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_error":"exit 1: FAIL"}'
hook '{"hook_event_name":"Stop"}'
echo 'предупреждение CLI не в JSON'
echo '{"type":"result","subtype":"success","result":"Готово: файл поправлен.","total_cost_usd":0.05,"usage":{"input_tokens":2000,"cache_creation_input_tokens":300,"cache_read_input_tokens":100000,"output_tokens":250},"modelUsage":{"claude-test-1":{"contextWindow":200000}}}'
`)
	col := &stepCollector{}
	a := &claudeAdapter{cfg: cfg}
	ctx := context.Background()
	run, err := a.Run(ctx, ws, "сделай задачу", col.sink())
	if err != nil {
		t.Fatal(err)
	}
	if run.FinalText != "Готово: файл поправлен." || run.Model != "claude-test-1" {
		t.Fatalf("run: %+v", run)
	}
	// Usage из result: вход — всё прочитанное моделью, ctx — последний обмен.
	if run.Usage.TokensIn == nil || *run.Usage.TokensIn != 102300 ||
		run.Usage.TokensOut == nil || *run.Usage.TokensOut != 250 ||
		run.Usage.CostUSD == nil || *run.Usage.CostUSD != 0.05 {
		t.Fatalf("usage: in=%v out=%v cost=%v", run.Usage.TokensIn, run.Usage.TokensOut, run.Usage.CostUSD)
	}
	if run.Usage.CtxPct == nil || *run.Usage.CtxPct != 25 { // (1000+50000)/200000
		t.Fatalf("ctx_pct: %v", run.Usage.CtxPct)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	// Скрипт шлёт хуки синхронно, но доставка через сокет асинхронна к
	// завершению процесса — подождём недостающие шаги без гонки.
	if len(col.steps) != 3 {
		t.Fatalf("шагов %d, ожидали 3: %+v", len(col.steps), col.steps)
	}
	edit, bash, stop := col.steps[0], col.steps[1], col.steps[2]
	// Путь приведён к относительному от рабочей копии... скрипт работает в ws.
	if edit.Kind != "tool" || edit.Tool != "Edit" || !edit.Ok ||
		len(edit.Files) != 1 || edit.Files[0] != "src/main.go" || edit.Text != "Edit src/main.go" {
		t.Fatalf("edit step: %+v", edit)
	}
	if bash.Kind != "tool" || bash.Tool != "Bash" || bash.Ok || len(bash.Files) != 0 ||
		!strings.Contains(bash.Detail, "exit 1: FAIL") || !strings.Contains(bash.Text, "ошибка") {
		t.Fatalf("bash step: %+v", bash)
	}
	if stop.Kind != "stop" {
		t.Fatalf("stop step: %+v", stop)
	}
	tr := col.buf.String()
	for _, want := range []string{"Claude Code: сессия s-1", "Правлю файл.", "→ Edit src/main.go",
		"предупреждение CLI не в JSON", "Готово: файл поправлен."} {
		if !strings.Contains(tr, want) {
			t.Fatalf("в транскрипте нет %q:\n%s", want, tr)
		}
	}
}

// Обрыв без result: маркеры ищутся в последнем тексте ассистента, usage
// остаётся пустым (данных нет ≠ ноль).
func TestClaudeAdapterNoResult(t *testing.T) {
	cfg, ws := fakeClaude(t, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"BLOCKED: не хватает требований"}]}}'
`)
	col := &stepCollector{}
	a := &claudeAdapter{cfg: cfg}
	run, err := a.Run(context.Background(), ws, "сделай", col.sink())
	if err != nil {
		t.Fatal(err)
	}
	if q, blocked := parseBlocked(run.FinalText); !blocked || q != "не хватает требований" {
		t.Fatalf("blocked из текста ассистента: %q", run.FinalText)
	}
	if run.Usage.TokensIn != nil || run.Usage.CostUSD != nil || run.Usage.CtxPct != nil {
		t.Fatalf("usage без result должен быть пустым: %+v", run.Usage)
	}
}

// Ошибка запуска агента (ненулевой выход без result) — ошибка стадии.
func TestClaudeAdapterExitError(t *testing.T) {
	cfg, ws := fakeClaude(t, `
echo 'падение до первого сообщения'
exit 3
`)
	col := &stepCollector{}
	a := &claudeAdapter{cfg: cfg}
	if _, err := a.Run(context.Background(), ws, "сделай", col.sink()); err == nil {
		t.Fatal("ожидали ошибку запуска")
	}
}

// Хук без связи с runner'ом завершается успешно (спека agent-integration).
func TestHookMainUnreachable(t *testing.T) {
	t.Setenv("RIVET_HOOK_SOCK", filepath.Join(t.TempDir(), "нет.sock"))
	r, w, _ := os.Pipe()
	_, _ = w.WriteString(`{"hook_event_name":"Stop"}`)
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if code := HookMain(); code != 0 {
		t.Fatalf("hook должен завершаться нулём, got %d", code)
	}
}
