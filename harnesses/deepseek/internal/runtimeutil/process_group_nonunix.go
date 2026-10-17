//go:build !unix

package runtimeutil

import (
	"os/exec"
)

// ConfigureProcessGroup is a no-op on platforms without Unix process groups.
func ConfigureProcessGroup(*exec.Cmd) {}
