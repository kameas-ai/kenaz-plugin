package installed

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// A journey owns every relay that runs from its sandbox, and none may
// outlive it: a relay left behind keeps a port and the temporary directory
// until its 15-minute idle exit. There are two kinds:
//
//   - the relay startRelay starts (j.relay), in its own process group;
//   - a relay `kenaz mcp` spawns, detached, if that one died mid-journey.
//     The journey never sees its process; daemon.json records it.
//
// stopRelays ends both on every return of Run, including one caused by a
// signal (Run turns SIGINT, SIGTERM and SIGHUP into a cancelled context).
// A journey that is killed outright runs no code, so the operating system
// has to do it: on Unix a watchdog process outside the journey's process
// group does the same clean-up when the journey's end of a pipe closes
// (watchdog_unix.go); on Windows the relay startRelay starts is in a job
// object that kills it when the journey's handle closes (proc_windows.go).
//
// Nothing here ever signals a process outside the sandbox: a recorded pid
// is stopped only while the operating system reports that its executable
// is the sandbox's own binary (processExe), so a stale daemon.json whose pid
// now belongs to something else, such as a developer's real relay, is left
// alone.

// relayPidFile is where startRelay records the relay's pid for the watchdog.
const relayPidFile = "relay.pid"

// recordedRelay is one daemon.json found inside the sandbox.
type recordedRelay struct {
	stateDir string
	pid      int
}

// recordedRelays lists the relays the sandbox's state says are running.
func (s *sandbox) recordedRelays() []recordedRelay {
	var out []recordedRelay
	matches, _ := filepath.Glob(filepath.Join(s.kenazHome, "plugin", "*", daemon.InfoFile))
	for _, m := range matches {
		var st struct {
			PID int `json:"pid"`
		}
		if b, err := os.ReadFile(m); err == nil && json.Unmarshal(b, &st) == nil && st.PID > 1 {
			out = append(out, recordedRelay{filepath.Dir(m), st.PID})
		}
	}
	return out
}

// ours reports whether pid is alive and running the sandbox's binary.
func (s *sandbox) ours(pid int) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	exe, alive := processExe(pid)
	if !alive || exe == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return within(exe, s.root) && samePath(exe, s.exe)
}

// stopRelays stops every relay of this sandbox: the one the journey started
// (own, with its process group) and any recorded in a daemon.json. A
// recorded relay is first asked to shut down the way `kenaz logout` asks
// (the request is authenticated by the nonce in the sandbox's own file, and
// the listener must prove it holds that nonce); one that is still running
// the sandbox's binary afterwards is killed. It returns the pids it had to
// kill.
func (s *sandbox) stopRelays(own *exec.Cmd, exited <-chan struct{}) (killed []int) {
	recorded := s.recordedRelays()
	for _, r := range recorded {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if c, err := daemon.ConnectAny(ctx, r.stateDir); err == nil {
			_ = c.Shutdown(ctx, false)
		}
		cancel()
	}
	gone := func(pid int, wait time.Duration) bool {
		deadline := time.Now().Add(wait)
		for s.ours(pid) {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(50 * time.Millisecond)
		}
		return true
	}
	if own != nil && own.Process != nil {
		pid := own.Process.Pid
		// It may have honoured the shutdown request above; give it a moment
		// to finish before it is killed.
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			killed = append(killed, pid)
			killGroup(own)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	for _, r := range recorded {
		if own != nil && own.Process != nil && r.pid == own.Process.Pid {
			continue
		}
		if gone(r.pid, 3*time.Second) {
			continue
		}
		// Checked again at the moment of the kill: only the sandbox's binary.
		if s.ours(r.pid) {
			killed = append(killed, r.pid)
			killTree(r.pid)
			gone(r.pid, 5*time.Second)
		}
	}
	_ = os.Remove(filepath.Join(s.root, relayPidFile))
	return killed
}

// recordRelayPid writes the started relay's pid where the watchdog reads it.
func (s *sandbox) recordRelayPid(pid int) {
	_ = os.WriteFile(filepath.Join(s.root, relayPidFile), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}
