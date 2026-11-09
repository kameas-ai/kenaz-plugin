package contextlib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Ledger is what Kenaz remembers about individual context files, kept outside
// the folders so that it travels neither with a file nor with a repository. It
// holds one small file per context file, each replaced atomically, so the MCP
// process and the relay never rewrite each other's records.
type Ledger struct{ dir string }

// NewLedger is the ledger in dir. The folder is created on the first update.
func NewLedger(dir string) Ledger { return Ledger{dir: dir} }

// FileRecord is one context file's record.
type FileRecord struct {
	// Path is the file's path as List reports it.
	Path string `json:"path"`
	// DraftedFromMemory marks text drawn from the user's private notes, until
	// that text has been shared. It holds no reference to which notes.
	DraftedFromMemory bool `json:"drafted_from_memory,omitempty"`
	// DraftedSHA256 is the hash of the marked text, as last saved through the tool.
	DraftedSHA256 string `json:"drafted_sha256,omitempty"`
	// Links tie this file to shared files on Kameas Fleet, one per account: the
	// same folder can be used by another account or environment, whose teams this
	// file must never reach by way of a link.
	Links []Link `json:"links,omitempty"`
	// Reservations hold the id of a share not approved yet, one per account, so that
	// asking twice can't make two files. They change no link: a link means the file
	// is on Fleet, and only a share that happened changes one.
	Reservations []Reservation `json:"reservations,omitempty"`
}

// Reservation is the node id a pending share of this file will use.
type Reservation struct {
	Account Account `json:"account"`
	NodeID  string  `json:"node_id"`
	TeamID  string  `json:"team_id"`
}

// ReservationFor returns the reservation belonging to a.
func (r FileRecord) ReservationFor(a Account) (Reservation, bool) {
	for _, s := range r.Reservations {
		if s.Account == a {
			return s, true
		}
	}
	return Reservation{}, false
}

// Reserve replaces the reservation belonging to s's account.
func (r *FileRecord) Reserve(s Reservation) {
	for i := range r.Reservations {
		if r.Reservations[i].Account == s.Account {
			r.Reservations[i] = s
			return
		}
	}
	r.Reservations = append(r.Reservations, s)
}

// Release drops a's reservation of node id, once that share has happened.
func (r *FileRecord) Release(a Account, id string) {
	for i, s := range r.Reservations {
		if s.Account == a && s.NodeID == id {
			r.Reservations = append(r.Reservations[:i], r.Reservations[i+1:]...)
			return
		}
	}
}

// Account is who a link belongs to.
type Account struct {
	Env    string `json:"env"`
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
}

// Link ties a local file to a shared file known to be on Kameas Fleet.
type Link struct {
	Account Account `json:"account"`
	NodeID  string  `json:"node_id"`
	TeamID  string  `json:"team_id"`
	// Version and BaseSHA256 describe the last text this device knows Fleet holds.
	Version    int    `json:"version,omitempty"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	// Authored says this account shared this file from this device.
	Authored bool `json:"authored,omitempty"`
	// OwnerUserID is the author Fleet reported, when it reported one.
	OwnerUserID string `json:"owner_user_id,omitempty"`
}

// Mine reports whether the signed-in member, with Fleet user id me, is known to
// be the file's author.
func (l Link) Mine(me string) bool {
	return l.Authored || (me != "" && l.OwnerUserID == me)
}

// LinkFor returns the link belonging to a.
func (r FileRecord) LinkFor(a Account) (Link, bool) {
	for _, l := range r.Links {
		if l.Account == a {
			return l, true
		}
	}
	return Link{}, false
}

// SetLink replaces the link belonging to l's account.
func (r *FileRecord) SetLink(l Link) {
	for i := range r.Links {
		if r.Links[i].Account == l.Account {
			r.Links[i] = l
			return
		}
	}
	r.Links = append(r.Links, l)
}

// Key names the record of the file at rel. It covers the library's folder, so
// the same name in two repositories is two files.
func (l *Library) Key(rel string) (string, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	return HashOf([]byte(string(l.source) + "\x00" + l.dir + "\x00" + clean)), nil
}

func (g Ledger) file(key string) (string, error) {
	if g.dir == "" || !fs.ValidPath(key) || path.Base(key) != key || key == "." {
		return "", fmt.Errorf("contextlib: invalid ledger key")
	}
	return filepath.Join(g.dir, key+".json"), nil
}

// Get returns the record for key, or a zero record when there is none.
func (g Ledger) Get(key string) (FileRecord, error) {
	p, err := g.file(key)
	if err != nil {
		return FileRecord{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return FileRecord{}, nil
	}
	if err != nil {
		return FileRecord{}, err
	}
	var r FileRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return FileRecord{}, fmt.Errorf("contextlib: ledger record: %w", err)
	}
	return r, nil
}

// Update applies change to the record for key and saves it.
func (g Ledger) Update(key string, change func(*FileRecord)) error {
	r, err := g.Get(key)
	if err != nil {
		return err
	}
	change(&r)
	p, err := g.file(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
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
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// LinkedFile is a ledger record with its key.
type LinkedFile struct {
	Key string
	Rec FileRecord
}

// LinksFor reads the whole ledger once and gives account a's records by node id.
func (g Ledger) LinksFor(a Account) map[string]LinkedFile {
	out := map[string]LinkedFile{}
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := g.Get(key)
		if err != nil {
			continue
		}
		if l, ok := r.LinkFor(a); ok {
			out[l.NodeID] = LinkedFile{Key: key, Rec: r}
		}
	}
	return out
}

// FindNode returns the record linking account a to node id, if any. It reads the
// whole ledger, which is one small file per context file.
func (g Ledger) FindNode(a Account, id string) (string, FileRecord, bool) {
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return "", FileRecord{}, false
	}
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := g.Get(key)
		if err != nil {
			continue
		}
		if l, ok := r.LinkFor(a); ok && l.NodeID == id {
			return key, r, true
		}
	}
	return "", FileRecord{}, false
}

// SlugOf returns the short name a file is shared under: its own name, tidied. It
// never includes a folder, because a path on this device stays on this device.
func SlugOf(rel string) (string, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	name := path.Base(clean)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	var b strings.Builder
	dash := false
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		s = "file"
	}
	if len(s) > maxSlugStem {
		s = strings.TrimRight(s[:maxSlugStem], "-")
	}
	s += strings.ToLower(ext)
	if !ValidSlug(s) {
		return "", fmt.Errorf("%w: no short name", ErrInvalidPath)
	}
	return s, nil
}

const maxSlugStem = 64

// ValidSlug reports whether s is a short name: one plain file name with a context
// file's extension. An absolute path, ~, .. or any folder fails.
func ValidSlug(s string) bool {
	stem := strings.TrimSuffix(s, path.Ext(s))
	if stem == "" || len(stem) > maxSlugStem || !hasAllowedExt(s) {
		return false
	}
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return stem[0] != '-'
}

// FolderName is a team's name made safe for a folder.
func FolderName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > maxSlugStem {
		s = strings.TrimRight(s[:maxSlugStem], "-")
	}
	if s == "" {
		return "team"
	}
	return s
}
