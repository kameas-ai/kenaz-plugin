//go:build windows

package installed

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// interruptSelf cancels the journey's context: Windows cannot deliver a
// console control event to this process alone.
func interruptSelf(_ *testing.T, cancel context.CancelFunc) { cancel() }

// daemonsUnder lists the pids whose executable is under root, from the
// operating system's process list.
func daemonsUnder(t *testing.T, root string) []int {
	t.Helper()
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_Process | ForEach-Object { "$($_.ProcessId)|$($_.ExecutablePath)" }`).Output()
	if err != nil {
		t.Fatalf("listing processes: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		id, exe, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || !strings.HasPrefix(strings.ToLower(exe), strings.ToLower(root)+`\`) {
			continue
		}
		if pid, err := strconv.Atoi(id); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
