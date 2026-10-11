package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Внешний адаптер (change add-adapter-sdk, спека agent-integration
// «Открытый SDK адаптеров»): адаптер — отдельная программа, общение
// построчным JSON.

// fakeAdapter — скрипт-адаптер в рабочем каталоге теста.
func fakeAdapter(t *testing.T, script string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "adapter.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{Workdir: dir, Adapter: AdapterExternal, AdapterCmd: bin}, ws
}

// Сквозная стадия: шаги, транскрипт, расход и итог доезжают до runner'а.
func TestExternalAdapterRun(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
# задание стадии приходит первой строкой stdin
head -n 1 > /dev/null
printf '%s\n' '{"type":"transcript","text":"старт"}'
printf '%s\n' '{"type":"step","kind":"tool","tool":"Edit","detail":"src/main.go","files":["src/main.go"],"ok":true}'
printf '%s\n' 'не json — должен игнорироваться'
printf '%s\n' '{"type":"неизвестное","text":"тоже игнорируется"}'
printf '%s\n' '{"type":"usage","tokens_in":1200,"tokens_out":300,"cost_usd":0.02,"model":"agent-1","ctx_pct":40}'
printf '%s\n' '{"type":"result","text":"готово","error":false}'
`)
	c := &stepCollector{}
	run, err := (&externalAdapter{cfg: cfg}).Run(t.Context(), ws, "промпт стадии", c.sink())
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if run.FinalText != "готово" || run.isError {
		t.Fatalf("итог: %+v", run)
	}
	if run.Model != "agent-1" || run.Usage.TokensIn == nil || *run.Usage.TokensIn != 1200 {
		t.Fatalf("расход: %+v %+v", run.Model, run.Usage)
	}
	if run.Usage.CtxPct == nil || *run.Usage.CtxPct != 40 {
		t.Fatalf("заполненность контекста: %+v", run.Usage)
	}
	if !strings.Contains(c.buf.String(), "старт") {
		t.Fatalf("транскрипт: %q", c.buf.String())
	}
	c.mu.Lock()
	steps := append([]*pb.AgentEvent(nil), c.steps...)
	c.mu.Unlock()
	if len(steps) != 1 || steps[0].Tool != "Edit" || len(steps[0].Files) != 1 {
		t.Fatalf("шаги: %+v", steps)
	}
	if steps[0].Text != "Edit src/main.go" {
		t.Fatalf("текст шага собирает runner, когда адаптер его не дал: %q", steps[0].Text)
	}
}

// Итога нет (адаптер оборвался): маркеры ищутся в накопленном тексте.
func TestExternalAdapterNoResult(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
printf '%s\n' '{"type":"transcript","text":"BLOCKED: нужен ответ человека"}'
`)
	c := &stepCollector{}
	run, err := (&externalAdapter{cfg: cfg}).Run(t.Context(), ws, "промпт", c.sink())
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if q, blocked := parseBlocked(run.FinalText); !blocked || q == "" {
		t.Fatalf("маркер из текста: %q", run.FinalText)
	}
}

