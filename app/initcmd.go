package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/apps"
	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/pathenv"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

const initUsage = `usage: kenaz init [--yes] [--no-path] [--env E] [--bundle DIR | --no-bundle] [--no-browser]

Sets Kenaz up in one go:
  1. puts kenaz on PATH (asks first; --no-path prints the line instead)
  2. signs in to Kameas Fleet if needed (kenaz login), which starts the relay
  3. finds Claude Code, Claude Desktop, the Codex CLI and the ChatGPT desktop app
  4. runs kenaz setup for each one found, showing every change first
  5. ends with kenaz setup doctor and kenaz status

--yes accepts every change without asking. If the Kenaz plugin marketplace
from GitHub (kameas-ai/kenaz-plugin) is already registered and pinned to this
release's commit, setup uses it; one that follows a branch or tag is replaced
by this download's verified plugin, with a warning. --bundle DIR replaces it
with that download's plugin, and --no-bundle uses kenaz from PATH instead of
this download's plugin.
`

// initOptions are kenaz init's flags.
type initOptions struct {
	yes, noPath, noBrowser bool
	env                    string
	setupFlags             []string // passed through to every `kenaz setup`
}

// initDeps are the steps kenaz init orchestrates. runInit fills them with
// the real commands; tests use fakes.
type initDeps struct {
	in          *bufio.Reader
	interactive bool

	// PATH (step 1).
	goos     string
	home     string
	getenv   func(string) string
	exe      string                          // this kenaz, as hosts should launch it
	lookPath func(string) (string, error)    // finds kenaz on PATH
	userEnv  func() (pathenv.UserEnv, error) // Windows user environment
	// pathAdded is told when init put dir on PATH, so the setup steps that
	// follow in this process see kenaz on PATH too.
	pathAdded func(dir string)

	// Sign-in (step 2).
	official bool
	signedIn func() bool
	login    func(args []string) int

	// Apps (steps 3, 4) and summary (step 5).
	detect  func() []apps.App
	setup   func(args []string, hk setup.Hooks) int
	summary func(args []string)
}

func runInit(env Env, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprint(env.Stderr, initUsage) }
	var o initOptions
	fs.BoolVar(&o.yes, "yes", false, "accept every change without asking")
	fs.BoolVar(&o.noPath, "no-path", false, "do not change PATH; print what to add instead")
	fs.BoolVar(&o.noBrowser, "no-browser", false, "sign in by printing the link instead of opening a browser")
	fs.StringVar(&o.env, "env", "", "Kameas Fleet environment (prod, stage, dev)")
	bundleDir := fs.String("bundle", "", "register this release bundle's plugin, replacing the GitHub marketplace")
	noBundle := fs.Bool("no-bundle", false, "use kenaz from PATH instead of this download's plugin")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return ExitUsage
	}
	if *bundleDir != "" && *noBundle {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+" init: --bundle and --no-bundle cannot be used together")
		return ExitUsage
	}
	if o.yes {
		o.setupFlags = append(o.setupFlags, "--yes")
	}
	if o.env != "" {
		o.setupFlags = append(o.setupFlags, "--env", o.env)
	}
	if *bundleDir != "" {
		abs, err := filepath.Abs(*bundleDir)
		if err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+" init:", err)
			return ExitUsage
		}
		o.setupFlags = append(o.setupFlags, "--bundle", abs)
	}
	if *noBundle {
		o.setupFlags = append(o.setupFlags, "--no-bundle")
	}

	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	if exe != "" {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		exe = bundle.StablePath(exe)
	}
	d := initDeps{
		in:          bufio.NewReader(os.Stdin),
		interactive: stdinInteractive(),
		goos:        runtime.GOOS,
		home:        home,
		getenv:      env.getenv,
		exe:         exe,
		lookPath:    exec.LookPath,
		userEnv:     pathenv.SystemUserEnv,
		pathAdded: func(dir string) {
			os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		},
		official: env.Provider.Flavor() != fleetapi.FlavorUnofficial,
		signedIn: func() bool {
			_, err := env.Provider.Auth().Session(env.Ctx)
			return err == nil
		},
		login:  func(a []string) int { return runLogin(env, a) },
		detect: func() []apps.App { return apps.Detect(apps.System()) },
		setup:  func(a []string, hk setup.Hooks) int { return runSetupWith(env, a, hk) },
		summary: func(a []string) {
			runSetup(env, append([]string{"doctor"}, a...))
			fmt.Fprintln(env.Stdout)
			runStatus(env, a)
		},
	}
	return doInit(env.Stdout, env.Stderr, o, d)
}

