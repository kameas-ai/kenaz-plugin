package spool

import (
	"context"
	"errors"

	"github.com/zalando/go-keyring"
)

// go-keyring's calls take no context. Each runs beside a wait on ctx
// (within), so a keychain that never answers costs a parked goroutine,
// never the caller. Tests replace them.
var (
	keyringGet    = keyring.Get
	keyringSet    = keyring.Set
	keyringDelete = keyring.Delete
)

// keychainGet reads an item. On every platform this is go-keyring's read:
// the Secret Service, the Credential Manager, and on macOS the keychain
// helper's lookup, which answers at once where storing an item blocks.
func keychainGet(ctx context.Context, service, item string) (string, error) {
	get := keyringGet // read here, not in the goroutine that may outlive this call
	v, err := within(ctx, func() (string, error) { return get(service, item) })
	if errors.Is(err, keyring.ErrNotFound) {
		return "", errKeychainNotFound
	}
	return v, err
}

func keychainDelete(ctx context.Context, service, item string) error {
	del := keyringDelete
	_, err := within(ctx, func() (struct{}, error) { return struct{}{}, del(service, item) })
	if errors.Is(err, keyring.ErrNotFound) {
		return errKeychainNotFound
	}
	return err
}
