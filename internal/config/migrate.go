package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Breadcrumb is the file Migrate leaves in an old directory it moved
// files out of.
const Breadcrumb = "MOVED.txt"

// migrateLock is the lock file, next to the root, that one migrating
// process holds.
const migrateLock = ".plugin-migrate.lock"

// relayLock is the relay's singleton lock in each per-env state directory
// (the daemon package's LockFile).
const relayLock = "daemon.lock"

// Test seams.
var (
	rename   = os.Rename
	makeLink = platformLink // a directory link at link pointing to target
	now      = time.Now
)

// Locker takes a non-blocking exclusive lock on path, creating the file,
// and returns an error when another process holds it.
type Locker func(path string) (release func(), err error)

// MigrateOptions are what the caller supplies to move an earlier layout.
type MigrateOptions struct {
	// Lock takes the migration lock and each old environment's relay lock
	// (the daemon's own lock: a relay that holds it is running). Required.
	Lock Locker
	// Relink means the caller registers the apps again right after the
	// move (kenaz init), so the state may move where no link can be left
	// at the old location. Otherwise Migrate first checks that it can
	// leave one, so apps registered with the old plugin path keep working.
	Relink bool
	// Registered reports whether an app registration recorded in the
	// root still loads the plugin through src, an old state location the
	// root was moved from. A later run makes the link at src again only
	// then, so a link or folder the user deleted after `kenaz init`
	// registered the apps from the root stays deleted. nil: always.
	Registered func(src string) bool
}

// MigrateResult is what Migrate did.
type MigrateResult struct {
	// Moved lists the old locations whose files now live in the root.
	Moved []string
	// Unused lists old files Kenaz no longer uses because the root
	// already had its own; the user may delete them.
	Unused []string
	// Linked lists old state locations where a link to the root was
	// (re)made: one a crash or an earlier failure had left without it.
	Linked []string
	// LinkFailed says why no link to the root could be left at an old
	// state location the files moved out of, "" when there was none to
	// make or it was made. Apps registered with the old plugin path need
	// `kenaz init` then.
	LinkFailed string
	// Refused says why nothing (more) moved, "" when everything did.
	Refused string
}

// Nothing reports a run with nothing to move.
func (r MigrateResult) Nothing() bool {
	return len(r.Moved) == 0 && len(r.Unused) == 0 && len(r.Linked) == 0 && r.LinkFailed == "" && r.Refused == ""
}

// busyHint is what to do when an app uses the old layout.
const busyHint = "close Claude Code, Codex and Claude Desktop (and any other app that uses Kenaz), then try again"

// stateRoot is the earlier release's state root that holds files, or "".
func (l Legacy) stateRoot() string {
	for _, r := range l.StateRoots {
		if legacyDir(r) {
			return r
		}
	}
	return ""
}

// present reports whether an earlier release's files are still there:
// a state root (a real directory, not the link Migrate leaves) with files
// other than the breadcrumb, or a config.toml that is a regular file.
func (l Legacy) present() bool { return l.stateRoot() != "" || l.configPresent() }

func (l Legacy) configPresent() bool {
	if l.ConfigDir == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(l.ConfigDir, "config.toml"))
	return err == nil && fi.Mode().IsRegular()
}

func legacyDir(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	return len(contents(dir)) > 0
}

// contents lists dir's entries other than the breadcrumb and dotfiles
// (.DS_Store, the move record).
func contents(dir string) []os.DirEntry {
	ents, _ := os.ReadDir(dir)
	out := ents[:0]
	for _, e := range ents {
		if e.Name() != Breadcrumb && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e)
		}
	}
	return out
}

// The root's state besides the per-environment directories (State).
const (
	// MemoryDir holds the on-device memory store (internal/memory.DirName).
	MemoryDir = "memory"
	// CredentialsDir records the credential store choice and, with the
	// file store, the credentials.
	CredentialsDir = "credentials"
	// MemoryDisclosureMarker records that the memory notice was shown
	// (internal/memory.DisclosureMarker).
	MemoryDisclosureMarker = "memory-disclosure-shown"
)