// doInit runs the init steps in order and returns the exit code: 0 when
// every step that ran succeeded or was declined, 1 when sign-in or an
// app's setup failed.
func doInit(w, ew io.Writer, o initOptions, d initDeps) int {
	failed := false
	fmt.Fprintln(w, "Setting up Kenaz.")

	fmt.Fprintln(w, "\n1. kenaz on PATH")
	if err := d.ensurePath(w, o); err != nil {
		fmt.Fprintln(ew, variant.Current.Binary+" init: PATH:", err)
		failed = true
	}

	fmt.Fprintln(w, "\n2. Sign in")
	switch {
	case !d.official:
		fmt.Fprintln(w, "This is an unofficial build, which cannot connect to Kameas Fleet; skipping sign-in.")
	case d.signedIn():
		fmt.Fprintln(w, "Already signed in to Kameas Fleet.")
	default:
		var la []string
		if o.env != "" {
			la = append(la, "--env", o.env)
		}
		if o.noBrowser {
			la = append(la, "--no-browser")
		}
		if code := d.login(la); code != ExitOK {
			fmt.Fprintln(ew, variant.Current.Binary+" init: sign-in did not complete; run `kenaz init` again (or `kenaz login`) when you're ready.")
			return code
		}
	}

	fmt.Fprintln(w, "\n3. Apps on this computer")
	found := d.detect()
	cc, cd := apps.Find(found, apps.ClaudeCode), apps.Find(found, apps.ClaudeDesktop)
	cx, cg := apps.Find(found, apps.Codex), apps.Find(found, apps.ChatGPTCodex)
	for _, a := range []apps.App{cc, cd, cx, cg} {
		if a.Installed {
			fmt.Fprintf(w, "  %-25s found (%s)\n", a.Name, a.Where)
		} else {
			fmt.Fprintf(w, "  %-25s not found\n", a.Name)
		}
	}

	fmt.Fprintln(w, "\n4. Set up")
	type target struct {
		name, host string
		run        bool
	}
	targets := []target{
		{"Claude Code", "claude", cc.Installed || cd.Installed},
		{"Codex", "codex", cx.Installed || cg.Installed},
	}
	configured := map[string]bool{}
	ran := false
	for _, t := range targets {
		if !t.run {
			continue
		}
		ran = true
		fmt.Fprintf(w, "\n== %s ==\n", t.name)
		var res error
		hk := setup.Hooks{In: d.in, Interactive: &d.interactive, Result: func(err error) { res = err }}
		code := d.setup(append([]string{t.host}, o.setupFlags...), hk)
		switch {
		case code == ExitOK:
			configured[t.host] = true
		case errors.Is(res, setup.ErrDeclined):
			fmt.Fprintf(w, "Skipped %s; run `kenaz setup %s` to set it up later.\n", t.name, t.host)
		default:
			failed = true
			fmt.Fprintf(w, "%s was not set up (see the error above); fix it, then run `kenaz setup %s`.\n", t.name, t.host)
		}
	}
	if !ran {
		fmt.Fprintln(w, "No supported app was found. Install Claude Code or Codex, then run `kenaz init` again.")
	}
	fmt.Fprintln(w)
	if cd.Installed {
		fmt.Fprintf(w, "Claude Desktop (Code tab): %s\n", coveredBy(configured["claude"], "Claude Code"))
	}
	if cg.Installed {
		fmt.Fprintf(w, "ChatGPT desktop (Codex): %s\n", coveredBy(configured["codex"], "Codex"))
	}
	if features.ClaudeDesktopChat {
		present := "Claude Desktop not found"
		if cd.Installed {
			present = "Claude Desktop found"
		}
		fmt.Fprintf(w, "Claude Desktop (chat): %s; to use Kenaz in chat, install the Kenaz desktop extension: %s\n", present, claudeDesktopDocs)
	}
	if configured["claude"] || configured["codex"] {
		fmt.Fprintln(w, "Restart running Claude Code, Codex, Claude Desktop and ChatGPT sessions to pick up the changes.")
	}

	fmt.Fprintln(w, "\n5. Summary")
	var sa []string
	if o.env != "" {
		sa = append(sa, "--env", o.env)
	}
	d.summary(sa)
	if failed {
		return ExitError
	}
	return ExitOK
}

// claudeDesktopDocs is the docs page for the Kenaz desktop extension
// (download, install, sign-in), named by `kenaz init` in builds with
// features.ClaudeDesktopChat.
const claudeDesktopDocs = "https://docs.kameas.ai/docs/kenaz-plugin/claude-desktop"

func coveredBy(ok bool, setupName string) string {
	if ok {
		return "covered by " + setupName + " setup"
	}
	return "not set up yet; it is covered by " + setupName + " setup"
}

