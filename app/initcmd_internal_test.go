package app

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/apps"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/pathenv"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// fakeInit records the steps kenaz init runs.
type fakeInit struct {
	t        *testing.T
	calls    []string
	signedIn bool
	loginRC  int
	found    []apps.App
	// setupRC and setupErr answer `kenaz setup <host>` by host.
	setupRC  map[string]int
	setupErr map[string]error
	ue       *fakeUserEnv
}

func (f *fakeInit) deps(home, shell, path, input string, interactive bool) initDeps {
	exe := filepath.Join(home, "kenaz", "install", "current", "bin", "kenaz")
	return initDeps{
		in:          bufio.NewReader(strings.NewReader(input)),
		interactive: interactive,
		goos:        "linux",
		home:        home,
		getenv: func(k string) string {
			return map[string]string{"SHELL": shell, "PATH": path}[k]
		},
		exe:       exe,
		lookPath:  func(string) (string, error) { return "", errors.New("not found") },
		userEnv:   func() (pathenv.UserEnv, error) { return f.ue, nil },
		pathAdded: func(dir string) { f.calls = append(f.calls, "path-added "+dir) },
		official:  true,
		signedIn:  func() bool { return f.signedIn },
		login: func(a []string) int {
			f.calls = append(f.calls, "login "+strings.Join(a, " "))
			return f.loginRC
		},
		detect: func() []apps.App { return f.found },
		setup: func(a []string, hk setup.Hooks) int {
			f.calls = append(f.calls, "setup "+strings.Join(a, " "))
			if hk.In == nil || hk.Interactive == nil || hk.Result == nil {
				f.t.Fatal("setup hooks not passed")
			}
			hk.Result(f.setupErr[a[0]])
			return f.setupRC[a[0]]
		},
		summary: func(a []string) { f.calls = append(f.calls, "summary "+strings.Join(a, " ")) },
	}
}

type fakeUserEnv struct {
	path      string
	expand    bool
	broadcast int
}

func (u *fakeUserEnv) Path() (string, bool, error)    { return u.path, u.expand, nil }
func (u *fakeUserEnv) SetPath(v string, e bool) error { u.path, u.expand = v, e; return nil }
func (u *fakeUserEnv) Broadcast()                     { u.broadcast++ }

func allApps() []apps.App {
	return []apps.App{
		{ID: apps.ClaudeCode, Name: "Claude Code", Installed: true, Where: "/usr/local/bin/claude"},
		{ID: apps.ClaudeDesktop, Name: "Claude Desktop", Installed: true, Where: "/Applications/Claude.app"},
		{ID: apps.Codex, Name: "Codex CLI", Installed: true, Where: "/usr/local/bin/codex"},
		{ID: apps.ChatGPTCodex, Name: "ChatGPT desktop (Codex)", Installed: true, Where: "/Applications/ChatGPT.app"},
	}
}

