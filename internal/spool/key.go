package spool

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// KeychainService is the OS keychain service for every plugin secret.
// Official builds store tokens under the same service.
var KeychainService = variant.Shared.KeychainService

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

// KeychainTimeout bounds one call to the OS keychain. The keychain can
// take a person's time (macOS may ask to unlock it), so it is generous; but
// it can also never answer (on macOS, `security -i` blocks for good in a
// home with no usable login keychain; a Secret Service can stop answering),
// and nothing that waits on it may wait forever. A var for tests.
var KeychainTimeout = 30 * time.Second

// ErrKeychainTimeout means the OS keychain did not answer within
// KeychainTimeout, or the caller's context ended first. The key was
// neither read nor written.
var ErrKeychainTimeout = errors.New("spool: the OS keychain did not answer")

// errKeychainNotFound is the platform call's "no such item".
var errKeychainNotFound = errors.New("spool: no such keychain item")

// Keychain stores keys in the OS keychain. This keeps keys out of files
// an agent could rewrite; it is not a boundary against processes running
// as the same user (on macOS the items are created via
// /usr/bin/security, readable by any process of the user without a
// prompt).
//
// Every call is bounded by KeychainTimeout and ends when Ctx does (nil:
// only the timeout). On macOS the helper process a call runs is killed
// with its children when that happens, so none is left behind.
type Keychain struct {
	Ctx context.Context
}

func (k Keychain) bound() (context.Context, context.CancelFunc) {
	ctx := k.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, KeychainTimeout)
}

func (k Keychain) Get(env string) ([]byte, error) {
	ctx, cancel := k.bound()
	defer cancel()
	v, err := keychainGet(ctx, KeychainService, itemName(env))
	if errors.Is(err, errKeychainNotFound) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(v)
}

func (k Keychain) Set(env string, key []byte) error {
	ctx, cancel := k.bound()
	defer cancel()
	return keychainSet(ctx, KeychainService, itemName(env), base64.StdEncoding.EncodeToString(key))
}

func (k Keychain) Delete(env string) error {
	ctx, cancel := k.bound()
	defer cancel()
	err := keychainDelete(ctx, KeychainService, itemName(env))
	if errors.Is(err, errKeychainNotFound) {
		return nil
	}
	return err
}

// within runs call, which cannot be interrupted, and returns its result,
// or ErrKeychainTimeout once ctx ends. call then finishes on its own in
// the background; its result is dropped.
func within[T any](ctx context.Context, call func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := call()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("%w (%v)", ErrKeychainTimeout, ctx.Err())
	}
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
