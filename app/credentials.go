package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// fileStoreNote is the security note doctor and the README give for the
// file credential store.
const fileStoreNote = "without an OS keychain, sign-in tokens and the spool key are protected only by file permissions in your home directory"

// errKeychainGone is what commands say when the recorded store is the OS
// keychain and it cannot be reached. The store never silently switches.
var errKeychainGone = errors.New("OS keychain unavailable; try again or run `kenaz logout` to reset")

// credStore records where this process keeps credentials. It is decided
// once per process (in RunEnv) and never changes afterwards. The first
// sign-in or relay start records the choice in a marker file, and every
// later process follows the marker instead of probing again.
type credStore struct {
	store    credstore.Store // "" means not chosen yet (hooks and mcp never probe)
	recorded bool            // store came from, or has been written to, the marker
	dir      string          // the credentials directory
	err      error           // unreadable marker, or the provider refused the file store
	probe    func() error    // the keychain probe for this process
}

func (c *credStore) files() bool { return c != nil && c.store == credstore.Files }

// label is the "credential store:" value doctor and status print.
func (c *credStore) label() string {
	switch {
	case c == nil || c.store == credstore.Keychain:
		return "OS keychain"
	case c.store == credstore.Files:
		return "files (no OS keychain)"
	default:
		return "not chosen yet (chosen at sign-in or relay start)"
	}
}

// record writes the marker the first time credentials are stored.
func (c *credStore) record() error {
	if c == nil || c.recorded || c.store == "" || c.dir == "" {
		return nil
	}
	if err := credstore.WriteMarker(c.dir, c.store); err != nil {
		return fmt.Errorf("record credential store: %w", err)
	}
	c.recorded = true
	return nil
}

// keychainReachable checks a recorded keychain store before a command
// relies on it.
func (c *credStore) keychainReachable() error {
	if c == nil || !c.recorded || c.store != credstore.Keychain || c.probe == nil {
		return nil
	}
	if c.probe() != nil {
		return errKeychainGone
	}
	return nil
}

// usable is the check commands that store credentials run first: a broken
// marker or file store, a store not chosen yet, or a recorded keychain that
// cannot be reached.
func (c *credStore) usable() error {
	if c == nil {
		return nil
	}
	if c.err != nil {
		return c.err
	}
	if c.store == "" {
		return errors.New("no credential store chosen yet")
	}
	return c.keychainReachable()
}

// keychainProbeTimeout bounds the probe: a Secret Service that never answers
// is as unusable as a missing one.
const keychainProbeTimeout = time.Second

// alwaysKeychain reports platforms whose OS keychain is always present.
func alwaysKeychain() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

// keychainProbe reports whether the OS keychain is usable. macOS and Windows
// always have one, so nothing changes there. Elsewhere (Linux and the BSDs)
// a lookup of an item that does not exist must answer "not found"; any other
// outcome (no D-Bus session, no Secret Service, no answer) means there is no
// usable keychain.
func keychainProbe() error {
	if alwaysKeychain() {
		return nil
	}
	ch := make(chan error, 1)
	go func() {
		_, err := keyring.Get(spool.KeychainService, "probe:keychain-available")
		if errors.Is(err, keyring.ErrNotFound) {
			err = nil
		}
		ch <- err
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(keychainProbeTimeout):
		return errors.New("the OS keychain did not respond")
	}
}

// paths is the on-disk layout, honoring the build's home override
// (KENAZ_HOME).
func (e Env) paths() (config.Paths, error) {
	if h := e.getenv(variant.Current.HomeVar); h != "" {
		abs, err := filepath.Abs(h)
		if err != nil {
			return config.Paths{}, err
		}
		return config.Paths{ConfigDir: filepath.Join(abs, "config"), StateRoot: filepath.Join(abs, "state")}, nil
	}
	return config.DefaultPaths()
}

// noProbeCommands run inside the harness's hook and startup budgets. They
// never probe: they follow the recorded store, or, before one is recorded,
// run with no credentials at all.
var noProbeCommands = map[string]bool{"hook": true, "mcp": true}

// withCredentialStore makes the once-per-process credential store decision
// for command cmd:
//
//  1. A recorded store (the marker) is always used as is.
//  2. Otherwise, where a keychain always exists (macOS, Windows) it is used.
//  3. Otherwise hook and mcp run with no credentials (not signed in) rather
//     than probe, and every other command probes once (at most 1 s): a
//     usable keychain selects it, anything else selects files.
//
// Files mode moves the spool key and, through fleetapi.FileCredentialStore,
// the provider's sign-in tokens to owner-only files in
// <state root>/credentials.
func (e Env) withCredentialStore(cmd string) Env {
	if e.cred != nil {
		return e
	}
	cs := &credStore{}
	e.cred = cs
	// An injected key store (tests) with no injected probe never probes.
	needProbe := e.KeychainProbe != nil || (!alwaysKeychain() && e.KeyStore == nil)
	if needProbe {
		cs.probe = e.KeychainProbe
		if cs.probe == nil {
			cs.probe = keychainProbe
		}
	}
	paths, err := e.paths()
	if err != nil {
		// resolve reports the same error to the command.
		cs.store = credstore.Keychain
		return e
	}
	cs.dir = filepath.Join(paths.StateRoot, "credentials")
	rec, err := credstore.ReadMarker(cs.dir)
	switch {
	case err != nil:
		cs.err = fmt.Errorf("credential store marker: %w", err)
		return e.useStore("")
	case rec != "":
		cs.recorded = true
		return e.useStore(rec)
	case !needProbe:
		return e.useStore(credstore.Keychain)
	case noProbeCommands[cmd]:
		return e.useStore("")
	case cs.probe() == nil:
		return e.useStore(credstore.Keychain)
	default:
		return e.useStore(credstore.Files)
	}
}

// useStore applies s. "" is the not-chosen state: no key can be created and
// the provider reads from the (empty) files directory, so nothing touches a
// keychain.
func (e Env) useStore(s credstore.Store) Env {
	cs := e.cred
	cs.store = s
	if s == credstore.Keychain {
		return e
	}
	if e.KeyStore == nil {
		if s == credstore.Files {
			e.KeyStore = spool.FileKeys{Dir: cs.dir}
		} else {
			e.KeyStore = spool.Undecided{}
		}
	}
	if e.Provider != nil {
		if fc, ok := e.Provider.Auth().TokenStore().(fleetapi.FileCredentialStore); ok {
			if err := fc.UseFiles(cs.dir); err != nil && cs.err == nil {
				cs.err = fmt.Errorf("file credential store: %w", err)
			}
		}
	}
	return e
}
