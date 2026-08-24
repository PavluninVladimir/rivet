//go:build realclaude

package runner

import (
	"context"
	"os/exec"
	"strings"
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
