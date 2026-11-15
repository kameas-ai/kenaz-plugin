package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/hook"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// envGateSkip are the commands the env gate leaves alone: the hook must
// never fail a harness session, and help-like commands select no env.
var envGateSkip = map[string]bool{"hook": true, "licenses": true, "version": true}

// retiredOnly are the commands that use no Fleet environment of their own
// but are still stopped by a retired one. `context` keeps the local
// context library, which needs no environment, working whatever
// environment is selected (the staff dev build with prod selected reads
// its local files as before); with a retired environment selected the
// device is misconfigured, and context says so like every other command
// instead of going on to sync or grant with it. (`migrate` is not here: it
// goes on, and says that it ignores the selection; see RunEnv.)
var retiredOnly = map[string]bool{"context": true}

// refuseEnv stops a command, with a usage error, when the Fleet
// environment it would use (its --env flag, the env variables or
// config.toml) is one this build does not connect to (the staff dev build
// refuses prod) or one that has been retired (every build refuses stage,
// except to remove what was set up for it: retiredEnvCleanup). Unknown env
// names are left to the command to report.
//
// It runs before anything else the command does, and changes nothing: it
// reads config.toml where it is now (pathsInPlace), so a refused `init`
// never moves an earlier release's files first.
func (e Env) refuseEnv(cmd string, args []string) (int, bool) {
	err := e.envRefusal(cmd, args)
	if err == nil {
		return 0, false
	}
	fmt.Fprintln(e.Stderr, err.Error())
	return ExitUsage, true
}

// envRefusal is why refuseEnv stops the command, or nil.
func (e Env) envRefusal(cmd string, args []string) error {
	if envGateSkip[cmd] || cmd == "migrate" {
		return nil
	}
	err := e.envSelectionErr(cmd, args)
	if retiredOnly[cmd] && !errors.Is(err, config.ErrEnvRetired) {
		return nil
	}
	return err
}

// envSelectionErr is what is wrong with the Fleet environment selected for
// cmd: one this build does not connect to, or a retired one. nil when it is
// usable, unknown (the command reports that) or cannot be read.
func (e Env) envSelectionErr(cmd string, args []string) error {
	paths, err := e.pathsInPlace()
	if err != nil {
		return nil
	}
	file, err := config.LoadFile(paths.ConfigFile())
	if err != nil {
		return nil
	}
	flagEnv, exact := commandEnvFlag(cmd, args)
	_, _, err = e.resolveEnv(flagEnv, file)
	if err == nil && !exact && !e.cleanup {
		// The scan above reads args the way most commands do, without
		// their flag sets. Whatever a command's own parsing makes of them,
		// a retired environment given to --env anywhere is refused here.
		for _, v := range envFlagValues(args) {
			if _, _, rerr := config.ResolveEnv(v, func(string) string { return "" }, config.File{}); errors.Is(rerr, config.ErrEnvRetired) {
				err = rerr
				break
			}
		}
	}
	if !errors.Is(err, config.ErrEnvNotAllowed) && !errors.Is(err, config.ErrEnvRetired) {
		return nil
	}
	return err
}

// migrateIgnoresRetired is what `kenaz migrate` says when a retired
// environment is selected. Migrate moves the files of every environment
// and connects to none, so the selection has no bearing on it and is no
// reason to refuse the move; it says so instead of going on silently.
func (e Env) migrateIgnoresRetired(args []string) string {
	err := e.envSelectionErr("migrate", args)
	if !errors.Is(err, config.ErrEnvRetired) {
		return ""
	}
	return fmt.Sprintf("%s: %s `%s migrate` uses no Fleet environment and moves the files of all of them, so it goes on.", variant.Current.Binary, err.Error(), variant.Current.Binary)
}

// hostStarted reports the commands an app starts on its own and keeps
// starting with the options it stored: the MCP server and the memory
// capture worker (which a hook starts with the hook's environment). A
// person cannot read their usage error, so for a retired environment they
// run signed out instead (retiredEnvNotice) and never contact Fleet.
func hostStarted(cmd string, args []string) bool {
	switch cmd {
	case "mcp":
		return true
	case "memory":
		return len(args) >= 2 && args[0] == "capture" && args[1] == "worker"
	}
	return false
}

// retiredEnvNotice is what a host-started command says, in place of every
// Fleet answer, when the app that started it is still set up for a retired
// environment: what is wrong and what to change to fix it. It is the error
// of a provider that never contacts Fleet (conflictProvider).
type retiredEnvNotice struct {
	env fleetapi.Env
	fix string
}

func (n *retiredEnvNotice) Error() string {
	return fmt.Sprintf("This app is still set up for the %s environment of Kameas Fleet, which has been retired, so Kameas Fleet is not available here. %s", n.env, n.fix)
}

func (n *retiredEnvNotice) Unwrap() error { return config.ErrEnvRetired }

// pluginOptionVar is the variable Claude Code exports the plugin's
// fleet_env option in (variant.Shared.EnvVars): the one of them that is
// neither Kenaz's own variable nor its original name.
func pluginOptionVar() string {
	for _, name := range variant.Shared.EnvVars {
		if strings.HasPrefix(name, "CLAUDE_PLUGIN_OPTION_") {
			return name
		}
	}
	return ""
}

