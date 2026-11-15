package contextlib

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The context share grant. A person says yes once, at a terminal, and from then on
// their AI tool can share a context file with a team without an approval of its own,
// up to an hourly cap. Only `kenaz context grant on` gives it. It belongs to one
// account, and goes at sign-out and when another account signs in.
//
// What the seal is for. The grant file carries an HMAC whose key is made from three
// things in the OS keychain: the approvals key, a random id made each time the grant
// is turned on and deleted when it ends, and the session's spool key, which every
// sign-out deletes (older releases' too). So a file written without the keychain is
// no grant, and neither is a copy of a real one put back after the grant was turned
// off or after a sign-out. This stops an agent that can only use the plugin's tools
// or only write files. It doesn't stop a program running as this user that can read
// the keychain or drive a terminal; such a program can already approve a single
// request today.

// MaxGrantedSharesPerHour bounds how many files a runaway agent can share under the
// grant. Shares past it wait for an approval, as they do without the grant. It is
// lower than the memory save grant's 300: a note syncs to the person's own account,
// and a shared file is readable by a whole team.
const MaxGrantedSharesPerHour = 30

// maxShareAudit is how many grant shares the history keeps: several hours at the cap.
const maxShareAudit = 500

// ErrGrantUnavailable means there is no keychain key to seal the grant with.
var ErrGrantUnavailable = errors.New("contextlib: the context share grant needs this device's keychain")

const (
	grantFile      = "grant.json"
	grantAuditFile = "history.json"
)

// GrantBinding is what a grant is tied to outside its file, in the OS keychain.
type GrantBinding interface {
	// Current is the secret the grant in force is bound to, or nil when there is none:
	// no grant was given, it ended, or the session it was given in has ended.
	Current() []byte
	// Renew makes a new secret, for a grant about to be given.
	Renew() error
	// Forget deletes it, so that no grant file made so far is a grant.
	Forget() error
}

// ShareGrant is the grant and its history, in dir. key seals the history; key and
// the binding together seal the grant.
type ShareGrant struct {
	dir  string
	key  []byte
	bind GrantBinding
}

// NewShareGrant is the grant kept in dir. A key under 32 bytes, or no binding, means
// no keychain: nothing is read as a grant and none can be given.
func NewShareGrant(dir string, key []byte, bind GrantBinding) ShareGrant {
	if len(key) < 32 || bind == nil {
		key = nil
	}
	return ShareGrant{dir: dir, key: key, bind: bind}
}

// Available reports whether a grant can be kept on this device.
func (g ShareGrant) Available() bool { return g.dir != "" && len(g.key) > 0 && g.bind != nil }

type grantRecord struct {
	Account   Account   `json:"account"`
	GrantedAt time.Time `json:"granted_at"`
}

// ShareAudit is one share made under the grant. It never holds the file's text.
type ShareAudit struct {
	Account   Account   `json:"account"`
	Slug      string    `json:"slug"`
	TeamID    string    `json:"team_id"`
	TeamName  string    `json:"team_name,omitempty"`
	SHA256    string    `json:"sha256"`
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
	// Status is how the request ended: done, failed or canceled (nothing was shared),
	// or executing or reconciling (sent, not confirmed when this was written).
	Status string `json:"status"`
}

type sealedGrantFile struct {
	Body json.RawMessage `json:"body"`
	MAC  string          `json:"mac"`
}

// sealKey is the key for name: the grant's own depends on the binding in force.
func (g ShareGrant) sealKey(name string) []byte {
	if name != grantFile {
		return g.key
	}
	cur := g.bind.Current()
	if len(cur) == 0 {
		return nil
	}
	h := hmac.New(sha256.New, g.key)
	h.Write([]byte("kenaz-context-share-grant-binding\x00"))
	h.Write(cur)
	return h.Sum(nil)
}

