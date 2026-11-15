// Package pathenv puts the kenaz executable's directory on the user's PATH.
// On macOS and Linux it adds one marked block to the
// login shell's profile (zsh, bash or fish); on Windows it prepends the
// directory to the user Path in the registry. Every edit is idempotent:
// a second run with the same directory changes nothing, and a run with a
// new directory replaces the block instead of adding another.
package pathenv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	ErrLinkLoop     = errors.New("the file is a symbolic link that leads back to itself")
	ErrNotRegular   = errors.New("the file is not a regular file")
	// ErrChanged means the file (or, from WriteWindowsPath, the user
	// Path) no longer holds what was read before the question was asked;
	// writing would lose the newer edit.
	ErrChanged = errors.New("the file changed after " + variant.Current.Binary + " init read it, so nothing was written; run " + variant.Current.Binary + " init again to retry")
)

// Target returns the file to edit for profile path: path itself, or for a
// symbolic link (a dotfiles manager's ~/.zshrc, say) the file it points
// at, so the link is kept and the block reaches the real file. It refuses
// a dangling link, a link loop, a non-regular file and a file that cannot
// be written.
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
			return "", linkError(err)
		}
		if fi, err = os.Stat(target); err != nil {
			return "", linkError(err)
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

// linkError names why a symbolic link could not be followed: its target
// is missing, or the links form a loop. Anything else (a directory on the
// way that cannot be searched, say) is reported as it is.
func linkError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrDanglingLink
	case errors.Is(err, syscall.ELOOP), strings.Contains(err.Error(), "too many links"):
		// filepath.EvalSymlinks gives up with a plain "too many links"
		// error; the operating system reports ELOOP.
		return ErrLinkLoop
	}
	return err
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

// TmpSuffix names the file WriteProfile writes next to a profile and then
// renames over it.
var TmpSuffix = "." + variant.Current.Binary + "-tmp"

// WriteProfile writes next to target (a path Target returned), keeping an
// existing file's permission bits exactly (the umask does not apply). A
// profile that did not exist gets what any new file gets: 0666 less the
// umask.
// cur and existed are what ReadProfile returned before the user was
// asked: if the file has changed since, nothing is written and the error
// is ErrChanged, so an edit made while the question was open is not lost.
// Before kenaz first changes an existing file it keeps a copy as target +
// BackupSuffix; a later change (a new install directory) never overwrites
// that copy, so it stays the user's original. It returns the backup path
// when it wrote one.
//
// The file is replaced by renaming a new one over it, so a crash or a
// full disk cannot leave half a profile. The cost is that the new file
// does not carry over extended attributes or ACLs, and is no longer the
// same file as any other hard link to the old one.
func WriteProfile(target string, cur []byte, existed bool, next []byte) (backup string, err error) {
	defer func() {
		// A backup is kept only next to a profile kenaz did change.
		if err != nil && backup != "" {
			os.Remove(backup)
			backup = ""
		}
	}()
	// A new profile is created with newFileMode and left to the umask; an
	// existing one keeps its mode exactly.
	mode, exact := os.FileMode(newFileMode), false
	if fi, err := os.Stat(target); err == nil {
		mode, exact = fi.Mode().Perm(), true
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := unchanged(target, cur, existed); err != nil {
		return "", err
	}
	if existed {
		b := target + BackupSuffix
		if _, err := os.Lstat(b); errors.Is(err, os.ErrNotExist) {
			if err := writeFileMode(b, cur, mode|0o600, true); err != nil {
				return "", err
			}
			backup = b
		}
	}
	tmp := target + TmpSuffix
	// A file left by an interrupted run would keep its own mode (and, as
	// a link, its own target): start from a new one.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return backup, err
	}
	if err := writeFileMode(tmp, next, mode, exact); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	// Checked again as late as possible: the backup and the new file are
	// written, and only the rename is left.
	if err := unchanged(target, cur, existed); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	return backup, nil
}

// unchanged reports ErrChanged unless target still holds cur (or is still
// missing, when it did not exist).
func unchanged(target string, cur []byte, existed bool) error {
	now, exists, err := ReadProfile(target)
	if err != nil {
		return err
	}
	if exists != existed || !bytes.Equal(now, cur) {
		return ErrChanged
	}
	return nil
}

// newFileMode is what a profile that did not exist is created with; the
// umask then takes its bits away, as for any new file.
const newFileMode = 0o666

// writeFileMode creates path (which must not exist) with data. With exact
// it has exactly mode: creating it alone would take the umask's bits
// away. Without, it is left as created, mode less the umask.
func writeFileMode(path string, data []byte, mode os.FileMode, exact bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !exact {
		return nil
	}
	return os.Chmod(path, mode)
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
// changes anything. When the Path is an expandable string (expand, a
// REG_EXPAND_SZ value), its entries are compared after expanding %VAR%
// references with getenv (nil: no expansion), so an entry written as
// %LOCALAPPDATA%\... counts as dir. In a plain string (REG_SZ) Windows
// does not expand them, so such an entry is a literal folder name and is
// compared as written. dir is a real folder and is never expanded.
func WindowsEdit(cur, dir string, expand bool, getenv func(string) string) (string, bool) {
	have := cur
	if expand {
		have = ExpandPercent(cur, getenv)
	}
	if OnPath(have, dir, ";", true) {
		return cur, false
	}
	if strings.TrimSpace(cur) == "" {
		return dir, true
	}
	return dir + ";" + cur, true
}

// WriteWindowsPath sets the user Path to next. cur and expand are what
// ue.Path returned before the user was asked: the Path is read again
// first, and if its value or its type has changed since, nothing is
// written and the error is ErrChanged, so a change made while the
// question was open (by an installer, or in the Environment Variables
// dialog) is not lost. The registry has no compare-and-set, so a change
// made between this read and the write is still overwritten.
func WriteWindowsPath(ue UserEnv, cur string, expand bool, next string) error {
	now, nowExpand, err := ue.Path()
	if err != nil {
		return err
	}
	if now != cur || nowExpand != expand {
		return ErrChanged
	}
	return ue.SetPath(next, expand)
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
// names compared case-insensitively by getenv's caller), the way Windows
// expands a REG_EXPAND_SZ value: when the text between two percent signs
// is not a set variable (or is empty, as in "%%"), the first percent sign
// is kept as it is and the scan goes on from the character after it, so
// the second one can still open a reference. "%%A%%" is therefore "%",
// the value of A, "%".
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
			s = s[i+2+j:]
			continue
		}
		b.WriteByte('%')
		s = s[i+1:]
	}
}
