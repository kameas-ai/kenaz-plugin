package spool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
)

// FileKeys stores each env's spool key as 32 raw bytes in
// <Dir>/spool-<env>.key, for devices with no OS keychain. Dir is created
// 0700 and each key file is written 0600 (atomically, temp + rename). A key
// file or directory that other users can read is refused, not trusted.
// Without a keychain the key is protected only by these file permissions.
type FileKeys struct {
	Dir string
}

// ErrInsecureKeyFile means a key file or its directory has permissions that
// let other users read it, or is not a plain file or directory owned by the
// current user.
var ErrInsecureKeyFile = credstore.ErrInsecure

func (f FileKeys) path(env string) (string, error) {
	if env == "" {
		return "", errors.New("spool: empty env")
	}
	for i := 0; i < len(env); i++ {
		c := env[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", fmt.Errorf("spool: invalid env name %q", env)
		}
	}
	if f.Dir == "" || !filepath.IsAbs(f.Dir) {
		return "", errors.New("spool: key directory must be an absolute path")
	}
	return filepath.Join(f.Dir, "spool-"+env+".key"), nil
}

// Get implements KeyStore.
func (f FileKeys) Get(env string) ([]byte, error) {
	p, err := f.path(env)
	if err != nil {
		return nil, err
	}
	k, err := credstore.ReadPrivate(f.Dir, filepath.Base(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("spool: key file %s has the wrong length", filepath.Base(p))
	}
	return k, nil
}

// Set implements KeyStore.
func (f FileKeys) Set(env string, key []byte) error {
	p, err := f.path(env)
	if err != nil {
		return err
	}
	return credstore.WriteAtomic(f.Dir, filepath.Base(p), key)
}

// Delete implements KeyStore. A missing key is not an error.
func (f FileKeys) Delete(env string) error {
	p, err := f.path(env)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
