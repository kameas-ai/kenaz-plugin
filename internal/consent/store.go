package consent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Store persists, per Fleet user, the notice version the member accepted
// on this device. Fleet's own record (notice_version) wins when
// present. This local copy covers Fleets that predate it. It also records,
// per organization, when this device first saw Fleet admit the organization
// to early access. It holds no secrets and no telemetry.
type Store struct {
	path string
	mu   sync.Mutex
}

type storeFile struct {
	Accepted map[string]string    `json:"accepted_notice_version"` // user id → version
	Admitted map[string]time.Time `json:"admitted_at,omitempty"`   // org id → first admitted response seen
	// QueueOwner is OwnerKey of the account whose telemetry the device
	// queue (spool) may hold. Absent in files written by earlier builds.
	QueueOwner string `json:"queue_owner,omitempty"`
}

// NewStore returns a Store backed by path (created on first write).
func NewStore(path string) *Store { return &Store{path: path} }

// Accepted returns the stored version for userID, or "".
func (s *Store) Accepted(userID string) string {
	if s == nil || userID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := s.read()
	return f.Accepted[userID]
}

// SetAccepted records version for userID.
func (s *Store) SetAccepted(userID, version string) error {
	if userID == "" {
		return errors.New("consent: empty user id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	f.Accepted[userID] = version
	return s.write(f)
}

// AdmittedAt returns when this device recorded orgID's admission, or zero.
func (s *Store) AdmittedAt(orgID string) time.Time {
	if s == nil || orgID == "" {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := s.read()
	return f.Admitted[orgID]
}

// SetAdmitted records orgID's admission moment; a zero t removes it.
func (s *Store) SetAdmitted(orgID string, t time.Time) error {
	if s == nil || orgID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	if _, had := f.Admitted[orgID]; t.IsZero() && !had {
		return nil
	}
	if f.Admitted == nil {
		f.Admitted = map[string]time.Time{}
	}
	if t.IsZero() {
		delete(f.Admitted, orgID)
	} else {
		f.Admitted[orgID] = t
	}
	return s.write(f)
}

// QueueOwner returns the recorded queue owner key, or "".
func (s *Store) QueueOwner() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := s.read()
	return f.QueueOwner
}

// SetQueueOwner records the account whose telemetry may be queued.
func (s *Store) SetQueueOwner(key string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	if f.QueueOwner == key {
		return nil
	}
	f.QueueOwner = key
	return s.write(f)
}

// Forget removes userID's record (logout).
func (s *Store) Forget(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	delete(f.Accepted, userID)
	return s.write(f)
}

func (s *Store) read() (storeFile, error) {
	f := storeFile{Accepted: map[string]string{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		// A corrupt file means "nothing accepted": safe default (re-show).
		return storeFile{Accepted: map[string]string{}}, nil
	}
	if f.Accepted == nil {
		f.Accepted = map[string]string{}
	}
	return f, nil
}

func (s *Store) write(f storeFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// Write, fsync and rename, then fsync the directory: the admission
	// moment decides whether queued data is purged, so it must not revert
	// after a crash.
	tmp := s.path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// Windows cannot fsync a directory (Access is denied), and its
	// rename is durable without it.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
