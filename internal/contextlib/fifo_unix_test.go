//go:build !windows

package contextlib

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A named pipe in a clone must not hang the hook or a tool call.
func TestNamedPipeNeverOpened(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, "a.md"), "x")
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		l := Personal(dir)
		files, _, _, _ := l.List(context.Background())
		if len(files) != 1 {
			t.Errorf("files = %v", paths(files))
		}
		if _, err := l.Read("pipe.md", 0, 10); !errors.Is(err, ErrNotRegular) {
			t.Errorf("Read = %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a named pipe")
	}
}
