//go:build !windows

package installed

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// interruptSelf delivers SIGTERM to this process, as a CI cancellation or
// `kill` does; Run turns it into a cancelled context.
func interruptSelf(t *testing.T, _ context.CancelFunc) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Errorf("SIGTERM: %v", err)
	}
}

// daemonsUnder lists the pids whose command starts with a path under root,
// from `ps`.
func daemonsUnder(t *testing.T, root string) []int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[1], root+"/") {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
