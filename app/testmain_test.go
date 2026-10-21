package app

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates every app test from the developer's real home
// directory. The daemon's skills lane and `logout` manage skill directories
// under ~/.claude and ~/.agents; tests must never read or write those.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kenaz-app-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	skillsHome = func() (string, error) { return home, nil }
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
