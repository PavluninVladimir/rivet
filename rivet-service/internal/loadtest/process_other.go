//go:build !unix

package loadtest

import "os/exec"

func protectProcess(cmd *exec.Cmd) {}
