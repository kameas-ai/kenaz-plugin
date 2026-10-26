package variant_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// legacyState is where releases up to 0.6.x kept the state on this
// platform.
func (m *machine) legacyState() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(m.home, "Library", "Application Support", "kameas", "kenaz", "state")
	}
	return filepath.Join(m.home, ".local", "state", "kameas", "kenaz")
}

// An install set up by a release that kept its files in the old location
// (simulated: set up, then put back where 0.6.x had them): every command
// keeps working from there; `kenaz migrate` moves them in one rename and
// leaves a link, so Claude Code still loads the plugin from the path it
// was given; the startup check names `kenaz init`; setup (what init runs
// for each app) registers the plugin from the new location.
func TestMigrateThenRegisterAgain(t *testing.T) {
	if variant.Current.Name != variant.Kenaz.Name {
		t.Skip("only the public build had the old layout")
	}
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	os.Remove(filepath.Join(l.state(id.DefaultEnv), "daemon.json"))

	// Put it back in the 0.6.x location, registrations included.
	old := m.legacyState()
	os.MkdirAll(filepath.Dir(old), 0o700)
	if err := os.Rename(l.StateRoot, old); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(m.home, ".claude", "settings.json"), filepath.Join(old, id.DefaultEnv, "setup-state.json")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(f, []byte(strings.ReplaceAll(string(b), l.StateRoot, old)), 0o600)
	}
	if !strings.Contains(m.claudeReg(id), old) {
		t.Fatalf("not registered from the old location: %s", m.claudeReg(id))
	}

	if r := m.mustRun(bin, nil, "status"); strings.Contains(r.Stderr, "moved") {
		t.Fatalf("status moved files: %q", r.Stderr)
	}
	if _, err := os.Stat(l.StateRoot); !os.IsNotExist(err) {
		t.Fatal("moved without kenaz migrate")
	}
	r := m.mustRun(bin, nil, "migrate")
	if !strings.Contains(r.Stdout, "moved its files to "+l.StateRoot) {
		t.Fatalf("migrate: %s", r.Stdout)
	}
	reg := m.claudeReg(id)
	if _, err := os.Stat(filepath.Join(old, id.DefaultEnv, "setup", "bundle")); err != nil {
		t.Fatalf("the registered plugin path no longer works (%s): %v", reg, err)
	}
	if r := m.mustRun(bin, nil, "status"); !strings.Contains(r.Stderr, "still uses its files from") || !strings.Contains(r.Stderr, "kenaz init") {
		t.Fatalf("status after migrate: %q", r.Stderr)
	}
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	if reg := m.claudeReg(id); !strings.Contains(reg, filepath.Join(l.StateRoot, id.DefaultEnv, "setup", "bundle")) || strings.Contains(reg, old) {
		t.Fatalf("not registered from the new location: %s", reg)
	}
	if r := m.mustRun(bin, nil, "status"); strings.Contains(r.Stderr, "still uses its files") {
		t.Fatalf("status after setup: %q", r.Stderr)
	}
}
