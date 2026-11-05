package runner

import (
	"context"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Подключение агента (add-native-agent, design решение 1): в продукте один
// адаптер — native, агентский цикл Rivet. Сторонние CLI-агенты (Claude
// Code, Codex, OpenCode), PTY-обёртка и внешние адаптеры удалены.

// AdapterNative — агентский цикл Rivet: runner сам ведёт диалог с моделью
// провайдера, внешних программ нет.
const AdapterNative = "native"

// AgentRivet — агент Rivet; единственный агент runner'а (runner выкатки
// регистрируется без агента).
const AgentRivet = "rivet"

// agentRun — итог одного запуска агента.
type agentRun struct {
	// FinalText — текст, по которому стадия разбирает маркеры (BLOCKED:,
	// VERDICT:): итог finish или ask_human.
	FinalText string
	// Usage — расход запуска; nil-поля = данных нет.
	Usage usageReport
	// Model — модель фактического запуска; пусто — из конфигурации runner'а.
	Model string
	// isError — агент сообщил об ошибке запуска.
	isError bool
}

// runSink — куда адаптер отдаёт данные запуска.
type runSink struct {
	// transcript — сырой поток вывода агента (транскрипт сессии).
	transcript func([]byte)
	// step — структурированные шаги сессии (вызовы инструментов).
	step func(ev *pb.AgentEvent)
	// session и contexts — обратный канал контекста: адаптер забирает
	// сообщения Rivet по своей сессии (спека agent-integration «Обратный
	// канал контекста»).
	session  string
	contexts *contextHub
	// stage — стадия запуска: native-адаптер по ней решает, считать ли
	// вердикт fail ошибкой запуска (реализация) или маркером (review, prompt).
	stage pb.StageResult_Stage
}

// adapter выполняет один запуск агента в каталоге рабочей копии.
type adapter interface {
	Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error)
}

// contextChannel — агент Rivet принимает контекст от Rivet следующим ходом.
func (c Config) contextChannel() bool { return c.Adapter == AdapterNative }

// depth — глубина данных: у агента Rivet полная (шаги инструментов, usage,
// транскрипт), у runner'а выкатки без агента — минимальная.
func (c Config) depth() string {
	if c.Adapter == AdapterNative {
		return "full"
	}
	return "minimal"
}

// testAdapter — подмена адаптера в тестах пакета (скриптовый агент вместо
// модели); в продукте nil.
var testAdapter func(Config) adapter

func newAdapter(cfg Config) adapter {
	if testAdapter != nil {
		if a := testAdapter(cfg); a != nil {
			return a
		}
	}
	return &nativeAdapter{cfg: cfg}
}
