//go:build !windows

package pathenv

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A profile that did not exist is created like any new file, 0666 less
// the umask; an existing one keeps its mode whatever the umask. (Not
// parallel: the umask belongs to the whole process.)
func TestWriteProfileNewFileFollowsUmask(t *testing.T) {
	for _, mask := range []int{0o077, 0o027, 0o002} {
		old := syscall.Umask(mask)
		dir := t.TempDir()
		rc := filepath.Join(dir, ".zshrc")
		next, _, _ := Edit(nil, Zsh, "/k/bin")
		_, err := WriteProfile(rc, nil, false, next)
		// An existing 0664 profile, under the same umask.
		kept := filepath.Join(dir, ".bashrc")
		os.WriteFile(kept, []byte("x\n"), 0o600)
		os.Chmod(kept, 0o664)
		_, keptErr := WriteProfile(kept, []byte("x\n"), true, next)
		syscall.Umask(old)
		if err != nil || keptErr != nil {
			t.Fatal(err, keptErr)
		}
		want := os.FileMode(0o666 &^ mask)
		if fi, _ := os.Stat(rc); fi.Mode().Perm() != want {
			t.Errorf("umask %04o: new profile mode %04o, want %04o", mask, fi.Mode().Perm(), want)
		}
		if fi, _ := os.Stat(kept); fi.Mode().Perm() != 0o664 {
			t.Errorf("umask %04o: existing profile mode %04o, want 0664", mask, fi.Mode().Perm())
		}
		if got, _ := os.ReadFile(rc); string(got) != string(next) {
			t.Errorf("umask %04o: %s", mask, got)
		}
	}
}
