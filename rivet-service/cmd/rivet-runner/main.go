// rivet-runner — агент на машине исполнителя: gRPC-клиент control plane,
// адаптеры подключения CLI-агента (нативный Claude Code, PTY-обёртка) и
// подкоманда hook для событий хуков Claude Code.
package main

import (
	"fmt"
	"os"

	"github.com/PavluninVladimir/rivet/internal/archcli"
	"github.com/PavluninVladimir/rivet/internal/runner"
)

func main() {
	// «rivet-runner hook» — команда хука Claude Code: stdin → unix-сокет
	// runner'а, всегда успешное завершение (хук не должен мешать агенту).
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(runner.HookMain())
	}
	// «rivet-runner architecture …» — проверка и представления описания
	// архитектуры в рабочей копии (add-architecture-c4): её зовут проверка
	// шага test и задание Архитектора через $RIVET_RUNNER_BIN.
	if len(os.Args) > 1 && os.Args[1] == "architecture" {
		os.Exit(archcli.Main(os.Args[2:], os.Stdout))
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rivet-runner:", err)
		os.Exit(1)
	}
}