func (g ShareGrant) mac(name string, b []byte) string {
	key := g.sealKey(name)
	if len(key) == 0 {
		return ""
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("kenaz-context-share-grant-v1\x00" + name + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// read loads name into v. ok is false for a missing file and for one the key doesn't
// vouch for.
func (g ShareGrant) read(name string, v any) bool {
	if !g.Available() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(g.dir, name))
	if err != nil {
		return false
	}
	var f sealedGrantFile
	if json.Unmarshal(b, &f) != nil {
		return false
	}
	want := g.mac(name, f.Body)
	return want != "" && hmac.Equal([]byte(f.MAC), []byte(want)) && json.Unmarshal(f.Body, v) == nil
}

func (g ShareGrant) write(name string, v any) error {
	if !g.Available() {
		return ErrGrantUnavailable
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	mac := g.mac(name, body)
	if mac == "" {
		return ErrGrantUnavailable
	}
	b, err := json.Marshal(sealedGrantFile{Body: body, MAC: mac})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(g.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(g.dir, name))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Give records the grant for a. Only a command a person answered at a terminal calls
// it: no tool, hook or control route reaches it.
func (g ShareGrant) Give(a Account, now time.Time) error {
	if a.OrgID == "" || a.UserID == "" {
		return errors.New("contextlib: a grant needs a signed-in account")
	}
	if !g.Available() {
		return ErrGrantUnavailable
	}
	// A new binding each time, so no earlier grant file is one again.
	if err := g.bind.Renew(); err != nil {
		return err
	}
	return g.write(grantFile, grantRecord{Account: a, GrantedAt: now})
}

// Revoke ends the grant, whoever it belongs to: the file goes and so does what it was
// bound to, so a copy of the file put back later is no grant. The history stays.
func (g ShareGrant) Revoke() error {
	var errs []error
	if g.bind != nil {
		errs = append(errs, g.bind.Forget())
	}
	if g.dir != "" {
		if err := os.Remove(filepath.Join(g.dir, grantFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Wipe ends the grant and deletes its history too, at sign-out: the history holds
// file short names and team names.
func (g ShareGrant) Wipe() error {
	err := g.Revoke()
	if g.dir != "" {
		if rerr := os.Remove(filepath.Join(g.dir, grantAuditFile)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
	}
	return err
}

// On reports whether a holds the grant, and since when. A grant that belongs to
// another account is removed: it never carries over. One the key doesn't vouch for is
// no grant.
func (g ShareGrant) On(a Account) (time.Time, bool) {
	var r grantRecord
	if !g.read(grantFile, &r) {
		return time.Time{}, false
	}
	if r.Account != a || a.OrgID == "" || a.UserID == "" {
		_ = g.Revoke()
		return time.Time{}, false
	}
	return r.GrantedAt, true
}

// Record adds one share to the history.
func (g ShareGrant) Record(e ShareAudit) error {
	var all []ShareAudit
	g.read(grantAuditFile, &all)
	all = append(all, e)
	if len(all) > maxShareAudit {
		all = all[len(all)-maxShareAudit:]
	}
	return g.write(grantAuditFile, all)
}

// Settle sets the final status of the share made for request id.
func (g ShareGrant) Settle(requestID, status string) error {
	var all []ShareAudit
	if !g.read(grantAuditFile, &all) {
		return nil
	}
	for i := range all {
		if all[i].RequestID == requestID {
			all[i].Status = status
		}
	}
	return g.write(grantAuditFile, all)
}

// History is a's shares under the grant, oldest first.
func (g ShareGrant) History(a Account) []ShareAudit {
	var all, out []ShareAudit
	g.read(grantAuditFile, &all)
	for _, e := range all {
		if e.Account == a {
			out = append(out, e)
		}
	}
	return out
}

// UsedThisHour counts a's shares under the grant in the clock hour of now.
func (g ShareGrant) UsedThisHour(a Account, now time.Time) int {
	n := 0
	for _, e := range g.History(a) {
		if e.At.Unix()/3600 == now.Unix()/3600 {
			n++
		}
	}
	return n
}
