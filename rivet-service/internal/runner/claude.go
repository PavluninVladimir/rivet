package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Нативный адаптер Claude Code (design add-claude-code-adapter): запуск
// claude в неинтерактивном режиме с потоковым машиночитаемым выводом и
// хуками. Шаги и затронутые файлы — из событий хуков (PostToolUse и др.),
// токены/стоимость/модель — из финального result, заполненность контекста —
// из usage последнего сообщения ассистента. Маркер USAGE: не требуется.

type claudeAdapter struct {
	cfg Config
}

// claudeContextWindow — окно контекста по умолчанию, когда result не несёт
// modelUsage.contextWindow.
const claudeContextWindow = 200_000

func (c *claudeAdapter) Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error) {
	hooksDir := filepath.Join(c.cfg.Workdir, "hooks")
	// 0700: к сокету хуков не должен подключаться другой локальный
	// пользователь (инжекция фальшивых шагов).
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return agentRun{}, err
	}
	runID := newMsgID()
	sockPath, sockCleanup, err := hookSocketPath(hooksDir)
	if err != nil {
		return agentRun{}, err
	}
	defer sockCleanup()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return agentRun{}, fmt.Errorf("hook socket: %w", err)
	}
	ul := ln.(*net.UnixListener)
	defer func() { _ = ul.Close() }()
	exe, err := os.Executable()
	if err != nil {
		return agentRun{}, err
	}
	// Путь бинарника уезжает в shell-команду хука — кавычки обязательны.
	hookCmd := shellQuote(exe) + " hook"
	settings, err := hookSettings(hooksDir, runID, hookCmd)
	if err != nil {
		return agentRun{}, err
	}
	defer settings.cleanup()

	// Приём событий хуков: одно подключение — одно событие JSON'ом.
	// Барьер доставки: Run не возвращается, пока принятые события не отданы
	// в sink — иначе StageResult закрыл бы сессию раньше поздних шагов.
	// Хук мог успеть подключиться (backlog), но не быть принятым к выходу
	// claude — после Wait приём продолжается до короткого deadline, а не
	// обрывается закрытием сокета.
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ul.Accept()
			if err != nil {
				return // deadline или закрытие — приём завершён
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				raw, err := io.ReadAll(io.LimitReader(conn, 1<<20))
				if err != nil || len(raw) == 0 {
					return
				}
				if ev, ok := parseHookEvent(raw, dir); ok {
					sink.step(ev)
				}
			}()
		}
	}()

	// Промпт уходит через stdin, не в argv: аргументы видны в ps любому
	// пользователю машины, а большой diff review упёрся бы в лимит execve.
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions",
		"--settings", settings.path,
	}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	bin := c.cfg.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	// RIVET_HOOK_CMD дублирует команду хука для стендов и тестов: fake-claude
	// вызывает её сам вместо чтения settings-файла.
	cmd.Env = append(os.Environ(), "RIVET_HOOK_SOCK="+sockPath, "RIVET_HOOK_CMD="+hookCmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agentRun{}, err
	}
	cmd.Stderr = cmd.Stdout // stderr агента — в тот же поток строк (не JSON — уйдёт в транскрипт)
	if err := cmd.Start(); err != nil {
		return agentRun{}, fmt.Errorf("запуск %s: %w", bin, err)
	}
	run, scanErr := parseClaudeStream(stdout, dir, sink.transcript)
	werr := cmd.Wait()
	// Процесс завершён: допринимаем подключения из backlog до короткого
	// deadline, дожидаемся доставки принятых событий, затем возвращаемся.
	_ = ul.SetDeadline(time.Now().Add(300 * time.Millisecond))
	<-acceptDone
	wg.Wait()
	if run.isError {
		return run, fmt.Errorf("claude завершил запуск с ошибкой: %s", clipRunes(run.FinalText, 500))
	}
	if run.FinalText == "" {
		if werr != nil {
			return run, fmt.Errorf("claude: %w", werr)
		}
		if scanErr != nil {
			return run, fmt.Errorf("чтение вывода claude: %w", scanErr)
		}
	}
	return run, werr
}

