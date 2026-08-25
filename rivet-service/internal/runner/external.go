package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/PavluninVladimir/rivet/pkg/adapter"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Внешний адаптер (change add-adapter-sdk, спека agent-integration
// «Открытый SDK адаптеров»): адаптер агента — отдельная программа,
// написанная кем угодно на чём угодно. Runner отдаёт ей задание стадии и
// контекст построчным JSON на stdin и читает события со stdout.
// Неизвестные и битые строки игнорируются: адаптер новее runner'а не
// должен ломать стадию.

// externalAdapter запускает адаптер-процесс.
type externalAdapter struct {
	cfg Config
}

func (a *externalAdapter) Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error) {
	if strings.TrimSpace(a.cfg.AdapterCmd) == "" {
		return agentRun{}, fmt.Errorf("не задана команда адаптера (-adapter-cmd)")
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.cfg.AdapterCmd)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	// Адаптер — чужая программа, которая обычно запускает агента: убивать
	// надо всю группу процессов, иначе внуки переживут отмену стадии и
	// удержат её вывод открытым. WaitDelay страхует тот же случай, когда
	// потомок унаследовал stdout и не закрывает его.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = adapterWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return agentRun{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agentRun{}, err
	}
	// stderr адаптера — в транскрипт: диагностика чужой программы должна
	// быть видна человеку, а не пропадать.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return agentRun{}, err
	}
	if err := cmd.Start(); err != nil {
		return agentRun{}, fmt.Errorf("запуск адаптера: %w", err)
	}

	// Очередь контекста: адаптер с объявленным обратным каналом получает
	// его теми же строками, что и задание.
	var queue *contextQueue
	if a.cfg.AdapterContext {
		queue = sink.contexts.open(sink.session)
		defer sink.contexts.close(sink.session)
	}
	var wg sync.WaitGroup
	feedDone := make(chan struct{})
	var closeFeed sync.Once
	// stopFeed закрывает подачу stdin, а с ней и сам stdin адаптера.
	// Вызывается сразу после итога стадии: адаптер, который читает stdin
	// до EOF, иначе ждал бы его, а runner — конца его stdout.
	stopFeed := func() { closeFeed.Do(func() { close(feedDone) }) }
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = stdin.Close() }()
		enc := json.NewEncoder(stdin)
		if err := enc.Encode(sdk.Prompt{
			Type: sdk.TypePrompt, Text: prompt, Session: sink.session,
		}); err != nil {
			return
		}
		if queue == nil {
			return
		}
		// Пока адаптер работает, докладываем ему приходящий контекст.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-feedDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				for _, text := range queue.take() {
					if err := enc.Encode(sdk.Context{
						Type: sdk.TypeContext, Kind: "overlap", Text: text,
					}); err != nil {
						return
					}
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 32*1024), 1<<20)
		for sc.Scan() {
			if sink.transcript != nil {
				sink.transcript([]byte(sc.Text() + "\n"))
			}
		}
	}()

	run, scanErr := readAdapterEvents(ctx, stdout, sink, stopFeed)
	// Вход закрывается до ожидания процесса при любом исходе чтения:
	// адаптер, который ждёт EOF на stdin, иначе не выйдет, а Wait не
	// вернётся (находка ревью).
	stopFeed()
	werr := cmd.Wait()
	wg.Wait()

	if run.isError {
		return run, fmt.Errorf("адаптер завершил запуск с ошибкой: %s", clipRunes(run.FinalText, 500))
	}
	if run.FinalText == "" {
		if werr != nil {
			return run, fmt.Errorf("адаптер: %w", werr)
		}
		if scanErr != nil {
			return run, fmt.Errorf("чтение вывода адаптера: %w", scanErr)
		}
	}
	return run, werr
}

// adapterWaitDelay — сколько ждём закрытия вывода после выхода процесса
// (потомок мог унаследовать stdout).
const adapterWaitDelay = 5 * time.Second

// adapterTailGrace — сколько ждём хвост вывода после итога стадии: итог
// терминален, а адаптер, который не закрыл stdout, не должен держать
// стадию.
const adapterTailGrace = 5 * time.Second

