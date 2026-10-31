package variant_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

// The commands these tests run start relays (`kenaz daemon`) that outlive
// the command, as they do on a real machine. Every relay a test starts is
// stopped by the test: each machine's cleanup stops the relays under its
// root, and TestMain fails the run if any relay from the run is left.

// process is a running process: its pid and command line.
type process struct {
	pid  int
	args string
}

// processes lists every process of this user's session that ps shows.
func processes() []process {
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		return nil
	}
	var ps []process
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidStr, args, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == os.Getpid() {
			continue
		}
		ps = append(ps, process{pid, strings.TrimSpace(args)})
	}
	return ps
}

// isRelay reports whether args is a relay run from one of roots.
func isRelay(args string, roots []string) bool {
	if !strings.Contains(args, " daemon") {
		return false
	}
	for _, r := range roots {
		if r != "" && strings.HasPrefix(args, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// relaysUnder returns the relays whose executable is under one of roots.
func relaysUnder(roots []string) []process {
	var out []process
	for _, p := range processes() {
		if isRelay(p.args, roots) {
			out = append(out, p)
		}
	}
	return out
}

// withResolved adds each root's symlink-free form (on macOS the temp
// directory is reached through /var -> /private/var).
func withResolved(roots []string) []string {
	out := append([]string(nil), roots...)
	for _, r := range roots {
		if e, err := filepath.EvalSymlinks(r); err == nil && e != r {
			out = append(out, e)
		}
	}
	return out
}

// stopRelays stops every relay started on this machine: first through the
// control API of each relay that left a daemon.json under the root (a
// clean stop, which also removes the file), then SIGKILL for any of them
// still there. It fails the test when one survives even that.
//
// A relay is this machine's when it runs from the machine's root, or runs
// from the shared build directory and a daemon.json under the machine's
// root names its pid. A relay another machine started from the shared
// builds is never touched, so tests may run in parallel. One that exits
// before writing its daemon.json is caught by TestMain's guard.
func (m *machine) stopRelays() {
	own := withResolved([]string{m.root})
	shared := withResolved([]string{built.dir})
	// named: pid -> state directory, for every daemon.json under the root.
	// prepare writes daemon.json files for relays that never ran, so a
	// pid counts only while a relay from our roots runs under it.
	named := map[int]string{}
	filepath.WalkDir(m.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != daemonstate.FileName {
			return nil
		}
		if st, err := daemonstate.Read(filepath.Dir(p)); err == nil {
			named[st.PID] = filepath.Dir(p)
		}
		return nil
	})
	mine := func() []process {
		var out []process
		for _, p := range processes() {
			_, ok := named[p.pid]
			if isRelay(p.args, own) || (ok && isRelay(p.args, shared)) {
				out = append(out, p)
			}
		}
		return out
	}
	for _, p := range mine() {
		dir, ok := named[p.pid]
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if c, err := daemon.ConnectAny(ctx, dir); err == nil && c.Info.PID == p.pid {
			c.Shutdown(ctx, false)
		}
		cancel()
	}
	if left := waitGone(mine, 3*time.Second); len(left) > 0 {
		for _, p := range left {
			syscall.Kill(p.pid, syscall.SIGKILL)
		}
		if left := waitGone(mine, 3*time.Second); len(left) > 0 {
			m.t.Errorf("relays still running after SIGKILL: %v", left)
		}
	}
}

// waitGone polls list until it is empty or wait elapses, and returns
// what is left.
func waitGone(list func() []process, wait time.Duration) []process {
	deadline := time.Now().Add(wait)
	for {
		left := list()
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// machineRoots records every machine's root, for TestMain's guard.
var machineRoots struct {
	sync.Mutex
	roots []string
}

func recordMachine(root string) {
	machineRoots.Lock()
	machineRoots.roots = append(machineRoots.roots, root)
	machineRoots.Unlock()
}

// leftRoots are the directories a relay of this run can be run from: the
// shared builds and every machine's root. Nothing else (another run's
// tests, the developer's own relay) is ever touched.
func leftRoots() []string {
	machineRoots.Lock()
	defer machineRoots.Unlock()
	return withResolved(append([]string{built.dir}, machineRoots.roots...))
}

// sweepRelays kills every relay run from under roots (TestMain's guard)
// and returns their command lines.
func sweepRelays(roots []string) []string {
	var left []string
	for _, p := range relaysUnder(roots) {
		syscall.Kill(p.pid, syscall.SIGKILL)
		left = append(left, p.args)
	}
	return left
}
