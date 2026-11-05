//go:build linux

package main

import "golang.org/x/sys/unix"

// hardenProcess помечает процесс недампируемым: процессы того же
// пользователя (в том числе команды агента) не читают его память и
// /proc/<pid>/environ, к нему нельзя подключиться ptrace.
func hardenProcess() {
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// processDumpable — признак PR_GET_DUMPABLE этого процесса (для проверки).
func processDumpable() int {
	v, _ := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	return v
}
