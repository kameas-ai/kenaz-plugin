package app

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// layoutState is the once-per-process decision of where the files are
// (RunEnv sets it; see paths).
type layoutState struct {
	cmd   string
	once  sync.Once
	paths config.Paths
	err   error
	moved *config.MigrateResult // set when the command moved files
}

// migrateCommands move an earlier release's files into ~/.kenaz/plugin
// before they run: `migrate` itself, and `init`, which registers the apps
// again afterwards (so it may move where no link can be left). Every other
// command uses the files where they are (config.Paths.Current).
var migrateCommands = map[string]bool{"init": true, "migrate": true}

// paths is the on-disk layout (~/.kenaz/plugin, or $KENAZ_HOME/plugin, or an
// earlier release's files where they still are). It is decided once per
// process.
func (e Env) paths() (config.Paths, error) {
	if e.layout == nil {
		return e.resolvePaths("")
	}
	e.layout.once.Do(func() { e.layout.paths, e.layout.err = e.resolvePaths(e.layout.cmd) })
	return e.layout.paths, e.layout.err
}

func (e Env) resolvePaths(cmd string) (config.Paths, error) {
	p, err := config.Layout(e.getenv)
	if err != nil {
		return p, err
	}
	if migrateCommands[cmd] {
		res := p.Migrate(config.MigrateOptions{Lock: daemon.TryLock, Relink: cmd == "init", Registered: registeredThrough(p)})
		e.layout.moved = &res
		if cmd == "init" && e.Stderr != nil {
			reportMigration(e.Stderr, res, p.Current(), cmd)
		}
	}
	return p.Current(), nil
}

// reportMigration prints what Migrate did.
func reportMigration(w io.Writer, res config.MigrateResult, cur config.Paths, cmd string) {
	bin := variant.Current.Binary
	if len(res.Moved) > 0 {
		fmt.Fprintf(w, "%s: moved its files to %s (from %s).\n", bin, cur.StateRoot, strings.Join(res.Moved, ", "))
	}
	if len(res.Linked) > 0 {
		fmt.Fprintf(w, "%s: left a link to %s at %s, so apps registered there keep working.\n", bin, cur.StateRoot, strings.Join(res.Linked, ", "))
	}
	if res.LinkFailed != "" {
		if cmd == "init" {
			fmt.Fprintf(w, "%s: %s; it registers the apps from %s now instead.\n", bin, res.LinkFailed, cur.StateRoot)
		} else {
			fmt.Fprintf(w, "%s: %s. Run `%s init` so every app loads the plugin from %s.\n", bin, res.LinkFailed, bin, cur.StateRoot)
		}
	}
	if len(res.Unused) > 0 {
		fmt.Fprintf(w, "%s: %s already has its files, so it no longer uses these older ones:\n", bin, cur.StateRoot)
		for _, u := range res.Unused {
			fmt.Fprintf(w, "  - %s\n", unusedAdvice(u, cur, cmd == "init"))
		}
	}
	if res.Refused != "" {
		fmt.Fprintf(w, "%s: not moving its files yet: %s. Everything keeps working from where it is.\n", bin, res.Refused)
	}
}

// runMigrate implements `kenaz migrate`: the move itself happened in
// paths (RunEnv resolves the layout first); this reports it.
func runMigrate(env Env, args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	cur, err := env.paths()
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	res := config.MigrateResult{}
	if env.layout != nil && env.layout.moved != nil {
		res = *env.layout.moved
	}
	if res.Nothing() {
		fmt.Fprintf(env.Stdout, "Nothing to move: Kenaz keeps its files in %s.\n", cur.StateRoot)
		return ExitOK
	}
	reportMigration(env.Stdout, res, cur, "migrate")
	if res.Refused != "" {
		return ExitError
	}
	if len(res.Moved) > 0 {
		fmt.Fprintf(env.Stdout, "Run `%s init` so every app loads the plugin from the new location.\n", variant.Current.Binary)
	}
	return ExitOK
}

// unusedAdvice says what to do with u, an earlier release's file or folder
// Kenaz no longer uses (config.Paths.Unused, MigrateResult.Unused).
// initing is true while `kenaz init` reports it, before it registers the
// apps again.
func unusedAdvice(u string, cur config.Paths, initing bool) string {
	bin := variant.Current.Binary
	if fi, err := os.Stat(u); err != nil || !fi.IsDir() || strings.Contains(filepath.Base(u), ".unused-") {
		return u + " is no longer used; delete it when you no longer need it"
	}
	still, reRegistered := registrationsOf(u, cur)
	switch {
	case initing:
		return fmt.Sprintf("%s is from an earlier release and holds its memories and sign-in data; once this init has registered the apps again, delete it if you no longer need them", u)
	case still:
		return fmt.Sprintf("%s is from an earlier release and an app still loads the plugin from it, so do not delete it: run `%s init` first. It also holds that release's memories and sign-in data", u, bin)
	case reRegistered:
		return fmt.Sprintf("%s is from an earlier release and no longer used; it holds that release's memories and sign-in data, so delete it only if you no longer need them", u)
	}
	return fmt.Sprintf("%s is from an earlier release and holds its memories and sign-in data; run `%s init` first, then delete it if you no longer need them", u, bin)
}

// registrationsOf reports whether a recorded Claude Code or Codex
// registration still loads the plugin from inside old, an earlier release's
// state root (still), and whether the apps were registered from outside it
// since (reRegistered: `kenaz init` from the new root). A record in old's
// own setup state counts only while the root's record for the same
// environment and app does not register it from elsewhere: init rewrites
// the root's record, never old's.
func registrationsOf(old string, cur config.Paths) (still, reRegistered bool) {
	for _, e := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		now := setup.Registrations(cur.State(e))
		for _, r := range now {
			if setup.Within(old, r) {
				still = true
			} else {
				reRegistered = true
			}
		}
		for host, r := range setup.Registrations(filepath.Join(old, string(e))) {
			if n, ok := now[host]; setup.Within(old, r) && (!ok || setup.Within(old, n)) {
				still = true
			}
		}
	}
	return still, reRegistered
}

// registeredThrough is MigrateOptions.Registered: whether a registration
// recorded in the root (after the move, the records that moved with it)
// still loads the plugin through src, the old state location.
func registeredThrough(root config.Paths) func(src string) bool {
	return func(src string) bool {
		for _, e := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
			for _, r := range setup.Registrations(root.State(e)) {
				if setup.Within(src, r) {
					return true
				}
			}
		}
		return false
	}
}
