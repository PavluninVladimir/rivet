//go:build unix && !linux

package native

import (
	"fmt"
	"os/exec"
	"syscall"
)

// NetworkIsolationAvailable — на этой платформе изоляции сети нет: команды
// агента выполняются с сетью, runner объявляет это честно.
func NetworkIsolationAvailable() bool { return false }

// launch — sh -c в своей группе процессов (режим direct). Песочницы здесь
// нет, поэтому с requireSandbox команда не запускается.
func launch(spec commandSpec, env []string) (launched, error) {
	if spec.requireSandbox {
		return launched{}, fmt.Errorf("%w: %s", ErrNoSandbox, SandboxUnavailableReason())
	}
	bin, err := exec.LookPath("sh")
	if err != nil {
		return launched{}, fmt.Errorf("оболочка недоступна: %w", err)
	}
	cmd := exec.Command(bin, "-c", spec.command)
	cmd.Dir = spec.dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return launched{cmd: cmd, mode: modeDirect, close: func() {}}, nil
}

// SandboxUnavailableReason — песочницы на этой платформе нет.
func SandboxUnavailableReason() string {
	return "песочница команд есть только на Linux"
}
