package runner

import (
	"context"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Адаптеры подключения агента (спека agent-integration «Официальные
// адаптеры»): универсальная PTY-обёртка (минимальная глубина) и нативный
// адаптер Claude Code (полная глубина). Выбор — флагом -adapter; шов для
// будущих адаптеров других агентов.

const (
	AdapterWrap       = "wrap"
	AdapterClaudeCode = "claude-code"
)

// DefaultAgentCmd — команда обёртки по умолчанию (флаг -cmd); её
// переопределение переводит выбор адаптера по умолчанию на обёртку.
const DefaultAgentCmd = `claude -p "$(cat "$RIVET_PROMPT_FILE")" --dangerously-skip-permissions`

// DefaultAdapter — выбор адаптера, когда флаг -adapter не задан: нативный
// Claude Code для агента claude-code с командой по умолчанию, иначе
// обёртка (существующие стенды с RIVET_AGENT_CMD продолжают работать).
func DefaultAdapter(agent, agentCmd string) string {
	if agent == AdapterClaudeCode && agentCmd == DefaultAgentCmd {
		return AdapterClaudeCode
	}
	return AdapterWrap
}

// agentRun — итог запуска агента.
type agentRun struct {
	// FinalText — текст, по которому стадия разбирает маркеры
	// (BLOCKED:, VERDICT:): у обёртки — весь вывод, у нативного
	// адаптера — финальный текст результата.
	FinalText string
	// Usage — расход запуска; nil-поля = данных нет.
	Usage usageReport
	// Model — модель фактического запуска; пусто — из конфигурации runner'а.
	Model string
	// isError — агент сообщил об ошибке запуска в машиночитаемом итоге.
	isError bool
}

// runSink — потоки запуска: чанки транскрипта и структурированные шаги.
// step получает событие без task_id/session_id — их заполняет стадия.
type runSink struct {
	transcript func([]byte)
	step       func(ev *pb.AgentEvent)
}

// adapter выполняет один запуск агента в каталоге рабочей копии.
type adapter interface {
	Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error)
}

// depthOf — глубина данных адаптера (объявляется при регистрации,
// спека agent-integration «Уровни глубины данных»).
func depthOf(adapterName string) string {
	if adapterName == AdapterClaudeCode {
		return "full"
	}
	return "minimal"
}

// newAdapter — адаптер по конфигурации.
func newAdapter(cfg Config) adapter {
	if cfg.Adapter == AdapterClaudeCode {
		return &claudeAdapter{cfg: cfg}
	}
	return &wrapAdapter{cfg: cfg}
}

// wrapAdapter — универсальная CLI-обёртка: команда агента в PTY, разбор
// маркера USAGE: из вывода (минимальная глубина, поведение до этого change).
type wrapAdapter struct {
	cfg Config
}

func (w *wrapAdapter) Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error) {
	pf, err := promptFile(w.cfg.Workdir, prompt)
	if err != nil {
		return agentRun{}, err
	}
	defer pf.cleanup()
	out, err := runPTY(ctx, dir, w.cfg.AgentCmd, []string{"RIVET_PROMPT_FILE=" + pf.path}, sink.transcript)
	return agentRun{FinalText: out, Usage: parseUsage(out)}, err
}

type tmpFile struct {
	path    string
	cleanup func()
}

// promptFile кладёт промпт во временный файл: PTY эхоирует stdin в вывод и
// ломает разбор транскрипта, поэтому промпт передаётся путём в окружении.
func promptFile(workdir, prompt string) (tmpFile, error) {
	f, err := createTemp(workdir, "prompt-*.md", prompt)
	if err != nil {
		return tmpFile{}, err
	}
	return f, nil
}
