//go:build windows

package variant_test

import "os"

// kill ends the process pid at once (TerminateProcess; Windows has no
// SIGKILL).
func kill(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
		p.Release()
	}
}
