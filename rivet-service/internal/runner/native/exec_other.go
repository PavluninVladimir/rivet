//go:build !unix

package native

import (
	"context"
	"errors"
	"time"
)

// На платформах без групп процессов инструмент run недоступен: runner
// поддерживается на Linux и macOS.

type commandSpec struct {
	dir, command string
	timeout      time.Duration
	home, path   string
	isolate      bool
	maxOut       int
	killGrace    time.Duration
	readOnly     []CacheDir
	hidePaths    []string
	extraEnv     []string
	// requireSandbox, onOutput и network — см. exec_unix.go.
	requireSandbox bool
	onOutput       func([]byte)
	network        bool
}

type commandResult struct {
	output    string
	exitCode  int
	timedOut  bool
	leftovers int
	isolated  bool
	sandboxed bool
	err       error
}

// NetworkIsolationAvailable — изоляции сети нет.
func NetworkIsolationAvailable() bool { return false }

func runCommand(context.Context, commandSpec) commandResult {
	return commandResult{err: errors.New("инструмент run не поддерживается на этой платформе")}
}

// SandboxUnavailableReason — песочницы на этой платформе нет.
func SandboxUnavailableReason() string {
	return "песочница команд есть только на Linux"
}
