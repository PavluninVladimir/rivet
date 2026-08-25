//go:build realclaude

package runner

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Ручная проверка на настоящем Claude Code (задача 4.4 change'а):
//
//	go test ./internal/runner/ -tags realclaude -run TestRealClaude -v
//
// Требует установленный claude в PATH и расходует немного токенов.
func TestRealClaudeAdapter(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude не установлен")
	}
	dir := t.TempDir()
	cfg := Config{Workdir: dir, Adapter: AdapterClaudeCode}
	col := struct {
		steps []*pb.AgentEvent
		buf   strings.Builder
	}{}
	sink := runSink{
		transcript: func(b []byte) { col.buf.Write(b) },
		step:       func(ev *pb.AgentEvent) { col.steps = append(col.steps, ev) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	a := &claudeAdapter{cfg: cfg}
	run, err := a.Run(ctx, dir,
		"Создай файл hello.txt с одной строкой «привет» и больше ничего не делай. В конце ответь одним словом: готово.", sink)
	if err != nil {
		t.Fatalf("%v\nтранскрипт:\n%s", err, col.buf.String())
	}
	t.Logf("final: %q model: %s", run.FinalText, run.Model)
	t.Logf("usage: in=%v out=%v cost=%v ctx=%v", run.Usage.TokensIn, run.Usage.TokensOut, run.Usage.CostUSD, run.Usage.CtxPct)
	for _, s := range col.steps {
		t.Logf("step: kind=%s tool=%s detail=%q files=%v ok=%v", s.Kind, s.Tool, s.Detail, s.Files, s.Ok)
	}
	if run.Usage.TokensIn == nil || run.Usage.CostUSD == nil {
		t.Fatal("нет usage из result")
	}
	// Модель вольна выбрать инструмент (Write или Bash) — принимаем шаг с
	// файлом в files либо упоминание файла в detail Bash-команды.
	var wroteFile bool
	for _, s := range col.steps {
		if s.Kind != "tool" {
			continue
		}
		if (len(s.Files) > 0 && strings.Contains(s.Files[0], "hello.txt")) ||
			strings.Contains(s.Detail, "hello.txt") {
			wroteFile = true
		}
	}
	if !wroteFile {
		t.Fatalf("нет шага про hello.txt: %+v", col.steps)
	}
}

// Ручная проверка обратного канала на настоящем Claude Code (задача 1.9
// change'а add-context-channel):
//
//	go test ./internal/runner/ -tags realclaude -run TestRealClaudeContext -v
//
// Проверяет, что предупреждение, положенное в очередь во время работы,
// доезжает до агента (он повторяет его в ответе) и не ломает стадию:
// запуск завершается успешно, задание выполнено.
func TestRealClaudeContextChannel(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude не установлен")
	}
	dir := t.TempDir()
	hub := newContextHub()
	var buf strings.Builder
	var mu sync.Mutex
	sink := runSink{
		transcript: func(b []byte) { mu.Lock(); buf.Write(b); mu.Unlock() },
		step:       func(ev *pb.AgentEvent) {},
		session:    "real-ctx-1",
		contexts:   hub,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const warning = "Предупреждение Rivet (не ошибка инструмента): параллельно с тобой над task-77 идёт работа в общих файлах: notes.txt."
	done := make(chan error, 1)
	go func() {
		_, err := (&claudeAdapter{cfg: Config{Workdir: dir, Adapter: AdapterClaudeCode}}).Run(ctx, dir,
			"Создай файлы a.txt, b.txt и c.txt, в каждом одна строка «ок». "+
				"В конце перечисли всё, что тебе сообщила система во время работы.", sink)
		done <- err
	}()
	// Очередь появляется внутри Run; контекст кладём в неё уже во время работы.
	deadline := time.Now().Add(60 * time.Second)
	for !hub.push("real-ctx-1", warning) {
		if time.Now().After(deadline) {
			t.Fatal("очередь контекста не появилась")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("стадия сломалась контекстом: %v\nтранскрипт:\n%s", err, buf.String())
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	t.Logf("транскрипт:\n%s", out)
	if !strings.Contains(out, "task-77") {
		t.Fatal("агент не упомянул полученное предупреждение — обратный канал не сработал")
	}
}