// stateEntry reports whether e, an entry of the root, is state the layout
// defines: a Fleet environment's directory, memory/, credentials/ or the
// memory notice marker. A directory counts only when it holds something
// (dotfiles aside). config.toml and its temporary files, dotfiles
// (.DS_Store) and anything else are not state.
func stateEntry(root string, e os.DirEntry) bool {
	name := e.Name()
	switch {
	case name == MemoryDisclosureMarker:
		return e.Type().IsRegular()
	case name == MemoryDir, name == CredentialsDir, fleetapi.Env(name).Valid():
		return e.IsDir() && len(contents(filepath.Join(root, name))) > 0
	}
	return false
}

// rootHasState reports whether the root holds state (stateEntry).
func (p Paths) rootHasState() bool {
	ents, _ := os.ReadDir(p.StateRoot)
	for _, e := range ents {
		if stateEntry(p.StateRoot, e) {
			return true
		}
	}
	return false
}

// movedFrom is the file, in the root, recording the old state location
// the root was renamed from. moveState writes it into the old state root
// just before the rename, so it moves with the files in the same atomic
// step and the link at the old location can be made (again) on any later
// run (repairLinks).
const movedFrom = ".moved-from"

// Migrate moves an earlier release's files into the root, explicitly
// (`kenaz migrate`, `kenaz init`), atomically, or not at all:
//
//   - Only with no relay running in the old state (its lock is free);
//     otherwise it refuses and asks to close the apps.
//   - The state root moves with one rename, then config.toml with
//     another. Each is used from wherever it is (Current), so stopping
//     between the two hides nothing.
//   - A rename refused for any reason (another volume, a file an app has
//     open on Windows, permissions) changes nothing: those files stay in
//     use where they are. Nothing is ever copied or deleted.
//   - When the root already holds state (a newer build created it first),
//     nothing moves; the old files are reported as unused.
//   - A link to the root is left at the old state location (a junction on
//     Windows where symlinks are not allowed), so apps registered with the
//     old plugin path keep working, with a Breadcrumb where none can be
//     made. Every run makes the link again where a crash or a failure left
//     the old location missing or empty (repairLinks). Without Relink,
//     Migrate refuses to move where no link can be made.
func (p Paths) Migrate(o MigrateOptions) (res MigrateResult) {
	if !p.Legacy.present() && len(p.linksToRepair(o.Registered)) == 0 {
		return res
	}
	parent := filepath.Dir(p.StateRoot)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		res.Refused = err.Error()
		return res
	}
	unlock, err := o.Lock(filepath.Join(parent, migrateLock))
	if err != nil {
		res.Refused = "another kenaz process is moving the files"
		return res
	}
	defer unlock()
	defer p.repairLinks(&res, o.Registered)

	src := p.Legacy.stateRoot()
	switch {
	case src != "" && p.rootHasState():
		res.Unused = append(res.Unused, src)
	case src != "":
		aside, msg := p.moveState(src, o)
		if aside != "" {
			res.Unused = append(res.Unused, aside)
		}
		if msg != "" {
			res.Refused = msg
			return res
		}
		res.Moved = append(res.Moved, src)
	}
	// Further old state roots (Windows: the %AppData% one of 0.4.x).
	for _, r := range p.Legacy.StateRoots {
		if r != src && legacyDir(r) {
			res.Unused = append(res.Unused, r)
		}
	}

	if p.Legacy.configPresent() {
		old := filepath.Join(p.Legacy.ConfigDir, "config.toml")
		if exists(p.ConfigFile()) {
			res.Unused = append(res.Unused, old)
		} else if err := rename(old, p.ConfigFile()); err != nil {
			res.Refused = "config.toml stays in " + p.Legacy.ConfigDir + ": " + reason(err)
			return res
		} else {
			_ = os.Symlink(p.ConfigFile(), old) // best effort: Windows needs a privilege for it
			writeBreadcrumb(p.Legacy.ConfigDir, p.ConfigDir)
			res.Moved = append(res.Moved, p.Legacy.ConfigDir)
		}
	}
	return res
}