// Ошибка запуска агента: адаптер сообщает её итогом, стадия падает.
func TestExternalAdapterError(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
printf '%s\n' '{"type":"result","text":"агент не найден","error":true}'
`)
	c := &stepCollector{}
	if _, err := (&externalAdapter{cfg: cfg}).Run(t.Context(), ws, "промпт", c.sink()); err == nil {
		t.Fatal("ошибка адаптера должна валить стадию")
	}
}

// Контекст от Rivet доезжает до адаптера строками stdin.
func TestExternalAdapterContext(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
# ждём строку контекста и подтверждаем её транскриптом
line=$(head -n 1)
case "$line" in
*пересечении*) printf '%s\n' '{"type":"transcript","text":"контекст получен"}' ;;
*) printf '%s\n' '{"type":"transcript","text":"контекста нет"}' ;;
esac
printf '%s\n' '{"type":"result","text":"готово","error":false}'
`)
	cfg.AdapterContext = true
	hub := newContextHub()
	c := &stepCollector{}
	sink := c.sink()
	sink.session, sink.contexts = "s-1", hub

	done := make(chan error, 1)
	go func() {
		_, err := (&externalAdapter{cfg: cfg}).Run(context.Background(), ws, "промпт", sink)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !hub.push("s-1", "предупреждение о пересечении") {
		if time.Now().After(deadline) {
			t.Fatal("очередь контекста не появилась")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if !strings.Contains(c.buf.String(), "контекст получен") {
		t.Fatalf("контекст не доехал до адаптера: %q", c.buf.String())
	}
}

// Глубина и обратный канал объявляются запуском runner'а, а не адаптером.
func TestExternalAdapterDeclaration(t *testing.T) {
	cfg := Config{Adapter: AdapterExternal, AdapterCmd: "./a", AdapterDepth: "full", AdapterContext: true}
	if cfg.depth() != "full" || !cfg.contextChannel() {
		t.Fatalf("объявление: %s %v", cfg.depth(), cfg.contextChannel())
	}
	// Неизвестная глубина занижается: обещать шаги, которых нет, нельзя.
	cfg.AdapterDepth = "супер"
	if cfg.depth() != "minimal" {
		t.Fatalf("неизвестная глубина: %s", cfg.depth())
	}
	// Встроенные адаптеры на флаги внешнего не смотрят.
	wrap := Config{Adapter: AdapterWrap, AdapterDepth: "full", AdapterContext: true}
	if wrap.depth() != "minimal" || wrap.contextChannel() {
		t.Fatalf("обёртка: %s %v", wrap.depth(), wrap.contextChannel())
	}
}

// Адаптер, который читает stdin до EOF, не подвешивает стадию: runner
// закрывает вход сразу после итога (находка ревью).
func TestExternalAdapterClosesStdinAfterResult(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
printf '%s\n' '{"type":"result","text":"готово","error":false}'
# дальше ждём EOF на stdin: без закрытия входа это дедлок
cat > /dev/null
`)
	cfg.AdapterContext = true
	hub := newContextHub()
	c := &stepCollector{}
	sink := c.sink()
	sink.session, sink.contexts = "s-eof", hub

	done := make(chan error, 1)
	go func() {
		_, err := (&externalAdapter{cfg: cfg}).Run(context.Background(), ws, "промпт", sink)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("запуск: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("стадия не завершилась: вход адаптера не закрыт после итога")
	}
}

// Адаптер отдал итог, но не закрыл вывод (его унаследовал фоновый
// процесс): стадия всё равно завершается по отсрочке (находка ревью).
func TestExternalAdapterTailGrace(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
printf '%s\n' '{"type":"result","text":"готово","error":false}'
# фоновый процесс держит stdout открытым дольше отсрочки
sleep 30 &
exit 0
`)
	c := &stepCollector{}
	start := time.Now()
	run, err := (&externalAdapter{cfg: cfg}).Run(context.Background(), ws, "промпт", c.sink())
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if run.FinalText != "готово" {
		t.Fatalf("итог: %q", run.FinalText)
	}
	if elapsed := time.Since(start); elapsed > adapterTailGrace+10*time.Second {
		t.Fatalf("стадия ждала незакрытый вывод слишком долго: %s", elapsed)
	}
}

// Отмена стадии убивает группу процессов адаптера: чтение вывода не
// подвисает на внуке, пережившем оболочку.
func TestExternalAdapterCancel(t *testing.T) {
	cfg, ws := fakeAdapter(t, `
head -n 1 > /dev/null
printf '%s\n' '{"type":"transcript","text":"работаю"}'
sleep 60 &
sleep 60
`)
	c := &stepCollector{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = (&externalAdapter{cfg: cfg}).Run(ctx, ws, "промпт", c.sink())
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("отмена стадии не завершила запуск адаптера")
	}
}
