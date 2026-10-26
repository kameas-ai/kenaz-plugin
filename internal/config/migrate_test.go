package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// fakeTree writes files (slash paths relative to base; a trailing "/"
// makes a directory).
func fakeTree(t *testing.T, base string, files map[string]string) {
	t.Helper()
	for rel, c := range files {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// listTree returns every file under dir with its content, following no
// links.
func listTree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.Name() == migrateLock || d.Name() == movedFrom {
			return nil // the migration's own lock and record, not data
		}
		if !d.Type().IsRegular() {
			out = append(out, filepath.ToSlash(rel)+" (link)")
			return nil
		}
		b, _ := os.ReadFile(p)
		out = append(out, filepath.ToSlash(rel)+"="+string(b))
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// macLayout is a temp HOME with the macOS layout of releases up to 0.6.x.
func macLayout(t *testing.T) (Paths, string) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := pathsFor("darwin", envMap(nil), func() (string, error) { return home, nil },
		func() (string, error) { return filepath.Join(home, "Library", "Application Support"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return p, home
}

func lockOpts() MigrateOptions { return MigrateOptions{Lock: daemon.TryLock} }

var oldInstall = map[string]string{
	"config.toml":                         "env = \"prod\"\nmemory = \"on\"\n",
	"state/credentials/STORE":             "files\n",
	"state/memory/memory.db":              "notes",
	"state/prod/setup-state.json":         `{"version":1}`,
	"state/prod/daemon.lock":              "",
	"state/prod/spool/queued":             "batch",
	"state/prod/setup/bundle/x/plugin.js": "plugin",
	"state/dev/consent.json":              "{}",
}

// sameDir reports whether a and b are the same directory (a may be a
// symlink or a junction to b).
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// failRename makes renames matching match fail with err.
func failRename(t *testing.T, match func(from, to string) bool, err error) {
	t.Helper()
	orig := rename
	rename = func(from, to string) error {
		if match(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
		}
		return orig(from, to)
	}
	t.Cleanup(func() { rename = orig })
}

// readsAs checks what Current resolves: config.toml's env and that the
// state has the sign-in store and the queue.
func readsAs(t *testing.T, p Paths, env string) {
	t.Helper()
	cur := p.Current()
	f, err := LoadFile(cur.ConfigFile())
	if err != nil || f.Get("env") != env {
		t.Errorf("config.toml from %s: %v %v", cur.ConfigDir, f.Values, err)
	}
	for _, rel := range []string{"credentials/STORE", "memory/memory.db", "prod/spool/queued"} {
		if !exists(filepath.Join(cur.StateRoot, filepath.FromSlash(rel))) {
			t.Errorf("%s missing from %s", rel, cur.StateRoot)
		}
	}
}

// A new install uses the root; an existing one keeps working where its
// files are, for every command, with nothing moved.
func TestCurrentKeepsOldLayoutInPlace(t *testing.T) {
	p, home := macLayout(t)
	if got := p.Current(); fmt.Sprint(got) != fmt.Sprint(p) {
		t.Fatalf("new install: %+v", got)
	}
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	before := listTree(t, home)
	got := p.Current()
	if got.StateRoot != p.Legacy.StateRoots[0] || got.ConfigDir != p.Legacy.ConfigDir {
		t.Fatalf("old install: %+v", got)
	}
	readsAs(t, p, "prod")
	if listTree(t, home) != before || exists(filepath.Dir(p.StateRoot)) {
		t.Fatal("Current changed the disk")
	}
	// A config.toml written to the root meanwhile (no old one) leaves the
	// state where it is.
	q, _ := macLayout(t)
	fakeTree(t, q.Legacy.ConfigDir, map[string]string{"state/prod/x": "y"})
	fakeTree(t, q.StateRoot, map[string]string{"config.toml": "env = \"dev\"\n"})
	if got := q.Current(); got.StateRoot != q.Legacy.StateRoots[0] || got.ConfigDir != q.ConfigDir {
		t.Fatalf("root config, old state: %+v", got)
	}
}

// kenaz migrate on one volume: one rename for the state, one for
// config.toml; a link and breadcrumbs at the old locations; a second run
// does nothing.
func TestMigrateMoves(t *testing.T) {
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	want := strings.ReplaceAll(listTree(t, p.Legacy.ConfigDir), "state/", "")
	res := p.Migrate(lockOpts())
	if res.Refused != "" || len(res.Moved) != 2 || len(res.Unused) != 0 {
		t.Fatalf("%+v", res)
	}
	if have := listTree(t, p.StateRoot); have != want {
		t.Fatalf("root:\n%s\nwant:\n%s", have, want)
	}
	if got := p.Current(); fmt.Sprint(got) != fmt.Sprint(p) {
		t.Fatalf("Current after the move: %+v", got)
	}
	// Apps registered with the old plugin path still find it.
	if c, err := os.ReadFile(filepath.Join(p.Legacy.StateRoots[0], "prod", "setup", "bundle", "x", "plugin.js")); err != nil || string(c) != "plugin" {
		t.Fatalf("through the old path: %q %v", c, err)
	}
	if !sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
		t.Fatal("no link at the old state location")
	}
	if b, _ := os.ReadFile(filepath.Join(p.Legacy.ConfigDir, Breadcrumb)); !strings.Contains(string(b), p.StateRoot) {
		t.Fatalf("breadcrumb: %q", b)
	}
	if res := p.Migrate(lockOpts()); !res.Nothing() {
		t.Fatalf("second run: %+v", res)
	}
}

// Linux kept config and state in separate XDG directories.
func TestMigrateLinuxLayout(t *testing.T) {
	home := t.TempDir()
	p, _ := pathsFor("linux", envMap(nil), func() (string, error) { return home, nil }, nil)
	fakeTree(t, p.Legacy.ConfigDir, map[string]string{"config.toml": "env = \"dev\"\n"})
	fakeTree(t, p.Legacy.StateRoots[0], map[string]string{"dev/daemon.json": "{}", "credentials/STORE": "files\n"})
	if res := p.Migrate(lockOpts()); res.Refused != "" || len(res.Moved) != 2 {
		t.Fatalf("%+v", res)
	}
	if have := listTree(t, p.StateRoot); have != "config.toml=env = \"dev\"\n\ncredentials/STORE=files\n\ndev/daemon.json={}\ndev/daemon.lock=" {
		t.Fatalf("root:\n%s", have)
	}
}

// A running relay holds its lock: migrate refuses, asks to close the
// apps, and changes nothing.
func TestMigrateRefusesWhileBusy(t *testing.T) {
	p, home := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	before := listTree(t, home)
	rel, err := daemon.TryLock(filepath.Join(p.Legacy.StateRoots[0], "prod", "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	res := p.Migrate(lockOpts())
	rel()
	if !strings.Contains(res.Refused, "close Claude Code, Codex and Claude Desktop") || len(res.Moved) != 0 {
		t.Fatalf("%+v", res)
	}
	if listTree(t, home) != before {
		t.Fatal("changed something")
	}
	readsAs(t, p, "prod")
	// Once the apps are closed it moves.
	if res := p.Migrate(lockOpts()); res.Refused != "" {
		t.Fatalf("after closing: %+v", res)
	}
	readsAs(t, p, "prod")
}

// Any refused rename (another volume, permissions, a file an app has
// open) changes nothing: the files stay in use where they are.
func TestMigrateRefusedRenameChangesNothing(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"cross-volume": {errCrossDevice, "different volume"},
		"EACCES":       {syscall.EACCES, "close Claude Code"},
		"open file":    {syscallBusy(), "close Claude Code"},
	} {
		t.Run(name, func(t *testing.T) {
			p, home := macLayout(t)
			fakeTree(t, p.Legacy.ConfigDir, oldInstall)
			before := listTree(t, home)
			failRename(t, func(from, to string) bool { return from == p.Legacy.StateRoots[0] }, c.err)
			res := p.Migrate(lockOpts())
			if !strings.Contains(res.Refused, c.want) || len(res.Moved) != 0 {
				t.Fatalf("%+v", res)
			}
			if listTree(t, home) != before {
				t.Fatalf("changed:\n%s\nwas:\n%s", listTree(t, home), before)
			}
			readsAs(t, p, "prod")
		})
	}
}

// The state moved and config.toml could not: each is used from where it
// is; the next run moves config.toml.
func TestMigrateConfigLeftBehind(t *testing.T) {
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	failRename(t, func(from, _ string) bool { return strings.HasSuffix(from, "config.toml") }, errCrossDevice)
	res := p.Migrate(lockOpts())
	if len(res.Moved) != 1 || !strings.Contains(res.Refused, "config.toml stays") {
		t.Fatalf("%+v", res)
	}
	if cur := p.Current(); cur.StateRoot != p.StateRoot || cur.ConfigDir != p.Legacy.ConfigDir {
		t.Fatalf("%+v", cur)
	}
	readsAs(t, p, "prod")
	rename = os.Rename
	if res := p.Migrate(lockOpts()); res.Refused != "" || len(res.Moved) != 1 {
		t.Fatalf("second run: %+v", res)
	}
	readsAs(t, p, "prod")
}

// A crash after any rename (simulated by stopping there) never hides or
// loses a file: every component is read from wherever it is, and the next
// run finishes the move.
func TestMigrateInterruptedAnywhere(t *testing.T) {
	for _, withRootConfig := range []bool{false, true} {
		for k := 1; k <= 6; k++ {
			t.Run(fmt.Sprintf("rootconfig=%v/stop=%d", withRootConfig, k), func(t *testing.T) {
				p, home := macLayout(t)
				fakeTree(t, p.Legacy.ConfigDir, oldInstall)
				env := "prod"
				if withRootConfig {
					// Written to the root while the state was still old: it
					// is the config in use.
					fakeTree(t, p.StateRoot, map[string]string{"config.toml": "env = \"stage\"\n"})
					env = "stage"
				}
				readsAs(t, p, env)
				n := 0
				orig := rename
				rename = func(from, to string) error {
					if n++; n == k {
						return errors.New("stopped here")
					}
					return orig(from, to)
				}
				p.Migrate(lockOpts())
				rename = orig
				readsAs(t, p, env)
				for c := range map[string]bool{"files\n": true, "notes": true, "batch": true, "plugin": true} {
					if !strings.Contains(listTree(t, home), "="+c) {
						t.Errorf("%q gone from the disk", c)
					}
				}
				if res := p.Migrate(lockOpts()); res.Refused != "" {
					t.Fatalf("rerun: %+v", res)
				}
				readsAs(t, p, env)
				if cur := p.Current(); fmt.Sprint(cur) != fmt.Sprint(p) {
					t.Fatalf("not finished: %+v", cur)
				}
			})
		}
	}
}

// The root already holds state (kenaz-dev created it first, or a second
// old state root on Windows): nothing moves or merges; the root stays in
// use and the old files are reported as unused.
func TestMigrateTargetExists(t *testing.T) {
	p, home := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	fakeTree(t, p.StateRoot, map[string]string{"dev/consent.json": "new"})
	before := listTree(t, home)
	if cur := p.Current(); cur.StateRoot != p.StateRoot || cur.ConfigDir != p.Legacy.ConfigDir {
		t.Fatalf("Current: %+v", cur)
	}
	res := p.Migrate(lockOpts())
	if len(res.Unused) != 1 || res.Unused[0] != p.Legacy.StateRoots[0] || res.Refused != "" {
		t.Fatalf("%+v", res)
	}
	// The old state is untouched; config.toml (the root had none) moved.
	oldState := strings.ReplaceAll(listTree(t, p.Legacy.StateRoots[0]), "\n", "|")
	if !strings.Contains(before, "state/prod/spool/queued=batch") || !strings.Contains(oldState, "prod/spool/queued=batch") || !exists(p.ConfigFile()) {
		t.Fatalf("old state touched or config not moved:\n%s", listTree(t, home))
	}
	if u := p.Unused(); len(u) != 1 || u[0] != p.Legacy.StateRoots[0] {
		t.Fatalf("Unused %v", u)
	}

	// Windows: the 0.4.x %AppData% state next to the newer one.
	base := t.TempDir()
	w := Paths{ConfigDir: filepath.Join(base, "new"), StateRoot: filepath.Join(base, "new"), Legacy: Legacy{
		ConfigDir:  filepath.Join(base, "roaming"),
		StateRoots: []string{filepath.Join(base, "local", "state"), filepath.Join(base, "roaming", "state")},
	}}
	fakeTree(t, w.Legacy.StateRoots[0], map[string]string{"prod/q": "local"})
	fakeTree(t, w.Legacy.StateRoots[1], map[string]string{"prod/q": "roaming"})
	res = w.Migrate(lockOpts())
	if len(res.Moved) != 1 || len(res.Unused) != 1 || res.Unused[0] != w.Legacy.StateRoots[1] {
		t.Fatalf("two roots: %+v", res)
	}
}

// Where no link can be made at the old location, only kenaz init (which
// registers the apps again) moves the state.
func TestMigrateNeedsLinkOrRelink(t *testing.T) {
	p, home := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	orig := makeLink
	makeLink = func(string, string) error { return errors.New("no links here") }
	defer func() { makeLink = orig }()
	before := listTree(t, home)
	if res := p.Migrate(lockOpts()); !strings.Contains(res.Refused, "init") || listTree(t, home) != before {
		t.Fatalf("without relink: %+v", res)
	}
	o := lockOpts()
	o.Relink = true
	if res := p.Migrate(o); res.Refused != "" {
		t.Fatalf("relink: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(p.Legacy.StateRoots[0], Breadcrumb)); err != nil || !strings.Contains(string(b), p.StateRoot) {
		t.Fatalf("breadcrumb: %q %v", b, err)
	}
	if p.Legacy.present() {
		t.Fatal("still present")
	}
	readsAs(t, p, "prod")
}

// Two migrating processes at once: one moves, the other refuses or finds
// nothing left; nothing is lost.
func TestMigrateConcurrent(t *testing.T) {
	for i := 0; i < 20; i++ {
		p, _ := macLayout(t)
		fakeTree(t, p.Legacy.ConfigDir, oldInstall)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; p.Migrate(lockOpts()) }()
		}
		close(start)
		wg.Wait()
		p.Migrate(lockOpts())
		readsAs(t, p, "prod")
		if cur := p.Current(); fmt.Sprint(cur) != fmt.Sprint(p) {
			t.Fatalf("run %d: %+v", i, cur)
		}
	}
}

// A real permission refusal (the old location is read-only).
func TestRealPermissionRefusal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions as a non-root user")
	}
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	before := listTree(t, p.Legacy.ConfigDir)
	os.Chmod(p.Legacy.ConfigDir, 0o500)
	res := p.Migrate(lockOpts())
	os.Chmod(p.Legacy.ConfigDir, 0o700)
	if res.Refused == "" || listTree(t, p.Legacy.ConfigDir) != before {
		t.Fatalf("read-only: %+v", res)
	}
	readsAs(t, p, "prod")
}

// The root's state entries are the ones the layout defines.
func TestStateEntryNames(t *testing.T) {
	if MemoryDir != memory.DirName || MemoryDisclosureMarker != memory.DisclosureMarker {
		t.Fatal("config's names for the memory store differ from internal/memory's")
	}
	for name, files := range map[string]map[string]string{
		"env dir":     {"stage/consent.json": "{}"},
		"memory":      {"memory/memory.db": "notes"},
		"credentials": {"credentials/STORE": "files\n"},
		"notice":      {MemoryDisclosureMarker: ""},
	} {
		p, _ := macLayout(t)
		fakeTree(t, p.StateRoot, files)
		if !p.rootHasState() {
			t.Errorf("%s: not state", name)
		}
	}
}

// Entries that are not state (a SetValue temp file, Finder's .DS_Store,
// empty directories) neither hide the old state nor make it "unused": it
// stays in use, and migrate moves it, putting those entries aside.
func TestRootJunkIsNotState(t *testing.T) {
	for name, junk := range map[string]map[string]string{
		"config.toml.tmp": {"config.toml.tmp": "env = \"dev\"\n"},
		".DS_Store":       {".DS_Store": "x"},
		"empty dir":       {"foo/": ""},
		"empty env dir":   {"prod/": "", "memory/.DS_Store": "x"},
		"with config":     {"config.toml": "env = \"prod\"\n", "config.toml.tmp": "x", ".DS_Store": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := macLayout(t)
			fakeTree(t, p.Legacy.ConfigDir, oldInstall)
			fakeTree(t, p.StateRoot, junk)
			if cur := p.Current(); cur.StateRoot != p.Legacy.StateRoots[0] {
				t.Fatalf("Current: %+v", cur)
			}
			for _, u := range p.Unused() {
				if u == p.Legacy.StateRoots[0] {
					t.Fatalf("old state called unused: %v", u)
				}
			}
			readsAs(t, p, "prod")
			res := p.Migrate(lockOpts())
			if res.Refused != "" || len(res.Moved) == 0 || len(res.Unused) != 1 || !strings.Contains(res.Unused[0], ".unused-") {
				t.Fatalf("%+v", res)
			}
			readsAs(t, p, "prod")
			if cur := p.Current(); fmt.Sprint(cur) != fmt.Sprint(p) {
				t.Fatalf("not moved: %+v", cur)
			}
			if !sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
				t.Fatal("no link")
			}
		})
	}
}

