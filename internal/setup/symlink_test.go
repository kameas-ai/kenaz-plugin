package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplyUndoKeepsSymlink: a settings file managed as a symlink (dotfile
// repos) stays a symlink through setup and --undo; the target is edited and
// restored in place (setup is reversible).
func TestApplyUndoKeepsSymlink(t *testing.T) {
	e := newEnv(t, 4318)
	real := filepath.Join(t.TempDir(), "dotfiles", "claude-settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	orig := []byte("{\n  \"theme\": \"dark\"\n}\n")
	if err := os.WriteFile(real, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	link := e.opts.path(harnessClaude)
	os.MkdirAll(filepath.Dir(link), 0o700)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("apply replaced the symlink with a regular file")
	}
	if b, _ := os.ReadFile(real); string(b) == string(orig) {
		t.Fatal("apply did not update the symlink target")
	}
	if err := Undo(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("undo replaced the symlink with a regular file")
	}
	if b, _ := os.ReadFile(real); string(b) != string(orig) {
		t.Fatalf("undo did not restore the target: %q", b)
	}
}
