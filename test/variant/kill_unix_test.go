//go:build !windows

package variant_test

import "syscall"

// kill ends the process pid at once (SIGKILL).
func kill(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
