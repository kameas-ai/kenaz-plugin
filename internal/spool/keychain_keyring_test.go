package spool

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// go-keyring's calls take no context (the Secret Service and the
// Credential Manager in-process; on macOS the lookup and the delete). A
// keyring that never answers ends each call with ErrKeychainTimeout once
// the bound passes; one that answers is used as before. (Storing on macOS
// goes through the helper: keychain_darwin_test.go.)
func TestKeychainCallEndsWhenTheKeyringNeverAnswers(t *testing.T) {
	block := make(chan struct{})
	oldGet, oldSet, oldDelete, oldTimeout := keyringGet, keyringSet, keyringDelete, KeychainTimeout
	t.Cleanup(func() {
		close(block)
		keyringGet, keyringSet, keyringDelete, KeychainTimeout = oldGet, oldSet, oldDelete, oldTimeout
	})
	keyringGet = func(string, string) (string, error) { <-block; return "", nil }
	keyringSet = func(string, string, string) error { <-block; return nil }
	keyringDelete = func(string, string) error { <-block; return nil }
	KeychainTimeout = 100 * time.Millisecond
	calls := map[string]func() error{
		"get":           func() error { _, err := Keychain{}.Get("prod"); return err },
		"delete":        func() error { return Keychain{}.Delete("prod") },
		"get or create": func() error { _, err := GetOrCreateKey(Keychain{}, "prod"); return err },
	}
	if runtime.GOOS != "darwin" {
		calls["set"] = func() error { return Keychain{}.Set("prod", make([]byte, 32)) }
	}
	for name, call := range calls {
		start := time.Now()
		if err := call(); !errors.Is(err, ErrKeychainTimeout) || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %v after %s", name, err, time.Since(start))
		}
	}

	if runtime.GOOS == "darwin" {
		return
	}
	// A keyring that answers: not found is ErrNoKey, and a new key is stored.
	stored := map[string]string{}
	keyringGet = func(_, item string) (string, error) {
		v, ok := stored[item]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	keyringSet = func(_, item, v string) error { stored[item] = v; return nil }
	keyringDelete = func(_, item string) error {
		if _, ok := stored[item]; !ok {
			return keyring.ErrNotFound
		}
		delete(stored, item)
		return nil
	}
	if _, err := (Keychain{}).Get("prod"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("get of nothing: %v", err)
	}
	k, err := GetOrCreateKey(Keychain{}, "prod")
	if err != nil || len(k) != 32 {
		t.Fatalf("%v %d", err, len(k))
	}
	again, err := GetOrCreateKey(Keychain{}, "prod")
	if err != nil || string(again) != string(k) {
		t.Fatalf("second read: %v", err)
	}
	if err := (Keychain{}).Delete("prod"); err != nil {
		t.Fatal(err)
	}
	if err := (Keychain{}).Delete("prod"); err != nil {
		t.Fatalf("delete of nothing: %v", err)
	}
}