// moveState renames the old state root src to the root; it returns why
// not, with nothing changed. aside is where a root holding no state but
// other entries was moved out of the way.
func (p Paths) moveState(src string, o MigrateOptions) (aside, msg string) {
	if !o.Relink && !canLink(filepath.Dir(src), filepath.Dir(p.StateRoot)) {
		return "", "no link can be left at " + src + " for apps registered there; run `" + variant.Current.Binary + " init`, which moves the files and registers the apps again"
	}
	var held []func()
	release := func() {
		for _, r := range held {
			r()
		}
		held = nil
	}
	defer release()
	for _, envDir := range relayDirs(src) {
		rel, err := o.Lock(filepath.Join(envDir, relayLock))
		if err != nil {
			return "", "Kenaz is running from " + src + "; " + busyHint
		}
		held = append(held, rel)
	}
	// Windows cannot rename a directory with an open file in it.
	if runtime.GOOS == "windows" {
		release()
	}
	// The root may exist holding only config.toml (written while the
	// state was still in the old location) and entries that are not state:
	// move them out of the way first.
	if exists(p.StateRoot) {
		if aside, msg = p.clearRoot(); msg != "" {
			return aside, msg
		}
	}
	record := filepath.Join(src, movedFrom)
	if err := os.WriteFile(record, []byte(src+"\n"), 0o600); err != nil {
		return aside, "the files stay in " + src + ": " + reason(err)
	}
	if err := rename(src, p.StateRoot); err != nil {
		_ = os.Remove(record)
		return aside, "the files stay in " + src + ": " + reason(err)
	}
	return aside, ""
}

// clearRoot empties a root that holds no state, so the old state root can
// be renamed onto it. The root's config.toml is the one in use (Current):
// it moves to the old config location, where it stays in use, and an old
// config.toml it replaces is kept as config.toml.unused-*. Anything else
// left in the root (a config.toml.tmp, .DS_Store, empty directories) moves
// with the root directory to aside, plugin.unused-*. Every step is one
// rename, and after each one Current reads the same config and state.
func (p Paths) clearRoot() (aside, msg string) {
	stamp := now().UTC().Format("20060102T150405Z")
	if cfg := p.ConfigFile(); exists(cfg) {
		if p.Legacy.ConfigDir == "" {
			return "", p.StateRoot + " already exists"
		}
		old := filepath.Join(p.Legacy.ConfigDir, "config.toml")
		if p.Legacy.configPresent() {
			if err := rename(old, old+".unused-"+stamp); err != nil {
				return "", "the files stay where they are: " + reason(err)
			}
		}
		if err := os.MkdirAll(p.Legacy.ConfigDir, 0o700); err != nil {
			return "", err.Error()
		}
		if err := rename(cfg, old); err != nil {
			return "", "the files stay where they are: " + reason(err)
		}
	}
	if os.Remove(p.StateRoot) == nil {
		return "", ""
	}
	aside = p.StateRoot + ".unused-" + stamp
	if err := rename(p.StateRoot, aside); err != nil {
		return "", p.StateRoot + " is not empty: " + reason(err)
	}
	return aside, ""
}

// linksToRepair lists the old state locations the root was moved from
// (movedFrom) that hold no link to it: missing (a crash between the
// rename and the link), or a real directory with nothing in it but the
// breadcrumb (an earlier link failure, or on Windows a crash between
// creating the junction's directory and making it a junction).
// Only those registered reports true for (nil: all).
func (p Paths) linksToRepair(registered func(string) bool) []string {
	b, err := os.ReadFile(filepath.Join(p.StateRoot, movedFrom))
	if err != nil {
		return nil
	}
	var out []string
	for _, src := range strings.Split(string(b), "\n") {
		src = strings.TrimSpace(src)
		if src == "" || !filepath.IsAbs(src) || !p.ownLegacyRoot(src) || (registered != nil && !registered(src)) {
			continue
		}
		fi, err := os.Lstat(src)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out = append(out, src)
		case err == nil && fi.IsDir() && onlyBreadcrumb(src):
			out = append(out, src)
		}
	}
	return out
}

func (p Paths) ownLegacyRoot(dir string) bool {
	for _, r := range p.Legacy.StateRoots {
		if r == dir {
			return true
		}
	}
	return false
}

// onlyBreadcrumb reports whether dir holds nothing but the breadcrumb.
func onlyBreadcrumb(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.Name() != Breadcrumb {
			return false
		}
	}
	return true
}