func TestInitFullFlow(t *testing.T) {
	old := features.ClaudeDesktopChat
	features.ClaudeDesktopChat = false
	t.Cleanup(func() { features.ClaudeDesktopChat = old })
	home := t.TempDir()
	f := &fakeInit{t: t, found: allApps()}
	var out, errb bytes.Buffer
	// Interactive: the user answers "y" to the PATH question.
	d := f.deps(home, "/bin/zsh", "/usr/bin:/bin", "y\n", true)
	code := doInit(&out, &errb, initOptions{env: "dev", setupFlags: []string{"--env", "dev"}}, d)
	if code != ExitOK {
		t.Fatalf("code %d\n%s\n%s", code, out.String(), errb.String())
	}
	dir := filepath.Dir(d.exe)
	want := []string{
		"path-added " + dir,
		"login --env dev",
		"setup claude --env dev",
		"setup codex --env dev",
		"summary --env dev",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls\n%v\nwant\n%v", f.calls, want)
	}
	rc, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !strings.Contains(string(rc), "export PATH='"+dir+"'") {
		t.Fatalf(".zshrc:\n%s", rc)
	}
	s := out.String()
	for _, w := range []string{
		"Add " + dir + " to PATH in " + filepath.Join(home, ".zshrc") + "? [y/N] ",
		"+# >>> " + variant.Current.Binary + " PATH >>>",
		"Claude Desktop            found (/Applications/Claude.app)",
		"Claude Desktop (Code tab): covered by Claude Code setup",
		"ChatGPT desktop (Codex): covered by Codex setup",
		"== Claude Code ==",
		"== Codex ==",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("output lacks %q:\n%s", w, s)
		}
	}
	if strings.Contains(s, "chat extension") {
		t.Error("Claude Desktop chat mentioned with the feature off")
	}

	// Run again: PATH already in the profile, already signed in, nothing
	// asked about PATH.
	f.calls, f.signedIn = nil, true
	out.Reset()
	d = f.deps(home, "/bin/zsh", "/usr/bin:/bin", "", true)
	if code := doInit(&out, &errb, initOptions{}, d); code != ExitOK {
		t.Fatal(code)
	}
	if strings.Join(f.calls, "|") != "path-added "+dir+"|setup claude|setup codex|summary " || !strings.Contains(out.String(), "already adds") || !strings.Contains(out.String(), "Already signed in") {
		t.Fatalf("%v\n%s", f.calls, out.String())
	}
}

func TestInitAlreadyOnPathAndYes(t *testing.T) {
	home := t.TempDir()
	f := &fakeInit{t: t, signedIn: true, found: allApps()[:1]}
	d := f.deps(home, "/bin/bash", "", "", false)
	d.getenv = func(k string) string {
		return map[string]string{"SHELL": "/bin/bash", "PATH": "/usr/bin:" + filepath.Dir(d.exe)}[k]
	}
	var out, errb bytes.Buffer
	if code := doInit(&out, &errb, initOptions{yes: true, setupFlags: []string{"--yes"}}, d); code != ExitOK {
		t.Fatal(code)
	}
	if strings.Join(f.calls, "|") != "setup claude --yes|summary " {
		t.Fatalf("%v", f.calls)
	}
	if !strings.Contains(out.String(), variant.Current.Binary+" is on PATH") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(err) {
		t.Fatal("profile written although kenaz is on PATH")
	}
}

func TestInitNoPathAndNonInteractive(t *testing.T) {
	home := t.TempDir()
	f := &fakeInit{t: t, signedIn: true}
	var out, errb bytes.Buffer
	d := f.deps(home, "/usr/bin/fish", "/usr/bin", "", false)
	if code := doInit(&out, &errb, initOptions{noPath: true}, d); code != ExitOK {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "set -gx PATH") || !strings.Contains(out.String(), "No supported app was found") {
		t.Fatal(out.String())
	}
	// Not a terminal and no --yes: nothing written, instructions printed.
	out.Reset()
	if code := doInit(&out, &errb, initOptions{}, d); code != ExitOK {
		t.Fatal(code)
	}
	if _, err := os.Stat(pathenv.ProfileFor(pathenv.Fish, "linux", home, func(string) string { return "" }, nil)); !os.IsNotExist(err) {
		t.Fatal("profile written without consent")
	}
	if !strings.Contains(out.String(), "re-run with --yes") {
		t.Fatal(out.String())
	}
}

