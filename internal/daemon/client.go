package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Client talks to a running daemon over loopback, authenticated with the
// nonce from daemon.json (0600, so only the same OS user can read it).
type Client struct {
	Info Info
	HTTP *http.Client
}

// Connect reads daemon.json and verifies the daemon with Probe.
func Connect(ctx context.Context, stateDir string) (*Client, error) {
	in, err := ReadInfo(stateDir)
	if err != nil {
		return nil, err
	}
	if err := Probe(ctx, in); err != nil {
		return nil, err
	}
	return &Client{Info: in, HTTP: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", c.Info.Port, path), bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Info.Nonce)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("daemon %s %s: http %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// SessionEndPath is the control endpoint a SessionEnd hook calls.
const SessionEndPath = "/control/session-end"

// SessionEnd tells the daemon a harness session ended. Only the on-device
// local hash (canon.LocalSessionHash) is sent, never the native id.
func (c *Client) SessionEnd(ctx context.Context, harness, localHash string) error {
	b, err := json.Marshal(map[string]string{"client_kind": harness, "session_hash": localHash})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", c.Info.Port, SessionEndPath), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Info.Nonce)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("daemon POST %s: http %d", SessionEndPath, resp.StatusCode)
	}
	return nil
}

// Status fetches the daemon's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.do(ctx, http.MethodGet, "/control/status", &s)
	return s, err
}

// Ping registers or refreshes an MCP client (keeps the daemon alive).
func (c *Client) Ping(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/control/clients/"+id, nil)
}

// Bye unregisters an MCP client.
func (c *Client) Bye(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/control/clients/"+id, nil)
}

// Kick asks for an immediate heartbeat and capability refresh.
func (c *Client) Kick(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/control/kick", nil)
}

// Shutdown stops the daemon; purge also drops queued data (logout).
func (c *Client) Shutdown(ctx context.Context, purge bool) error {
	p := "/control/shutdown"
	if purge {
		p += "?purge=1"
	}
	return c.do(ctx, http.MethodPost, p, nil)
}

// SpawnFunc starts a daemon process. EnsureRunning calls it at most once.
type SpawnFunc func() error

// EnsureRunning returns a client for the healthy daemon, spawning one if
// needed and waiting up to wait for it. Concurrent callers may each
// spawn; the lockfile guarantees only one daemon survives, and every
// caller connects to it.
func EnsureRunning(ctx context.Context, stateDir string, spawn SpawnFunc, wait time.Duration) (*Client, error) {
	if c, err := Connect(ctx, stateDir); err == nil {
		return c, nil
	}
	if spawn == nil {
		return nil, ErrNotRunning
	}
	if err := spawn(); err != nil {
		return nil, fmt.Errorf("spawn daemon: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		if c, err := Connect(ctx, stateDir); err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrNotRunning
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// SpawnSelf returns a SpawnFunc that starts `<this executable> daemon
// --env <env>` detached, with output appended to stateDir/logs/daemon.out.
func SpawnSelf(stateDir string, env fleetapi.Env) SpawnFunc {
	return func() error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		logDir := filepath.Join(stateDir, "logs")
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(logDir, "daemon.out"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		cmd := spawnCommand(exe, stateDir, env)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, nil
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}

// spawnCommand is the detached daemon's command. It runs in the state
// directory, never in the spawner's working directory: an MCP host may start
// kenaz from inside a versioned install folder (Claude Desktop runs extensions
// from theirs), and a long-lived relay whose working directory is that folder
// would keep it in use, which on Windows blocks the host's update or
// uninstall.
func spawnCommand(exe, stateDir string, env fleetapi.Env) *exec.Cmd {
	cmd := exec.Command(exe, "daemon", "--env", string(env))
	cmd.Dir = stateDir
	return cmd
}

// IsNotRunning reports whether err means no daemon is reachable.
func IsNotRunning(err error) bool { return errors.Is(err, ErrNotRunning) }

// APIError is a non-2xx control-API reply. Code is the JSON error code the
// endpoint returned ({"code": ...}), if any.
type APIError struct {
	Method, Path string
	Status       int
	Code         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daemon %s %s: http %d %s", e.Method, e.Path, e.Status, e.Code)
}

// APICode returns the endpoint's error code.
func (e *APIError) APICode() string { return e.Code }

// DoJSON calls a nonce-authenticated control endpoint with in as the JSON
// body (nil: none) and decodes the JSON reply into out (nil: ignored).
func (c *Client) DoJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", c.Info.Port, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Info.Nonce)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Code string `json:"code"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Code: e.Code}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
