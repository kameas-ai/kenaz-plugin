//go:build !darwin

package spool

import "context"

// The Secret Service (Linux, the BSDs) and the Credential Manager (Windows)
// are reached in-process: there is no helper process to kill, and storing
// an item is bounded the way reading one is (keychain_keyring.go).
func keychainSet(ctx context.Context, service, item, secret string) error {
	set := keyringSet
	_, err := within(ctx, func() (struct{}, error) { return struct{}{}, set(service, item, secret) })
	return err
}