func TestInitWindowsPath(t *testing.T) {
	f := &fakeInit{t: t, signedIn: true, ue: &fakeUserEnv{path: `C:\tools`, expand: true}}
	d := f.deps(t.TempDir(), "", `C:\Windows`, "", false)
	d.goos = "windows"
	d.exe = `C:\Users\u\AppData\Local\kameas\kenaz\install\current\bin\kenaz.exe`
	var out, errb bytes.Buffer
	if code := doInit(&out, &errb, initOptions{yes: true}, d); code != ExitOK {
		t.Fatal(code, errb.String())
	}
	dir := `C:\Users\u\AppData\Local\kameas\kenaz\install\current\bin`
	if f.ue.path != dir+`;C:\tools` || !f.ue.expand || f.ue.broadcast != 1 {
		t.Fatalf("%+v", f.ue)
	}
	// Idempotent.
	if code := doInit(&out, &errb, initOptions{yes: true}, d); code != ExitOK || f.ue.path != dir+`;C:\tools` || f.ue.broadcast != 1 {
		t.Fatalf("%+v", f.ue)
	}
}

func TestInitSetupOutcomes(t *testing.T) {
	f := &fakeInit{t: t, signedIn: true, found: allApps(),
		setupRC:  map[string]int{"claude": ExitError, "codex": ExitError},
		setupErr: map[string]error{"claude": setup.ErrDeclined, "codex": errors.New("boom")},
	}
	var out, errb bytes.Buffer
	d := f.deps(t.TempDir(), "/bin/zsh", "", "", false)
	code := doInit(&out, &errb, initOptions{noPath: true}, d)
	if code != ExitError {
		t.Fatalf("a failed setup must fail init: %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "Skipped Claude Code") || !strings.Contains(s, "Codex was not set up") ||
		!strings.Contains(s, "Claude Desktop (Code tab): not set up yet") || !strings.Contains(s, "summary") && len(f.calls) == 0 {
		t.Fatal(s)
	}
	if f.calls[len(f.calls)-1] != "summary " {
		t.Fatalf("summary must still run: %v", f.calls)
	}
}

func TestInitLoginFailureStops(t *testing.T) {
	f := &fakeInit{t: t, loginRC: ExitError, found: allApps()}
	var out, errb bytes.Buffer
	d := f.deps(t.TempDir(), "/bin/zsh", "", "", false)
	if code := doInit(&out, &errb, initOptions{noPath: true, noBrowser: true}, d); code != ExitError {
		t.Fatal(code)
	}
	if strings.Join(f.calls, "|") != "login --no-browser" {
		t.Fatalf("%v", f.calls)
	}
}

func TestInitUnofficialSkipsSignIn(t *testing.T) {
	f := &fakeInit{t: t, found: allApps()[2:3]}
	var out, errb bytes.Buffer
	d := f.deps(t.TempDir(), "/bin/zsh", "", "", false)
	d.official = false
	if code := doInit(&out, &errb, initOptions{noPath: true}, d); code != ExitOK {
		t.Fatal(code)
	}
	if strings.Join(f.calls, "|") != "setup codex|summary " || !strings.Contains(out.String(), "unofficial build") {
		t.Fatalf("%v\n%s", f.calls, out.String())
	}
}

// The Claude Desktop chat line is behind features.ClaudeDesktopChat.
func TestInitClaudeDesktopChatSwitch(t *testing.T) {
	old := features.ClaudeDesktopChat
	t.Cleanup(func() { features.ClaudeDesktopChat = old })
	for _, on := range []bool{false, true} {
		features.ClaudeDesktopChat = on
		for _, present := range []bool{false, true} {
			found := allApps()
			found[1].Installed = present
			f := &fakeInit{t: t, signedIn: true, found: found}
			var out, errb bytes.Buffer
			doInit(&out, &errb, initOptions{noPath: true}, f.deps(t.TempDir(), "/bin/zsh", "", "", false))
			s := out.String()
			switch {
			case !on && strings.Contains(s, "Claude Desktop (chat)"):
				t.Fatalf("off but mentioned:\n%s", s)
			case on && present && !strings.Contains(s, "Claude Desktop (chat): Claude Desktop found; the Kenaz chat extension is not available yet."):
				t.Fatalf("on, present:\n%s", s)
			case on && !present && !strings.Contains(s, "Claude Desktop (chat): Claude Desktop not found; the Kenaz chat extension is not available yet."):
				t.Fatalf("on, absent:\n%s", s)
			}
			if strings.Contains(s, "ChatGPT (chat)") {
				t.Fatal("ChatGPT chat must never be mentioned")
			}
		}
	}
}

func TestInitFlags(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb}
	if code := runInit(env, []string{"--bundle", "x", "--no-bundle"}); code != ExitUsage {
		t.Fatal(code)
	}
	if code := runInit(env, []string{"extra"}); code != ExitUsage {
		t.Fatal(code)
	}
}

// Profiles kenaz init must not rewrite: a read-only one, one with a begin
// marker and no end marker, and a dangling symlink. Each is left exactly
// as it was, init prints the line to add, and init still succeeds.
func TestInitProfileNotEditable(t *testing.T) {
	cases := map[string]func(rc string){
		"read-only": func(rc string) { os.WriteFile(rc, []byte("x\n"), 0o444) },
		"unterminated block": func(rc string) {
			os.WriteFile(rc, []byte("# >>> "+variant.Current.Binary+" PATH >>>\nexport PATH=/old:$PATH\n"), 0o600)
		},
		"dangling symlink": func(rc string) { os.Symlink(filepath.Join(filepath.Dir(rc), "gone"), rc) },
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			rc := filepath.Join(home, ".zshrc")
			seed(rc)
			before, _ := os.ReadFile(rc)
			f := &fakeInit{t: t, signedIn: true}
			var out, errb bytes.Buffer
			if code := doInit(&out, &errb, initOptions{yes: true}, f.deps(home, "/bin/zsh", "/usr/bin", "", false)); code != ExitOK {
				t.Fatalf("code %d: %s", code, errb.String())
			}
			if !strings.Contains(out.String(), "can't edit") || !strings.Contains(out.String(), "export PATH='") {
				t.Fatal(out.String())
			}
			after, _ := os.ReadFile(rc)
			if !bytes.Equal(before, after) {
				t.Fatalf("profile changed:\n%s", after)
			}
			if _, err := os.Lstat(rc + pathenv.BackupSuffix); !os.IsNotExist(err) {
				t.Fatal("backup written")
			}
		})
	}
}