// readAdapterEvents читает события адаптера так, чтобы чтение всегда
// заканчивалось: по концу вывода, по отмене стадии или по отсрочке после
// итога. Иначе адаптер с незакрытым stdout подвесил бы стадию.
func readAdapterEvents(ctx context.Context, stdout io.ReadCloser, sink runSink, stopFeed func()) (agentRun, error) {
	type parsed struct {
		run agentRun
		err error
	}
	done := make(chan parsed, 1)
	result := make(chan struct{}, 1)
	go func() {
		run, err := parseAdapterStream(stdout, sink, func() {
			stopFeed()
			select {
			case result <- struct{}{}:
			default:
			}
		})
		done <- parsed{run, err}
	}()
	for {
		select {
		case p := <-done:
			return p.run, p.err
		case <-ctx.Done():
			// Стадию отменили: процесс убьёт Cancel, вывод закроется.
			_ = stdout.Close()
			p := <-done
			return p.run, p.err
		case <-result:
			select {
			case p := <-done:
				return p.run, p.err
			case <-time.After(adapterTailGrace):
				// Итог есть, вывод не закрыт — дальше ждать нечего.
				_ = stdout.Close()
				p := <-done
				return p.run, p.err
			case <-ctx.Done():
				_ = stdout.Close()
				p := <-done
				return p.run, p.err
			}
		}
	}
}

// parseAdapterStream разбирает построчный JSON адаптера. Неизвестный вид
// и нечитаемая строка игнорируются: контракт расширяется добавлением
// событий, а мусор в stdout (предупреждение чужой библиотеки) не должен
// валить стадию.
func parseAdapterStream(r io.Reader, sink runSink, onResult func()) (agentRun, error) {
	var run agentRun
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		switch head.Type {
		case sdk.TypeStep:
			var s sdk.Step
			if err := json.Unmarshal(raw, &s); err != nil || sink.step == nil {
				continue
			}
			sink.step(&pb.AgentEvent{
				Kind: s.Kind, Tool: s.Tool, Detail: s.Detail,
				Files: s.Files, Ok: s.OK, Text: stepText(s),
			})
		case sdk.TypeTranscript:
			var t sdk.Transcript
			if err := json.Unmarshal(raw, &t); err != nil || t.Text == "" {
				continue
			}
			text.WriteString(t.Text)
			if sink.transcript != nil {
				sink.transcript([]byte(t.Text))
			}
		case sdk.TypeUsage:
			var u sdk.Usage
			if err := json.Unmarshal(raw, &u); err != nil {
				continue
			}
			run.Usage = usageReport{TokensIn: u.TokensIn, TokensOut: u.TokensOut,
				CostUSD: u.CostUSD, CtxPct: saneCtxPct(u.CtxPct)}
			if u.Model != "" {
				run.Model = u.Model
			}
		case sdk.TypeResult:
			var res sdk.Result
			if err := json.Unmarshal(raw, &res); err != nil {
				continue
			}
			run.FinalText, run.isError = res.Text, res.Error
			// Итог — терминальное событие контракта: подачу stdin можно
			// закрывать, не дожидаясь выхода процесса.
			if onResult != nil {
				onResult()
			}
		}
	}
	if run.FinalText == "" {
		// Итога не было (обрыв, падение адаптера): маркеры ищем в тексте,
		// как у универсальной обёртки.
		run.FinalText = text.String()
	}
	return run, sc.Err()
}

// stepText — читаемое представление шага: адаптер может его не давать.
func stepText(s sdk.Step) string {
	if s.Text != "" {
		return s.Text
	}
	out := s.Tool
	if out == "" {
		out = s.Kind
	}
	if s.Detail != "" {
		out += " " + s.Detail
	}
	if out == "" {
		out = "шаг агента"
	}
	return out
}

// saneCtxPct отбрасывает заполненность контекста вне 0–100: значение
// приходит от чужой программы.
func saneCtxPct(v *int32) *int32 {
	if v == nil || *v < 0 || *v > 100 {
		return nil
	}
	return v
}
