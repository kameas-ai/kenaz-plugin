package device

import (
	"regexp"
	"strings"
	"testing"
)

func TestDefaultDisplayName(t *testing.T) {
	got := DefaultDisplayName("darwin", "01JABCDEFGHJKMNPQRSTVWXYZ0")
	if !regexp.MustCompile(`^macOS device [0-9a-f]{4}$`).MatchString(got) {
		t.Fatalf("got %q", got)
	}
	if got != DefaultDisplayName("darwin", "01JABCDEFGHJKMNPQRSTVWXYZ0") {
		t.Fatal("not deterministic")
	}
	if strings.Contains(got, "01JA") {
		t.Fatal("leaks node id")
	}
	if !strings.HasPrefix(DefaultDisplayName("plan9", "x"), "Unknown OS device ") {
		t.Fatal("unknown os label")
	}
}

func TestSanitizeDisplayName(t *testing.T) {
	if _, err := SanitizeDisplayName(" \n\t "); err != ErrEmptyName {
		t.Fatalf("err = %v", err)
	}
	got, err := SanitizeDisplayName("  my\x00 laptop\n ")
	if err != nil || got != "my laptop" {
		t.Fatalf("got %q, %v", got, err)
	}
	long, _ := SanitizeDisplayName(strings.Repeat("é", 100))
	if n := len([]rune(long)); n != 64 {
		t.Fatalf("len = %d", n)
	}
}