// parseClaudeStream разбирает stream-json построчно: текст ассистента и
// вызовы инструментов идут в транскрипт читаемыми строками, финальный
// result даёт usage и текст для маркеров. Неизвестные строки и типы
// игнорируются (формат Claude Code меняется между версиями).
func parseClaudeStream(r io.Reader, dir string, transcript func([]byte)) (agentRun, error) {
	var run agentRun
	var lastText strings.Builder
	var lastAssistant *claudeUsage
	emit := func(s string) {
		if transcript != nil && s != "" {
			transcript([]byte(s))
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var line claudeLine
		if err := json.Unmarshal(raw, &line); err != nil {
			// Не JSON (stderr, предупреждения CLI) — в транскрипт как есть.
			emit(string(raw) + "\n")
			continue
		}
		switch line.Type {
		case "system":
			if line.Subtype == "init" {
				run.Model = line.Model
				emit(fmt.Sprintf("Claude Code: сессия %s, модель %s\n", line.SessionID, line.Model))
			}
		case "assistant":
			if line.Message == nil {
				continue
			}
			if line.Message.Model != "" {
				run.Model = line.Message.Model
			}
			if line.Message.Usage != nil {
				lastAssistant = line.Message.Usage
			}
			for _, b := range line.Message.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						emit(b.Text + "\n")
						lastText.WriteString(b.Text + "\n")
					}
				case "tool_use":
					emit("→ " + b.Name + toolSummarySuffix(b.Name, b.Input, dir) + "\n")
				}
			}
		case "result":
			run.FinalText = line.Result
			run.isError = line.IsError
			run.Usage = claudeUsageReport(line, lastAssistant)
			if line.Result != "" {
				emit(line.Result + "\n")
			}
		}
	}
	if run.FinalText == "" {
		// result не пришёл (обрыв, отмена): маркеры ищем в последнем тексте
		// ассистента, usage остаётся с nil-полями (данных нет ≠ ноль).
		run.FinalText = lastText.String()
	}
	return run, sc.Err()
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

