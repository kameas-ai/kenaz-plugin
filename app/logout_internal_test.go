package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// Logout waits for the relay it asked to stop for as long as that relay
// holds the state directory's lock, and no longer than its bound.
func TestWaitRelayExit(t *testing.T) {
	dir := t.TempDir()
	if !waitRelayExit(dir, time.Second) {
		t.Fatal("waited for a relay that never ran here")
	}
	lock := filepath.Join(dir, daemon.LockFile)
	release, err := daemon.TryLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if waitRelayExit(dir, 200*time.Millisecond) {
		t.Fatal("a running relay was taken for gone")
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 3*time.Second {
		t.Fatalf("waited %s", took)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()
	if !waitRelayExit(dir, 5*time.Second) {
		t.Fatal("the relay's exit was not noticed")
	}
	// The wait took the lock only to look: a relay can start afterwards.
	again, err := daemon.TryLock(lock)
	if err != nil {
		t.Fatalf("the lock was left held: %v", err)
	}
	again()
	if _, err := os.Stat(lock); err != nil {
		t.Fatal(err)
	}
}
