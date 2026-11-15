// Package setup points Claude Code, Codex and OpenCode at the local relay:
// it merges the relay's OTEL settings (Claude Code, Codex) or the Kenaz
// adapter and MCP registration (OpenCode) into the user's own harness
// config with a diff and confirmation, records exactly what it changed so
// `--undo` can restore it, and prints the managed-configuration snippets
// admins deploy fleet-wide.
//
// Setup owns a closed set of keys and nothing else. It never writes
// permission, hook, tool allow/deny or rules configuration, and writes MCP
// configuration only for OpenCode, which has no plugin manifest that could
// carry the kenaz server; `make check-no-policy` enforces that on this
// package's sources.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/apps"
	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

type harness int

const (
	harnessClaude harness = iota
	harnessCodex
	harnessOpenCode
)

func (h harness) label() string {
	switch h {
	case harnessClaude:
		return "Claude Code"
	case harnessOpenCode:
		return "OpenCode"
	}
	return "Codex"
}

func parseHarness(s string) (harness, bool) {
	switch s {
	case "claude", "claude-code":
		return harnessClaude, true
	case "codex":
		return harnessCodex, true
	case "opencode":
		if features.OpenCode {
			return harnessOpenCode, true
		}
	}
	return 0, false
}

// Options configures one setup invocation. Paths are resolved by
// DefaultOptions; tests fill them with temp directories.
type Options struct {
	ClaudeDir string // ~/.claude, or $CLAUDE_CONFIG_DIR
	CodexDir  string // ~/.codex, or $CODEX_HOME
	StateDir  string // relay $STATE for the selected environment
	// OldRecord says StateDir is an earlier release's folder that is no
	// longer used, read only so that --undo can clear a record left there.
	// Each host has one plugin registration and one settings file for
	// every environment and build, so what such a record set up may since
	// have become the current setup's, and a later setup that found
	// nothing to change left no record to say so. The undo therefore never
	// restores or strips the host's settings from such a record, and runs
	// the host CLI only to remove a plugin the host still loads from that
	// folder (see Undo). Settings the earlier setup wrote may stay behind;
	// that is the accepted cost of never removing a current setup's.
	OldRecord bool

	// OpenCode: its global config directory, the kenaz binary and adapter
	// directory rendered into opencode.json (absolute paths), the Fleet env
	// passed to the MCP server ("" = the server's default selection), and
	// an optional probe for the installed OpenCode version.
	OpenCodeDir     string
	KenazBin        string
	AdapterDir      string
	FleetEnv        string
	OpenCodeVersion func() (string, bool)

	// BundleRoot is a verified self-contained release archive to register
	// (see bundle.go); "" for a PATH-based install. HostRun runs host CLIs
	// (`claude`, `codex`) for bundle registration; nil uses exec.
	BundleRoot string
	HostRun    Runner
	// BundleExplicit is true when --bundle named the bundle: it then
	// replaces the public GitHub marketplace instead of adopting it.
	BundleExplicit bool
	// Release is the signed release this kenaz was built from. A GitHub
	// marketplace is adopted only when pinned to Release.Commit.
	Release Release
	// HostBins maps "claude" and "codex" to the executable to run when the
	// name is not on PATH ("" or absent: run the name).
	HostBins map[string]string
	// KenazOnPath reports whether `kenaz` resolves on PATH (the GitHub
	// plugin launches it from there); nil skips the check.
	KenazOnPath func() bool
	// StartRelay starts the relay for this environment when it is not
	// running (nil: setup asks the user to start it).
	StartRelay func() error
	// RelayWait bounds how long setup waits for a relay it started.
	RelayWait time.Duration

	// System-wide managed files, read (never written) by doctor.
	ClaudeManagedPath string
	CodexManagedPath  string

	Now         func() time.Time
	In          io.Reader
	Out, Err    io.Writer
	Interactive bool // stdin is a terminal

	Yes, DryRun, Traces bool
}

// StateDirResolver maps a --env value ("" = default selection) to the
// relay's $STATE directory. The app sets it to the config resolver
// so setup, hook and daemon agree on daemon.json's location; when nil,
// the daemonstate defaults are used (tests).
type StateDirResolver func(env string) (string, error)

// DefaultOptions resolves paths for the current user and the environment
// selected by --env or KENAZ_ENV.
func DefaultOptions(env string, stdout, stderr io.Writer) (Options, error) {
	return defaultOptions(env, stdout, stderr, nil)
}

