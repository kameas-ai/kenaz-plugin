package installed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// moduleRoot is the checkout the journeys build.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// needsPublicVariant skips a test that runs a real journey when this test
// binary is not the public kenaz variant (go test -tags kenaz_dev): the
// journey builds and bundles the public variant, and the bundle loader
// compiled into this binary then refuses it before any relay starts.
func needsPublicVariant(t *testing.T) {
	t.Helper()
	if variant.Current.Name != variant.Kenaz.Name {
		t.Skipf("the journeys run the public kenaz variant; this test binary is %s", variant.Current.Name)
	}
}

// waitNoDaemons fails unless, within wait, no process runs a binary under
// root: checked through the operating system's process list (daemonsUnder),
// not through the journey's own bookkeeping.
func waitNoDaemons(t *testing.T, root string, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		left := daemonsUnder(t, root)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d process(es) still run a binary under the journey's sandbox: pids %v", len(left), left)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A journey interrupted mid-run stops the relay it started and a relay that
// `kenaz mcp` spawned detached after the first one died, and removes its
// sandbox. Both are found through daemon.json and the process list.
func TestInterruptedJourneyLeavesNoRelay(t *testing.T) {
	needsPublicVariant(t)
	var root string
	var first, second int
	afterRelayStarted = func(j *journey) {
		root, first = j.s.root, j.relay.Process.Pid
		if !j.s.ours(first) {
			t.Errorf("the started relay (pid %d) is not seen as the sandbox's binary", first)
		}
		// The relay dies mid-journey; the next `kenaz mcp` spawns another,
		// detached from this process.
		killGroup(j.relay)
		<-j.relayExited
		c, err := j.s.startMCP(context.Background())
		if err != nil {
			t.Errorf("kenaz mcp: %v", err)
			return
		}
		defer c.close()
		if _, err := c.initialize("conformance-abort-test"); err != nil {
			t.Errorf("initialize: %v", err)
			return
		}
		_, _ = c.call("fleet_status", nil)
		deadline := time.Now().Add(20 * time.Second)
		for second == 0 && time.Now().Before(deadline) {
			for _, r := range j.s.recordedRelays() {
				if r.pid != first && j.s.ours(r.pid) {
					second = r.pid
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if second == 0 {
			t.Error("kenaz mcp did not spawn a detached relay to replace the dead one")
		}
		if got := daemonsUnder(t, root); len(got) == 0 {
			t.Error("the process list shows no relay under the sandbox while one is running: the check below would prove nothing")
		}
		interruptJourney(t)
	}
	t.Cleanup(func() { afterRelayStarted = nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interruptJourney = func(t *testing.T) { interruptSelf(t, cancel) }
	res, err := Run(ctx, Options{Root: moduleRoot(t), Feature: "private-memory", Host: "codex-cli"})
	if root == "" {
		t.Fatalf("the journey never started its relay: %v %+v", err, res.Steps)
	}
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("an interrupted journey returned %v", err)
	}
	waitNoDaemons(t, root, 10*time.Second)
	for _, pid := range []int{first, second} {
		if exe, alive := processExe(pid); alive && strings.Contains(exe, filepath.Base(root)) {
			t.Errorf("pid %d still runs the sandbox's binary", pid)
		}
	}
	if _, err := os.Stat(root); err == nil {
		t.Error("the sandbox directory was not removed")
	}
}

var interruptJourney func(t *testing.T)

const abortHelperEnv = "KENAZ_CONFORMANCE_ABORT_HELPER"

// TestKilledJourneyHelper is the journey TestKilledJourneyLeavesNoRelay
// kills: it starts the relay, says where, and waits to be killed.
func TestKilledJourneyHelper(t *testing.T) {
	report := os.Getenv(abortHelperEnv)
	if report == "" {
		t.Skip("helper of TestKilledJourneyLeavesNoRelay")
	}
	afterRelayStarted = func(j *journey) {
		_ = os.WriteFile(report+".tmp", []byte(j.s.root+"\n"+strconv.Itoa(j.relay.Process.Pid)+"\n"), 0o600)
		_ = os.Rename(report+".tmp", report)
		select {}
	}
	_, _ = Run(context.Background(), Options{Root: moduleRoot(t), Feature: "private-memory", Host: "codex-cli"})
}

// A journey killed outright (SIGKILL, TerminateProcess) runs no clean-up of
// its own. The relay must still not outlive it: the watchdog (Unix) or the
// job object (Windows) ends it. On Unix the watchdog also removes the
// sandbox.
func TestKilledJourneyLeavesNoRelay(t *testing.T) {
	needsPublicVariant(t)
	report := filepath.Join(t.TempDir(), "relay")
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledJourneyHelper$", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), abortHelperEnv+"="+report)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	var root string
	var pid int
	deadline := time.Now().Add(10 * time.Minute)
	for root == "" {
		if b, err := os.ReadFile(report); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) == 2 {
				root = lines[0]
				pid, _ = strconv.Atoi(lines[1])
			}
		}
		select {
		case <-exited:
			if root == "" {
				t.Fatal("the helper journey exited before it started a relay")
			}
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the helper journey did not start a relay")
		}
	}
	// Whatever happens below, do not leave the helper's sandbox behind. It is
	// a directory this test's own helper created with os.MkdirTemp.
	t.Cleanup(func() {
		if strings.HasPrefix(filepath.Base(root), "kenaz-conformance-") {
			_ = os.RemoveAll(root)
		}
	})
	if exe, alive := processExe(pid); !alive || !strings.Contains(exe, filepath.Base(root)) {
		t.Fatalf("the helper's relay (pid %d) is not running the sandbox's binary before the kill", pid)
	}
	if len(daemonsUnder(t, root)) == 0 {
		t.Fatal("the process list shows no relay under the sandbox before the kill")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	waitNoDaemons(t, root, 20*time.Second)
	if exe, alive := processExe(pid); alive && strings.Contains(exe, filepath.Base(root)) {
		t.Errorf("relay pid %d outlived the killed journey", pid)
	}
	if runtime.GOOS != "windows" {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(root); err != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Error("the watchdog did not remove the killed journey's sandbox")
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// stopRelays never touches a pid that does not run the sandbox's binary: a
// daemon.json naming another live process (here, this test) leaves it alone.
func TestStopRelaysLeavesOtherProcessesAlone(t *testing.T) {
	s, err := newSandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	// A long-lived process that is not the sandbox's binary.
	other := exec.Command(os.Args[0], "-test.run=^TestSleepHelper$")
	other.Env = append(os.Environ(), sleepHelperEnv+"=1")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	state := filepath.Join(s.kenazHome, "plugin", "prod")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	info := `{"pid":` + strconv.Itoa(other.Process.Pid) + `,"port":1,"nonce":"00"}`
	if err := os.WriteFile(filepath.Join(state, "daemon.json"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.recordedRelays(); len(got) != 1 || got[0].pid != other.Process.Pid {
		t.Fatalf("recorded relays: %+v", got)
	}
	if s.ours(other.Process.Pid) || s.ours(os.Getpid()) || s.ours(1) {
		t.Fatal("a process outside the sandbox is taken for the sandbox's relay")
	}
	if killed := s.stopRelays(nil, nil); len(killed) != 0 {
		t.Fatalf("killed %v", killed)
	}
	if _, alive := processExe(other.Process.Pid); !alive {
		t.Fatal("stopRelays killed a process that is not the sandbox's binary")
	}
}

const sleepHelperEnv = "KENAZ_CONFORMANCE_SLEEP_HELPER"

func TestSleepHelper(t *testing.T) {
	if os.Getenv(sleepHelperEnv) == "" {
		t.Skip("helper of TestStopRelaysLeavesOtherProcessesAlone")
	}
	time.Sleep(2 * time.Minute)
}