func (u *claudeUsage) contextTokens() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Message   *struct {
		Model   string       `json:"model"`
		Usage   *claudeUsage `json:"usage"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result       string       `json:"result"`
	IsError      bool         `json:"is_error"`
	TotalCostUSD *float64     `json:"total_cost_usd"`
	Usage        *claudeUsage `json:"usage"`
	ModelUsage   map[string]struct {
		ContextWindow int64 `json:"contextWindow"`
	} `json:"modelUsage"`
}

// claudeUsageReport — usage стадии из финального result (api-contract:
// tokens_in — всё, что модель прочитала, включая кэш).
func claudeUsageReport(line claudeLine, lastAssistant *claudeUsage) usageReport {
	var r usageReport
	if line.Usage != nil {
		in, out := line.Usage.contextTokens(), line.Usage.OutputTokens
		r.TokensIn, r.TokensOut = &in, &out
	}
	r.CostUSD = line.TotalCostUSD
	if lastAssistant != nil {
		window := int64(claudeContextWindow)
		for _, mu := range line.ModelUsage {
			if mu.ContextWindow > 0 {
				window = mu.ContextWindow
				break
			}
		}
		pct := int32(lastAssistant.contextTokens() * 100 / window)
		if pct >= 0 && pct <= 100 {
			r.CtxPct = &pct
		}
	}
	return r
}

// hookSocketPath — путь unix-сокета для событий хуков. Путь сокета ограничен
// ~104 байтами (sun_path), поэтому длинный workdir заменяется коротким
// приватным каталогом в /tmp (0700, MkdirTemp).
func hookSocketPath(hooksDir string) (string, func(), error) {
	p := filepath.Join(hooksDir, newMsgID()[:8]+".sock")
	if len(p) < 90 {
		return p, func() { _ = os.Remove(p) }, nil
	}
	dir, err := os.MkdirTemp("/tmp", "rvthk")
	if err != nil {
		return "", nil, fmt.Errorf("hook socket dir: %w", err)
	}
	return filepath.Join(dir, "h.sock"), func() { _ = os.RemoveAll(dir) }, nil
}

// ─── хуки ────────────────────────────────────────────────────────────────

// hookSettings — файл настроек Claude Code с хуками на команду
// «rivet-runner hook» (абсолютный путь текущего бинарника). PreToolUse не
// подключается: два события на инструмент удвоили бы event log (design).
func hookSettings(dir, runID, hookCmd string) (tmpFile, error) {
	hook := []map[string]any{{
		"hooks": []map[string]any{{"type": "command", "command": hookCmd, "timeout": 10}},
	}}
	settings := map[string]any{"hooks": map[string]any{
		"PostToolUse":        hook,
		"PostToolUseFailure": hook,
		"Stop":               hook,
		"SubagentStop":       hook,
		"SessionStart":       hook,
		"PreCompact":         hook,
	}}
	raw, err := json.Marshal(settings)
	if err != nil {
		return tmpFile{}, err
	}
	return createTemp(dir, "settings-"+runID+"-*.json", string(raw))
}

// hookEvent — полезная нагрузка хука Claude Code на stdin.
type hookEvent struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolError     string         `json:"tool_error"`
}

// parseHookEvent превращает событие хука в шаг сессии; false — событие не
// порождает шаг. Пути приводятся к относительным от рабочей копии.
func parseHookEvent(raw []byte, dir string) (*pb.AgentEvent, bool) {
	var ev hookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, false
	}
	switch ev.HookEventName {
	case "PostToolUse", "PostToolUseFailure":
		ok := ev.HookEventName == "PostToolUse"
		detail, files := toolDetail(ev.ToolName, ev.ToolInput, dir)
		text := ev.ToolName
		if detail != "" {
			text += " " + detail
		}
		if !ok {
			text += " — ошибка"
			if ev.ToolError != "" {
				detail = clipRunes(ev.ToolError, 500)
			}
		}
		return &pb.AgentEvent{Kind: "tool", Tool: ev.ToolName, Detail: detail,
			Files: files, Ok: ok, Text: text}, true
	case "Stop":
		return &pb.AgentEvent{Kind: "stop", Ok: true, Text: "агент завершил работу"}, true
	case "SubagentStop":
		return &pb.AgentEvent{Kind: "note", Ok: true, Text: "субагент завершил работу"}, true
	case "SessionStart":
		return &pb.AgentEvent{Kind: "note", Ok: true, Text: "сессия Claude Code начата"}, true
	case "PreCompact":
		return &pb.AgentEvent{Kind: "note", Ok: true, Text: "контекст агента сжимается"}, true
	}
	return nil, false
}

// toolDetail — краткий аргумент и затронутые файлы по известным
// инструментам Claude Code; чтение файлов затронутым не считается.
func toolDetail(tool string, input map[string]any, dir string) (detail string, files []string) {
	str := func(key string) string {
		v, _ := input[key].(string)
		return v
	}
	switch tool {
	case "Edit", "Write", "MultiEdit":
		return touched(str("file_path"), dir)
	case "NotebookEdit":
		return touched(str("notebook_path"), dir)
	case "Read":
		return relPath(str("file_path"), dir), nil
	case "Bash":
		return clipRunes(str("command"), 200), nil
	case "Glob", "Grep":
		d := str("pattern")
		if p := str("path"); p != "" {
			d += " в " + relPath(p, dir)
		}
		return clipRunes(d, 200), nil
	}
	return "", nil
}

// touched — detail и затронутые файлы для инструмента правки: в files идут
// только пути внутри рабочей копии (протокол обещает пути от её корня, а
// файлы вне worktree не относятся к проекту и раскрывали бы пути хоста).
func touched(p, dir string) (string, []string) {
	rel := relPath(p, dir)
	if rel == "" {
		return "", nil
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return rel, nil
	}
	return rel, []string{filepath.Clean(rel)}
}

// relPath — путь относительно рабочей копии; вне её — как есть.
// Каталог сверяется и по разыменованным симлинкам: на macOS /var — симлинк
// на /private/var, а хук отдаёт разыменованный путь.
func relPath(p, dir string) string {
	if p == "" || dir == "" {
		return p
	}
	dirs := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		dirs = append(dirs, resolved)
	}
	for _, d := range dirs {
		if rel, err := filepath.Rel(d, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}

// toolSummarySuffix — хвост читаемой строки транскрипта для tool_use.
func toolSummarySuffix(tool string, input map[string]any, dir string) string {
	if d, _ := toolDetail(tool, input, dir); d != "" {
		return " " + d
	}
	return ""
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ─── подкоманда hook ─────────────────────────────────────────────────────

// HookMain — команда хука Claude Code: читает JSON события со stdin и
// пересылает runner'у по unix-сокету из RIVET_HOOK_SOCK. Завершается
// успешно всегда: хук не должен блокировать или прерывать агента
// (спека agent-integration «Хук без связи с runner'ом»).
func HookMain() int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		return 0
	}
	sock := os.Getenv("RIVET_HOOK_SOCK")
	if sock == "" {
		return 0
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return 0
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(raw); err != nil {
		slog.Debug("hook: событие не доставлено", "err", err)
	}
	return 0
}

// createTemp — временный файл с содержимым и функцией очистки.
func createTemp(dir, pattern, content string) (tmpFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return tmpFile{}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return tmpFile{}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return tmpFile{}, err
	}
	return tmpFile{path: f.Name(), cleanup: cleanup}, nil
}