// ensurePath is step 1.
func (d initDeps) ensurePath(w io.Writer, o initOptions) error {
	if d.exe == "" {
		return errors.New("cannot find this kenaz executable")
	}
	dir := filepath.Dir(d.exe)
	if d.goos == "windows" {
		dir = d.exe[:max(strings.LastIndexAny(d.exe, `/\`), 0)]
	}
	sep, fold := string(os.PathListSeparator), d.goos == "windows"
	if d.goos == "windows" {
		sep = ";"
	}
	if pathenv.OnPath(d.getenv("PATH"), dir, sep, fold) || d.sameKenazOnPath() {
		fmt.Fprintf(w, "%s is on PATH (%s).\n", variant.Current.Binary, dir)
		return nil
	}
	if d.goos == "windows" {
		return d.windowsPath(w, o, dir)
	}
	sh := pathenv.ShellFrom(d.getenv("SHELL"))
	if sh == "" {
		fmt.Fprintf(w, "%[3]s is not on PATH, and your shell (%[1]q) is not one %[3]s init edits (zsh, bash, fish). Add this directory to PATH yourself:\n  %[2]s\n", d.getenv("SHELL"), dir, variant.Current.Binary)
		return nil
	}
	profile := pathenv.ProfileFor(sh, d.goos, d.home, d.getenv, nil)
	line, err := pathenv.Line(sh, dir)
	if err != nil {
		return err
	}
	if o.noPath {
		fmt.Fprintf(w, "%s is not on PATH. To add it, put this in %s:\n%s\n", variant.Current.Binary, profile, line)
		return nil
	}
	cantEdit := func(err error) error {
		fmt.Fprintf(w, "%[4]s init can't edit %[1]s (%[2]v). To put %[4]s on PATH, add this to it yourself:\n%[3]s\n", profile, err, line, variant.Current.Binary)
		return nil
	}
	// A symlinked profile (dotfiles) is edited at its target, keeping the
	// link; a read-only or dangling one is left alone.
	target, err := pathenv.Target(profile)
	if err != nil {
		return cantEdit(err)
	}
	cur, existed, err := pathenv.ReadProfile(target)
	if err != nil {
		return cantEdit(err)
	}
	next, changed, err := pathenv.Edit(cur, sh, dir)
	if err != nil {
		return cantEdit(err)
	}
	if !changed {
		fmt.Fprintf(w, "%s already adds %s to PATH; open a new terminal to use kenaz there.\n", profile, dir)
		d.added(dir)
		return nil
	}
	where := profile
	if target != profile {
		where = fmt.Sprintf("%s (a link to %s)", profile, target)
	}
	fmt.Fprint(w, setup.Diff(target, cur, next))
	if !d.ask(w, o, fmt.Sprintf("Add %s to PATH in %s? [y/N] ", dir, where)) {
		fmt.Fprintf(w, "PATH not changed. To add it yourself, put this in %s:\n%s\n", profile, line)
		return nil
	}
	backup, err := pathenv.WriteProfile(target, cur, existed, next)
	if err != nil {
		return cantEdit(err)
	}
	switch {
	case backup != "":
		fmt.Fprintf(w, "Updated %s (the version before kenaz is saved as %s).\n", where, backup)
	case existed:
		fmt.Fprintf(w, "Updated %s.\n", where)
	default:
		fmt.Fprintf(w, "Created %s.\n", where)
	}
	fmt.Fprintln(w, "Open a new terminal to use kenaz there. Quit and reopen Claude Desktop and ChatGPT so they see it too.")
	d.added(dir)
	return nil
}

func (d initDeps) added(dir string) {
	if d.pathAdded != nil {
		d.pathAdded(dir)
	}
}

func (d initDeps) windowsPath(w io.Writer, o initOptions, dir string) error {
	if o.noPath {
		fmt.Fprintf(w, "%s is not on PATH. To add it, add this folder to your user Path (Settings > System > About > Advanced system settings > Environment Variables):\n  %s\n", variant.Current.Binary, dir)
		return nil
	}
	ue, err := d.userEnv()
	if err != nil {
		return err
	}
	cur, expand, err := ue.Path()
	if err != nil {
		return err
	}
	next, changed := pathenv.WindowsEdit(cur, dir, d.getenv)
	if !changed {
		fmt.Fprintf(w, "Your user Path already has %s; open a new terminal to use kenaz there.\n", dir)
		d.added(dir)
		return nil
	}
	fmt.Fprintf(w, "Your user Path (HKCU\\Environment) will start with:\n  %s\n", dir)
	if !d.ask(w, o, fmt.Sprintf("Add %s to your user Path? [y/N] ", dir)) {
		fmt.Fprintf(w, "PATH not changed. To add it yourself, add this folder to your user Path:\n  %s\n", dir)
		return nil
	}
	if err := ue.SetPath(next, expand); err != nil {
		return err
	}
	ue.Broadcast()
	d.added(dir)
	fmt.Fprintln(w, "Updated your user Path. Open a new terminal to use kenaz there. Quit and reopen Claude Desktop and ChatGPT so they see it too.")
	return nil
}

// sameKenazOnPath reports whether `kenaz` on PATH is this executable
// (for example through a symlink in ~/.local/bin).
func (d initDeps) sameKenazOnPath() bool {
	if d.lookPath == nil {
		return false
	}
	p, err := d.lookPath(variant.Current.Binary)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return bundle.StablePath(p) == d.exe
}

// ask asks a yes/no question; --yes answers yes, and without a terminal
// the answer is no.
func (d initDeps) ask(w io.Writer, o initOptions, q string) bool {
	if o.yes {
		return true
	}
	if !d.interactive {
		fmt.Fprintln(w, "(not a terminal: re-run with --yes to accept)")
		return false
	}
	fmt.Fprint(w, q)
	line, _ := d.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
