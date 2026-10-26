package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Every build keeps a Fleet environment's setup in the same state
// directory. Setup state another build wrote there is refused (one build
// sets up the apps for an environment at a time); an empty record is free;
// "" is kenaz; this build records itself the way the next run reads it.
func TestSetupStateOfOtherBuild(t *testing.T) {
	other := variant.KenazDev
	if variant.Current.Name == other.Name {
		other = variant.Kenaz
	}
	write := func(t *testing.T, dir, rec string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorded := `"` + other.Name + `"`
	if other.Name == variant.Kenaz.Name {
		recorded = `""`
	}
	dir := t.TempDir()
	write(t, dir, `{"version":1,"variant":`+recorded+`,"claude":{"path":"x","keys":{}}}`)
	var ob *OtherBuildError
	if _, err := loadState(dir); !errors.As(err, &ob) || ob.Variant != other.Name || !strings.Contains(err.Error(), other.Name+" setup --undo") {
		t.Fatalf("other build's state: %v", err)
	}
	if hs := HostStates(dir); hs[0].State != HostUnknown {
		t.Fatalf("host states: %+v", hs)
	}

	write(t, dir, `{"version":1,"variant":`+recorded+`}`)
	if _, err := loadState(dir); err != nil {
		t.Fatalf("empty record: %v", err)
	}

	s := &State{Version: 1, Claude: &FileState{Path: "x", Keys: map[string]KeyState{}}}
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, stateFile))
	if (variant.Current.Name == variant.Kenaz.Name) == strings.Contains(string(b), `"variant"`) {
		t.Fatalf("recorded variant: %s", b)
	}
	if _, err := loadState(dir); err != nil {
		t.Fatalf("own state: %v", err)
	}
}

// A registration whose plugin files are outside the state directory (it
// moved to ~/.kenaz/plugin) is reported, naming kenaz init.
func TestBrokenRegistrationsAfterMove(t *testing.T) {
	old, dir := t.TempDir(), t.TempDir()
	launcher := filepath.Join(t.TempDir(), "kenaz")
	os.WriteFile(launcher, nil, 0o755)
	rendered := filepath.Join(old, "setup", "bundle", "x")
	os.MkdirAll(rendered, 0o700)
	s := &State{Version: 1, Claude: &FileState{Path: "x", Keys: map[string]KeyState{},
		Bundle: &BundleReg{Root: old, Rendered: rendered, Launcher: launcher}}}
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	got := BrokenRegistrations(dir)
	if len(got) != 1 || !strings.Contains(got[0], "still uses its files from "+filepath.Join(old, "setup", "bundle")) || !strings.Contains(got[0], tBin+" init") {
		t.Fatalf("%q", got)
	}
	// Re-registered inside the state directory: nothing to report.
	s.Claude.Bundle.Rendered = filepath.Join(dir, "setup", "bundle", "x")
	os.MkdirAll(s.Claude.Bundle.Rendered, 0o700)
	saveState(dir, s)
	if got := BrokenRegistrations(dir); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestWithin(t *testing.T) {
	d := filepath.FromSlash("/a/b")
	for p, want := range map[string]bool{"/a/b/c": true, "/a/b": true, "/a/bc": false, "/a": false, "/x/y": false, "r": false} {
		if got := within(d, filepath.FromSlash(p)); got != want {
			t.Errorf("within(%s, %s) = %v", d, p, got)
		}
	}
}

// After the state directory moved and a link was left at the old place,
// a registration recorded under the old path still names a live plugin
// copy: loadState maps it onto the new directory (keeping the old path as
// the registered one), and pruning after another app's re-registration
// keeps it.
func TestMovedStateLinkKeepsRegisteredCopy(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "new", "prod")
	oldRoot := filepath.Join(base, "old")
	os.MkdirAll(filepath.Join(dir, "setup", "bundle", "claude-copy"), 0o700)
	os.MkdirAll(filepath.Join(dir, "setup", "bundle", "codex-copy"), 0o700)
	if err := config.DirLink(filepath.Join(base, "new"), oldRoot); err != nil {
		t.Fatal(err)
	}
	viaOld := filepath.Join(oldRoot, "prod", "setup", "bundle", "claude-copy")
	launcher := filepath.Join(base, "kenaz")
	os.WriteFile(launcher, nil, 0o755)
	s := &State{Version: 1, Claude: &FileState{Path: "x", Keys: map[string]KeyState{},
		Bundle: &BundleReg{Root: base, Rendered: viaOld, Launcher: launcher}}}
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Claude.Bundle; b.Rendered != filepath.Join(dir, "setup", "bundle", "claude-copy") || b.registered() != viaOld {
		t.Fatalf("rebase: %q (registered %q)", b.Rendered, b.registered())
	}
	Options{StateDir: dir}.pruneRendered(filepath.Join(dir, "setup", "bundle", "codex-copy"))
	if _, err := os.Stat(filepath.Join(dir, "setup", "bundle", "claude-copy")); err != nil {
		t.Fatal("pruned the copy Claude Code still loads through the old path")
	}
	if got := BrokenRegistrations(dir); len(got) != 1 || !strings.Contains(got[0], "still uses its files from") {
		t.Fatalf("%q", got)
	}
}
