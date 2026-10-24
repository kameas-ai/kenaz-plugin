package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
)

// Profile is whose memory a store holds.
//
// The local profile (zero Env/OrgID/UserID) is the device notebook every
// signed-out session uses: memory/memory.db, exactly where v1 kept it. It
// is never synced and never has an outbox; nothing in it is uploaded, even
// after the user signs in, unless the user explicitly adopts a note into an
// account profile (`kenaz memory sync adopt`), which is an authorized save.
//
// An account profile is one Fleet environment + organization + user. Each
// has its own database (records, outbox, cursor, clock) under
// memory/profiles/<key>/, so a queue can never be sent with another
// identity's token and an account switch never mixes data. While signed in,
// retrieval reads the active account profile and the local notebook;
// signed out (or after an account switch) the old account's notes leave
// retrieval at once and stay on disk until that account signs in again.
type Profile struct {
	Env    string `json:"env,omitempty"`
	OrgID  string `json:"org_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// Local is the device notebook profile.
var Local = Profile{}

// IsLocal reports whether p is the device notebook.
func (p Profile) IsLocal() bool { return p == Local }

// Key names p on disk. It is a hash, so no account identifier appears in a
// path.
func (p Profile) Key() string {
	if p.IsLocal() {
		return "local"
	}
	sum := sha256.Sum256([]byte(p.Env + "\x00" + p.OrgID + "\x00" + p.UserID))
	return p.Env + "-" + hex.EncodeToString(sum[:])[:20]
}

// Valid reports whether p is local or fully specified.
func (p Profile) Valid() bool {
	return p.IsLocal() || (p.Env != "" && p.OrgID != "" && p.UserID != "")
}

// ProfilePath is where p's store lives under the kenaz state root.
func ProfilePath(stateRoot string, p Profile) string {
	if p.IsLocal() {
		return PathIn(stateRoot)
	}
	return filepath.Join(stateRoot, DirName, "profiles", p.Key(), FileName)
}

// ErrProfileMismatch means a store file belongs to another profile than
// the one opening it (a copied or misplaced file). It is never used.
var ErrProfileMismatch = errors.New("this memory store belongs to a different account")

func bindProfile(ctx context.Context, tx *sql.Tx, p Profile, device string) error {
	for k, v := range map[string]string{
		"profile_env": p.Env, "profile_org": p.OrgID, "profile_user": p.UserID, "device_id": device,
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func readMeta(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func checkProfile(ctx context.Context, db *sql.DB, p Profile) (device string, err error) {
	var got Profile
	if got.Env, err = readMeta(ctx, db, "profile_env"); err != nil {
		return "", fmt.Errorf("%w: %v", ErrCorruptStore, err)
	}
	if got.OrgID, err = readMeta(ctx, db, "profile_org"); err != nil {
		return "", err
	}
	if got.UserID, err = readMeta(ctx, db, "profile_user"); err != nil {
		return "", err
	}
	if got != p {
		return "", ErrProfileMismatch
	}
	return readMeta(ctx, db, "device_id")
}

// newDeviceID is this store's HLC device id: random per profile store, so
// it identifies nothing about the machine.
func newDeviceID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "kz-" + hex.EncodeToString(b)
}
