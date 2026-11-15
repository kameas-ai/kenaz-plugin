package spool

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Windows refuses to remove or rename a file while a handle to it is open
// ("The process cannot access the file because it is being used by another
// process"); POSIX systems unlink it and let the handle live on. The spool
// removes segments on purge, eviction, delivery and quarantine, so it must
// never do so while it still holds one of them open: the writer of the
// active segment, or a reader.
//
// Every file the package opens goes through openTracked, and every removal
// and rename through removeFile and renameFile. That lets a test turn the
// Windows rule on for every platform (enforceOpenRule): a removal that would
// fail on Windows then fails everywhere, so the discipline is checked on
// the machines developers and most of CI use, not only on a Windows runner.
// The count is per process, as the handles are: it also catches a second
// Spool on the same directory, which is what a test of a "restart" builds.

// ErrInUse is what removeFile and renameFile return under enforceOpenRule
// when this process still holds the file open.
var ErrInUse = errors.New("spool: file is open in this process (Windows would refuse)")

var (
	enforceOpenRule atomic.Bool
	openMu          sync.Mutex
	openCount       = map[string]int{}
)

func openKey(path string) string { return filepath.Clean(path) }

// tracked is an open file that is counted until it is closed.
type tracked struct {
	*os.File
	key  string
	once sync.Once
}

// Close closes the file and stops counting it.
func (t *tracked) Close() error {
	err := t.File.Close()
	t.once.Do(func() {
		openMu.Lock()
		if openCount[t.key]--; openCount[t.key] <= 0 {
			delete(openCount, t.key)
		}
		openMu.Unlock()
	})
	return err
}

// openTracked is os.OpenFile for a file inside the spool.
func openTracked(path string, flag int, mode os.FileMode) (*tracked, error) {
	f, err := os.OpenFile(path, flag, mode)
	if err != nil {
		return nil, err
	}
	t := &tracked{File: f, key: openKey(path)}
	openMu.Lock()
	openCount[t.key]++
	openMu.Unlock()
	return t, nil
}

func inUse(path string) bool {
	openMu.Lock()
	defer openMu.Unlock()
	return openCount[openKey(path)] > 0
}

// removeFile is os.Remove for a file inside the spool.
func removeFile(path string) error {
	if enforceOpenRule.Load() && inUse(path) {
		return &os.PathError{Op: "remove", Path: path, Err: ErrInUse}
	}
	return os.Remove(path)
}

// renameFile is os.Rename for files inside the spool.
func renameFile(from, to string) error {
	if enforceOpenRule.Load() {
		for _, p := range []string{from, to} {
			if inUse(p) {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: ErrInUse}
			}
		}
	}
	return os.Rename(from, to)
}
