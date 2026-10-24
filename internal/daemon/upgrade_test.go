package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Upgrade while the previous release's relay is still running: the new
// binary's clients reuse that relay and a newly started relay refuses to
// run, so there is exactly one sender until the old one exits idle.
func TestUpgradeReusesRunningPreviousRelay(t *testing.T) {
	dir := t.TempDir()
	p := newFake(true)
	old := testOpts(t, p, dir)
	old.Version = "0.3.1"
	startDaemon(t, old)
	waitFor(t, func() bool { return p.enrolls.Load() >= 1 })

	var spawned atomic.Int32
	c, err := EnsureRunning(context.Background(), dir, func() error { spawned.Add(1); return nil }, time.Second)
	if err != nil || spawned.Load() != 0 || c.Info.Version != "0.3.1" {
		t.Fatalf("new client spawned a second relay: spawned=%d err=%v info=%+v", spawned.Load(), err, c)
	}
	next := testOpts(t, p, dir)
	next.Version = "0.4.0"
	if err := New(next).Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second relay over the same state: %v", err)
	}
	in, err := ReadInfo(dir)
	if err != nil || in.Version != "0.3.1" {
		t.Fatalf("daemon.json replaced by the refused relay: %+v %v", in, err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := p.enrolls.Load(); n != 1 {
		t.Fatalf("enrolls = %d, want 1 (one node, one sender)", n)
	}
}

// Upgrade after the previous relay exited without cleaning up (crash,
// reboot): its stale daemon.json and lock file do not block the new
// relay, and exactly one is spawned.
func TestUpgradeReplacesStalePreviousRelayState(t *testing.T) {
	dir := t.TempDir()
	stale := Info{PID: 999999, Port: 1, Version: "0.3.1", StartedAt: time.Unix(1_700_000_000, 0), Nonce: "stale"}
	if err := WriteInfo(dir, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newFake(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	var spawned atomic.Int32
	spawn := func() error {
		spawned.Add(1)
		opt := testOpts(t, p, dir)
		opt.Version = "0.4.0"
		go func() { done <- New(opt).Run(ctx) }()
		return nil
	}
	t.Cleanup(func() {
		cancel()
		for i := int32(0); i < spawned.Load(); i++ {
			<-done
		}
	})
	c, err := EnsureRunning(ctx, dir, spawn, 3*time.Second)
	if err != nil || c.Info.Version != "0.4.0" || spawned.Load() != 1 {
		t.Fatalf("spawned=%d err=%v info=%+v", spawned.Load(), err, c)
	}
	if c2, err := EnsureRunning(ctx, dir, spawn, time.Second); err != nil || c2.Info.Nonce != c.Info.Nonce || spawned.Load() != 1 {
		t.Fatalf("second client did not reuse the relay: spawned=%d err=%v", spawned.Load(), err)
	}
}
