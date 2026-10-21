package spool

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileKeysRoundTripAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	ks := FileKeys{Dir: dir}
	if _, err := ks.Get("dev"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("missing dir: %v", err)
	}
	k, err := GetOrCreateKey(ks, "dev")
	if err != nil || len(k) != 32 {
		t.Fatalf("create: %v len=%d", err, len(k))
	}
	again, err := GetOrCreateKey(ks, "dev")
	if err != nil || !bytes.Equal(k, again) {
		t.Fatalf("reload: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %#o", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Join(dir, "spool-dev.key")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %#o", fi.Mode().Perm())
		}
	}
	if err := ks.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Delete("dev"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := ks.Get("dev"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("after delete: %v", err)
	}
}

// Overwrites go through a temp file and rename: no temp file is left behind
// and the new value replaces the old one whole.
func TestFileKeysAtomicWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	ks := FileKeys{Dir: dir}
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if err := ks.Set("prod", a); err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("prod", b); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Get("prod")
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("got %v err %v", got, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "spool-prod.key" {
		t.Fatalf("dir entries: %v", ents)
	}
}

func TestFileKeysRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	dir := filepath.Join(t.TempDir(), "credentials")
	ks := FileKeys{Dir: dir}
	if err := ks.Set("dev", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "spool-dev.key")
	for _, m := range []os.FileMode{0o640, 0o604, 0o644} {
		os.Chmod(f, m)
		if _, err := ks.Get("dev"); !errors.Is(err, ErrInsecureKeyFile) || !strings.Contains(err.Error(), "other users could read it") {
			t.Fatalf("mode %#o: %v", m, err)
		}
	}
	os.Chmod(f, 0o600)
	os.Chmod(dir, 0o755)
	if _, err := ks.Get("dev"); !errors.Is(err, ErrInsecureKeyFile) {
		t.Fatalf("loose dir: %v", err)
	}
	if err := ks.Set("dev", make([]byte, 32)); !errors.Is(err, ErrInsecureKeyFile) {
		t.Fatalf("set into loose dir: %v", err)
	}
	os.Chmod(dir, 0o700)
	// A symlink in place of the key file is refused too.
	os.Remove(f)
	other := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(other, make([]byte, 32), 0o600)
	os.Symlink(other, f)
	if _, err := ks.Get("dev"); !errors.Is(err, ErrInsecureKeyFile) {
		t.Fatalf("symlink: %v", err)
	}
}

func TestFileKeysRejectsBadNames(t *testing.T) {
	ks := FileKeys{Dir: filepath.Join(t.TempDir(), "credentials")}
	for _, env := range []string{"", "../x", "a/b", "..", "DEV", "a\x00b", `a\b`} {
		if err := ks.Set(env, make([]byte, 32)); err == nil {
			t.Fatalf("Set(%q) accepted", env)
		}
		if _, err := ks.Get(env); err == nil || errors.Is(err, ErrNoKey) {
			t.Fatalf("Get(%q) = %v", env, err)
		}
	}
	if err := (FileKeys{Dir: "relative/dir"}).Set("dev", make([]byte, 32)); err == nil {
		t.Fatal("relative dir accepted")
	}
}

func TestFileKeysRejectsWrongLength(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "spool-dev.key"), []byte("short"), 0o600)
	if _, err := (FileKeys{Dir: dir}).Get("dev"); err == nil {
		t.Fatal("short key accepted")
	}
}