// The link at the old state location is made again by the next run when
// a crash (or a failure) left it out: between the rename and the link, or
// on Windows between creating the junction's directory and making it a
// junction; or the link failed and left the breadcrumb.
func TestMigrateRepairsLink(t *testing.T) {
	type stop struct{}
	for name, broken := range map[string]func(target, link string) error{
		"crash after rename": func(string, string) error { panic(stop{}) },
		"crash in junction":  func(_, link string) error { os.Mkdir(link, 0o700); panic(stop{}) },
		"link failed":        func(string, string) error { return errors.New("no links here") },
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := macLayout(t)
			fakeTree(t, p.Legacy.ConfigDir, oldInstall)
			orig := makeLink
			makeLink = func(target, link string) error {
				if strings.Contains(filepath.Base(link), "link-probe") {
					return orig(target, link)
				}
				return broken(target, link)
			}
			res := func() (res MigrateResult) {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(stop); !ok {
							panic(r)
						}
					}
				}()
				return p.Migrate(lockOpts())
			}()
			makeLink = orig
			if name == "link failed" && !strings.Contains(res.LinkFailed, "no links here") {
				t.Fatalf("link failure not reported: %+v", res)
			}
			if sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
				t.Fatal("linked already")
			}
			if cur := p.Current(); cur.StateRoot != p.StateRoot {
				t.Fatalf("state: %+v", cur)
			}
			res = p.Migrate(lockOpts())
			if res.Refused != "" || res.LinkFailed != "" || len(res.Linked) != 1 || res.Linked[0] != p.Legacy.StateRoots[0] {
				t.Fatalf("rerun: %+v", res)
			}
			if !sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
				t.Fatal("not repaired")
			}
			if c, err := os.ReadFile(filepath.Join(p.Legacy.StateRoots[0], "prod", "setup", "bundle", "x", "plugin.js")); err != nil || string(c) != "plugin" {
				t.Fatalf("through the old path: %q %v", c, err)
			}
			readsAs(t, p, "prod")
			if res := p.Migrate(lockOpts()); !res.Nothing() {
				t.Fatalf("third run: %+v", res)
			}
		})
	}
}

