//go:build darwin

package spool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeSecurity replaces the keychain helper with a script, and restores it.
func fakeSecurity(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "security")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	old, oldTimeout := securityPath, KeychainTimeout
	securityPath = p
	t.Cleanup(func() { securityPath, KeychainTimeout = old, oldTimeout })
	return dir
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pidsIn(t *testing.T, file string) []int {
	t.Helper()
	var out []int
	deadline := time.Now().Add(5 * time.Second)
	for len(out) < 2 && time.Now().Before(deadline) {
		out = out[:0]
		b, _ := os.ReadFile(file)
		for _, f := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
		if len(out) < 2 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return out
}

// The helper that stores an item can block for good (`security -i` in a
// home with no usable login keychain). The call returns once its bound
// passes, with ErrKeychainTimeout, and the helper is gone with the child
// it started: nothing is left running.
func TestKeychainCallEndsAndKillsAHelperThatNeverAnswers(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(Keychain) error
	}{
		{"set", func(k Keychain) error { return k.Set("prod", make([]byte, 32)) }},
		{"get or create", func(k Keychain) error {
			// No key yet: the lookup answers, storing the new one is what blocks.
			old := keyringGet
			keyringGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
			defer func() { keyringGet = old }()
			_, err := GetOrCreateKey(k, "prod")
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := fakeSecurity(t, "")
			pids := filepath.Join(dir, "pids")
			// The helper, with a child of its own, both waiting for ever.
			os.WriteFile(securityPath, []byte("#!/bin/sh\nsleep 600 &\necho $$ $! > '"+pids+"'\nwait\n"), 0o700)
			KeychainTimeout = 300 * time.Millisecond
			start := time.Now()
			err := c.call(Keychain{})
			if !errors.Is(err, ErrKeychainTimeout) {
				t.Fatalf("err = %v, want ErrKeychainTimeout", err)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("the call took %s", took)
			}
			got := pidsIn(t, pids)
			if len(got) != 2 {
				t.Fatalf("the helper did not start: %v", got)
			}
			deadline := time.Now().Add(5 * time.Second)
			for (alive(got[0]) || alive(got[1])) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if alive(got[0]) || alive(got[1]) {
				syscall.Kill(got[0], syscall.SIGKILL)
				syscall.Kill(got[1], syscall.SIGKILL)
				t.Fatalf("left running: helper %d alive=%v, its child %d alive=%v", got[0], alive(got[0]), got[1], alive(got[1]))
			}
		})
	}
}

// A call ends with its context, at once: a relay told to stop does not
// wait out the keychain's bound.
func TestKeychainCallEndsWithItsContext(t *testing.T) {
	dir := fakeSecurity(t, "")
	pids := filepath.Join(dir, "pids")
	os.WriteFile(securityPath, []byte("#!/bin/sh\nsleep 600 &\necho $$ $! > '"+pids+"'\nwait\n"), 0o700)
	KeychainTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Keychain{Ctx: ctx}.Set("prod", make([]byte, 32)) }()
	got := pidsIn(t, pids)
	if len(got) != 2 {
		t.Fatalf("the helper did not start: %v", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrKeychainTimeout) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end with its context")
	}
	deadline := time.Now().Add(5 * time.Second)
	for (alive(got[0]) || alive(got[1])) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(got[0]) || alive(got[1]) {
		syscall.Kill(got[0], syscall.SIGKILL)
		syscall.Kill(got[1], syscall.SIGKILL)
		t.Fatal("the helper or its child was left running")
	}
}

// A helper that answers is given what go-keyring gives it: the same
// command, with the secret on stdin and never in the arguments.
func TestKeychainStoresThroughTheHelperAsGoKeyringDoes(t *testing.T) {
	dir := fakeSecurity(t, "")
	log := filepath.Join(dir, "log")
	os.WriteFile(securityPath, []byte("#!/bin/sh\necho \"args: $*\" >> '"+log+"'\ncat >> '"+log+"'\n"), 0o700)
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := (Keychain{}).Set("prod", key); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	// base64(key), then base64 again with go-keyring's prefix.
	want := "args: -i\nadd-generic-password -U -s " + KeychainService + " -a prod:spool_key -w go-keyring-base64:TURFeU16UTFOamM0T1dGaVkyUmxaakF4TWpNME5UWTNPRGxoWW1Oa1pXWT0=\n"
	if string(b) != want {
		t.Fatalf("the helper was given:\n%s\nwant:\n%s", b, want)
	}
	if shellQuote("it's") != `'it'"'"'s'` || shellQuote("") != "''" || shellQuote("a b") != "'a b'" || shellQuote("kenaz-plugin") != "kenaz-plugin" {
		t.Fatal("quoting differs from go-keyring's")
	}
}
