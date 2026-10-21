package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Info is $STATE/daemon.json (mode 0600). Nonce authenticates the MCP
// process to the daemon and lets clients detect a foreign process on the
// port; it is never sent over the wire in the clear by the health check.
type Info struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Nonce     string    `json:"nonce"`
}

// File names inside the state directory.
const (
	InfoFile = "daemon.json"
	LockFile = "daemon.lock"
)

// ErrNotRunning means no healthy relay daemon was found.
var ErrNotRunning = errors.New("kenaz daemon is not running")

func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteInfo writes daemon.json atomically with mode 0600.
func WriteInfo(stateDir string, in Info) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, InfoFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, InfoFile))
}

// ReadInfo reads daemon.json.
func ReadInfo(stateDir string) (Info, error) {
	var in Info
	b, err := os.ReadFile(filepath.Join(stateDir, InfoFile))
	if errors.Is(err, os.ErrNotExist) {
		return in, ErrNotRunning
	}
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("daemon.json: %w", err)
	}
	return in, nil
}

// proof is the health-check response: HMAC-SHA256(nonce, challenge).
func proof(nonce, challenge string) string {
	m := hmac.New(sha256.New, []byte(nonce))
	m.Write([]byte(challenge))
	return hex.EncodeToString(m.Sum(nil))
}

// healthResponse is GET /healthz?challenge=<hex>.
type healthResponse struct {
	Version string `json:"version"`
	Proof   string `json:"proof"`
}

// Probe checks that the process listening on in.Port is our daemon: it
// must prove knowledge of in.Nonce for a fresh random challenge. A
// foreign listener (e.g. a local OTEL collector on 4318) fails.
func Probe(ctx context.Context, in Info) error {
	if in.Port <= 0 || in.Nonce == "" {
		return ErrNotRunning
	}
	ch, err := newNonce()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz?challenge=%s", in.Port, ch), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ErrNotRunning
	}
	defer resp.Body.Close()
	var hr healthResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hr) != nil {
		return ErrNotRunning
	}
	if !hmac.Equal([]byte(hr.Proof), []byte(proof(in.Nonce, ch))) {
		return ErrNotRunning
	}
	return nil
}
