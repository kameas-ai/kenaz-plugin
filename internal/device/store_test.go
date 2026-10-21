package device

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	host := func() (string, error) { return "canary-host-7731", nil }
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	def := Resolve(s, "darwin", "01JNODE", host)
	if !regexp.MustCompile(`^macOS device [0-9a-f]{4}$`).MatchString(def) || strings.Contains(def, "canary") {
		t.Fatalf("default = %q", def)
	}
	if def != Resolve(s, "darwin", "01JNODE", nil) {
		t.Fatal("default must not depend on hostname")
	}
	if _, err := Rename(dir, "  Work\tlaptop \x00 "); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(dir)
	if got := Resolve(s, "darwin", "01JNODE", host); got != "Worklaptop" {
		t.Fatalf("renamed = %q", got)
	}
	if err := UseComputerName(dir); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(dir)
	if got := Resolve(s, "darwin", "01JNODE", host); got != "canary-host-7731" {
		t.Fatalf("opt-in = %q", got)
	}
	failing := func() (string, error) { return "", errors.New("x") }
	if got := Resolve(s, "linux", "01JNODE", failing); !strings.HasPrefix(got, "Linux device ") {
		t.Fatalf("fallback = %q", got)
	}
	if _, err := Rename(dir, " \x01 "); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("empty rename err = %v", err)
	}
	Reset(dir)
	s, _ = Load(dir)
	if Resolve(s, "darwin", "01JNODE", host) != def {
		t.Fatal("reset did not restore default")
	}
}
