// Package memory is Kenaz's memory store: short notes an agent (or the
// user, through `kenaz memory`) saves and finds again in later sessions.
//
// Notes live in SQLite on this device, one database per profile (see
// Profile): the device notebook used while signed out, and one per Kameas
// Fleet account. Search is keyword search over the local index only; there
// is no remote search and no embeddings.
//
// Nothing in this package talks to the network, and a test pins that it
// imports none of the packages that do. Optional Fleet sync (off by
// default, separate consent, human-authorized) is the job of
// internal/memory/memsync, which reads this store's outbox and applies
// Fleet's answers through the methods in sync*.go. Until a user turns sync
// on for an account, nothing in that account's store is queued for upload,
// and the device notebook is never queued at all.
//
// When an agent reads a memory into a session, that text becomes part of
// the conversation and goes to the user's AI provider like anything else
// the agent reads. That is the harness's business, not this package's.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go SQLite with FTS5; no cgo
)

// Limits on one note and on the whole store.
const (
	MaxTitleChars = 200
	MaxBodyBytes  = 8 << 10
	MaxTags       = 10
	MaxTagChars   = 40
	MaxNotes      = 2000
	MaxStoreBytes = 16 << 20 // sum of title, body and tag bytes
	MaxLimit      = 20
	DefaultLimit  = 10
	MaxGetIDs     = 10
	MaxSnippet    = 200
)

// Sources are who saved a note.
const (
	SourceClaudeCode    = "claude-code"
	SourceClaudeDesktop = "claude-desktop"
	SourceCodex         = "codex"
	SourceCLI           = "cli"
	SourceOpenCode      = "opencode"
)

// GlobalProject is the project key of notes saved with project=global, and
// of every note pulled from Fleet (global and long_term scopes are not tied
// to one checkout).
const GlobalProject = "global"

// Scopes. Fleet syncs global and long_term; project notes stay on the
// device until Fleet supports project scope (scope_not_supported_yet).
const (
	ScopeProject  = "project"
	ScopeGlobal   = "global"
	ScopeLongTerm = "long_term"
)

// Sync states of a record, as status and tool results report them.
const (
	// StateLocal: on this device only (sync off, the device notebook, or a
	// scope Fleet does not sync).
	StateLocal = "local"
	// StateUnapproved: sync is on but no human has authorized uploading
	// this note yet; nothing is queued.
	StateUnapproved = "unapproved"
	// StatePending: queued for Fleet, not yet acknowledged.
	StatePending = "pending"
	// StateSynced: Fleet acknowledged it (accepted, unchanged or merged),
	// or it came from Fleet.
	StateSynced = "synced"
	// StateRejected: Fleet (or the on-device credential scan) refused it
	// permanently; SyncCode says why. It stays on this device.
	StateRejected = "rejected"
	// StateDenied: a human denied the authorization to upload it.
	StateDenied = "denied"
)

// Errors callers map to messages.
var (
	ErrNoStore      = errors.New("no memory store yet")
	ErrNotFound     = errors.New("no memory with that id")
	ErrFull         = errors.New("Kenaz memory is full (2,000 notes or 16 MiB). Nothing was saved. Remove notes with `kenaz memory forget <id>`, or delete everything with `kenaz memory purge`.")
	ErrLooksSecret  = errors.New("Not saved: this looks like it contains a secret.")
	ErrEmptyQuery   = errors.New("the query has no searchable words")
	ErrInvalidInput = errors.New("invalid memory")
)

// DirName and FileName place the store under the kenaz state root.
const (
	DirName  = "memory"
	FileName = "memory.db"
)

// PathIn returns the device notebook's path under the kenaz state root.
func PathIn(stateRoot string) string { return filepath.Join(stateRoot, DirName, FileName) }

// Note is one stored memory.
type Note struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ProjectKey  string    `json:"project_key"`
	ProjectRoot string    `json:"project_root,omitempty"` // local display only
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Tags        []string  `json:"tags"`
	Source      string    `json:"source"`
	Scope       string    `json:"scope"`
	Kind        string    `json:"kind"`
	// Origin is "device" (saved here) or "fleet" (pulled from the
	// account's other devices).
	Origin    string `json:"origin"`
	SyncState string `json:"sync_state"`
	SyncCode  string `json:"sync_code,omitempty"`
	// Replaces is the id this note is replacing while an edit is still
	// converging with Fleet (see replace.go).
	Replaces string `json:"replaces,omitempty"`
}