// A symlinked profile (dotfiles) is edited at its target and stays a link.
func TestInitSymlinkedProfile(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "dotfiles", "zshrc")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("alias g=git\n"), 0o600)
	rc := filepath.Join(home, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Skip(err)
	}
	f := &fakeInit{t: t, signedIn: true}
	var out, errb bytes.Buffer
	d := f.deps(home, "/bin/zsh", "/usr/bin", "", false)
	if code := doInit(&out, &errb, initOptions{yes: true}, d); code != ExitOK {
		t.Fatal(code)
	}
	if fi, _ := os.Lstat(rc); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced by a regular file")
	}
	got, _ := os.ReadFile(real)
	if !strings.HasPrefix(string(got), "alias g=git\n") || !strings.Contains(string(got), filepath.Dir(d.exe)) || !strings.Contains(out.String(), "a link to") {
		t.Fatalf("%s\n%s", got, out.String())
	}
}

// On macOS with only ~/.profile, init edits ~/.profile rather than create a
// ~/.bash_profile that would stop login bash from reading it.
func TestInitMacBashKeepsProfile(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".profile"), []byte("export X=1\n"), 0o600)
	f := &fakeInit{t: t, signedIn: true}
	d := f.deps(home, "/bin/bash", "/usr/bin", "", false)
	d.goos = "darwin"
	var out, errb bytes.Buffer
	if code := doInit(&out, &errb, initOptions{yes: true}, d); code != ExitOK {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(home, ".bash_profile")); !os.IsNotExist(err) {
		t.Fatal("created .bash_profile in front of .profile")
	}
	if got, _ := os.ReadFile(filepath.Join(home, ".profile")); !strings.Contains(string(got), variant.Current.Binary+" PATH") {
		t.Fatalf("%s", got)
	}
}