func defaultOptions(env string, stdout, stderr io.Writer, resolve StateDirResolver) (Options, error) {
	r, err := daemonstate.System()
	if err != nil {
		return Options{}, err
	}
	if env == "" && resolve == nil {
		if env, err = r.Env(); err != nil {
			return Options{}, err
		}
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(r.Home, ".claude")
	}
	codexDir := os.Getenv("CODEX_HOME")
	if codexDir == "" {
		codexDir = filepath.Join(r.Home, ".codex")
	}
	claudeManaged := "/etc/claude-code/managed-settings.json"
	if r.GOOS == "darwin" {
		claudeManaged = "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	stateDir := r.StateDir(env)
	if resolve != nil {
		d, err := resolve(env)
		if err != nil {
			return Options{}, err
		}
		stateDir = d
	}
	bin, _ := os.Executable()
	if bin != "" {
		if r, err := filepath.EvalSymlinks(bin); err == nil {
			bin = r
		}
		// An installer-managed version registers through install/current,
		// so upgrades, rollbacks and pruning reach the hosts.
		bin = bundle.StablePath(bin)
	}
	probe := apps.System()
	return Options{
		HostBins:          map[string]string{"claude": probe.ClaudeCLI(), "codex": probe.CodexCLI()},
		KenazOnPath:       func() bool { _, err := exec.LookPath(variant.Current.Binary); return err == nil },
		ClaudeDir:         claudeDir,
		CodexDir:          codexDir,
		OpenCodeDir:       openCodeConfigDir(r.Home, os.Getenv),
		KenazBin:          bin,
		AdapterDir:        defaultAdapterDir(bin),
		StateDir:          stateDir,
		ClaudeManagedPath: claudeManaged,
		CodexManagedPath:  "/etc/codex/managed_config.toml",
		Now:               time.Now,
		In:                os.Stdin,
		Out:               stdout,
		Err:               stderr,
		Interactive:       isTerminal(os.Stdin),
	}, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (o Options) path(h harness) string {
	switch h {
	case harnessClaude:
		return filepath.Join(o.ClaudeDir, "settings.json")
	case harnessOpenCode:
		return filepath.Join(o.OpenCodeDir, "opencode.json")
	}
	return filepath.Join(o.CodexDir, "config.toml")
}

func (o Options) snapshotPath(h harness) string {
	name := "claude-settings.json.orig"
	switch h {
	case harnessCodex:
		name = "codex-config.toml.orig"
	case harnessOpenCode:
		name = "opencode-config.json.orig"
	}
	return filepath.Join(o.StateDir, "setup", name)
}

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 64
)

const usageOpenCode = `usage:
  {bin} setup claude|codex [--yes] [--dry-run] [--traces] [--env E] [--bundle DIR | --no-bundle]
  {bin} setup opencode [--yes] [--dry-run] [--env E] [--adapter DIR] [--bundle DIR | --no-bundle]
  {bin} setup --undo claude|codex|opencode [--yes] [--dry-run] [--env E]
  {bin} setup print-managed claude|codex [--port N] [--raw] [--release vX.Y.Z --commit SHA]
  {bin} setup doctor [--env E]

To set up PATH, sign-in and every app at once, run {bin} init.
` + usageMarketplace

const usageBase = `usage:
  {bin} setup claude|codex [--yes] [--dry-run] [--traces] [--env E] [--bundle DIR | --no-bundle]
  {bin} setup --undo claude|codex [--yes] [--dry-run] [--env E]
  {bin} setup print-managed claude|codex [--port N] [--raw] [--release vX.Y.Z --commit SHA]
  {bin} setup doctor [--env E]

To set up PATH, sign-in and every app at once, run {bin} init.
` + usageMarketplace

const usageMarketplace = `
Plugin source (Claude Code, Codex): a downloaded {bin} registers its own
verified plugin by absolute path. A "{marketplace}" marketplace from GitHub
({repo}) is used instead only when it is pinned to this release's
commit; one that follows a branch or tag is replaced, with a warning.
--bundle DIR registers that bundle's plugin and replaces the GitHub
marketplace; --no-bundle never registers a bundle.
Setup starts the Kenaz relay if it is not running.
`

// usageLocalOnly replaces usageMarketplace in a build that has no public
// marketplace.
const usageLocalOnly = `
Plugin source (Claude Code, Codex): {bin} registers the plugin of the
bundle it runs from by absolute path (--bundle DIR: that bundle's).
Setup starts the {bin} relay if it is not running.
`

// Usage is the setup help for this build (OpenCode only when enabled).
func Usage() string {
	u := usageBase
	if features.OpenCode {
		u = usageOpenCode
	}
	if variant.Current.MarketplaceRepo == "" {
		u = strings.Replace(u, usageMarketplace, usageLocalOnly, 1)
	}
	return strings.NewReplacer("{bin}", variant.Current.Binary, "{marketplace}", variant.Current.Marketplace, "{repo}", variant.Current.MarketplaceRepo).Replace(u)
}

// Command runs `kenaz setup ...` and returns the exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return CommandWith(ctx, args, stdout, stderr, nil)
}

// CommandWith is Command with the relay's state-dir resolver.
func CommandWith(ctx context.Context, args []string, stdout, stderr io.Writer, resolve StateDirResolver) int {
	return CommandHooks(ctx, args, stdout, stderr, Hooks{Resolve: resolve})
}

// Hooks are the app's seams into `kenaz setup`.
type Hooks struct {
	Resolve StateDirResolver
	// StartRelay starts the relay for the environment whose state
	// directory is stateDir (see Options.StartRelay).
	StartRelay func(stateDir string) error
	// In replaces stdin for confirmations (kenaz init shares one reader
	// across steps); Interactive then says whether it is a terminal.
	In          io.Reader
	Interactive *bool
	// Result receives Apply's or Undo's error (nil on success), so a
	// caller can tell a declined change from a failure.
	Result func(error)
	// Release is the signed release this kenaz was built from (the app's
	// Version and BuildCommit).
	Release Release
	// OldRecordRoot, asked after Resolve, says the state directory Resolve
	// returned is an earlier release's folder that is no longer used, by
	// returning the state root that is in use ("" otherwise). See
	// Options.OldRecord.
	OldRecordRoot func() string
}

// Diff is the unified diff setup prints before a change.
func Diff(path string, before, after []byte) string {
	return unifiedDiff(path, path+" (after kenaz)", string(before), string(after))
}

// commandFlags are `kenaz setup`'s flags as parsed.
type commandFlags struct {
	yes, dry, traces, undo, raw, noBundle      *bool
	env, pinTag, pinCommit, bundleDir, adapter *string
	port                                       *int
}