// Entry is a search or index result (never the body).
type Entry struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Snippet   string    `json:"snippet,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	SyncState string    `json:"sync_state,omitempty"`
	// Rank orders search hits (lower is better). It is not exported.
	Rank float64 `json:"-"`
}

// Store is an open memory database for one profile.
type Store struct {
	db      *sql.DB
	path    string
	now     func() time.Time
	profile Profile
	device  string
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Profile returns whose store this is.
func (s *Store) Profile() Profile { return s.profile }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// dsn builds a file: URI SQLite accepts on every platform (spaces in
// "Application Support" are escaped; Windows drive paths get a leading /).
func dsn(path string, readOnly bool, busy time.Duration) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	if !readOnly {
		q.Add("_pragma", "secure_delete(on)")
		q.Add("_pragma", "foreign_keys(on)")
		q.Set("_txlock", "immediate")
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String()
}

// writerBusy is how long a write waits for another process's write (the
// MCP server, the CLI and the relay's sync worker and approval executors
// share each store). Writers take the lock up front (BEGIN IMMEDIATE), so
// a waiting writer never fails half way through a transaction.
const writerBusy = 5 * time.Second

// Open opens (creating if needed) the device notebook store at path.
func Open(path string) (*Store, error) { return OpenProfile(path, Local) }

// OpenProfile opens (creating if needed) p's store at path for reading and
// writing, upgrading its schema transactionally. The directory is 0700 and
// the database file 0600; SQLite gives its journal the database file's
// mode. A file that belongs to another profile is refused.
func OpenProfile(path string, p Profile) (*Store, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("%w: incomplete profile", ErrInvalidInput)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	// SQLite creates the file. Never open and close it here: closing any
	// descriptor of a file drops every POSIX lock this process holds on
	// it, including those of another open Store's connection (the relay
	// opens profile stores while its sync worker holds one), and two
	// writers would then corrupt the database.
	db, err := sql.Open("sqlite", dsn(path, false, writerBusy))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := migrate(ctx, db, p, newDeviceID); err != nil {
		db.Close()
		return nil, err
	}
	// The directory is 0700, so the file is never reachable by others
	// before this; the journal takes the database file's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	dev, err := checkProfile(ctx, db, p)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path, now: time.Now, profile: p, device: dev}, nil
}

// OpenReadOnly opens an existing device-notebook store without changing
// its contents. busy bounds how long a read waits for a writer.
func OpenReadOnly(path string, busy time.Duration) (*Store, error) {
	return OpenProfileReadOnly(path, Local, busy)
}

// OpenProfileReadOnly opens p's existing store read-only. A store still on
// an older schema is upgraded first (that is the only write).
func OpenProfileReadOnly(path string, p Profile, busy time.Duration) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoStore
		}
		return nil, err
	}
	open := func() (*sql.DB, int, error) {
		db, err := sql.Open("sqlite", dsn(path, true, busy))
		if err != nil {
			return nil, 0, err
		}
		db.SetMaxOpenConns(1)
		var v int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			db.Close()
			return nil, 0, fmt.Errorf("%w: %v", ErrCorruptStore, err)
		}
		return db, v, nil
	}
	db, v, err := open()
	if err != nil {
		return nil, err
	}
	if v > SchemaVersion {
		db.Close()
		return nil, ErrNewerSchema
	}
	if v < SchemaVersion {
		db.Close()
		w, err := OpenProfile(path, p)
		if err != nil {
			return nil, err
		}
		w.Close()
		if db, _, err = open(); err != nil {
			return nil, err
		}
	}
	dev, err := checkProfile(context.Background(), db, p)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path, now: time.Now, profile: p, device: dev}, nil
}

// NewNote is what Save takes.
type NewNote struct {
	ProjectKey  string
	ProjectRoot string
	Title       string
	Body        string
	Tags        []string
	Source      string
}

func normSpace(s string) string { return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ") }

func validTitle(t string) (string, error) {
	t = normSpace(t)
	if t == "" {
		return "", fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(t) > MaxTitleChars {
		return "", fmt.Errorf("%w: title is longer than %d characters", ErrInvalidInput, MaxTitleChars)
	}
	return t, nil
}

func validBody(b string) error {
	if strings.TrimSpace(b) == "" {
		return fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	if !utf8.ValidString(b) || strings.ContainsRune(b, 0) {
		return fmt.Errorf("%w: body is not valid UTF-8 text", ErrInvalidInput)
	}
	if len(b) > MaxBodyBytes {
		return fmt.Errorf("%w: body is larger than %d bytes (8 KiB); save a shorter summary", ErrInvalidInput, MaxBodyBytes)
	}
	return nil
}

func validTags(in []string) ([]string, error) {
	if len(in) > MaxTags {
		return nil, fmt.Errorf("%w: at most %d tags", ErrInvalidInput, MaxTags)
	}
	seen := map[string]bool{}
	tags := make([]string, 0, len(in))
	for _, t := range in {
		t = normSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > MaxTagChars {
			return nil, fmt.Errorf("%w: tag %q is longer than %d characters", ErrInvalidInput, t, MaxTagChars)
		}
		seen[t] = true
		tags = append(tags, t)
	}
	return tags, nil
}

// Validate normalizes n and enforces the size limits and the secret check.
func (n *NewNote) Validate() error {
	t, err := validTitle(n.Title)
	if err != nil {
		return err
	}
	n.Title = t
	if err := validBody(n.Body); err != nil {
		return err
	}
	if n.Tags, err = validTags(n.Tags); err != nil {
		return err
	}
	switch n.Source {
	case SourceClaudeCode, SourceClaudeDesktop, SourceCodex, SourceCLI, SourceOpenCode:
	default:
		return fmt.Errorf("%w: unknown source %q", ErrInvalidInput, n.Source)
	}
	if n.ProjectKey == "" {
		return fmt.Errorf("%w: no project", ErrInvalidInput)
	}
	if LooksLikeSecret(n.Title) || LooksLikeSecret(n.Body) || LooksLikeSecret(strings.Join(n.Tags, " ")) {
		return ErrLooksSecret
	}
	return nil
}

// newID mints a record id: "mem-" and 32 hex characters, within Fleet's
// [A-Za-z0-9._-]{1,64}.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "mem-" + hex.EncodeToString(b)
}

func newKey() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// SaveOptions qualify a save.
type SaveOptions struct {
	// RequestKey makes the save idempotent: a repeated key returns the
	// first save's result and stores nothing new. Deliberate saves of the
	// same text with different keys stay separate notes.
	RequestKey string
	// SyncApproved records that a human authorized uploading this note.
	// Without it, a note saved while sync is on stays StateUnapproved and
	// nothing is queued.
	SyncApproved bool
}

// SaveResult is what a save did.
type SaveResult struct {
	ID        string `json:"id"`
	State     string `json:"sync_state"`
	Code      string `json:"sync_code,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// Save validates and stores n and returns its id. It refuses (and stores
