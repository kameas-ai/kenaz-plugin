// Package logx is the relay's logging setup:
// structured slog to $STATE/logs/relay.log, rotated 5×5 MB, with a
// handler that redacts any attribute whose key names a secret or personal
// field. The redaction is defense in depth: call sites must not log these
// at all, and `make check-privacy-logs` greps for violations.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Rotation defaults.
const (
	DefaultMaxBytes = 5 << 20
	DefaultKeep     = 5
)

// forbidden key fragments (lowercase). Any attribute whose key contains one
// is replaced with "[redacted]".
var forbidden = []string{
	"token", "secret", "password", "authorization", "cookie",
	"email", "claim", "jwt", "hostname", "prompt", "content",
	"arguments", "tool_input", "tool_output", "path", "cwd",
}

// Redacted is the replacement value.
const Redacted = "[redacted]"

func sensitive(key string) bool {
	k := strings.ToLower(key)
	for _, f := range forbidden {
		if strings.Contains(k, f) {
			return true
		}
	}
	return false
}

// redact implements slog.HandlerOptions.ReplaceAttr.
func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.MessageKey || a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.SourceKey {
		return a
	}
	if sensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// NewHandler returns a JSON handler that redacts sensitive keys.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: redact})
}

// Rotator is an io.Writer that rotates path at MaxBytes, keeping Keep
// files: path, path.1 … path.<Keep-1>.
type Rotator struct {
	Path     string
	MaxBytes int64
	Keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func (r *Rotator) open() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *Rotator) rotate() error {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	keep := r.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	os.Remove(r.Path + "." + strconv.Itoa(keep-1))
	for i := keep - 2; i >= 1; i-- {
		os.Rename(r.Path+"."+strconv.Itoa(i), r.Path+"."+strconv.Itoa(i+1))
	}
	os.Rename(r.Path, r.Path+".1")
	return r.open()
}

// Write implements io.Writer.
func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	max := r.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > max && r.size > 0 {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the current file.
func (r *Rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// New returns a logger writing to stateDir/logs/relay.log and its closer.
func New(stateDir string, level slog.Leveler) (*slog.Logger, io.Closer) {
	r := &Rotator{Path: filepath.Join(stateDir, "logs", "relay.log")}
	return slog.New(NewHandler(r, level)), r
}

// Discard is a logger that writes nothing (tests, one-shot CLI commands).
func Discard() *slog.Logger { return slog.New(discard{}) }

type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (d discard) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discard) WithGroup(string) slog.Handler           { return d }
