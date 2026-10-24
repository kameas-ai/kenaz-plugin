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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
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
		env = r.Env()
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
	return Options{
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
  kenaz setup claude|codex [--yes] [--dry-run] [--traces] [--env E] [--bundle DIR]
  kenaz setup opencode [--yes] [--dry-run] [--env E] [--adapter DIR] [--bundle DIR]
  kenaz setup --undo claude|codex|opencode [--yes] [--dry-run] [--env E]
  kenaz setup print-managed claude|codex [--port N] [--raw]
  kenaz setup doctor [--env E]
`

const usageBase = `usage:
  kenaz setup claude|codex [--yes] [--dry-run] [--traces] [--env E] [--bundle DIR]
  kenaz setup --undo claude|codex [--yes] [--dry-run] [--env E]
  kenaz setup print-managed claude|codex [--port N] [--raw]
  kenaz setup doctor [--env E]
`

// Usage is the setup help for this build (OpenCode only when enabled).
func Usage() string {
	if features.OpenCode {
		return usageOpenCode
	}
	return usageBase
}

// Command runs `kenaz setup ...` and returns the exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return CommandWith(ctx, args, stdout, stderr, nil)
}

// CommandWith is Command with the relay's state-dir resolver.
func CommandWith(ctx context.Context, args []string, stdout, stderr io.Writer, resolve StateDirResolver) int {
	fl := flag.NewFlagSet("setup", flag.ContinueOnError)
	fl.SetOutput(stderr)
	yes := fl.Bool("yes", false, "apply without asking")
	dry := fl.Bool("dry-run", false, "show the diff and change nothing")
	traces := fl.Bool("traces", false, "Claude Code only: also export traces (beta)")
	undo := fl.Bool("undo", false, "restore what setup changed")
	env := fl.String("env", "", "Kameas Fleet environment (prod, stage, dev)")
	port := fl.Int("port", 4318, "print-managed: relay port to pin fleet-wide")
	raw := fl.Bool("raw", false, "print-managed: print only the file content")
	bundleDir := fl.String("bundle", "", "register a self-contained release bundle (default: the bundle this kenaz runs from, if any)")
	noBundle := fl.Bool("no-bundle", false, "ignore the bundle this kenaz runs from and use kenaz from PATH")
	adapter := new(string)
	if features.OpenCode {
		adapter = fl.String("adapter", "", "opencode: the Kenaz OpenCode adapter directory (default: opencode/ next to the binary's bin/)")
	}
	fl.Usage = func() { fmt.Fprint(stderr, Usage()) }

	var pos []string
	rest := args
	for {
		if err := fl.Parse(rest); err != nil {
			return exitUsage
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}

	if len(pos) == 2 && pos[0] == "print-managed" {
		h, ok := parseHarness(pos[1])
		if !ok {
			fmt.Fprint(stderr, Usage())
			return exitUsage
		}
		if h == harnessOpenCode {
			fmt.Fprintln(stderr, "kenaz setup: print-managed is not available for OpenCode yet; run `kenaz setup opencode` for each user")
			return exitUsage
		}
		return printManaged(stdout, h, *port, *raw)
	}

	opts, err := defaultOptions(*env, stdout, stderr, resolve)
	if err != nil {
		fmt.Fprintln(stderr, "kenaz setup:", err)
		return exitError
	}
	opts.Yes, opts.DryRun, opts.Traces = *yes, *dry, *traces
	opts.FleetEnv = *env
	if *adapter != "" {
		a, err := filepath.Abs(*adapter)
		if err != nil {
			fmt.Fprintln(stderr, "kenaz setup:", err)
			return exitError
		}
		opts.AdapterDir = a
	}
	switch {
	case *bundleDir != "":
		opts.BundleRoot = bundle.StableRoot(*bundleDir)
	case !*noBundle:
		opts.BundleRoot = DetectBundle(opts.KenazBin)
	}
	opts.OpenCodeVersion = func() (string, bool) {
		out, err := ExecRunner(ctx, "opencode", "--version")
		return strings.TrimSpace(out), err == nil
	}

	if len(pos) == 1 && pos[0] == "doctor" {
		return printDoctor(stdout, Doctor(ctx, opts, ExecRunner))
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
	if err != nil {
		fmt.Fprintln(stderr, "kenaz setup:", err)
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
		st, err := daemonstate.Read(o.StateDir)
		if errors.Is(err, daemonstate.ErrNoDaemon) {
			return errors.New("the Kenaz relay has not started yet, so its port is unknown. Start a harness session with the plugin installed (or run `kenaz daemon`), then re-run setup")
		}
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

	// Claude Code and Codex from a bundle: plan the plugin registration.
	var cmds [][]string
	var reg *BundleReg
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
		if cmds, err = o.planRegistration(h, bundle, rendered, digest, prevReg); err != nil {
			return err
		}
		reg = &BundleReg{Root: bundle.Root, Version: bundle.Version, Rendered: rendered, Digest: digest}
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
		fmt.Fprintf(o.Out, "Setup will register the Kenaz plugin from %s:\n", bundle.Root)
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
		if _, err := renderPlugin(bundle, reg.Rendered, true); err != nil {
			return err
		}
		if err := o.runAll(h, cmds); err != nil {
			return err
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
	if reg != nil {
		fs.Bundle = reg
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
	if reg != nil {
		fmt.Fprintf(o.Out, "Registered the Kenaz plugin from %s in %s (launching %s).\n", bundle.Root, h.label(), bundle.Exe)
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
		o.KenazBin, o.AdapterDir = b.Exe, b.AdapterDir()
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
	cur, exists, err := readFile(fs.Path)
	if err != nil {
		return err
	}
	// Whether the file still holds exactly what setup wrote, decided before
	// any host CLI touches it.
	untouched := exists && sha(cur) == fs.WrittenSHA256

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
	}
	if !exists {
		fmt.Fprintf(o.Err, "warning: %s no longer exists; forgetting setup state\n", fs.Path)
		return o.forget(state, h, fs)
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
	return nil
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