// nothing) when the store is full; nothing is ever deleted automatically.
func (s *Store) Save(ctx context.Context, n NewNote) (string, error) {
	r, err := s.SaveNote(ctx, n, SaveOptions{})
	return r.ID, err
}

func scopeOf(projectKey string) string {
	if projectKey == GlobalProject {
		return ScopeGlobal
	}
	return ScopeProject
}

// SaveNote stores n durably in one transaction with its index entry, HLC
// tick and (when eligible) its outbox upsert.
func (s *Store) SaveNote(ctx context.Context, n NewNote, o SaveOptions) (SaveResult, error) {
	if err := n.Validate(); err != nil {
		return SaveResult{}, err
	}
	tags, _ := json.Marshal(n.Tags)
	var res SaveResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if o.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, o.RequestKey); err != nil {
				return err
			} else if ok {
				if err := json.Unmarshal(prev, &res); err != nil {
					return err
				}
				res.Duplicate = true
				return nil
			}
		}
		var count, size int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(title AS BLOB)) + length(CAST(content AS BLOB)) + length(CAST(tags AS BLOB))), 0) FROM records`).Scan(&count, &size); err != nil {
			return err
		}
		add := int64(len(n.Title) + len(n.Body) + len(tags))
		if count >= MaxNotes || size+add > MaxStoreBytes {
			return ErrFull
		}
		h, err := tickHLC(ctx, tx, s.now(), s.device)
		if err != nil {
			return err
		}
		now := s.now().UnixMilli()
		r := record{
			ID: newID(), Content: n.Body, Hash: ContentHash(n.Body), Title: n.Title, Kind: "raw",
			Scope: scopeOf(n.ProjectKey), ProjectKey: n.ProjectKey, ProjectRoot: n.ProjectRoot, Tags: string(tags),
			Source: n.Source, Origin: "device", CreatedAt: now, UpdatedAt: now,
			CreatedHLC: string(h), TitleHLC: string(h), ScopeHLC: string(h), Visibility: "visible",
		}
		if err := insertRecord(ctx, tx, r, o.RequestKey); err != nil {
			return err
		}
		st, code, _, err := s.makeEligible(ctx, tx, &r, o.SyncApproved, 0)
		if err != nil {
			return err
		}
		res = SaveResult{ID: r.ID, State: st, Code: code}
		if o.RequestKey != "" {
			return recordApplied(ctx, tx, o.RequestKey, "memory.save", res, now)
		}
		return nil
	})
	return res, err
}

// Scope selects notes by project. An empty Keys means every project.
type Scope struct{ Keys []string }

// ProjectScope is the current project plus global notes.
func ProjectScope(key string) Scope { return Scope{Keys: []string{key, GlobalProject}} }

// AllProjects is every note in the store.
var AllProjects = Scope{}

func (sc Scope) where(col string) (string, []any) {
	if len(sc.Keys) == 0 {
		return "1=1", nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(sc.Keys)), ",")
	args := make([]any, len(sc.Keys))
	for i, k := range sc.Keys {
		args[i] = k
	}
	return col + " IN (" + ph + ")", args
}

func clampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

func decodeTags(s string) []string {
	var t []string
	_ = json.Unmarshal([]byte(s), &t)
	return t
}

// Recent returns the newest visible notes in sc, newest first (no bodies).
func (s *Store) Recent(ctx context.Context, sc Scope, limit int) ([]Entry, error) {
	w, args := sc.where("project_key")
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, tags, created_at, scope, sync_state FROM records WHERE visibility = 'visible' AND `+w+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, append(args, clampLimit(limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var tags string
		var ms int64
		if err := rows.Scan(&e.ID, &e.Title, &tags, &ms, &e.Scope, &e.SyncState); err != nil {
			return nil, err
		}
		e.Tags, e.CreatedAt = decodeTags(tags), time.UnixMilli(ms).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// ftsQuery turns free text into an FTS5 query: each word is quoted (so
// FTS5 operators in the input are inert), prefix-matched and ORed;
// results are ranked by bm25.
func ftsQuery(q string) (string, error) {
	words := wordRe.FindAllString(q, 16)
	if len(words) == 0 {
		return "", ErrEmptyQuery
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + w + `"*`
	}
	return strings.Join(parts, " OR "), nil
}

// Search full-text searches title, body and tags of visible notes in sc.
// Hidden (replaced) and forgotten notes are not in the index.
func (s *Store) Search(ctx context.Context, query string, sc Scope, limit int) ([]Entry, error) {
	fq, err := ftsQuery(query)
	if err != nil {
		return nil, err
	}
	w, args := sc.where("m.project_key")
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.title, m.tags, m.created_at, m.scope, m.sync_state, snippet(records_fts, 1, '', '', '…', 24), bm25(records_fts, 5.0, 1.0, 2.0)
FROM records_fts JOIN records m ON m.rowid = records_fts.rowid
WHERE records_fts MATCH ? AND m.visibility = 'visible' AND `+w+`
ORDER BY bm25(records_fts, 5.0, 1.0, 2.0) LIMIT ?`, append(append([]any{fq}, args...), clampLimit(limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var tags string
		var ms int64
		if err := rows.Scan(&e.ID, &e.Title, &tags, &ms, &e.Scope, &e.SyncState, &e.Snippet, &e.Rank); err != nil {
			return nil, err
		}
		e.Tags, e.CreatedAt = decodeTags(tags), time.UnixMilli(ms).UTC()
		e.Snippet = Clip(strings.Join(strings.Fields(e.Snippet), " "), MaxSnippet)
		out = append(out, e)
	}
	return out, rows.Err()
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// resolveID follows an alias (an id Fleet merged into a canonical one).
func resolveID(ctx context.Context, q rowQuerier, id string) string {
	var c string
	if err := q.QueryRowContext(ctx, `SELECT canonical_id FROM aliases WHERE alias_id = ?`, id).Scan(&c); err == nil {
		return c
	}
	return id
}

const noteCols = `r.id, r.created_at, r.updated_at, r.project_key, r.project_root, r.title, r.content, r.tags, r.source, r.scope, r.kind, r.provenance, r.sync_state, r.sync_code,
coalesce((SELECT old_id FROM replacements j WHERE j.new_id = r.id AND j.phase NOT IN ('complete','canceled') ORDER BY job_id DESC LIMIT 1), '')`

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	var tags string
	var c, u int64
	err := row.Scan(&n.ID, &c, &u, &n.ProjectKey, &n.ProjectRoot, &n.Title, &n.Body, &tags, &n.Source, &n.Scope, &n.Kind, &n.Origin, &n.SyncState, &n.SyncCode, &n.Replaces)
	n.Tags, n.CreatedAt, n.UpdatedAt = decodeTags(tags), time.UnixMilli(c).UTC(), time.UnixMilli(u).UTC()
	if n.Tags == nil {
		n.Tags = []string{}
	}
	return n, err
}

// Get returns the visible notes with the given ids (aliases resolve), in
// the order asked; missing ids are returned separately.
func (s *Store) Get(ctx context.Context, ids []string) (found []Note, missing []string, err error) {
	for _, id := range ids {
		n, err := scanNote(s.db.QueryRowContext(ctx, `SELECT `+noteCols+` FROM records r WHERE r.id = ? AND r.visibility = 'visible'`, resolveID(ctx, s.db, id)))
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		found = append(found, n)
	}
	return found, missing, nil
}

// Count returns how many visible notes are stored.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE visibility = 'visible'`).Scan(&n)
	return n, err
}

// Purge deletes a whole store file: the database and its journal files.
// The directory is removed if it is then empty.
func Purge(path string) error {
	var errs []error
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	_ = os.Remove(filepath.Dir(path)) // only succeeds when empty
	return errors.Join(errs...)
}

// PurgeAll deletes the device notebook and every account profile store
// under stateRoot. It does not touch anything held by Kameas Fleet.
func PurgeAll(stateRoot string) error {
	var errs []error
	for _, p := range ProfileStores(stateRoot) {
		errs = append(errs, Purge(p))
	}
	_ = os.Remove(filepath.Join(stateRoot, DirName, "profiles"))
	errs = append(errs, Purge(PathIn(stateRoot)))
	return errors.Join(errs...)
}

// ProfileStores lists the account profile store paths under stateRoot.
func ProfileStores(stateRoot string) []string {
	var out []string
	dir := filepath.Join(stateRoot, DirName, "profiles")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name(), FileName)
		if _, err := os.Stat(p); e.IsDir() && err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Clip truncates s to at most n bytes on a rune boundary, marking the cut
// with an ellipsis.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := n - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}

// tx runs fn in one IMMEDIATE transaction (the DSN's _txlock), so
// concurrent writers from the MCP process, the CLI and the relay serialize
// instead of failing half way.
//
// Every write transaction ends by dropping held-forget labels whose op is
// gone, whichever path removed it (ack, revert, pulled tombstone, reset).
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return pruneHeldLabels(ctx, tx)
	})
}

func appliedResult(ctx context.Context, tx *sql.Tx, key string) ([]byte, bool, error) {
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT result FROM applied_requests WHERE request_key = ?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func recordApplied(ctx context.Context, tx *sql.Tx, key, op string, result any, now int64) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO applied_requests(request_key, operation, result, created_at) VALUES (?,?,?,?)`, key, op, b, now)
	return err
}

// DeviceID is this store's HLC device id (random per profile store).
func (s *Store) DeviceID() string { return s.device }
