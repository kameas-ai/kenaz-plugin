// Package credstore holds the file-permission helpers for devices with no
// OS keychain, and the marker that records which credential store a device
// uses so the choice never flips between runs.
//
// Everything lives in one owner-only directory (0700) of owner-only files
// (0600). Files that other users can read are refused, never trusted.
package credstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Store names a credential store. The values are what the marker file holds.
type Store string

const (
	Keychain Store = "keychain" // the OS keychain
	Files    Store = "files"    // owner-only files in the credentials directory
)

// MarkerName is the file in the credentials directory that records the
// device's store.
const MarkerName = "STORE"

// ErrInsecure means a credentials file or directory could be read by other
// users, is not owned by the current user, or is not a plain file or
// directory.
var ErrInsecure = errors.New("insecure credential file permissions")

// CheckPrivate refuses symlinks, the wrong file type, group/other permission
// bits and files owned by someone else. Windows has no POSIX modes, so only
// the type checks apply there.
func CheckPrivate(fi fs.FileInfo, dir bool) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symbolic link", ErrInsecure, fi.Name())
	case dir && !fi.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrInsecure, fi.Name())
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecure, fi.Name())
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		want := "600"
		if dir {
			want = "700"
		}
		return fmt.Errorf("%w: %s has mode %#o; other users could read it (fix with chmod %s)", ErrInsecure, fi.Name(), perm, want)
	}
	if !ownedByCurrentUser(fi) {
		return fmt.Errorf("%w: %s is not owned by the current user", ErrInsecure, fi.Name())
	}
	return nil
}

// CheckDir verifies dir. A missing dir is reported as fs.ErrNotExist.
func CheckDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return CheckPrivate(fi, true)
}

// ReadPrivate reads name from dir after checking both. A missing dir or
// file is reported as fs.ErrNotExist.
func ReadPrivate(dir, name string) ([]byte, error) {
	if err := CheckDir(dir); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if err := CheckPrivate(fi, false); err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WriteAtomic writes data to dir/name with mode 0600 through a synced temp
// file and a rename, creating dir 0700 if needed.
func WriteAtomic(dir, name string, data []byte) error {
	if !filepath.IsAbs(dir) {
		return errors.New("credentials directory must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := CheckDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tname := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tname)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tname, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ReadMarker returns the recorded store, or "" with a nil error when none
// is recorded yet.
func ReadMarker(dir string) (Store, error) {
	b, err := ReadPrivate(dir, MarkerName)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch s := Store(strings.TrimSpace(string(b))); s {
	case Keychain, Files:
		return s, nil
	default:
		return "", fmt.Errorf("credential store marker %s holds an unknown value", filepath.Join(dir, MarkerName))
	}
}

// WriteMarker records s.
func WriteMarker(dir string, s Store) error {
	if s != Keychain && s != Files {
		return fmt.Errorf("unknown credential store %q", s)
	}
	return WriteAtomic(dir, MarkerName, []byte(string(s)+"\n"))
}

// RemoveMarker forgets the recorded store. A missing marker is not an error.
func RemoveMarker(dir string) error {
	err := os.Remove(filepath.Join(dir, MarkerName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
