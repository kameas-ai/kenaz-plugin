package app

import (
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
)

// runSetup is `kenaz setup`. State paths come from the
// config resolver so setup reads the same daemon.json the daemon
// writes. If the running daemon reports that Fleet refused this org
// (early access), setup says so on stderr; the local
// setup result and exit code are unchanged.
func runSetup(env Env, args []string) int {
	daemonDir := ""
	code := setup.CommandWith(env.Ctx, args, env.Stdout, env.Stderr, func(e string) (string, error) {
		r, err := env.resolve(e)
		if err != nil {
			return "", err
		}
		daemonDir = r.StateDir
		return r.StateDir, nil
	})
	if daemonDir != "" {
		if c := connectDaemon(env.Ctx, daemonDir); c != nil {
			if st, err := c.Status(env.Ctx); err == nil && st.Enroll == string(enroll.StateEarlyAccess) {
				fmt.Fprintln(env.Stderr, "kenaz: "+fleetapi.EarlyAccessMessage)
			}
		}
	}
	if code == ExitOK {
		switch setupAction(args) {
		case "apply":
			if mp, err := env.memoryPaths(); err == nil {
				if d := mp.discloseOnce(env.now()); d != "" {
					fmt.Fprintln(env.Stdout, d)
				}
			}
		case "undo":
			if l := env.keptMemoryLine(); l != "" {
				fmt.Fprintln(env.Stdout, l)
			}
		}
	}
	return code
}

// setupAction classifies a successful `kenaz setup` invocation: "apply"
// for `setup claude|codex`, "undo" for `setup --undo claude|codex`, and ""
// for dry runs, print-managed and doctor.
func setupAction(args []string) string {
	undo, harness := false, false
	for _, a := range args {
		switch strings.TrimLeft(a, "-") {
		case "undo":
			undo = true
		case "dry-run":
			return ""
		case "print-managed", "doctor":
			return ""
		case "claude", "codex":
			harness = true
		}
	}
	switch {
	case !harness:
		return ""
	case undo:
		return "undo"
	default:
		return "apply"
	}
}