// A root that was never moved from an old location gets no link there.
func TestNoLinkWithoutMove(t *testing.T) {
	p, home := macLayout(t)
	fakeTree(t, p.StateRoot, map[string]string{"prod/consent.json": "{}"})
	if res := p.Migrate(lockOpts()); !res.Nothing() {
		t.Fatalf("%+v", res)
	}
	if exists(p.Legacy.StateRoots[0]) || exists(filepath.Join(home, "Library")) {
		t.Fatal("made something at the old location")
	}
}

// Once no registration loads the plugin through the old location (init
// registered the apps from the root), a link or folder the user deleted
// there is not made again.
func TestDeletedLinkStaysDeleted(t *testing.T) {
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	registered := true
	o := lockOpts()
	o.Registered = func(string) bool { return registered }
	if res := p.Migrate(o); res.Refused != "" || !sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
		t.Fatalf("%+v", res)
	}
	if err := os.Remove(p.Legacy.StateRoots[0]); err != nil {
		t.Fatal(err)
	}
	registered = false
	if res := p.Migrate(o); !res.Nothing() || exists(p.Legacy.StateRoots[0]) {
		t.Fatalf("made again: %+v", res)
	}
	registered = true
	if res := p.Migrate(o); len(res.Linked) != 1 || !sameDir(t, p.Legacy.StateRoots[0], p.StateRoot) {
		t.Fatalf("still registered there, not repaired: %+v", res)
	}
}

// What clearRoot put aside is listed as unused, also when a crash right
// after left Migrate no chance to report it.
func TestUnusedListsAside(t *testing.T) {
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	aside := p.StateRoot + ".unused-20261025T000000Z"
	fakeTree(t, aside, map[string]string{".DS_Store": "x"})
	fakeTree(t, filepath.Dir(p.StateRoot), map[string]string{"other.unused-x/": ""})
	u := p.Unused()
	if len(u) != 1 || u[0] != aside {
		t.Fatalf("Unused %v", u)
	}
	readsAs(t, p, "prod")
}
