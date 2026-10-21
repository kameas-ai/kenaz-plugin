//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detach makes cmd survive the spawning MCP process (new session, no
// controlling terminal).
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
