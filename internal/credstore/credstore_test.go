package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMarkerRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	if s, err := ReadMarker(dir); s != "" || err != nil {
		t.Fatalf("missing = %q, %v", s, err)
	}
	for _, want := range []Store{Files, Keychain} {
		if err := WriteMarker(dir, want); err != nil {
			t.Fatal(err)
		}
		if s, err := ReadMarker(dir); s != want || err != nil {
			t.Fatalf("read = %q, %v", s, err)
		}
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, MarkerName))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("marker mode %#o", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %#o", di.Mode().Perm())
		}
	}
	if err := WriteMarker(dir, "other"); err == nil {
		t.Fatal("unknown store written")
	}
	if err := RemoveMarker(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMarker(dir); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if s, err := ReadMarker(dir); s != "" || err != nil {
		t.Fatalf("after remove = %q, %v", s, err)
	}
}

func TestMarkerRejectsUnknownAndLoose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, MarkerName), []byte("cloud\n"), 0o600)
	if _, err := ReadMarker(dir); err == nil {
		t.Fatal("unknown value accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	WriteMarker(dir, Files)
	os.Chmod(filepath.Join(dir, MarkerName), 0o644)
	if _, err := ReadMarker(dir); !errors.Is(err, ErrInsecure) {
		t.Fatalf("loose marker: %v", err)
	}
	if err := WriteAtomic("relative", "x", nil); err == nil {
		t.Fatal("relative dir accepted")
	}
}
