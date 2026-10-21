package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// keepBackups is how many timestamped backups setup keeps per file.
const keepBackups = 3

const backupInfix = ".kameas-bak-"

func readFile(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func fileMode(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o600
}

// writeAtomic replaces path with data via a temp file and rename,
// preserving the existing file mode. When path is a symlink (settings kept
// in a dotfiles repo), the link's target is replaced instead, so the link
// survives setup and --undo.
func writeAtomic(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = target
	}
	mode := fileMode(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".kameas-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// backup copies path to path.kameas-bak-<UTC timestamp> and prunes all
// but the newest keepBackups.
func backup(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	name := path + backupInfix + now.UTC().Format("20060102T150405.000000000Z")
	if err := os.WriteFile(name, data, fileMode(path)); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(path + backupInfix + "*")
	sort.Strings(old)
	for len(old) > keepBackups {
		_ = os.Remove(old[0])
		old = old[1:]
	}
	return name, nil
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
