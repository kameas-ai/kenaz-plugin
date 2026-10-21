// Package setup points Claude Code and Codex at the local relay:
// it merges the relay's OTEL settings into the user's
// own harness config with a diff and confirmation, records exactly what it
// changed so `--undo` can restore it, and prints the managed-configuration
// snippets admins deploy fleet-wide.
//
// Setup owns a closed set of telemetry keys and nothing else. It never
// writes permission, hook, MCP-server or rules configuration;
// `make check-no-policy` enforces that on this package's sources.
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

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

type harness int

const (
	harnessClaude harness = iota
	harnessCodex
)

func (h harness) label() string {
	if h == harnessClaude {
		return "Claude Code"
	}
	return "Codex"
}

func parseHarness(s string) (harness, bool) {
	switch s {
	case "claude", "claude-code":
		return harnessClaude, true
	case "codex":
		return harnessCodex, true
	}
	return 0, false
}

// Options configures one setup invocation. Paths are resolved by
// DefaultOptions; tests fill them with temp directories.
type Options struct {
	ClaudeDir string // ~/.claude, or $CLAUDE_CONFIG_DIR
	CodexDir  string // ~/.codex, or $CODEX_HOME
	StateDir  string // relay $STATE for the selected environment

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
	return Options{
		ClaudeDir:         claudeDir,
		CodexDir:          codexDir,
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
	if h == harnessClaude {
		return filepath.Join(o.ClaudeDir, "settings.json")
	}
	return filepath.Join(o.CodexDir, "config.toml")
}

func (o Options) snapshotPath(h harness) string {
	name := "claude-settings.json.orig"
	if h == harnessCodex {
		name = "codex-config.toml.orig"
	}
	return filepath.Join(o.StateDir, "setup", name)
}

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 64
)

const usage = `usage:
  kenaz setup claude|codex [--yes] [--dry-run] [--traces] [--env E]
  kenaz setup --undo claude|codex [--yes] [--dry-run] [--env E]
  kenaz setup print-managed claude|codex [--port N] [--raw]
  kenaz setup doctor [--env E]
`

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
	fl.Usage = func() { fmt.Fprint(stderr, usage) }

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
			fmt.Fprint(stderr, usage)
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

	if len(pos) == 1 && pos[0] == "doctor" {
		return printDoctor(stdout, Doctor(ctx, opts, ExecRunner))
	}
	if len(pos) != 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	h, ok := parseHarness(pos[0])
	if !ok {
		fmt.Fprint(stderr, usage)
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

// Apply configures harness h to export to the relay.
func Apply(o Options, h harness) error {
	st, err := daemonstate.Read(o.StateDir)
	if errors.Is(err, daemonstate.ErrNoDaemon) {
		return errors.New("the Kenaz relay has not started yet, so its port is unknown. Start a harness session with the plugin installed (or run `kenaz daemon`), then re-run setup")
	}
	if err != nil {
		return err
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

	var next []byte
	var fs *FileState
	var changed bool
	if h == harnessClaude {
		next, fs, changed, err = claudeApply(cur, st.Port, o.Traces, prev)
	} else {
		next, fs, changed, err = codexApply(cur, st.Port, prev)
	}
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(o.Out, "%s is already configured for the Kenaz relay (%s).\n", h.label(), path)
		return nil
	}
	if err := o.confirm(path, cur, next); err != nil {
		return err
	}
	if o.DryRun {
		return nil
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
	if err := writeAtomic(path, next); err != nil {
		return err
	}
	fs.WrittenSHA256 = sha(next)
	state.put(h, fs)
	if err := saveState(o.StateDir, state); err != nil {
		return fmt.Errorf("saving setup state: %w", err)
	}
	fmt.Fprintf(o.Out, "Configured %s to send telemetry to the Kenaz relay on 127.0.0.1:%d. Restart running sessions to pick it up.\n", h.label(), st.Port)
	return nil
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
	if !exists {
		fmt.Fprintf(o.Err, "warning: %s no longer exists; forgetting setup state\n", fs.Path)
		return o.forget(state, h, fs)
	}

	var next []byte
	remove := false
	var warnings []string
	switch {
	case sha(cur) == fs.WrittenSHA256 && fs.OriginalAbsent:
		remove = true
	case sha(cur) == fs.WrittenSHA256 && fs.Snapshot != "":
		// Untouched since setup: restore the original bytes exactly.
		if next, err = os.ReadFile(fs.Snapshot); err != nil {
			return fmt.Errorf("reading pre-setup snapshot: %w", err)
		}
	case h == harnessClaude:
		next, warnings, err = claudeUndo(cur, fs)
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
