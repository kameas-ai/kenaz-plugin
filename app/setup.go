package app

import (
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// runSetup is `kenaz setup`. State paths come from the
// config resolver so setup reads the same daemon.json the daemon
// writes. If the running daemon reports that Fleet refused this org
// (early access), setup says so on stderr; the local
// setup result and exit code are unchanged.
func runSetup(env Env, args []string) int {
	return runSetupWith(env, args, setup.Hooks{})
}

// runSetupWith is runSetup with setup seams (kenaz init shares its input
// reader). Setup starts the relay itself when it is not running,
// the same way login does.
func runSetupWith(env Env, args []string, hk setup.Hooks) int {
	daemonDir := ""
	var res resolved
	hk.Resolve = func(e string) (string, error) {
		r, err := env.resolve(e)
		if err != nil {
			return "", err
		}
		daemonDir, res = r.StateDir, r
		return r.StateDir, nil
	}
	hk.StartRelay = func(string) error {
		if err := env.cred.usable(); err != nil {
			return err
		}
		return res.spawner(env)()
	}
	code := setup.CommandHooks(env.Ctx, args, env.Stdout, env.Stderr, hk)
	if daemonDir != "" {
		if c := connectDaemon(env.Ctx, daemonDir); c != nil {
			if st, err := c.Status(env.Ctx); err == nil && st.Enroll == string(enroll.StateEarlyAccess) {
				fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+fleetapi.EarlyAccessMessage)
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
// for `setup claude|codex|opencode`, "undo" for `setup --undo ...`, and ""
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
		case "opencode":
			harness = features.OpenCode
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

// registrationCheckCommands start with a check of the plugin
// registrations setup made (warnBrokenRegistrations). Not hook, mcp or
// daemon (the hosts' hot path), not init or setup (they re-register), and
// not version or licenses (scripted).
var registrationCheckCommands = map[string]bool{
	"status": true, "doctor": true, "login": true, "skills": true,
	"memory": true, "approvals": true, "device": true, "optin": true,
}

// warnBrokenRegistrations is the startup self-check: when a Claude Code or
// Codex plugin registration launches a kenaz that no longer exists (a
// version `brew upgrade` removed, registered before 0.6.0), it says so on
// stderr and names the fix, `kenaz init`. It reads the default
// environment's setup state only and never writes.
func (e Env) warnBrokenRegistrations() {
	paths, err := e.paths()
	if err != nil {
		return
	}
	file, err := config.LoadFile(paths.ConfigFile())
	if err != nil {
		return
	}
	env, _, err := config.ResolveEnv("", e.getenv, file)
	if err != nil {
		return
	}
	for _, line := range setup.BrokenRegistrations(paths.State(env)) {
		fmt.Fprintln(e.Stderr, variant.Current.Binary+": "+line)
	}
}
