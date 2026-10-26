//go:build windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// errCrossDevice is the platform's "different volumes" rename error.
var errCrossDevice error = windows.ERROR_NOT_SAME_DEVICE

func TestCrossDeviceWindows(t *testing.T) {
	if !crossDevice(&os.LinkError{Err: windows.ERROR_NOT_SAME_DEVICE}) || crossDevice(&os.LinkError{Err: windows.ERROR_ACCESS_DENIED}) || crossDevice(syscall.EXDEV) {
		t.Fatal("crossDevice")
	}
}

// A real junction, made natively, on paths with a space and characters a
// shell would interpret: it resolves to the target and removes cleanly.
func TestJunctionOnAwkwardPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Kenaz files & 100% ^odd")
	target := filepath.Join(base, "new root")
	link := filepath.Join(base, "old state")
	os.MkdirAll(target, 0o700)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
	if err := createJunction(target, link); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(link, "f")); err != nil || string(b) != "x" {
		t.Fatalf("through the junction: %q %v", b, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.IsDir() {
		t.Fatalf("a junction must not look like a directory to Migrate: %v %v", fi, err)
	}
	if legacyDir(link) {
		t.Fatal("a junction counts as an old directory")
	}
	if err := os.Remove(link); err != nil || !exists(filepath.Join(target, "f")) {
		t.Fatalf("removing the junction: %v", err)
	}
}

func syscallBusy() error { return windows.ERROR_SHARING_VIOLATION }

// Windows refuses to rename a directory with a file open in it: nothing
// moves, and the files stay in use where they are.
func TestRenameRefusedOpenFileWindows(t *testing.T) {
	p, home := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	f, err := os.OpenFile(filepath.Join(p.Legacy.StateRoots[0], "prod", "spool", "queued"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := listTree(t, home)
	res := p.Migrate(lockOpts())
	if res.Refused == "" || len(res.Moved) != 0 || listTree(t, home) != before {
		t.Fatalf("%+v", res)
	}
	readsAs(t, p, "prod")
}

// Where symlinks are not allowed, Migrate leaves a junction at the old
// location, and the old plugin path keeps working through it.
func TestMigrateLeavesJunction(t *testing.T) {
	orig := makeLink
	makeLink = createJunction
	defer func() { makeLink = orig }()
	p, _ := macLayout(t)
	fakeTree(t, p.Legacy.ConfigDir, oldInstall)
	if res := p.Migrate(lockOpts()); res.Refused != "" {
		t.Fatalf("%+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(p.Legacy.StateRoots[0], "prod", "setup", "bundle", "x", "plugin.js")); err != nil || string(b) != "plugin" {
		t.Fatalf("through the junction: %q %v", b, err)
	}
	if p.Legacy.present() {
		t.Fatal("the junction counts as old files")
	}
}
