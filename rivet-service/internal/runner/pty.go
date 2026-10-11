package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
)

// runPTY запускает команду в псевдотерминале: агенты ведут себя как в живом
// терминале, а вывод стримится чанками (live-наблюдение, глубина minimal).
// Промпт передаётся не через stdin (PTY эхоирует ввод в вывод и ломает
// парсинг транскрипта), а окружением — команда получает RIVET_PROMPT_FILE.
func runPTY(ctx context.Context, dir, command string, env []string, transcript func([]byte)) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		return "", err
	}
	defer f.Close()

	var out strings.Builder
	buf := make([]byte, 16*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if transcript != nil {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				transcript(chunk)
			}
		}
		if err != nil {
			break // io.EOF или закрытие PTY при завершении процесса
		}
	}
	werr := cmd.Wait()
	return out.String(), werr
}

func newMsgID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