// retiredNotice builds the notice for err, the gate's refusal of a retired
// environment. The fix names what actually selects the environment, since
// each source is changed in a different place and none of them is changed
// by another's fix:
//
//   - the Kenaz extension's setting in Claude Desktop;
//   - the plugin's option in Claude Code, which Claude Code stores and
//     passes on (as the plugin option variable, and as KENAZ_ENV through
//     the plugin's .mcp.json). Kenaz never writes it, so the fix is to
//     set the option, with the command Claude Code itself names for it
//     (`claude plugin configure <id>` in a terminal only shows the
//     options, unless values are piped to it);
//   - an env variable in the environment the app starts in (the only way
//     Codex, which passes no option, selects one besides config.toml);
//   - the env line in config.toml;
//   - --env on the command the app starts Kenaz with, which setup wrote:
//     undoing that setup, then init.
func (e Env) retiredNotice(err error) *retiredEnvNotice {
	env, src, _ := config.RetiredEnv(err)
	bin, id := variant.Current.Binary, variant.Current
	h := hook.DetectHarness(e.getenv)
	app := "this app"
	switch {
	case mcp.IsClaudeDesktop(e.getenv):
		app = "Claude Desktop"
	case h == hook.HarnessCodex:
		app = "Codex"
	case h == hook.HarnessClaude:
		app = "Claude Code"
	}
	option := pluginOptionVar()
	optionRetired := option != "" && variant.Retired(strings.ToLower(strings.TrimSpace(e.getenv(option))))
	var fix string
	switch {
	case src == config.SourceEnvVar && mcp.IsClaudeDesktop(e.getenv):
		fix = fmt.Sprintf("To fix it, set the Kameas Fleet environment to %s in the Kenaz extension's settings in Claude Desktop, then restart Claude Desktop.", id.EnvChoices(" or "))
	case src == config.SourceEnvVar && h != hook.HarnessCodex && (optionRetired || h == hook.HarnessClaude):
		fix = fmt.Sprintf("To fix it, set the %s plugin's Kameas Fleet environment option to %s in Claude Code: run `/plugin configure %s` there. Then restart Claude Code.", id.DisplayName, id.DefaultEnv, id.PluginID())
	case src == config.SourceEnvVar:
		name := config.SelectingEnvVar(e.getenv)
		fix = fmt.Sprintf("To fix it, remove %s=%s from the environment %s starts in (for example your shell profile), or set it to %s, then restart %s.", name, env, app, id.EnvChoices(" or "), app)
		if h == hook.HarnessUnknown {
			// Which app this is can't be told. Claude Code sets the variable
			// itself, from the plugin's option.
			fix += fmt.Sprintf(" If this app is Claude Code, the value comes from the %s plugin's Kameas Fleet environment option: change it with `/plugin configure %s` there.", id.DisplayName, id.PluginID())
		}
	case src == config.SourceFile:
		file := "config.toml"
		if paths, perr := e.pathsInPlace(); perr == nil {
			file = paths.ConfigFile()
		}
		fix = fmt.Sprintf("To fix it, remove the line `env = \"%s\"` from %s, then restart %s.", env, file, app)
	default:
		// --env on the command line the app runs, which setup wrote.
		undo := func(host string) string { return fmt.Sprintf("`%s setup --undo %s --env %s`", bin, host, env) }
		then := fmt.Sprintf(", then `%s init`, and restart %s.", bin, app)
		switch h {
		case hook.HarnessCodex:
			fix = "To fix it, run " + undo("codex") + then
		case hook.HarnessClaude:
			fix = "To fix it, run " + undo("claude") + then
		default:
			fix = "To fix it, run " + undo("claude") + " (in Codex: " + undo("codex") + ")" + then
		}
	}
	return &retiredEnvNotice{env: env, fix: fix}
}

// resolveEnv applies the env precedence. A cleanup command (Env.cleanup)
// may also name a retired environment.
func (e Env) resolveEnv(flagEnv string, file config.File) (fleetapi.Env, config.Source, error) {
	if e.cleanup {
		return config.ResolveEnvForCleanup(flagEnv, e.getenv, file)
	}
	return config.ResolveEnv(flagEnv, e.getenv, file)
}

// retiredEnvCleanup reports whether the command only removes what an
// earlier setup or sign-in left on this device: `logout` and `setup
// --undo <host>`. Those still take a retired environment, so a person can
// remove an old registration for one; nothing else does. Setup itself says
// whether its arguments are an undo, so the two can't disagree.
func retiredEnvCleanup(cmd string, args []string) bool {
	switch cmd {
	case "logout":
		return true
	case "setup":
		return setup.UndoRequested(args)
	}
	return false
}

// commandEnvFlag is the --env value cmd will read from args. For setup and
// init it comes from the command's own flag parsing (exact), since they
// act before they resolve the environment: setup takes flags after `--`
// and its positional arguments, and both have flags whose value may look
// like another --env. For mcp it does too: mcp serves for a retired
// environment instead of being refused, so the gate and the command must
// read the same value. For every other command it is the last --env before
// a `--` (envFlag); those resolve the environment before they do anything.
func commandEnvFlag(cmd string, args []string) (env string, exact bool) {
	switch cmd {
	case "setup":
		if v, ok := setup.EnvRequested(args); ok {
			return v, true
		}
	case "init":
		if v, ok := initEnvFlag(args); ok {
			return v, true
		}
	case "mcp":
		if v, ok := mcpEnvFlag(args); ok {
			return v, true
		}
	}
	return envFlag(args), false
}

// envFlagValues is every value args could give an --env (or -env) flag:
// each `--env=v`, and the argument after each `--env`, wherever it is.
func envFlagValues(args []string) []string {
	var out []string
	for i, a := range args {
		name, val, hasVal := strings.Cut(a, "=")
		if name != "-env" && name != "--env" {
			continue
		}
		switch {
		case hasVal:
			out = append(out, val)
		case i+1 < len(args):
			out = append(out, args[i+1])
		}
	}
	return out
}

// envFlag returns the value of the last --env (or -env) flag in args.
func envFlag(args []string) string {
	v := ""
	for i, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "env" {
			continue
		}
		switch {
		case hasVal:
			v = val
		case i+1 < len(args):
			v = args[i+1]
		}
	}
	return v
}
