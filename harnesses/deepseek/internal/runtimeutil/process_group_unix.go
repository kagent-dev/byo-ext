//go:build unix

package runtimeutil

import (
	"os/exec"
	"syscall"
)

// ConfigureProcessGroup prepares command for process-group signaling.
func ConfigureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
