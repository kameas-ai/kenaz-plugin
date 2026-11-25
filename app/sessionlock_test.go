package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
)

// The session lock's path is in the plugin's own state root, follows KENAZ_HOME,
// is absolute and the same on every call, and asking for it creates nothing.
func TestSessionLockPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KENAZ_HOME", home)
	got, err := SessionLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "plugin", ".session.lock"); got != want || !filepath.IsAbs(got) {
		t.Fatalf("%q, want %q", got, want)
	}
	if again, _ := SessionLockPath(); again != got {
		t.Fatalf("%q then %q", got, again)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("asking for the path created %v", entries)
	}
	// It is beside the per-environment folders, where each relay's daemon.lock is.
	env := Env{Getenv: os.Getenv}
	p, err := env.pathsInPlace()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != p.StateRoot || filepath.Dir(p.State("dev")) != filepath.Dir(got) {
		t.Fatalf("%q is not in the state root %q", got, p.StateRoot)
	}
	// Another KENAZ_HOME, another path.
	other := t.TempDir()
	t.Setenv("KENAZ_HOME", other)
	if moved, _ := SessionLockPath(); moved != filepath.Join(other, "plugin", ".session.lock") {
		t.Fatal(moved)
	}
}

// While an earlier release's files are still where it kept them, and the plugin's
// own folder holds no state, the lock belongs with those files: every process
// reads them there, so that is where they must exclude each other. Once the
// plugin's folder holds state, the lock is there.
func TestSessionLockPathFollowsAnEarlierLayout(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(k, home)
	}
	t.Setenv("KENAZ_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg-state"))
	t.Setenv("AppData", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	layout, err := config.Layout(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Legacy.StateRoots) == 0 {
		t.Skip("this platform has no earlier layout")
	}
	legacy := layout.Legacy.StateRoots[0]
	for _, dir := range []string{legacy, layout.StateRoot} {
		if rel, err := filepath.Rel(home, dir); err != nil || strings.HasPrefix(rel, "..") {
			t.Skipf("%s is outside the test's home; not touching it", dir)
		}
	}
	// Nothing anywhere yet: the plugin's own folder.
	if got, _ := SessionLockPath(); got != filepath.Join(layout.StateRoot, ".session.lock") {
		t.Fatalf("with no files anywhere: %q", got)
	}
	// An earlier release's state, and none in the plugin's folder.
	if err := os.MkdirAll(filepath.Join(legacy, "dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "dev", "node_id"), []byte("01K7ZQ5V3W4X5Y6Z7A8B9C0D1E\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := SessionLockPath(); got != filepath.Join(legacy, ".session.lock") {
		t.Fatalf("with an earlier release's files in %s: %q", legacy, got)
	}
	// The plugin's folder holds state now: the lock is there.
	if err := os.MkdirAll(filepath.Join(layout.StateRoot, "dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.StateRoot, "dev", "node_id"), []byte("01K7ZQ5V3W4X5Y6Z7A8B9C0D1F\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := SessionLockPath(); got != filepath.Join(layout.StateRoot, ".session.lock") {
		t.Fatalf("with state in the plugin's folder: %q", got)
	}
}
