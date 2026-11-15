package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// stuckKeys is a key store that never answers, as the macOS keychain does
// in a home with no usable login keychain. calls counts who is waiting.
type stuckKeys struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (k *stuckKeys) wait() {
	k.mu.Lock()
	k.calls++
	k.mu.Unlock()
	<-k.release
}
func (k *stuckKeys) waiting() int { k.mu.Lock(); defer k.mu.Unlock(); return k.calls }

func (k *stuckKeys) Get(string) ([]byte, error) { k.wait(); return nil, spool.ErrNoKey }
func (k *stuckKeys) Set(string, []byte) error   { k.wait(); return nil }
func (k *stuckKeys) Delete(string) error        { k.wait(); return nil }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The relay takes its single-instance lock before it asks the key store
// for anything, and never waits on the store for ever:
//
//   - while one relay waits on a store that does not answer, a second one
//     for the same environment leaves at once ("relay already running")
//     instead of joining the wait, and never asks the store;
//   - told to stop (the context a signal cancels), the waiting relay exits
//     promptly and releases the lock;
//   - left alone, it gives up when the store's bound passes and refuses to
//     start, as it does for any unusable store: it never runs without the
//     key.
func TestRelayStartupNeverBlocksOnTheKeyStore(t *testing.T) {
	old := spool.KeychainTimeout
	t.Cleanup(func() { spool.KeychainTimeout = old })

	t.Run("a second relay leaves; a stop is obeyed", func(t *testing.T) {
		spool.KeychainTimeout = time.Minute
		h := newHarness(t, stub.New())
		keys := &stuckKeys{release: make(chan struct{})}
		defer close(keys.release)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var out, errb bytes.Buffer
		env := h.env(ctx, &out, &errb)
		env.KeyStore = keys
		done := make(chan int, 1)
		go func() { done <- app.RunEnv(env, []string{"daemon"}) }()
		waitFor(t, "the relay to ask for its key", func() bool { return keys.waiting() == 1 })

		// The lock is held already: nobody else can take it.
		lock := filepath.Join(h.state(), daemon.LockFile)
		if release, err := daemon.TryLock(lock); err == nil {
			release()
			t.Fatal("the relay waits on the key store without holding its lock")
		}
		// A second relay for the environment leaves at once.
		var out2, errb2 bytes.Buffer
		env2 := h.env(context.Background(), &out2, &errb2)
		env2.KeyStore = keys
		second := make(chan int, 1)
		go func() { second <- app.RunEnv(env2, []string{"daemon"}) }()
		select {
		case code := <-second:
			if code != app.ExitOK || !strings.Contains(errb2.String(), "relay already running") {
				t.Fatalf("second relay: exit %d %q", code, errb2.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a second relay hung instead of leaving because of the lock")
		}
		if n := keys.waiting(); n != 1 {
			t.Fatalf("%d callers wait on the key store, want only the first relay", n)
		}

		// Told to stop, the first one exits and frees the lock.
		cancel()
		select {
		case code := <-done:
			if code != app.ExitOK || errb.Len() != 0 {
				t.Fatalf("stopped relay: exit %d %q", code, errb.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the relay ignored the stop while waiting on the key store")
		}
		release, err := daemon.TryLock(lock)
		if err != nil {
			t.Fatalf("the lock was not released: %v", err)
		}
		release()
		if _, err := os.Stat(filepath.Join(h.state(), "spool")); !os.IsNotExist(err) {
			t.Errorf("a spool was opened without a key: %v", err)
		}
	})

	t.Run("it gives up when the bound passes", func(t *testing.T) {
		spool.KeychainTimeout = 50 * time.Millisecond
		h := newHarness(t, stub.New())
		keys := &stuckKeys{release: make(chan struct{})}
		defer close(keys.release)
		var out, errb bytes.Buffer
		env := h.env(context.Background(), &out, &errb)
		env.KeyStore = keys
		done := make(chan int, 1)
		go func() { done <- app.RunEnv(env, []string{"daemon"}) }()
		select {
		case code := <-done:
			if code != app.ExitError || !strings.Contains(errb.String(), "the relay cannot start") {
				t.Fatalf("exit %d %q", code, errb.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the relay waited on the key store past its bound")
		}
		if _, err := os.Stat(filepath.Join(h.state(), "spool")); !os.IsNotExist(err) {
			t.Errorf("a spool was opened without a key: %v", err)
		}
		if _, err := os.Stat(filepath.Join(h.state(), daemon.InfoFile)); !os.IsNotExist(err) {
			t.Errorf("the relay announced itself: %v", err)
		}
		release, err := daemon.TryLock(filepath.Join(h.state(), daemon.LockFile))
		if err != nil {
			t.Fatalf("the lock was not released: %v", err)
		}
		release()
	})
}
