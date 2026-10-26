package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Build metadata set at link time by the release pipeline:
// -X .../app.BuildCommit=<public commit> -X .../app.PrivateModuleVersion=<v>.
var (
	BuildCommit          = ""
	PrivateModuleVersion = ""
)

// flavorLine is what doctor and status print first.
func flavorLine(p fleetapi.Provider) string {
	if p.Flavor() == fleetapi.FlavorUnofficial {
		return "unofficial (stub)"
	}
	commit, pmv := BuildCommit, PrivateModuleVersion
	if commit == "" {
		commit = "unknown-commit"
	}
	if pmv == "" {
		pmv = "unknown-module"
	}
	return fmt.Sprintf("official %s+%s", commit, pmv)
}

func (e Env) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

func (e Env) keyStore() spool.KeyStore {
	if e.KeyStore != nil {
		return e.KeyStore
	}
	return spool.Keychain{}
}

// resolved is the per-invocation env + paths.
type resolved struct {
	Env      fleetapi.Env
	Source   config.Source
	Paths    config.Paths
	StateDir string
	// Conflict is set when another build's relay is using the
	// environment (only one build at a time can).
	Conflict error
}

// resolve applies the env precedence and creates the state directory. It
// refuses an environment another build is using (Conflict).
func (e Env) resolve(flagEnv string) (resolved, error) {
	r, err := e.resolveState(flagEnv)
	if err == nil && r.Conflict != nil {
		return resolved{}, r.Conflict
	}
	return r, err
}

// resolveState is resolve, reporting another build's use of the
// environment in Conflict instead of failing (mcp serves anyway; doctor
// diagnoses it).
func (e Env) resolveState(flagEnv string) (resolved, error) {
	paths, err := e.paths()
	if err != nil {
		return resolved{}, err
	}
	file, err := config.LoadFile(paths.ConfigFile())
	if err != nil {
		return resolved{}, err
	}
	env, src, err := config.ResolveEnv(flagEnv, e.getenv, file)
	if err != nil {
		return resolved{}, err
	}
	dir, err := paths.EnsureState(env)
	if err != nil {
		return resolved{}, err
	}
	return resolved{Env: env, Source: src, Paths: paths, StateDir: dir, Conflict: otherBuildRelay(dir)}, nil
}

// otherBuildRelay refuses a Fleet environment whose relay another build
// is running. Every build keeps an environment's state in the same
// directory, and only one build at a time can use it: the relay signs in,
// queues and syncs skills with its own build's Fleet connection.
// daemon.json names the build, so this costs a file read, plus a health
// check only when the file names another build.
func otherBuildRelay(stateDir string) error {
	in, err := daemon.ReadInfo(stateDir)
	if err != nil || in.Build() == variant.Current.Name {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if daemon.Probe(ctx, in) != nil {
		return nil // not running: the next relay of this build takes over
	}
	return &daemon.OtherBuildError{Variant: in.Build(), PID: in.PID}
}

// spawner returns the func that starts the on-demand relay daemon for r
// (a detached self-exec unless a test replaces it).
func (r resolved) spawner(env Env) func() error {
	if env.Spawn != nil {
		return func() error { return env.Spawn(r.StateDir, r.Env) }
	}
	return daemon.SpawnSelf(r.StateDir, r.Env)
}

// connectDaemon returns a client for a healthy daemon, or nil.
func connectDaemon(ctx context.Context, stateDir string) *daemon.Client {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, stateDir)
	if err != nil {
		return nil
	}
	return c
}

// errNoDisplay means there is no graphical session to open a browser in.
var errNoDisplay = errors.New("no graphical display")

// openerWait is how long openBrowser waits for the platform opener to fail
// before assuming it handed the URL to a browser.
var openerWait = 2 * time.Second

// openBrowser opens url with the platform opener. Errors are returned so
// the caller can fall back to printing the URL (it always prints anyway).
// On Linux and the BSDs, no DISPLAY or WAYLAND_DISPLAY means no browser, and
// an opener that exits non-zero right away (xdg-open finding nothing to run)
// counts as a failure.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errNoDisplay
		}
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(openerWait):
		return nil
	}
}
