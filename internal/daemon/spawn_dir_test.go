package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The relay runs in the state directory, not the spawner's working
// directory (for Claude Desktop, the versioned extension folder).
func TestSpawnCommandRunsInStateDir(t *testing.T) {
	state := t.TempDir()
	cmd := spawnCommand("/opt/x/server/kenaz", state, "dev")
	if cmd.Dir != state {
		t.Fatalf("daemon working directory %q, want the state dir %q", cmd.Dir, state)
	}
	if got := strings.Join(cmd.Args[1:], " "); got != "daemon --env dev" {
		t.Fatalf("args %q", got)
	}
}

// The started process really runs in the state directory while the spawner
// sits elsewhere: a stand-in executable records its working directory.
func TestSpawnedProcessWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stand-in; TestSpawnCommandRunsInStateDir covers Windows")
	}
	state, other, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(other)
	marker := filepath.Join(tmp, "cwd")
	exe := filepath.Join(tmp, "fake-kenaz")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\npwd -P > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := spawnCommand(exe, state, "dev")
	detach(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(state)
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("spawned process ran in %q, want %q", got, want)
	}
}
