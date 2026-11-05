// rivet-runner — runner на машине исполнителя: gRPC-клиент control plane,
// агентский цикл Rivet и исполнение заданий выкатки.
package main

import (
	"fmt"
	"os"

	"github.com/PavluninVladimir/rivet/internal/archcli"
)

func main() {
	// «rivet-runner architecture …» — проверка и представления описания
	// архитектуры в рабочей копии (add-architecture-c4): её зовут проверка
	// шага test и задание Архитектора через $RIVET_RUNNER_BIN. Идёт до
	// разбора секретов: подкоманде не нужны ни токен, ни перезапуск.
	if len(os.Args) > 1 && os.Args[1] == "architecture" {
		os.Exit(archcli.Main(os.Args[2:], os.Stdout))
	}
	// Секреты — из окружения и аргументов в память, с перезапуском
	// процесса на очищенном окружении (secrets.go).
	st, err := prepareSecrets(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rivet-runner:", err)
		os.Exit(1)
	}
	for _, w := range st.warnings {
		fmt.Fprintln(os.Stderr, "rivet-runner:", w)
	}
	if err := run(st.args, st.secrets); err != nil {
		fmt.Fprintln(os.Stderr, "rivet-runner:", err)
		os.Exit(1)
	}
}