// parseCommand parses `kenaz setup`'s arguments: flags may come before,
// between and after the positional ones. ok is false on a flag error,
// which the flag package has reported on stderr.
func parseCommand(args []string, stderr io.Writer) (f commandFlags, pos []string, ok bool) {
	fl := flag.NewFlagSet("setup", flag.ContinueOnError)
	fl.SetOutput(stderr)
	f.yes = fl.Bool("yes", false, "apply without asking")
	f.dry = fl.Bool("dry-run", false, "show the diff and change nothing")
	f.traces = fl.Bool("traces", false, "Claude Code only: also export traces (beta)")
	f.undo = fl.Bool("undo", false, "restore what setup changed")
	f.env = fl.String("env", "", "Kameas Fleet environment ("+variant.Current.EnvChoices(", ")+")")
	f.port = fl.Int("port", variant.Shared.RelayPorts[0], "print-managed: relay port to pin fleet-wide")
	f.raw = fl.Bool("raw", false, "print-managed: print only the file content")
	f.pinTag = fl.String("release", "", "print-managed: release tag to pin the plugin to (default: this kenaz's release)")
	f.pinCommit = fl.String("commit", "", "print-managed: full commit of --release (default: this kenaz's release commit)")
	f.bundleDir = fl.String("bundle", "", "register a self-contained release bundle (default: the bundle this kenaz runs from, if any)")
	f.noBundle = fl.Bool("no-bundle", false, "ignore the bundle this kenaz runs from and use kenaz from PATH")
	f.adapter = new(string)
	if features.OpenCode {
		f.adapter = fl.String("adapter", "", "opencode: the Kenaz OpenCode adapter directory (default: opencode/ next to the binary's bin/)")
	}
	fl.Usage = func() { fmt.Fprint(stderr, Usage()) }

	rest := args
	for {
		if err := fl.Parse(rest); err != nil {
			return f, nil, false
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	return f, pos, true
}

// UndoRequested reports whether args, parsed exactly as the command parses
// them, run the undo of one host's setup (`setup --undo claude`), which
// only removes what an earlier setup left. Everything else, a dry run of an
// apply and the doctor included, is not an undo.
func UndoRequested(args []string) bool {
	_, ok := UndoTarget(args)
	return ok
}

// UndoTarget is UndoRequested together with the host whose setup the
// arguments undo ("claude", "codex", "opencode").
func UndoTarget(args []string) (host string, ok bool) {
	f, pos, ok := parseCommand(args, io.Discard)
	if !ok || !*f.undo || len(pos) != 1 {
		return "", false
	}
	if _, isHost := parseHarness(pos[0]); !isHost {
		return "", false
	}
	return pos[0], true
}

// EnvRequested is the --env value the command reads from args, parsed
// exactly as the command parses them (flags before, between and after the
// positional arguments, and a flag's value never taken for a flag). ok is
// false when the command would reject the arguments as a flag error, which
// it does before it acts.
func EnvRequested(args []string) (env string, ok bool) {
	f, _, ok := parseCommand(args, io.Discard)
	if !ok {
		return "", false
	}
	return *f.env, true
}

// Recorded reports whether the setup state in stateDir holds a record of
// host's setup, whichever build made it. It only reads the file.
func Recorded(stateDir, host string) bool {
	h, ok := parseHarness(host)
	if !ok {
		return false
	}
	b, err := os.ReadFile(filepath.Join(stateDir, stateFile))
	if err != nil {
		return false
	}
	var s State
	return json.Unmarshal(b, &s) == nil && s.get(h) != nil
}

// CommandHooks is Command with every app seam.
func CommandHooks(ctx context.Context, args []string, stdout, stderr io.Writer, hk Hooks) int {
	resolve := hk.Resolve
	f, pos, ok := parseCommand(args, stderr)
	if !ok {
		return exitUsage
	}
	yes, dry, traces, undo, env, port, raw := f.yes, f.dry, f.traces, f.undo, f.env, f.port, f.raw
	pinTag, pinCommit, bundleDir, noBundle, adapter := f.pinTag, f.pinCommit, f.bundleDir, f.noBundle, f.adapter

	if len(pos) == 2 && pos[0] == "print-managed" {
		h, ok := parseHarness(pos[1])
		if !ok {
			fmt.Fprint(stderr, Usage())
			return exitUsage
		}
		if h == harnessOpenCode {
			fmt.Fprintf(stderr, "%[1]s setup: print-managed is not available for OpenCode yet; run `%[1]s setup opencode` for each user\n", variant.Current.Binary)
			return exitUsage
		}
		if variant.Current.MarketplaceRepo == "" {
			// Managed configuration installs the published plugin fleet-wide;
			// this build has none.
			fmt.Fprintf(stderr, "%s setup: print-managed is not available in this build; it is never deployed fleet-wide\n", variant.Current.Binary)
			return exitUsage
		}
		rel, err := managedRelease(hk.Release, *pinTag, *pinCommit)
		if err != nil {
			fmt.Fprintln(stderr, variant.Current.Binary+" setup:", err)
			return exitUsage
		}
		return printManaged(stdout, h, *port, *raw, rel)
	}

	opts, err := defaultOptions(*env, stdout, stderr, resolve)
	if err != nil {
		fmt.Fprintln(stderr, variant.Current.Binary+" setup:", err)
		return exitError
	}
	opts.Yes, opts.DryRun, opts.Traces = *yes, *dry, *traces
	opts.Release = hk.Release
	opts.FleetEnv = *env
	opts.OldRecord = hk.OldRecordRoot != nil && hk.OldRecordRoot() != ""
	if hk.In != nil {
		opts.In = hk.In
	}
	if hk.Interactive != nil {
		opts.Interactive = *hk.Interactive
	}
	if hk.StartRelay != nil {
		dir := opts.StateDir
		opts.StartRelay = func() error { return hk.StartRelay(dir) }
	}
	if *bundleDir != "" && *noBundle {
		fmt.Fprintln(stderr, variant.Current.Binary+" setup: --bundle and --no-bundle cannot be used together")
		return exitUsage
	}
	if *adapter != "" {
		a, err := filepath.Abs(*adapter)
		if err != nil {
			fmt.Fprintln(stderr, variant.Current.Binary+" setup:", err)
			return exitError
		}
		opts.AdapterDir = a
	}
	switch {
	case *bundleDir != "":
		opts.BundleRoot = bundle.StableRoot(*bundleDir)
		opts.BundleExplicit = true
	case !*noBundle:
		opts.BundleRoot = DetectBundle(opts.KenazBin)
	}
	opts.OpenCodeVersion = func() (string, bool) {
		out, err := ExecRunner(ctx, "opencode", "--version")
		return strings.TrimSpace(out), err == nil
	}

	if len(pos) == 1 && pos[0] == "doctor" {
		// Run the same claude and codex setup would (PATH, or the copy a
		// desktop app ships).
		run := func(ctx context.Context, name string, args ...string) (string, error) {
			if p := opts.HostBins[name]; p != "" {
				name = p
			}
			return ExecRunner(ctx, name, args...)
		}
		return printDoctor(stdout, Doctor(ctx, opts, run))
	}
	if len(pos) != 1 {
		fmt.Fprint(stderr, Usage())
		return exitUsage
	}
	h, ok := parseHarness(pos[0])
	if !ok {
		fmt.Fprint(stderr, Usage())
		return exitUsage
	}
	if *undo {
		err = Undo(opts, h)
	} else {
		err = Apply(opts, h)
	}
	if hk.Result != nil {
		hk.Result(err)
	}
	if err != nil {
		fmt.Fprintln(stderr, variant.Current.Binary+" setup:", err)
		return exitError
	}
	return exitOK
}

// ErrDeclined is returned when the user does not confirm, or when stdin is
// not a terminal and --yes was not given.
var ErrDeclined = errors.New("no changes made")

// Apply configures harness h to export to the relay. In bundle mode
// (o.BundleRoot set) it also registers the bundle's plugin (Claude Code,
// Codex) or binary and adapter (OpenCode) by absolute path.
func Apply(o Options, h harness) error {
	bundle, err := o.loadBundle(h)
	if err != nil {
		return err
	}
	var port int
	var oc openCodeConfig
	if h == harnessOpenCode {
		// The adapter finds the relay's port itself (daemon.json), so
		// OpenCode can be set up before the relay has ever run.
		if oc, err = o.openCodePreflight(); err != nil {
			return err
		}
	} else {
		st, err := o.relayState()
		if err != nil {
			return err
		}
		port = st.Port
	}
	state, err := loadState(o.StateDir)
	if err != nil {
		return fmt.Errorf("reading setup state: %w", err)
	}
	path := o.path(h)
	prev := state.get(h)
	if prev != nil && prev.Path != path {
		return fmt.Errorf("setup already configured %s; run `kenaz setup --undo` first", prev.Path)
	}
	cur, exists, err := readFile(path)
	if err != nil {
		return err
	}

	// Claude Code and Codex: which plugin registration to use. The public
	// GitHub marketplace, when the user already registered it, is adopted
	// rather than replaced, unless --bundle asked for this download's copy.
	var cmds [][]string
	var reg *BundleReg
	var adopt *Adopted
	var mk marketplace
	if h != harnessOpenCode {
		mk = o.kenazMarketplace(h)
	}
	pinned, why := false, ""
	if h != harnessOpenCode && mk.official() {
		pinned, why = o.releasePin(h, mk, bundle)
	}
	// recheckCache: setup (re)installs the plugin from a pinned Codex
	// marketplace, and checks afterwards that Codex now runs that copy.
	recheckCache := false
	// A Claude Code "settings" (inline) catalog named kenaz is declared in
	// a settings file, typically managed settings: Claude Code allows one
	// marketplace per name, so setup neither adds a second nor removes it.
	inline := h == harnessClaude && mk.Inline
	switch {
	case inline:
		if pinned {
			fmt.Fprintf(o.Out, "%s declares the Kenaz plugin marketplace inline (managed or user settings), pinned to this release's commit; setup uses it.\n", h.label())
		} else {
			detail := "is not recognizably this release's pinned plugin"
			if mk.official() {
				detail = why
			}
			fmt.Fprintf(o.Err, "warning: %s declares the Kenaz plugin marketplace inline (managed or user settings), and it %s. Setup does not replace it or add a second %q marketplace, and configures telemetry only. To pin it, deploy the extraKnownMarketplaces entry from this release's `%s setup print-managed claude`.\n", h.label(), detail, bundleMarketplace, variant.Current.Binary)
		}
		bundle = nil
	case bundle != nil && h != harnessOpenCode && mk.official() && !o.BundleExplicit:
		if pinned {
			fmt.Fprintf(o.Out, "%s already has the Kenaz plugin marketplace from GitHub (%s), pinned to this release's commit; setup uses it instead of registering this download's copy. Pass --bundle to replace it.\n", h.label(), OfficialRepo)
			bundle = nil
		} else {
			// A marketplace on a moving ref (a branch, a tag, the default
			// branch), or whose checkout is not the release commit, is not
			// a signed release: register this download's verified copy
			// instead, as --bundle would.
			fmt.Fprintf(o.Err, "warning: the Kenaz plugin marketplace from GitHub (%s) in %s %s; setup replaces it with this download's verified copy.%s\n", OfficialRepo, h.label(), why, pinHint(h, o.Release))
		}
	}
	if bundle == nil && h != harnessOpenCode && mk.official() && !pinned && !inline {
		// No verified copy to fall back on (Homebrew, npm, --no-bundle):
		// keep using it, and say how to pin it.
		fmt.Fprintf(o.Err, "warning: the Kenaz plugin marketplace from GitHub (%s) in %s %s, so it can change without a release. %s\n", OfficialRepo, h.label(), why, noBundleHint(h, o.Release))
	}
	switch {
	case bundle == nil && inline && !pinned:
		// Not ours to change: no host command, telemetry keys only.
		adopt = &Adopted{Source: "settings"}
	case bundle == nil && h != harnessOpenCode && mk.official():
		adopt = &Adopted{Source: mk.Remote}
		if prev != nil && prev.Adopted != nil {
			adopt.InstalledPlugin = prev.Adopted.InstalledPlugin
		}
		switch {
		case !mk.Installed:
			cmds = [][]string{installCmd(h)}
			adopt.InstalledPlugin = true
		case mk.Off:
			// Installed by the user and turned off: their choice. Setup
			// neither re-enables it nor, on --undo, uninstalls it.
			fmt.Fprintf(o.Err, "warning: the Kenaz plugin (%s) is installed in %s but turned off, so it will not run. %s\n", bundlePluginID, h.label(), enableHint(h))
		case h == harnessCodex && pinned:
			// Codex runs the copy it made when the plugin was installed,
			// not the marketplace clone, and keeps that copy when the
			// marketplace is removed and added again with --ref.
			if why := o.codexCacheStale(); why != "" {
				fmt.Fprintf(o.Out, "The marketplace is pinned to this release, but the copy of the Kenaz plugin that Codex runs %s. Setup reinstalls the plugin from the pinned marketplace.\n", why)
				cmds = [][]string{uninstallCmd(h), installCmd(h)}
			}
		}
		recheckCache = h == harnessCodex && pinned && len(cmds) > 0
		o.warnNotOnPath(h)
	}
	if bundle != nil && h != harnessOpenCode {
		rendered := o.renderDir(bundle)
		digest, err := renderPlugin(bundle, rendered, false)
		if err != nil {
			return err
		}
		var prevReg *BundleReg
		if prev != nil {
			prevReg = prev.Bundle
		}
		if cmds, err = o.planRegistration(h, mk, rendered, digest, prevReg); err != nil {
			return err
		}
		reg = &BundleReg{Root: bundle.Root, Version: bundle.Version, Rendered: rendered, Digest: digest, Launcher: bundle.Launch}
		o.warnVersionedLaunch(bundle)
		switch {
		case mk.official():
			reg.Replaced = mk.Remote
		case prevReg != nil:
			reg.Replaced = prevReg.Replaced
		}
	}

	next, fs, changed, err := o.applyKeys(h, cur, port, oc, prev)
	if err != nil {
		return err
	}
	if !changed && len(cmds) == 0 {
		fmt.Fprintf(o.Out, "%s is already configured for the Kenaz relay (%s).\n", h.label(), path)
		return nil
	}
	if len(cmds) > 0 {
		if reg != nil {
			fmt.Fprintf(o.Out, "Setup will register the Kenaz plugin from %s:\n", bundle.Root)
		} else {
			fmt.Fprintf(o.Out, "Setup will install the Kenaz plugin from the %q marketplace you already registered:\n", bundleMarketplace)
		}
		for _, c := range cmds {
			fmt.Fprintf(o.Out, "  %s %s\n", hostCLI(h), strings.Join(c, " "))
		}
	}
	if err := o.confirm(path, cur, next); err != nil {
		return err
	}
	if o.DryRun {
		return nil
	}

	if fs == nil {
		fs = &FileState{Keys: map[string]KeyState{}}
	}
	fs.Path = path
	if prev == nil {
		fs.OriginalAbsent = !exists
		if exists {
			snap := o.snapshotPath(h)
			if err := os.MkdirAll(filepath.Dir(snap), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(snap, cur, 0o600); err != nil {
				return err
			}
			fs.Snapshot = snap
		}
	}
	if exists {
		if _, err := backup(path, o.Now()); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if len(cmds) > 0 {
		if reg != nil {
			if _, err := renderPlugin(bundle, reg.Rendered, true); err != nil {
				return err
			}
		}
		if err := o.runAll(h, cmds); err != nil {
			return err
		}
		if recheckCache {
			if why := o.codexCacheStale(); why != "" {
				fmt.Fprintf(o.Err, "warning: after installing the Kenaz plugin from the pinned marketplace, the copy that Codex runs still %s. Run `codex plugin remove %s`, then re-run setup.\n", why, bundlePluginID)
			}
		}
		// The host CLI edited the same file: apply the owned keys to what
		// it wrote.
		if cur, _, err = readFile(path); err != nil {
			return err
		}
		if next, fs2, _, err := o.applyKeys(h, cur, port, oc, fs); err != nil {
			return err
		} else {
			fs2.Path, fs2.OriginalAbsent, fs2.Snapshot = fs.Path, fs.OriginalAbsent, fs.Snapshot
			fs, changed = fs2, true
			cur = next
		}
	}
	switch {
	case reg != nil:
		fs.Bundle, fs.Adopted = reg, nil
	case adopt != nil:
		fs.Bundle, fs.Adopted = nil, adopt
	}
	final := next
	if len(cmds) > 0 {
		final = cur
	}
	if err := writeAtomic(path, final); err != nil {
		return err
	}
	fs.WrittenSHA256 = sha(final)
	state.put(h, fs)
	if err := saveState(o.StateDir, state); err != nil {
		return fmt.Errorf("saving setup state: %w", err)
	}
	if reg != nil {
		o.pruneRendered(reg.Rendered)
	}
	if h == harnessOpenCode {
		fmt.Fprintf(o.Out, "Configured OpenCode to load the Kenaz adapter and the kenaz MCP server (%s). Restart OpenCode to pick it up.\n", path)
		return nil
	}
	switch {
	case reg != nil:
		fmt.Fprintf(o.Out, "Registered the Kenaz plugin from %s in %s (launching %s).\n", bundle.Root, h.label(), bundle.Launch)
	case adopt != nil:
		fmt.Fprintf(o.Out, "%s uses the Kenaz plugin from GitHub (%s), which launches kenaz from PATH.\n", h.label(), OfficialRepo)
	}
	fmt.Fprintf(o.Out, "Configured %s to send telemetry to the Kenaz relay on 127.0.0.1:%d. Restart running sessions to pick it up.\n", h.label(), port)
	return nil
}

// applyKeys computes the owned-key edit of cur for harness h.
func (o Options) applyKeys(h harness, cur []byte, port int, oc openCodeConfig, prev *FileState) ([]byte, *FileState, bool, error) {
	switch h {
	case harnessClaude:
		return claudeApply(cur, port, o.Traces, prev)
	case harnessOpenCode:
		return openCodeApply(cur, oc, prev)
	}
	return codexApply(cur, port, prev)
}

// loadBundle verifies o.BundleRoot (when set) and, for OpenCode, points the
// rendered paths into it.
func (o *Options) loadBundle(h harness) (*Bundle, error) {
	if o.BundleRoot == "" {
		return nil, nil
	}
	b, err := LoadBundle(o.BundleRoot)
	if err != nil {
		return nil, err
	}
	if h == harnessOpenCode {
		if !b.OpenCode {
			return nil, fmt.Errorf("the bundle at %s has no OpenCode adapter (bundle.json opencode is false)", b.Root)
		}
		if err := checkAdapter(b.AdapterDir()); err != nil {
			return nil, err
		}
		o.KenazBin, o.AdapterDir = b.Launch, b.AdapterDir()
	}
	return b, nil
}

// runAll runs host CLI commands in order. Removing a registration that is
// already gone is not an error.
func (o Options) runAll(h harness, cmds [][]string) error {
	for _, c := range cmds {
		err := o.run(h, c)
		if err != nil && (c[1] == "uninstall" || c[1] == "remove") {
			fmt.Fprintln(o.Err, "warning:", err)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pruneRendered removes rendered plugin copies other than keep, unless a
// host registration still points at them.
func (o Options) pruneRendered(keep string) {
	base := filepath.Dir(keep)
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	state, err := loadState(o.StateDir)
	if err != nil {
		return
	}
	// loadState rebased every recorded path onto this state directory,
	// including one recorded through the link a moved state directory left
	// behind, so plain path comparison sees every copy in use.
	inUse := map[string]bool{keep: true}
	for _, fs := range []*FileState{state.Claude, state.Codex} {
		if fs != nil && fs.Bundle != nil {
			inUse[fs.Bundle.Rendered] = true
		}
	}
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		if e.IsDir() && !inUse[p] {
			_ = os.RemoveAll(p)
		}
	}
}

// Undo restores what Apply changed for harness h.
func Undo(o Options, h harness) error {
	state, err := loadState(o.StateDir)
	if err != nil {
		return fmt.Errorf("reading setup state: %w", err)
	}
	fs := state.get(h)
	if fs == nil {
		fmt.Fprintf(o.Out, "Nothing to undo for %s.\n", h.label())
		return nil
	}
	// A record in an earlier release's folder. What it set up may be the
	// current setup's by now, and removing a current setup by mistake is
	// worse than leaving a stale one, so:
	if o.OldRecord {
		recordOnly := ""
		switch {
		case fs.Bundle == nil || h == harnessOpenCode:
			// It registered no plugin of its own (kenaz from PATH, or a
			// marketplace the person had added), so nothing tells its work
			// from a later setup's: that one finds the same settings, changes
			// nothing and records nothing. Nothing in the host is touched.
			recordOnly = fmt.Sprintf("This earlier setup of %s (recorded in %s) registered no plugin of its own, so the plugin and the settings %s has now may be the current setup's. They were left as they are; to remove the current setup, run `%s setup --undo %s`.",
				h.label(), o.StateDir, h.label(), variant.Current.Binary, hostCLI(h))
			if h == harnessOpenCode {
				recordOnly = fmt.Sprintf("This earlier setup of %s (recorded in %s) may have been replaced by the current one, so its configuration was left as it is; to remove the current setup, run `%s setup --undo opencode`.", h.label(), o.StateDir, variant.Current.Binary)
			}
		case !o.stillOwns(h, fs):
			// The app has been set up again since: its one plugin
			// registration and its settings are the current setup's now.
			recordOnly = fmt.Sprintf("%s has been set up again since this earlier setup (recorded in %s), so its plugin registration and its settings are the current setup's and were not changed.", h.label(), o.StateDir)
		}
		// Otherwise the host still loads the plugin this record registered
		// from the old folder: that registration is the record's own and is
		// removed below. The settings are left there too (keepSettings).
		if recordOnly != "" {
			fmt.Fprintln(o.Out, recordOnly)
			if o.DryRun {
				fmt.Fprintln(o.Out, "(dry run: the record of the earlier setup was kept)")
				return nil
			}
			if err := o.forget(state, h, fs); err != nil {
				return err
			}
			fmt.Fprintln(o.Out, "Removed only the record of the earlier setup.")
			return nil
		}
	}
	cur, exists, err := readFile(fs.Path)
	if err != nil {
		return err
	}
	// Whether the file still holds exactly what setup wrote, decided before
	// any host CLI touches it.
	untouched := exists && sha(cur) == fs.WrittenSHA256

	var pluginErr error
	if fs.Adopted != nil && fs.Adopted.InstalledPlugin && h != harnessOpenCode {
		c := uninstallCmd(h)
		fmt.Fprintf(o.Out, "Setup will uninstall the Kenaz plugin it installed from the %q marketplace (the marketplace itself stays registered):\n  %s %s\n", bundleMarketplace, hostCLI(h), strings.Join(c, " "))
		if o.DryRun {
			fmt.Fprintln(o.Out, "(dry run: nothing run)")
		} else {
			if err := o.confirmRun(); err != nil {
				return err
			}
			if err := o.run(h, c); err != nil {
				// Keep restoring the configuration, then report it.
				pluginErr = fmt.Errorf("the Kenaz plugin may still be installed (%v); remove it with `%s %s`", err, hostCLI(h), strings.Join(c, " "))
				fmt.Fprintln(o.Err, "warning:", pluginErr)
			}
			if cur, exists, err = readFile(fs.Path); err != nil {
				return err
			}
		}
	}
	if fs.Bundle != nil && h != harnessOpenCode {
		cmds := unregisterCmds(h)
		fmt.Fprintf(o.Out, "Setup will remove the Kenaz plugin registered from %s:\n", fs.Bundle.Root)
		for _, c := range cmds {
			fmt.Fprintf(o.Out, "  %s %s\n", hostCLI(h), strings.Join(c, " "))
		}
		if o.DryRun {
			fmt.Fprintln(o.Out, "(dry run: nothing run)")
		} else {
			if err := o.confirmRun(); err != nil {
				return err
			}
			_ = o.runAll(h, cmds)
			if cur, exists, err = readFile(fs.Path); err != nil {
				return err
			}
			_ = os.RemoveAll(fs.Bundle.Rendered)
		}
		if fs.Bundle.Replaced != "" {
			fmt.Fprintf(o.Out, "Setup had replaced the %q marketplace from %s. To use it again: %s\n", bundleMarketplace, fs.Bundle.Replaced, readdHint(h, o.Release))
		}
	}
	if o.OldRecord {
		// The settings may be a current setup's: one that found them as it
		// would write them changed nothing and recorded nothing, and another
		// build writes the same keys with the same values. They are neither
		// restored nor stripped, and no backup of them is made.
		fmt.Fprintf(o.Out, "The settings this earlier setup wrote to %[1]s were left as they are: they may be the current setup's. If %[2]s is set up now, `%[3]s setup --undo %[4]s` removes that setup with its settings; `%[3]s init` sets it up again from here. Settings no current setup owns can be deleted from that file by hand.\n",
			fs.Path, h.label(), variant.Current.Binary, hostCLI(h))
		if o.DryRun {
			return nil
		}
		if err := o.forget(state, h, fs); err != nil {
			return err
		}
		fmt.Fprintln(o.Out, "Removed the record of the earlier setup.")
		return pluginErr
	}
	if !exists {
		fmt.Fprintf(o.Err, "warning: %s no longer exists; forgetting setup state\n", fs.Path)
		if err := o.forget(state, h, fs); err != nil {
			return err
		}
		return pluginErr
	}

	var next []byte
	remove := false
	var warnings []string
	switch {
	case untouched && fs.OriginalAbsent:
		remove = true
	case untouched && fs.Snapshot != "":
		// Untouched since setup: restore the original bytes exactly.
		if next, err = os.ReadFile(fs.Snapshot); err != nil {
			return fmt.Errorf("reading pre-setup snapshot: %w", err)
		}
	case h == harnessClaude:
		next, warnings, err = claudeUndo(cur, fs)
	case h == harnessOpenCode:
		next, warnings, err = openCodeUndo(cur, fs)
	default:
		next, warnings, err = codexUndo(cur, fs)
	}
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(o.Err, "warning:", w)
	}
	if remove {
		if err := o.confirm(fs.Path, cur, nil); err != nil {
			return err
		}
		if o.DryRun {
			return nil
		}
		if _, err := backup(fs.Path, o.Now()); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if err := os.Remove(fs.Path); err != nil {
			return err
		}
	} else if string(next) != string(cur) {
		if err := o.confirm(fs.Path, cur, next); err != nil {
			return err
		}
		if o.DryRun {
			return nil
		}
		if _, err := backup(fs.Path, o.Now()); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if err := writeAtomic(fs.Path, next); err != nil {
			return err
		}
	} else if o.DryRun {
		return nil
	}
	if err := o.forget(state, h, fs); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Restored %s.\n", fs.Path)
	return pluginErr
}

// stillOwns reports whether the plugin marketplace the host has registered
// is the one fs recorded rendering. Each host has one plugin registration
// for every environment, so a later setup replaces it.
func (o Options) stillOwns(h harness, fs *FileState) bool {
	if fs.Bundle == nil || h == harnessOpenCode {
		return false
	}
	cur, _ := o.registeredMarketplace(h)
	return cur != "" && (cur == fs.Bundle.Rendered || cur == fs.Bundle.registered())
}

// confirmRun asks before running host commands (same rules as confirm).
func (o Options) confirmRun() error {
	switch {
	case o.Yes:
		return nil
	case !o.Interactive:
		fmt.Fprintln(o.Out, "Re-run with --yes to apply.")
		return ErrDeclined
	}
	fmt.Fprint(o.Out, "Run these commands? [y/N] ")
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return ErrDeclined
}

func (o Options) forget(state *State, h harness, fs *FileState) error {
	if fs.Snapshot != "" {
		_ = os.Remove(fs.Snapshot)
	}
	state.put(h, nil)
	return saveState(o.StateDir, state)
}

// confirm prints the diff and asks to apply. next == nil
// means the file will be removed.
func (o Options) confirm(path string, cur, next []byte) error {
	after := path + " (after kenaz setup)"
	if next == nil {
		after = "/dev/null"
	}
	fmt.Fprint(o.Out, unifiedDiff(path, after, string(cur), string(next)))
	switch {
	case o.DryRun:
		fmt.Fprintln(o.Out, "(dry run: nothing written)")
		return nil
	case o.Yes:
		return nil
	case !o.Interactive:
		fmt.Fprintln(o.Out, "Re-run with --yes to apply.")
		return ErrDeclined
	}
	fmt.Fprint(o.Out, "Apply? [y/N] ")
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return ErrDeclined
}

// openCodePreflight validates everything `setup opencode` renders before
// any file is touched: a supported OpenCode release (when one is
// installed), the kenaz binary and the adapter package.
func (o Options) openCodePreflight() (openCodeConfig, error) {
	if o.OpenCodeVersion != nil {
		if out, ok := o.OpenCodeVersion(); ok {
			v, parsed := parseVersion(out)
			switch {
			case !parsed:
				fmt.Fprintf(o.Err, "warning: could not read the OpenCode version from %q; continuing\n", out)
			case !atLeast(v, minOpenCode):
				return openCodeConfig{}, fmt.Errorf("OpenCode %d.%d.%d is older than the oldest supported release %d.%d.%d; upgrade OpenCode, then re-run setup. Nothing was changed",
					v[0], v[1], v[2], minOpenCode[0], minOpenCode[1], minOpenCode[2])
			}
		} else {
			fmt.Fprintln(o.Err, "warning: opencode is not on PATH; configuring it anyway")
		}
	}
	if o.KenazBin == "" || !filepath.IsAbs(o.KenazBin) {
		return openCodeConfig{}, errors.New("cannot determine the absolute path of the kenaz binary")
	}
	if err := checkAdapter(o.AdapterDir); err != nil {
		return openCodeConfig{}, err
	}
	if o.OpenCodeDir == "" || o.StateDir == "" {
		return openCodeConfig{}, errors.New("cannot locate the OpenCode config directory or the relay state directory")
	}
	return openCodeConfig{Bin: o.KenazBin, AdapterDir: o.AdapterDir, StateDir: o.StateDir, Env: o.FleetEnv}, nil
}

// relayState returns the running relay's state. When the relay has not
// started (or has exited) and o.StartRelay is set, setup starts it and
// waits for it to write daemon.json.
func (o Options) relayState() (daemonstate.State, error) {
	st, err := daemonstate.Read(o.StateDir)
	if err == nil || !errors.Is(err, daemonstate.ErrNoDaemon) {
		return st, err
	}
	if o.StartRelay == nil {
		return st, errors.New("the Kenaz relay has not started yet, so its port is unknown. Run `kenaz login` (or `kenaz daemon`), then re-run setup")
	}
	if o.DryRun {
		return st, errors.New("the Kenaz relay is not running and a dry run does not start it. Run `kenaz login` (or set up without --dry-run)")
	}
	fmt.Fprintln(o.Out, "Starting the Kenaz relay...")
	if err := o.StartRelay(); err != nil {
		return st, fmt.Errorf("starting the Kenaz relay: %w", err)
	}
	wait := o.RelayWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		st, err = daemonstate.Read(o.StateDir)
		if err == nil || !errors.Is(err, daemonstate.ErrNoDaemon) {
			return st, err
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("the Kenaz relay did not start within %s; see %s, then re-run setup", wait, filepath.Join(o.StateDir, "logs", "daemon.out"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// warnVersionedLaunch warns when b runs from a Homebrew cask's versioned
// directory and no version-independent link to it was found, so the
// registration would stop working after `brew upgrade`.
func (o Options) warnVersionedLaunch(b *Bundle) {
	prefix, ok := bundle.HomebrewCaskPrefix(b.Exe)
	if !ok || b.Launch != b.Exe {
		return
	}
	fmt.Fprintf(o.Err, "warning: %[3]s runs from Homebrew's versioned directory %[1]s, and %[2]s does not link to it, so this registration will stop working after `brew upgrade`. Run `brew reinstall --cask %[4]s`, then `%[5]s`.\n",
		filepath.Dir(filepath.Dir(b.Exe)), filepath.Join(prefix, "bin", filepath.Base(b.Exe)), variant.Current.Binary, variant.Current.CaskToken, o.initCmd())
}

// warnNotOnPath warns when the adopted GitHub plugin would not find
// `kenaz` on PATH.
func (o Options) warnNotOnPath(h harness) {
	if o.KenazOnPath == nil || o.KenazOnPath() {
		return
	}
	dir := ""
	if o.KenazBin != "" {
		dir = filepath.Dir(o.KenazBin)
	}
	fmt.Fprintf(o.Err, "warning: the Kenaz plugin from GitHub starts `%[3]s` from PATH, and %[3]s is not on PATH. Run `%[4]s` to add %[1]s to PATH, then restart %[2]s.\n", dir, h.label(), variant.Current.Binary, o.initCmd())
}

// enableHint says how to turn the plugin back on in h.
func enableHint(h harness) string {
	if h == harnessClaude {
		return "Turn it on with `claude plugin enable " + bundlePluginID + "`."
	}
	return "Turn it on by setting enabled = true under [plugins.\"" + bundlePluginID + "\"] in ~/.codex/config.toml (or in Codex's /plugins)."
}
