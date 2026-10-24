package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/features"
)

// OpenCode discovers skills under ~/.agents/skills, so an OpenCode-only
// machine gets that root, once.
func TestDetectRootsOpenCode(t *testing.T) {
	withOpenCode(t)
	home := t.TempDir()
	if roots := DetectRoots(home); len(roots) != 0 {
		t.Fatalf("empty home: %v", roots)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	roots := DetectRoots(home)
	if len(roots) != 1 || roots[0].Dir != filepath.Join(home, ".agents", "skills") {
		t.Fatalf("got %v", roots)
	}
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	if roots := DetectRoots(home, HarnessOpenCode); len(roots) != 1 {
		t.Fatalf("shared root listed twice: %v", roots)
	}
}

// withOpenCode turns the OpenCode build switch on for one test (it is off
// in shipped builds).
func withOpenCode(t *testing.T) {
	t.Helper()
	old := features.OpenCode
	features.OpenCode = true
	t.Cleanup(func() { features.OpenCode = old })
}
