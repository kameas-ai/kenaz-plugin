package app

import (
	"fmt"
	"path/filepath"
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
// (its plan does not include the plugin), setup says so on stderr; the local
// setup result and exit code are unchanged.
func runSetup(env Env, args []string) int {
	return runSetupWith(env, args, setup.Hooks{})
}

// runSetupWith is runSetup with setup seams (kenaz init shares its input
// reader). Setup starts the relay itself when it is not running,
// the same way login does.
func runSetupWith(env Env, args []string, hk setup.Hooks) int {
	daemonDir, oldRecord := "", false
	var res resolved
	hk.Resolve = func(e string) (string, error) {
		r, err := env.resolve(e)
		if err != nil {
			return "", err
		}
		if dir := env.retiredUndoState(r, args); dir != r.StateDir {
			r.StateDir, oldRecord = dir, true
		}
		daemonDir, res = r.StateDir, r
		return r.StateDir, nil
	}
	hk.OldRecordRoot = func() string {
		if !oldRecord {
			return ""
		}
		return res.Paths.StateRoot
	}
	hk.StartRelay = func(string) error {
		if err := env.cred.usable(); err != nil {
			return err
		}
		return res.spawner(env)()
	}
	// The signed release this binary was built from: setup adopts a GitHub
	// marketplace only when it is pinned to this commit, and print-managed
	// pins the plugin to it.
	hk.Release = setup.Release{Version: Version, Commit: BuildCommit}
	code := setup.CommandHooks(env.Ctx, args, env.Stdout, env.Stderr, hk)
	if daemonDir != "" {
		if c := connectDaemon(env.Ctx, daemonDir); c != nil {
			if st, err := c.Status(env.Ctx); err == nil && st.Enroll == string(enroll.StatePlanRequired) {
				fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+fleetapi.PlanRequiredMessage)
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

// retiredUndoState is the state directory `setup --undo <host>` reads for
// a retired environment: r's, unless the host's setup is recorded only in
// the same environment's folder of an earlier release's state root that
// Kenaz no longer uses (config.Paths.Unused). Nothing sets an app up for a
// retired environment again, so such a record is never replaced by one in
// the root; `kenaz migrate` names this undo as what frees that folder
// (registrationsOf), and here it finds the record it is about. The undo
// then leaves alone whatever may be the current setup's
// (setup.Options.OldRecord). Every other setup command, and every
// environment that still exists, keeps to r's.
func (e Env) retiredUndoState(r resolved, args []string) string {
	host, undo := setup.UndoTarget(args)
	if !undo || !variant.Retired(string(r.Env)) || setup.Recorded(r.StateDir, host) {
		return r.StateDir
	}
	layout, err := config.Layout(e.getenv)
	if err != nil {
		return r.StateDir
	}
	for _, old := range layout.Unused() {
		if dir := filepath.Join(old, string(r.Env)); setup.Recorded(dir, host) {
			return dir
		}
	}
	return r.StateDir
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
// stderr and names the fix, `kenaz init`. It reads the setup state of the
// Fleet environment the command runs in (its --env flag in args, else the
// default) and never writes: these commands use the files where they are
// (Env.paths moves them only for init and migrate).
func (e Env) warnBrokenRegistrations(args []string) {
	paths, err := e.paths()
	if err != nil {
		return
	}
	file, err := config.LoadFile(paths.ConfigFile())
	if err != nil {
		return
	}
	env, _, err := config.ResolveEnv(envFlag(args), e.getenv, file)
	if err != nil {
		return
	}
	for _, line := range setup.BrokenRegistrations(paths.State(env)) {
		fmt.Fprintln(e.Stderr, variant.Current.Binary+": "+line)
	}
}

// devVersion is Version in a build made without a release version (`go
// build`, `make build`).
const devVersion = "0.0.0-dev"

// warnStalePluginFiles is `kenaz mcp` saying, once and on stderr only (stdout
// is the MCP protocol), that the plugin files the apps were registered with
// are from another version than this kenaz: after an upgrade the apps run
// the new kenaz with the files copied at the last `kenaz init`. It reads the
// setup state in stateDir and never writes. A build without a release
// version has no plugin files of its own to refresh to and says nothing.
func (e Env) warnStalePluginFiles(stateDir string) {
	if Version == devVersion || e.Stderr == nil {
		return
	}
	if line := setup.StalePluginFiles(stateDir, Version); line != "" {
		fmt.Fprintln(e.Stderr, variant.Current.Binary+": "+line)
	}
}
