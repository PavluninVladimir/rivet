// Команда rivet-adapter-example — эталонный адаптер агента Rivet
// (спека agent-integration «Открытый SDK адаптеров»).
//
// Запускает произвольный CLI-агент, стримит его вывод в транскрипт стадии
// и отдаёт итог. Годится и как рабочий адаптер для простого агента, и как
// шаблон для своего: весь контракт — в пакете pkg/adapter.
//
//	rivet-runner -adapter external \
//	  -adapter-cmd "rivet-adapter-example -cmd 'my-agent --prompt-file $RIVET_PROMPT_FILE'"
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/PavluninVladimir/rivet/pkg/adapter"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "adapter:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("rivet-adapter-example", flag.ContinueOnError)
	agentCmd := fs.String("cmd", os.Getenv("AGENT_CMD"),
		"команда агента; путь к файлу промпта приходит в $RIVET_PROMPT_FILE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*agentCmd) == "" {
		return fmt.Errorf("нужна команда агента (-cmd)")
	}
	out := adapter.NewWriter(os.Stdout)

	// Задание стадии приходит первой строкой; дальше могут идти строки
	// контекста от Rivet — этот адаптер дописывает их в промпт файла.
	in := adapter.NewReader(os.Stdin)
	prompt, _, err := in.Next()
	if err != nil || prompt == nil {
		return fmt.Errorf("задание стадии не получено: %v", err)
	}
	file, err := os.CreateTemp("", "rivet-prompt-*.md")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.WriteString(prompt.Text); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", *agentCmd)
	cmd.Env = append(os.Environ(), "RIVET_PROMPT_FILE="+file.Name())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запуск агента: %w", err)
	}

	var wg sync.WaitGroup
	var text strings.Builder
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream(stdout, out, &text)
	}()
	// Контекст от Rivet: показываем его агенту как строку транскрипта —
	// настоящий адаптер довёл бы его до самого агента. Горутина живёт до
	// выхода процесса и не удерживает его: ждать EOF на stdin нельзя, его
	// закрывает runner уже после нашего итога.
	go func() {
		for {
			_, ctxLine, err := in.Next()
			if err != nil {
				return
			}
			if ctxLine != nil {
				_ = out.Transcript("[контекст Rivet] " + ctxLine.Text + "\n")
			}
		}
	}()
	werr := cmd.Wait()
	wg.Wait()

	if werr != nil {
		return out.Result(fmt.Sprintf("агент завершился с ошибкой: %v", werr), true)
	}
	return out.Result(text.String(), false)
}

// stream отдаёт вывод агента в транскрипт построчно: человек видит ход
// стадии, а не итог в конце.
func stream(r io.Reader, out *adapter.Writer, text *strings.Builder) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		text.WriteString(line + "\n")
		_ = out.Transcript(line + "\n")
	}
}