// repairLinks makes the link at each old state location the root was
// moved from where it is missing (linksToRepair). Where none can be made
// it leaves the breadcrumb and says why in res.LinkFailed.
func (p Paths) repairLinks(res *MigrateResult, registered func(string) bool) {
	if !p.rootHasState() {
		return
	}
	// The location just moved from always gets its link: apps were
	// registered there until now.
	justMoved := func(src string) bool {
		return slices.Contains(res.Moved, src) || registered == nil || registered(src)
	}
	for _, src := range p.linksToRepair(justMoved) {
		if exists(src) {
			_ = os.Remove(filepath.Join(src, Breadcrumb))
			if err := os.Remove(src); err != nil {
				res.LinkFailed = "no link to " + p.StateRoot + " could be left at " + src + ": " + err.Error()
				writeBreadcrumb(src, p.StateRoot)
				continue
			}
		} else if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
			res.LinkFailed = "no link to " + p.StateRoot + " could be left at " + src + ": " + err.Error()
			continue
		}
		if err := makeLink(p.StateRoot, src); err != nil {
			res.LinkFailed = "no link to " + p.StateRoot + " could be left at " + src + ": " + err.Error()
			writeBreadcrumb(src, p.StateRoot)
			continue
		}
		if !slices.Contains(res.Moved, src) {
			res.Linked = append(res.Linked, src)
		}
	}
}

func reason(err error) string {
	if crossDevice(err) {
		return "it is on a different volume than the new location, so Kenaz keeps using it there"
	}
	return err.Error() + " (" + busyHint + ")"
}

// relayDirs lists root's per-environment directories that hold (or held)
// a relay.
func relayDirs(root string) []string {
	ents, _ := os.ReadDir(root)
	var out []string
	for _, e := range ents {
		d := filepath.Join(root, e.Name())
		if e.IsDir() && (exists(filepath.Join(d, relayLock)) || exists(filepath.Join(d, "daemon.json"))) {
			out = append(out, d)
		}
	}
	return out
}

// canLink reports whether a directory link to target can be made in dir.
func canLink(dir, target string) bool {
	var b [6]byte
	_, _ = rand.Read(b[:])
	probe := filepath.Join(dir, ".plugin-link-probe-"+hex.EncodeToString(b[:]))
	if makeLink(target, probe) != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func writeBreadcrumb(dir, root string) {
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		return // a link: nothing to write into
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	msg := fmt.Sprintf("Kenaz keeps its files in %s now. It moved them there from this directory.\nRun `%s init` if an app still uses Kenaz from this directory.\n", root, variant.Current.Binary)
	_ = os.WriteFile(filepath.Join(dir, Breadcrumb), []byte(msg), 0o600)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// DirLink makes link a directory link to target the way Migrate leaves one
// at an old location (a symlink, or on Windows a junction where symlinks
// are not allowed). For tests of code that meets such links.
func DirLink(target, link string) error { return makeLink(target, link) }

// OldInUse lists the earlier release's locations Current still uses.
func (p Paths) OldInUse() []string {
	cur := p.Current()
	var out []string
	if cur.StateRoot != p.StateRoot {
		out = append(out, cur.StateRoot)
	}
	if cur.ConfigDir != p.ConfigDir {
		out = append(out, filepath.Join(cur.ConfigDir, "config.toml"))
	}
	return out
}

// Unused lists the earlier release's files Current does not use (the root
// has its own); the user may delete them.
func (p Paths) Unused() []string {
	cur := p.Current()
	var out []string
	for _, r := range p.Legacy.StateRoots {
		if legacyDir(r) && r != cur.StateRoot {
			out = append(out, r)
		}
	}
	if p.Legacy.configPresent() && cur.ConfigDir != p.Legacy.ConfigDir {
		out = append(out, filepath.Join(p.Legacy.ConfigDir, "config.toml"))
	}
	// What clearRoot put aside (also after a crash before the move that
	// followed, which then never reported it).
	parent, prefix := filepath.Dir(p.StateRoot), filepath.Base(p.StateRoot)+".unused-"
	ents, _ := os.ReadDir(parent)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	return out
}
