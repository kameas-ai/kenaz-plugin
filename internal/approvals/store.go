package approvals

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// StateFile is the pending-write store inside the approvals directory. It
// is owner-only (0600) in an owner-only (0700) directory and is read and
// written only by the relay daemon, the single process that serves the
// approval page.
const StateFile = "approvals.json"

const storeVersion = 1

// record is one prepared write. Payload and Summary are the only fields
// that carry user content; both are purged when the record can no longer
// execute (done, denied, expired, canceled, failed).
type record struct {
	ID             string    `json:"id"`
	Operation      string    `json:"operation"`
	Identity       Identity  `json:"identity"`
	TargetIDs      []string  `json:"target_ids,omitempty"`
	Payload        []byte    `json:"payload,omitempty"`
	PayloadSHA256  string    `json:"payload_sha256"`
	IdempotencyKey string    `json:"idempotency_key"`
	Summary        string    `json:"summary,omitempty"`
	Impact         string    `json:"impact,omitempty"`
	Details        []Detail  `json:"details,omitempty"`
	Status         Status    `json:"status"`
	ProfileGen     uint64    `json:"profile_gen"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Result is bounded result metadata (MaxResult bytes), kept for
	// idempotent replies after completion.
	Result []byte `json:"result,omitempty"`
	// ReceiptSHA256 is the hash of the one-use internal receipt minted when
	// the assertion verified. The receipt itself lives only in memory and
	// is consumed by the approved → executing transition.
	ReceiptSHA256 string `json:"receipt_sha256,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	// Proof is the verified WebAuthn assertion that approved this record.
	// Its challenge is derived from the record's id, payload hash,
	// operation, identity and generation (challengeFor), so it cannot be
	// moved to another record or payload. Every transition into executing
	// re-verifies it against an enrolled credential.
	Proof *proof `json:"proof,omitempty"`
}

// proof is what re-verification needs.
type proof struct {
	Nonce     []byte `json:"nonce"`
	Origin    string `json:"origin"`
	Assertion []byte `json:"assertion"`
}

func (r *record) purgeContent() {
	r.Payload = nil
	r.Summary = ""
	r.Impact = ""
	r.Details = nil
	r.TargetIDs = nil
	r.ReceiptSHA256 = ""
	r.Proof = nil
}

func (r *record) clone() *record {
	c := *r
	c.TargetIDs = append([]string(nil), r.TargetIDs...)
	c.Details = append([]Detail(nil), r.Details...)
	c.Payload = append([]byte(nil), r.Payload...)
	c.Result = append([]byte(nil), r.Result...)
	return &c
}

// storedCredential is an enrolled authenticator, bound to one identity.
type storedCredential struct {
	Identity   Identity            `json:"identity"`
	Credential webauthn.Credential `json:"credential"`
	CreatedAt  time.Time           `json:"created_at"`
}

// state is the whole persisted store.
type state struct {
	Version int `json:"version"`
	// ProfileGen increments on sign-out, account switch and credential
	// recovery. Prepared records and challenges are bound to it.
	ProfileGen uint64 `json:"profile_gen"`
	// Bound is the identity ProfileGen is bound to (nil: none yet).
	Bound *Identity `json:"bound,omitempty"`
	// CredGen increments on every change to the credential set; a
	// registration begun under an older CredGen cannot finish (first
	// registration race).
	CredGen uint64 `json:"cred_gen"`
	// UserHandles maps an identity key to its random WebAuthn user handle.
	UserHandles map[string][]byte  `json:"user_handles,omitempty"`
	Credentials []storedCredential `json:"credentials,omitempty"`
	Records     map[string]*record `json:"records"`
}

// store persists state atomically. All access is serialized by mu.
//
// The file is authenticated: it holds the state and an HMAC-SHA256 over it,
// keyed with a secret kept outside user-writable files (the OS keychain).
// A file whose MAC does not verify, or an older unauthenticated file, is
// set aside and never trusted: no record, credential or user handle from it
// is used. Without a key (no OS keychain), nothing is read or written and
// the Manager reports approval_unavailable.
//
// Threat model: the keychain key stops an agent that acts only through the
// MCP tools, or can only write files, from planting or altering records.
// It does not stop an agent that can run shell commands as the same OS
// user: on macOS go-keyring creates the item through /usr/bin/security, so
// any process of that user can read it without a prompt (an unlocked
// Secret Service or Credential Manager is similar). Such an agent can
// already read the Fleet tokens the same way, so it is out of scope.
type store struct {
	path string
	key  []byte
	mu   sync.Mutex
	st   state
	// rejected is set when a file on disk failed authentication at open.
	rejected bool
}

// sealedFile is the on-disk form.
type sealedFile struct {
	Version int             `json:"version"`
	State   json.RawMessage `json:"state"`
	MAC     string          `json:"mac"`
}

const sealedVersion = 2

func freshState() state {
	return state{Version: storeVersion, Records: map[string]*record{}, UserHandles: map[string][]byte{}}
}

