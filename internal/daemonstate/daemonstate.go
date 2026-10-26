// Package daemonstate is the read-only view of the relay daemon's state
// file ($STATE/daemon.json) and its loopback control contract. The daemon
// itself (internal/daemon) is the only writer;
// setup and the harness hooks only read it.
//
// Contract with internal/daemon, proposed here so the
// readers can be built in parallel. internal/daemon adopts or amends it:
//
//   - daemon.json, mode 0600: {"pid", "port", "version", "started_at", "nonce"}.
//   - GET  http://127.0.0.1:<port>/healthz returns {"version", "nonce"}. A
//     nonce that does not match daemon.json means a foreign process owns
//     the port and must not be trusted.
//   - POST http://127.0.0.1:<port>/control/session-end with header
//     X-Kameas-Nonce: <nonce> and body {"session_hash", "reason", "harness"}.
//     The raw session id never crosses the socket; see SessionHash.
package daemonstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// FileName is the daemon state file inside the state directory.
const FileName = "daemon.json"

// NonceHeader authenticates control requests to the daemon.
const NonceHeader = "X-Kameas-Nonce"

// Environments the relay can target.
var Environments = []string{"prod", "stage", "dev"}

// ErrNoDaemon means daemon.json does not exist: the relay has never run in
// this environment, or it was purged by logout.
var ErrNoDaemon = errors.New("kenaz relay has not started yet (no daemon.json)")

// State is the content of daemon.json.
type State struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Nonce     string    `json:"nonce"`
	// Variant is the build running the relay ("" for releases before
	// builds recorded it, which were all kenaz).
	Variant string `json:"variant,omitempty"`
}

// Healthz is the body of GET /healthz.
type Healthz struct {
	Version string `json:"version"`
	Nonce   string `json:"nonce"`
}

// SessionEnd is the body of POST /control/session-end.
type SessionEnd struct {
	SessionHash string `json:"session_hash"`
	Reason      string `json:"reason"`
	Harness     string `json:"harness"`
}

// Resolver computes per-environment paths. Its zero value is not usable;
// use System() or fill every field in tests.
type Resolver struct {
	Home   string
	GOOS   string
	Getenv func(string) string
}

// System returns a Resolver for the current process.
func System() (Resolver, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Resolver{}, err
	}
	return Resolver{Home: home, GOOS: runtime.GOOS, Getenv: os.Getenv}, nil
}

// Env returns the selected environment: the first of the env variables
// (variant.Shared.EnvVars) that is set, or the build's default. Codex
// passes the plugin's ${user_config...} placeholders through unexpanded, so
// a value that still starts with "${" is skipped. Like config.ResolveEnv,
// an unknown environment, or one this build does not connect to (the
// staff dev build refuses prod), is an error, never a silent fallback.
func (r Resolver) Env() (string, error) {
	for _, name := range variant.Shared.EnvVars {
		v := strings.ToLower(strings.TrimSpace(r.Getenv(name)))
		if v == "" || strings.HasPrefix(v, "${") {
			continue
		}
		if !slices.Contains(Environments, v) {
			return "", fmt.Errorf("unknown Fleet env %q from %s (want %s)", v, name, variant.Current.EnvChoices("|"))
		}
		if !variant.Current.Allowed(v) {
			return "", fmt.Errorf("%s connects only to %s Fleet (%s=%s)", variant.Current.Binary, variant.Current.EnvChoices(" or "), name, v)
		}
		return v, nil
	}
	return variant.Current.DefaultEnv, nil
}

// StateDir is $STATE for env, the same on every platform and for every
// build: ~/.kenaz/plugin/<env>, or $KENAZ_HOME/plugin/<env> (see
// config.DefaultPaths, which the commands use; this is the fallback for
// callers without one).
func (r Resolver) StateDir(env string) string {
	if h := r.Getenv(variant.Shared.HomeVar); h != "" {
		return filepath.Join(h, variant.Shared.ProductDir, env)
	}
	return filepath.Join(r.Home, variant.Shared.RootDir, variant.Shared.ProductDir, env)
}

// Read loads daemon.json from stateDir.
func Read(stateDir string) (State, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNoDaemon
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", FileName, err)
	}
	if s.Port < 1 || s.Port > 65535 {
		return State{}, fmt.Errorf("%s: invalid port %d", FileName, s.Port)
	}
	return s, nil
}

// BaseURL is the daemon's loopback base URL.
func (s State) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.Port)
}

// SessionHash is the only form in which a harness session id leaves the
// hook process: the first 32 hex characters of its SHA-256. The daemon
// computes the same value from the session id it sees in OTEL to join
// SessionEnd to its session state, then applies its own keyed hash before
// anything goes to Kameas Fleet.
func SessionHash(sessionID string) string { return canon.LocalSessionHash(sessionID) }
