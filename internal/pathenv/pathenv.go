// Package pathenv puts the kenaz executable's directory on the user's PATH.
// On macOS and Linux it adds one marked block to the
// login shell's profile (zsh, bash or fish); on Windows it prepends the
// directory to the user Path in the registry. Every edit is idempotent:
// a second run with the same directory changes nothing, and a run with a
// new directory replaces the block instead of adding another.
package pathenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Block markers. Everything between them belongs to this build of kenaz
// (each build variant has its own block).
var (
	beginMarker = "# >>> " + variant.Current.Binary + " PATH >>>"
	endMarker   = "# <<< " + variant.Current.Binary + " PATH <<<"
)

// Shell is a supported login shell.
type Shell string

const (
	Zsh  Shell = "zsh"
	Bash Shell = "bash"
	Fish Shell = "fish"
)

// ShellFrom maps $SHELL to a supported shell ("" when unsupported).
func ShellFrom(shellEnv string) Shell {
	base := shellEnv[strings.LastIndexAny(shellEnv, `/\`)+1:]
	switch strings.TrimSuffix(base, ".exe") {
	case "zsh":
		return Zsh
	case "bash":
		return Bash
	case "fish":
		return Fish
	}
	return ""
}

// ProfileFor returns the file kenaz edits for sh. getenv supplies ZDOTDIR
// and XDG_CONFIG_HOME; exists (nil: the real file system) decides which
// bash login file is in use on macOS.
//
//	zsh:  $ZDOTDIR/.zshrc, or ~/.zshrc
//	bash: macOS (Terminal starts login shells): the first of ~/.bash_profile,
//	      ~/.bash_login and ~/.profile that exists, else ~/.bash_profile.
//	      Login bash reads only the first of these, so creating
//	      ~/.bash_profile next to an existing ~/.profile would stop it from
//	      being read. Elsewhere: ~/.bashrc.
//	fish: $XDG_CONFIG_HOME/fish/conf.d/kenaz.fish (a file of its own)
func ProfileFor(sh Shell, goos, home string, getenv func(string) string, exists func(string) bool) string {
	if exists == nil {
		exists = func(p string) bool { _, err := os.Lstat(p); return err == nil }
	}
	switch sh {
	case Zsh:
		if d := getenv("ZDOTDIR"); d != "" {
			return filepath.Join(d, ".zshrc")
		}
		return filepath.Join(home, ".zshrc")
	case Bash:
		if goos == "darwin" {
			for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
				if p := filepath.Join(home, name); exists(p) {
					return p
				}
			}
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	case Fish:
		cfg := getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "fish", "conf.d", variant.Current.Binary+".fish")
	}
	return ""
}

// ErrUnquotable means the directory cannot be written into a shell profile
// safely.
var ErrUnquotable = errors.New("the directory contains a quote or newline and cannot be added to a shell profile safely")

// Line is the PATH line for sh and dir.
func Line(sh Shell, dir string) (string, error) {
	if strings.ContainsAny(dir, "'\"\n\r") {
		return "", ErrUnquotable
	}
	if sh == Fish {
		return fmt.Sprintf("if not contains -- '%s' $PATH\n    set -gx PATH '%s' $PATH\nend", dir, dir), nil
	}
	return fmt.Sprintf("case \":$PATH:\" in *':%s:'*) ;; *) export PATH='%s':\"$PATH\" ;; esac", dir, dir), nil
}

// Block is the marked block kenaz writes.
func Block(sh Shell, dir string) (string, error) {
	l, err := Line(sh, dir)
	if err != nil {
		return "", err
	}
	return beginMarker + "\n" + l + "\n" + endMarker + "\n", nil
}

// Edit returns cur with the kenaz block for dir: added at the end when
// there is none, replaced in place when it differs, and cur unchanged
// (changed false) when it is already there.
func Edit(cur []byte, sh Shell, dir string) (next []byte, changed bool, err error) {
	block, err := Block(sh, dir)
	if err != nil {
		return nil, false, err
	}
	text := string(cur)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
		block = strings.ReplaceAll(block, "\n", "\r\n")
	}
	start := strings.Index(text, beginMarker)
	if start >= 0 {
		rel := strings.Index(text[start:], endMarker)
		if rel < 0 {
			return nil, false, fmt.Errorf("found %q without %q; fix the file by hand", beginMarker, endMarker)
		}
		end := start + rel + len(endMarker)
		// The block's own line ending, whichever style it was written in,
		// is part of the block (the new block brings its own).
		switch {
		case strings.HasPrefix(text[end:], "\r\n"):
			end += 2
		case strings.HasPrefix(text[end:], "\n"):
			end++
		}
		if text[start:end] == block {
			return cur, false, nil
		}
		return []byte(text[:start] + block + text[end:]), true, nil
	}
	// Append after one blank line, reusing the line endings and any blank
	// line already at the end of the file.
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += eol
	}
	trimmed := strings.TrimRight(text, "\r\n")
	if text != "" && strings.Count(text[len(trimmed):], "\n") < 2 {
		text += eol
	}
	return []byte(text + block), true, nil
}

// OnPath reports whether dir is one of the entries of the PATH value
// pathVar (sep is the list separator).
func OnPath(pathVar, dir string, sep string, fold bool) bool {
	want := clean(dir, fold)
	for _, e := range strings.Split(pathVar, sep) {
		if e != "" && clean(e, fold) == want {
			return true
		}
	}
	return false
}

func clean(p string, fold bool) string {
	p = strings.TrimRight(strings.TrimSpace(p), `/\`)
	if fold {
		p = strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	}
	return p
}

// Reasons a profile is not edited; the caller prints the line to add
// instead.
var (
	ErrNotWritable  = errors.New("the file is not writable")
	ErrDanglingLink = errors.New("the file is a symbolic link to a file that does not exist")
	ErrNotRegular   = errors.New("the file is not a regular file")
)

// Target returns the file to edit for profile path: path itself, or for a
// symbolic link (a dotfiles manager's ~/.zshrc, say) the file it points
// at, so the link is kept and the block reaches the real file. It refuses
// a dangling link, a non-regular file and a file that cannot be written.
// A missing path is returned as is (it will be created).
func Target(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	target := path
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return "", ErrDanglingLink
		}
		if fi, err = os.Stat(target); err != nil {
			return "", ErrDanglingLink
		}
	}
	if !fi.Mode().IsRegular() {
		return "", ErrNotRegular
	}
	if fi.Mode().Perm()&0o200 == 0 {
		return "", ErrNotWritable
	}
	f, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return "", ErrNotWritable
	}
	f.Close()
	return target, nil
}

// ReadProfile reads a profile; a missing file is empty.
func ReadProfile(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// BackupSuffix names the copy of a profile kept from before kenaz first
// edited it.
var BackupSuffix = "." + variant.Current.Binary + "-backup"

// WriteProfile writes next to target (a path Target returned), keeping an
// existing file's mode. Before kenaz first changes an existing file it
// keeps a copy as target + BackupSuffix; a later change (a new install
// directory) never overwrites that copy, so it stays the user's original.
// It returns the backup path when it wrote one.
func WriteProfile(target string, cur []byte, existed bool, next []byte) (backup string, err error) {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if existed {
		b := target + BackupSuffix
		if _, err := os.Lstat(b); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(b, cur, mode|0o600); err != nil {
				return "", err
			}
			backup = b
		}
	}
	tmp := target + "." + variant.Current.Binary + "-tmp"
	if err := os.WriteFile(tmp, next, mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return backup, nil
}

// UserEnv is the Windows per-user environment (HKCU\Environment). The
// registry implementation is in registry_windows.go; tests use a fake.
type UserEnv interface {
	// Path returns the user Path value ("" when unset) and whether it is
	// an expandable string (REG_EXPAND_SZ).
	Path() (value string, expand bool, err error)
	// SetPath writes the user Path, keeping its value type.
	SetPath(value string, expand bool) error
	// Broadcast tells running programs the environment changed (new
	// terminals then see the new Path).
	Broadcast()
}

// WindowsEdit returns the user Path with dir prepended, and whether that
// changes anything. Entries are compared after expanding %VAR% references
// with getenv (nil: no expansion), so an entry written as
// %LOCALAPPDATA%\... counts as dir.
func WindowsEdit(cur, dir string, getenv func(string) string) (string, bool) {
	if OnPath(ExpandPercent(cur, getenv), ExpandPercent(dir, getenv), ";", true) {
		return cur, false
	}
	if strings.TrimSpace(cur) == "" {
		return dir, true
	}
	return dir + ";" + cur, true
}

// RemoveFromWindowsPath drops every entry equal to dir (used by tests and
// to undo by hand).
func RemoveFromWindowsPath(cur, dir string) string {
	want := clean(dir, true)
	var out []string
	for _, e := range strings.Split(cur, ";") {
		if e != "" && clean(e, true) == want {
			continue
		}
		out = append(out, e)
	}
	return strings.Join(out, ";")
}

// ExpandPercent expands Windows %VAR% references with getenv (variable
// names compared case-insensitively by getenv's caller); unknown or
// unterminated references are kept as they are.
func ExpandPercent(s string, getenv func(string) string) string {
	if getenv == nil || !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v := getenv(name); name != "" && v != "" {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+2+j])
		}
		s = s[i+2+j:]
	}
}