func (s *store) mac(b []byte) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte("kenaz-approvals-store-v2\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func openStore(dir string, key []byte) (*store, error) {
	if len(key) == 0 {
		return &store{st: freshState()}, nil // memory only, never trusted files
	}
	if len(key) < 32 {
		return nil, errors.New("approvals store key is too short")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &store{path: filepath.Join(dir, StateFile), key: append([]byte(nil), key...), st: freshState()}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var f sealedFile
	var st state
	ok := json.Unmarshal(b, &f) == nil && f.Version == sealedVersion &&
		hmac.Equal([]byte(f.MAC), []byte(s.mac(f.State))) &&
		json.Unmarshal(f.State, &st) == nil && st.Version == storeVersion
	if !ok {
		// Tampered, written without the key, or an older format: keep it
		// for inspection but never use it.
		s.rejected = true
		if err := os.Rename(s.path, s.path+".rejected"); err != nil {
			return nil, fmt.Errorf("set aside unauthenticated approvals store: %w", err)
		}
		return s, nil
	}
	if st.Records == nil {
		st.Records = map[string]*record{}
	}
	if st.UserHandles == nil {
		st.UserHandles = map[string][]byte{}
	}
	s.st = st
	return s, nil
}

// update runs fn under the lock and persists the result if fn succeeds. A
// failed save rolls back to the previous state, so memory never runs ahead
// of disk.
func (s *store) update(fn func(st *state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	if err := fn(&s.st); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		s.restore(prev)
		return err
	}
	if err := s.save(); err != nil {
		s.restore(prev)
		return err
	}
	return nil
}

func (s *store) restore(prev []byte) {
	var st state
	if json.Unmarshal(prev, &st) == nil {
		if st.Records == nil {
			st.Records = map[string]*record{}
		}
		if st.UserHandles == nil {
			st.UserHandles = map[string][]byte{}
		}
		s.st = st
	}
}

// errNoChange, returned by an update func that changed nothing, skips the
// save.
var errNoChange = errors.New("no change")

// view runs fn under the lock without persisting.
func (s *store) view(fn func(st *state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

func (s *store) save() error {
	if s.path == "" {
		return nil // keyless: memory only
	}
	inner, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	b, err := json.Marshal(sealedFile{Version: sealedVersion, State: inner, MAC: s.mac(inner)})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".approvals-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// transition is the compare-and-swap primitive: it moves record id from one
// of from to to, applying mut, and fails with ErrConflict if the record is
// in any other state.
func (s *store) transition(id string, from []Status, to Status, now time.Time, mut func(r *record) error) (*record, error) {
	var out *record
	err := s.update(func(st *state) error {
		r, ok := st.Records[id]
		if !ok {
			return ErrNotFound
		}
		okFrom := false
		for _, f := range from {
			if r.Status == f {
				okFrom = true
				break
			}
		}
		if !okFrom {
			return fmt.Errorf("%w: %s", ErrConflict, r.Status)
		}
		if mut != nil {
			if err := mut(r); err != nil {
				return err
			}
		}
		r.Status = to
		r.UpdatedAt = now
		if to.final() {
			r.purgeContent()
		}
		out = r.clone()
		return nil
	})
	return out, err
}

// cancelOpen moves every record that has not started executing to
// canceled and purges its content. match selects records (nil: all).
func cancelOpen(st *state, now time.Time, match func(*record) bool) int {
	n := 0
	for _, r := range st.Records {
		if r.Status != StatusPending && r.Status != StatusApproved {
			continue
		}
		if match != nil && !match(r) {
			continue
		}
		r.Status = StatusCanceled
		r.UpdatedAt = now
		r.purgeContent()
		n++
	}
	return n
}

func payloadHash(p []byte) string {
	h := sha256.Sum256(p)
	return hex.EncodeToString(h[:])
}

// CancelAllOffline cancels and purges every open request in dir without a
// running daemon (used by `kenaz logout`, which stops the daemon first).
// The profile generation is bumped, so nothing prepared before sign-out can
// be approved afterwards, and unresolved writes become outcome_unknown.
// Without the store key the file cannot be trusted or rewritten, so it is
// set aside.
func CancelAllOffline(dir string, key []byte, now time.Time) error {
	path := filepath.Join(dir, StateFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if len(key) == 0 {
		return os.Rename(path, path+".rejected")
	}
	s, err := openStore(dir, key)
	if err != nil {
		return err
	}
	return s.update(func(st *state) error {
		signOutState(st, now)
		return nil
	})
}

// signOutState cancels open requests, closes unresolved ones as
// outcome_unknown (purging their content) and unbinds the profile.
func signOutState(st *state, now time.Time) {
	cancelOpen(st, now, nil)
	for _, r := range st.Records {
		if r.Status == StatusReconciling {
			r.Status, r.UpdatedAt = StatusOutcomeUnknown, now
			r.purgeContent()
		}
	}
	st.ProfileGen++
	st.Bound = nil
}
