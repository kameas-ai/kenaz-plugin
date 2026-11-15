//go:build !windows

package installed

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// abortSignals are the signals that end a journey early. Run turns them
// into a cancelled context so that its deferred clean-up still runs.
var abortSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// ownGroup starts cmd as the leader of a new process group, so that the
// relay and anything it starts can be killed together and a signal sent to
// the journey's own group (Ctrl-C) reaches the journey first.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// guardProcess is the Windows job object; Unix has the watchdog instead.
func guardProcess(*exec.Cmd) (release func()) { return func() {} }

// killGroup kills cmd's process group (cmd leads it, see ownGroup).
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

// killTree kills pid, and its process group when it leads one (a relay
// `kenaz mcp` spawns is a session and group leader). The caller has
// verified that pid runs the sandbox's binary.
func killTree(pid int) {
	if pid <= 1 {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// processExe returns the path of the executable pid runs, and whether pid
// is alive. Linux reads /proc; elsewhere `ps -o comm=` prints the path the
// process was executed from.
func processExe(pid int) (exe string, alive bool) {
	if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
		return "", false
	}
	if runtime.GOOS == "linux" {
		// No link: the process is gone, or a zombie, which runs nothing.
		p, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err != nil {
			return "", false
		}
		return strings.TrimSuffix(p, " (deleted)"), true
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=,comm=").Output()
	if err != nil {
		return "", false
	}
	stat, comm, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if strings.HasPrefix(stat, "Z") {
		return "", false
	}
	return strings.TrimSpace(comm), true
}
