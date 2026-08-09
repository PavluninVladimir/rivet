// rivet-runner — агент на машине исполнителя: gRPC-клиент control plane
// и PTY-обёртка CLI-агента. Наполняется в задачах 1.12–1.13.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rivet-runner:", err)
		os.Exit(1)
	}
}
