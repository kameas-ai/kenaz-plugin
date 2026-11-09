package app

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync/atomic"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// TestMain isolates every app test from the developer's real home
// directory. The daemon's skills lane and `logout` manage skill directories
// under ~/.claude and ~/.agents; tests must never read or write those.
func TestMain(m *testing.M) {
	if args := os.Getenv(cliTestArgs); args != "" {
		runCLIForTest(args)
	}
	home, err := os.MkdirTemp("", "kenaz-app-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	skillsHome = func() (string, error) { return home, nil }
	// A notice names the installed tools' versions; a test must not run a real one.
	harnessVersion = func(string) string { return "" }
	// A detached relay would outlive the test (and run the test binary as
	// one). Tests start relays through Env.Spawn or in-process; any real
	// self-exec fails the run.
	var selfSpawns atomic.Int32
	spawnSelf = func(string, fleetapi.Env) daemon.SpawnFunc {
		return func() error {
			selfSpawns.Add(1)
			fmt.Fprintf(os.Stderr, "detached relay start from a test:\n%s\n", debug.Stack())
			return errors.New("test tried to start a detached relay; set Env.Spawn")
		}
	}
	code := m.Run()
	if n := selfSpawns.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d test command(s) tried to start a detached relay; give them Env.Spawn\n", n)
		code = 1
	}
	os.RemoveAll(home)
	os.Exit(code)
}

// cliTestArgs makes this test binary run as the CLI, for a test that has to see a
// whole process (runCLIForTest).
const cliTestArgs = "KENAZ_TEST_CLI_ARGS"
