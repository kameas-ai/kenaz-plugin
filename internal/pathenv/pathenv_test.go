package pathenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestShellFrom(t *testing.T) {
	for in, want := range map[string]Shell{
		"/bin/zsh": Zsh, "/usr/local/bin/bash": Bash, "/opt/homebrew/bin/fish": Fish,
		"/bin/sh": "", "": "", `C:\Program Files\Git\bin\bash.exe`: Bash,
	} {
		if got := ShellFrom(in); got != want {
			t.Errorf("ShellFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProfileFor(t *testing.T) {
	none := env(nil)
	cases := []struct {
		sh   Shell
		goos string
		env  func(string) string
		want string
	}{
		{Zsh, "darwin", none, "/h/.zshrc"},
		{Zsh, "linux", env(map[string]string{"ZDOTDIR": "/z"}), "/z/.zshrc"},
		{Bash, "darwin", none, "/h/.bash_profile"},
		{Bash, "linux", none, "/h/.bashrc"},
		{Fish, "linux", none, "/h/.config/fish/conf.d/" + variant.Current.Binary + ".fish"},
		{Fish, "darwin", env(map[string]string{"XDG_CONFIG_HOME": "/x"}), "/x/fish/conf.d/" + variant.Current.Binary + ".fish"},
	}
	for _, c := range cases {
		if got := ProfileFor(c.sh, c.goos, "/h", c.env, func(string) bool { return false }); filepath.ToSlash(got) != c.want {
			t.Errorf("%s/%s: %s, want %s", c.sh, c.goos, got, c.want)
		}
	}
}

func TestEditIdempotent(t *testing.T) {
	dir := "/Users/u/Kenaz Install/current/bin"
	for _, sh := range []Shell{Zsh, Bash, Fish} {
		for _, start := range []string{"", "export EDITOR=vim\n", "alias ll='ls -l'"} {
			once, changed, err := Edit([]byte(start), sh, dir)
			if err != nil || !changed {
				t.Fatalf("%s %q: %v %v", sh, start, changed, err)
			}
			if !strings.HasPrefix(string(once), start) {
				t.Fatalf("%s: user content not kept:\n%s", sh, once)
			}
			if strings.Count(string(once), beginMarker) != 1 || !strings.Contains(string(once), "'"+dir+"'") {
				t.Fatalf("%s: block:\n%s", sh, once)
			}
			twice, changed, err := Edit(once, sh, dir)
			if err != nil || changed || string(twice) != string(once) {
				t.Fatalf("%s: second edit changed it (%v %v):\n%s", sh, changed, err, twice)
			}
			// A new directory replaces the block in place.
			moved, changed, _ := Edit(once, sh, "/opt/kenaz/bin")
			if !changed || strings.Count(string(moved), beginMarker) != 1 || strings.Contains(string(moved), dir) || !strings.Contains(string(moved), "/opt/kenaz/bin") {
				t.Fatalf("%s: replace:\n%s", sh, moved)
			}
		}
	}
}

func TestEditKeepsSurroundingLinesAndCRLF(t *testing.T) {
	in := "a\r\n" + beginMarker + "\r\nold\r\n" + endMarker + "\r\nb\r\n"
	out, changed, err := Edit([]byte(in), Zsh, "/k/bin")
	if err != nil || !changed {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "a\r\n"+beginMarker+"\r\n") || !strings.HasSuffix(s, endMarker+"\r\nb\r\n") || strings.Contains(s, "old") {
		t.Fatalf("%q", s)
	}
	if _, _, err := Edit([]byte(beginMarker+"\nno end\n"), Zsh, "/k"); err == nil {
		t.Fatal("unterminated block accepted")
	}
	if _, _, err := Edit(nil, Bash, "/it's/bin"); err != ErrUnquotable {
		t.Fatalf("quote: %v", err)
	}
}

// The posix line is a guarded prepend; the fish one a guarded set.
func TestLine(t *testing.T) {
	l, _ := Line(Zsh, "/k/bin")
	if l != `case ":$PATH:" in *':/k/bin:'*) ;; *) export PATH='/k/bin':"$PATH" ;; esac` {
		t.Fatal(l)
	}
	l, _ = Line(Fish, "/k/bin")
	if !strings.Contains(l, "if not contains -- '/k/bin' $PATH") || !strings.Contains(l, "set -gx PATH '/k/bin' $PATH") {
		t.Fatal(l)
	}
}

func TestOnPath(t *testing.T) {
	if !OnPath("/usr/bin:/k/bin/:/bin", "/k/bin", ":", false) || OnPath("/usr/bin", "/k/bin", ":", false) {
		t.Fatal("unix")
	}
	if !OnPath(`C:\Windows;c:\users\u\kenaz\BIN\`, `C:\Users\u\Kenaz\bin`, ";", true) {
		t.Fatal("windows")
	}
}

func TestWriteProfile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fish", "conf.d", "kenaz.fish")
	next, _, _ := Edit(nil, Fish, "/k/bin")
	if b, err := WriteProfile(p, nil, false, next); err != nil || b != "" {
		t.Fatal(b, err)
	}
	rc := filepath.Join(dir, ".zshrc")
	os.WriteFile(rc, []byte("x\n"), 0o600)
	cur, existed, _ := ReadProfile(rc)
	next, _, _ = Edit(cur, Zsh, "/k/bin")
	b, err := WriteProfile(rc, cur, existed, next)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b); string(got) != "x\n" {
		t.Fatalf("backup %q", got)
	}
	if fi, _ := os.Stat(rc); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// fakeUserEnv is the registry abstraction's test double.
type fakeUserEnv struct {
	path      string
	expand    bool
	sets      int
	broadcast int
}

func (f *fakeUserEnv) Path() (string, bool, error) { return f.path, f.expand, nil }
func (f *fakeUserEnv) SetPath(v string, e bool) error {
	f.path, f.expand = v, e
	f.sets++
	return nil
}
func (f *fakeUserEnv) Broadcast() { f.broadcast++ }

func TestWindowsEdit(t *testing.T) {
	var ue UserEnv = &fakeUserEnv{path: `%USERPROFILE%\bin;C:\tools`, expand: true}
	cur, expand, _ := ue.Path()
	next, changed := WindowsEdit(cur, `C:\Users\u\AppData\Local\kenaz\current\bin`, nil)
	if !changed || next != `C:\Users\u\AppData\Local\kenaz\current\bin;%USERPROFILE%\bin;C:\tools` {
		t.Fatal(next)
	}
	ue.SetPath(next, expand)
	again, changed := WindowsEdit(next, `c:\users\u\appdata\local\kenaz\current\bin\`, nil)
	if changed || again != next {
		t.Fatal("not idempotent")
	}
	if got, changed := WindowsEdit("", `C:\k`, nil); !changed || got != `C:\k` {
		t.Fatal(got)
	}
	if got := RemoveFromWindowsPath(next, `C:\Users\u\AppData\Local\kenaz\current\bin`); got != `%USERPROFILE%\bin;C:\tools` {
		t.Fatal(got)
	}
	if f := ue.(*fakeUserEnv); f.sets != 1 || !f.expand {
		t.Fatalf("%+v", f)
	}
}

// On macOS, login bash reads only the first of .bash_profile, .bash_login
// and .profile, so kenaz edits the one in use rather than creating
// .bash_profile in front of an existing .profile.
func TestProfileForMacBash(t *testing.T) {
	none := env(nil)
	for _, c := range []struct {
		have []string
		want string
	}{
		{nil, "/h/.bash_profile"},
		{[]string{"/h/.profile"}, "/h/.profile"},
		{[]string{"/h/.bash_login", "/h/.profile"}, "/h/.bash_login"},
		{[]string{"/h/.bash_profile", "/h/.bash_login", "/h/.profile"}, "/h/.bash_profile"},
	} {
		set := map[string]bool{}
		for _, p := range c.have {
			set[p] = true
		}
		got := ProfileFor(Bash, "darwin", "/h", none, func(p string) bool { return set[filepath.ToSlash(p)] })
		if filepath.ToSlash(got) != c.want {
			t.Errorf("have %v: %s, want %s", c.have, got, c.want)
		}
	}
	// Linux bash always uses .bashrc.
	if got := ProfileFor(Bash, "linux", "/h", none, func(string) bool { return true }); filepath.ToSlash(got) != "/h/.bashrc" {
		t.Fatal(got)
	}
}

func TestEditCRLFExact(t *testing.T) {
	block, _ := Block(Zsh, "/k")
	crlf := strings.ReplaceAll(block, "\n", "\r\n")
	for in, want := range map[string]string{
		"a\r\n":     "a\r\n\r\n" + crlf,
		"a\r\n\r\n": "a\r\n\r\n" + crlf, // an existing blank line is reused
		"a\r\nb":    "a\r\nb\r\n\r\n" + crlf,
		"a\r\nb\n":  "a\r\nb\n\r\n" + crlf, // mixed endings: no extra blank line
		"x\n\n":     "x\n\n" + block,
		"":          block,
		"a\r\n" + strings.ReplaceAll(block, "/k", "/old"): "a\r\n" + crlf,
		// A block written with LF in a CRLF file: no extra blank line after it.
		"a\r\n" + strings.ReplaceAll(block, "/k", "/old") + "b\r\n": "a\r\n" + crlf + "b\r\n",
	} {
		got, _, err := Edit([]byte(in), Zsh, "/k")
		if err != nil || string(got) != want {
			t.Errorf("Edit(%q) = %q, want %q (%v)", in, got, want, err)
		}
	}
}

func TestTarget(t *testing.T) {
	dir := t.TempDir()
	// Missing: created where asked.
	missing := filepath.Join(dir, "new", ".zshrc")
	if got, err := Target(missing); err != nil || got != missing {
		t.Fatal(got, err)
	}
	// Symlink (a dotfiles manager): the target is edited and the link kept.
	real := filepath.Join(dir, "dotfiles", "zshrc")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("export A=1\n"), 0o640)
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	target, err := Target(link)
	wantReal, _ := filepath.EvalSymlinks(real)
	if err != nil || target != wantReal {
		t.Fatal(target, err)
	}
	cur, existed, _ := ReadProfile(target)
	next, _, _ := Edit(cur, Zsh, "/k/bin")
	if _, err := WriteProfile(target, cur, existed, next); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	if got, _ := os.ReadFile(link); !strings.Contains(string(got), "/k/bin") || !strings.HasPrefix(string(got), "export A=1\n") {
		t.Fatalf("block did not reach the link target:\n%s", got)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Fatalf("target mode %v", fi.Mode())
	}
	// Dangling link: refused.
	dangling := filepath.Join(dir, ".bashrc")
	os.Symlink(filepath.Join(dir, "gone"), dangling)
	if _, err := Target(dangling); err != ErrDanglingLink {
		t.Fatal(err)
	}
	// Read-only: refused, and left alone.
	ro := filepath.Join(dir, ".profile")
	os.WriteFile(ro, []byte("x\n"), 0o444)
	if _, err := Target(ro); err != ErrNotWritable {
		t.Fatal(err)
	}
	// A directory: refused.
	os.Mkdir(filepath.Join(dir, "d"), 0o700)
	if _, err := Target(filepath.Join(dir, "d")); err != ErrNotRegular {
		t.Fatal(err)
	}
}

// The backup is the profile from before kenaz first edited it; a later
// edit (a new install directory) does not overwrite it.
func TestBackupKeepsOriginal(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	os.WriteFile(rc, []byte("original\n"), 0o600)
	for i, dir := range []string{"/a/bin", "/b/bin", "/c/bin"} {
		cur, existed, _ := ReadProfile(rc)
		next, _, _ := Edit(cur, Zsh, dir)
		b, err := WriteProfile(rc, cur, existed, next)
		if err != nil {
			t.Fatal(err)
		}
		if (i == 0) != (b != "") {
			t.Fatalf("run %d: backup %q", i, b)
		}
	}
	if got, _ := os.ReadFile(rc + BackupSuffix); string(got) != "original\n" {
		t.Fatalf("backup %q", got)
	}
	got, _ := os.ReadFile(rc)
	if strings.Count(string(got), beginMarker) != 1 || !strings.Contains(string(got), "/c/bin") {
		t.Fatalf("%s", got)
	}
}

// An entry written with %VAR% counts as the expanded directory.
func TestWindowsEditExpands(t *testing.T) {
	getenv := func(k string) string {
		if strings.EqualFold(k, "LOCALAPPDATA") {
			return `C:\Users\u\AppData\Local`
		}
		return ""
	}
	cur := `%LOCALAPPDATA%\kameas\kenaz\install\current\bin;C:\tools`
	if got, changed := WindowsEdit(cur, `C:\Users\u\AppData\Local\kameas\kenaz\install\current\bin`, getenv); changed || got != cur {
		t.Fatalf("duplicate added: %s", got)
	}
	if got := ExpandPercent(`%UNKNOWN%\x;%LOCALAPPDATA%;50%`, getenv); got != `%UNKNOWN%\x;C:\Users\u\AppData\Local;50%` {
		t.Fatal(got)
	}
}
