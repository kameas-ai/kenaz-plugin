package spool

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"

	"github.com/zalando/go-keyring"
)

// KeychainService is the OS keychain service for every plugin secret.
// Official builds store tokens under the same service.
const KeychainService = "kenaz-plugin"

// ErrNoKey means no spool key exists yet.
var ErrNoKey = errors.New("spool: no key")

// KeyStore holds the per-env spool key. The default is the OS keychain;
// devices without one use FileKeys, and tests inject Memory.
type KeyStore interface {
	Get(env string) ([]byte, error) // ErrNoKey if absent
	Set(env string, key []byte) error
	Delete(env string) error // nil if absent
}

func itemName(env string) string { return env + ":spool_key" }

// Keychain stores keys in the OS keychain. This keeps keys out of files
// an agent could rewrite; it is not a boundary against processes running
// as the same user (on macOS go-keyring creates items via
// /usr/bin/security, readable by any process of the user without a
// prompt).
type Keychain struct{}

func (Keychain) Get(env string) ([]byte, error) {
	v, err := keyring.Get(KeychainService, itemName(env))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(v)
}

func (Keychain) Set(env string, key []byte) error {
	return keyring.Set(KeychainService, itemName(env), base64.StdEncoding.EncodeToString(key))
}

func (Keychain) Delete(env string) error {
	err := keyring.Delete(KeychainService, itemName(env))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Memory is an in-process KeyStore.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *Memory) Get(env string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.m[env]
	if !ok {
		return nil, ErrNoKey
	}
	return append([]byte(nil), k...), nil
}

func (m *Memory) Set(env string, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string][]byte{}
	}
	m.m[env] = append([]byte(nil), key...)
	return nil
}

func (m *Memory) Delete(env string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, env)
	return nil
}

// GetOrCreateKey returns env's key, generating and storing a random one on
// first use.
func GetOrCreateKey(ks KeyStore, env string) ([]byte, error) {
	k, err := ks.Get(env)
	if err == nil && len(k) == 32 {
		return k, nil
	}
	if err != nil && !errors.Is(err, ErrNoKey) {
		return nil, err
	}
	k = make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, ks.Set(env, k)
}

// ErrStoreUndecided is returned by Undecided.Set.
var ErrStoreUndecided = errors.New("spool: no credential store chosen yet")

// Undecided is the KeyStore for short-lived commands that must not probe
// for a keychain before any store has been chosen: there is no key to read,
// and none may be created.
type Undecided struct{}

func (Undecided) Get(string) ([]byte, error) { return nil, ErrNoKey }
func (Undecided) Set(string, []byte) error   { return ErrStoreUndecided }
func (Undecided) Delete(string) error        { return nil }
