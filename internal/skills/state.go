package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Files under <state>/skills/.
const (
	stateSubdir       = "skills"
	subscriptionsFile = "subscriptions.json"
	syncStateFile     = "sync.json"
	generationFile    = "generation"
	hookSeenFile      = "hook-seen"
	lockFile          = "skills.lock"
)

// Subscription is one member-chosen catalog item. Version is the
// pinned version; "" follows the latest on each poll.
type Subscription struct {
	CatalogID string `json:"catalog_id"`
	Kind      string `json:"kind"`
	Slug      string `json:"slug,omitempty"`
	Version   string `json:"version,omitempty"`
}

// Store is the skills state directory: subscriptions, sync state, the
// reload generation and the cross-process lock shared by the daemon's sync
// and the MCP tools.
type Store struct{ Dir string }

// NewStore returns the store under stateDir.
func NewStore(stateDir string) Store { return Store{Dir: filepath.Join(stateDir, stateSubdir)} }

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

func (s Store) ensure() error { return os.MkdirAll(s.Dir, 0o700) }

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp-" + randSuffix()
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s Store) readJSON(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s Store) writeJSON(name string, v any) error {
	if err := s.ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(name), append(b, '\n'))
}

// Subscriptions returns the subscriptions keyed by catalog id.
func (s Store) Subscriptions() (map[string]Subscription, error) {
	var list []Subscription
	if err := s.readJSON(subscriptionsFile, &list); err != nil {
		return nil, err
	}
	out := make(map[string]Subscription, len(list))
	for _, sub := range list {
		out[sub.CatalogID] = sub
	}
	return out, nil
}

// SaveSubscriptions persists subs.
func (s Store) SaveSubscriptions(subs map[string]Subscription) error {
	list := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		list = append(list, sub)
	}
	sortSubs(list)
	return s.writeJSON(subscriptionsFile, list)
}

func sortSubs(l []Subscription) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].CatalogID < l[j-1].CatalogID; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// SyncState is what the daemon remembers between config polls.
type SyncState struct {
	Checksum string            `json:"checksum,omitempty"`
	Mandated map[string]string `json:"mandated,omitempty"` // catalog id -> version last applied
	Report   *Report           `json:"report,omitempty"`
}

// LoadSyncState reads the sync state.
func (s Store) LoadSyncState() (SyncState, error) {
	var st SyncState
	err := s.readJSON(syncStateFile, &st)
	return st, err
}

// SaveSyncState writes the sync state.
func (s Store) SaveSyncState(st SyncState) error { return s.writeJSON(syncStateFile, st) }

// Generation returns the materialization generation (0 if never written).
func (s Store) Generation() int64 {
	b, err := os.ReadFile(s.path(generationFile))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// BumpGeneration records that managed skill directories changed.
func (s Store) BumpGeneration() error {
	if err := s.ensure(); err != nil {
		return err
	}
	return writeFileAtomic(s.path(generationFile), []byte(strconv.FormatInt(s.Generation()+1, 10)+"\n"))
}

// ReloadNeeded reports whether managed skills changed since the last
// SessionStart that asked, and records the current generation as seen. The
// Claude SessionStart hook returns reloadSkills:true when this
// is true.
func (s Store) ReloadNeeded() bool {
	gen := s.Generation()
	b, _ := os.ReadFile(s.path(hookSeenFile))
	seen, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if gen == seen {
		return false
	}
	if s.ensure() == nil {
		_ = writeFileAtomic(s.path(hookSeenFile), []byte(strconv.FormatInt(gen, 10)+"\n"))
	}
	return true
}

// Lock takes the cross-process skills lock, waiting up to wait. A lock
// older than staleAfter is treated as abandoned.
func (s Store) Lock(wait time.Duration) (unlock func(), err error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	const staleAfter = 2 * time.Minute
	p := s.path(lockFile)
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > staleAfter {
			_ = os.Remove(p)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("skills: another kenaz process is updating skills; try again shortly")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Reset deletes subscriptions and sync state (logout).
func (s Store) Reset() error {
	for _, f := range []string{subscriptionsFile, syncStateFile} {
		if err := os.Remove(s.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
