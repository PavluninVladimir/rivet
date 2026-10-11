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
	// AdapterExternal — адаптер-процесс по публичному контракту
	// (пакет pkg/adapter): его пишет и запускает сам пользователь.
	AdapterExternal = "external"
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
	// session — сессия стадии: под ней адаптер с обратным каналом
	// регистрирует в contexts очередь контекста на время запуска
	// (спека agent-integration «Обратный канал контекста»).
	session  string
	contexts *contextHub
}

// adapter выполняет один запуск агента в каталоге рабочей копии.
type adapter interface {
	Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error)
}

// contextChannel — поддерживает ли адаптер runner'а обратный канал
// контекста: нативный доводит контекст до агента хуком, обёртка ничем не
// может (режим «только отправка»), внешний — как объявлено при запуске.
func (c Config) contextChannel() bool {
	if c.Adapter == AdapterExternal {
		return c.AdapterContext
	}
	return c.Adapter == AdapterClaudeCode
}

// depth — глубина данных адаптера (объявляется при регистрации, спека
// agent-integration «Уровни глубины данных»). У внешнего адаптера её
// задаёт запуск runner'а: сам адаптер о себе не отчитывается, а по
// умолчанию честнее занизить.
func (c Config) depth() string {
	switch c.Adapter {
	case AdapterClaudeCode:
		return "full"
	case AdapterExternal:
		switch c.AdapterDepth {
		case "full", "partial", "minimal":
			return c.AdapterDepth
		}
		return "minimal"
	}
	return "minimal"
}

// newAdapter — адаптер по конфигурации.
func newAdapter(cfg Config) adapter {
	switch cfg.Adapter {
	case AdapterClaudeCode:
		return &claudeAdapter{cfg: cfg}
	case AdapterExternal:
		return &externalAdapter{cfg: cfg}
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
	// Модель назначения уходит обёрнутому агенту переменной окружения:
	// что с ней делать, решает команда агента.
	env := []string{"RIVET_PROMPT_FILE=" + pf.path}
	if w.cfg.Model != "" {
		env = append(env, "RIVET_MODEL="+w.cfg.Model)
	}
	// Окружение профиля агента поверх окружения runner'а; аргументы профиля
	// добавляются к команде обёртки.
	env = append(env, w.cfg.ExtraEnv...)
	cmd := w.cfg.AgentCmd
	for _, arg := range w.cfg.ExtraArgs {
		cmd += " " + shellQuote(arg)
	}
	out, err := runPTY(ctx, dir, cmd, env, sink.transcript)
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
