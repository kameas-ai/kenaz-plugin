package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
)

// A caller that already holds the single-instance lock (the relay command
// takes it before anything that can block) hands it to Run with HeldLock:
// Run starts under that lock, and without it a second Run is still turned
// away while the first holds it.
func TestRunUnderALockTheCallerHolds(t *testing.T) {
	dir := t.TempDir()
	release, err := TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Provider: stub.New(), Env: fleetapi.EnvDev, StateDir: dir, Version: "t",
		Logger: logx.Discard(), Ports: []int{0}, Host: enroll.Host{GOOS: "linux", GOARCH: "amd64"}}

	// Without HeldLock, Run sees the lock as another relay's.
	if err := New(opt).Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run without the lock: %v", err)
	}
	opt.HeldLock = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(opt).Run(ctx) }()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := ReadInfo(dir); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the relay did not start under the caller's lock")
		}
	}
	// The caller's lock is still the one in force.
	if _, err := TryLock(filepath.Join(dir, LockFile)); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("the lock is free while the relay runs: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Run did not release what it did not take.
	if _, err := TryLock(filepath.Join(dir, LockFile)); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run released the caller's lock: %v", err)
	}
	release()
	again, err := TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		t.Fatalf("after the caller released it: %v", err)
	}
	again()
}
