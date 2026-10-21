package logx

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Canary token/email never reach the log.
func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelDebug))
	l.Info("refresh", "access_token", "CANARYTOKEN", "user_email", "canary@example.com",
		"Hostname", "canary-host", "port", 4318, "state", "enrolled")
	l.With("refresh_token", "CANARYTOKEN2").Info("x")
	out := buf.String()
	for _, c := range []string{"CANARYTOKEN", "canary@example.com", "canary-host"} {
		if strings.Contains(out, c) {
			t.Fatalf("leaked %q: %s", c, out)
		}
	}
	if !strings.Contains(out, `"port":4318`) || !strings.Contains(out, `"state":"enrolled"`) {
		t.Fatalf("benign attrs lost: %s", out)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	r := &Rotator{Path: filepath.Join(dir, "relay.log"), MaxBytes: 100, Keep: 3}
	line := []byte(strings.Repeat("a", 60) + "\n")
	for i := 0; i < 10; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	matches, _ := filepath.Glob(filepath.Join(dir, "relay.log*"))
	if len(matches) != 3 {
		t.Fatalf("files = %v", matches)
	}
	fi, _ := os.Stat(filepath.Join(dir, "relay.log"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
}
