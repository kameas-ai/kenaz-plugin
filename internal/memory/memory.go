// Package memory is Kenaz's on-device memory: short notes an agent (or the
// user, through `kenaz memory`) saves and finds again in later sessions.
//
// The store is one SQLite file per OS user, outside every Fleet env
// directory. It needs no sign-in and is not affected by consent. Nothing in
// this package talks to the network: memory content is never sent to
// Kameas Fleet, and a test pins that this package imports none of the
// packages that do (fleetapi, internal/forward, internal/spool).
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
	SourceClaudeCode = "claude-code"
	SourceCodex      = "codex"
	SourceCLI        = "cli"
)

// GlobalProject is the project key of notes saved with project=global.
const GlobalProject = "global"

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

// PathIn returns the store path under the kenaz state root.
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
}

// Entry is a search or index result (never the body).
type Entry struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Snippet   string    `json:"snippet,omitempty"`
}

// Store is an open memory database.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS memories (
  id           TEXT PRIMARY KEY,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  project_key  TEXT NOT NULL,
  project_root TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL,
  body         TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '[]',
  source       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS memories_project ON memories(project_key, created_at);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  title, body, tags, content='memories', content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags);
END;
PRAGMA user_version = 1;
`

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
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String()
}

// Open opens (creating if needed) the store at path for reading and
// writing. The directory is 0700 and the database file 0600; SQLite gives
// its journal the database file's mode.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	// SQLite creates the file. Never open and close it here: closing any
	// descriptor of a file drops every POSIX lock this process holds on
	// it, including those of another open Store's connection (overlapping
	// tool calls each open the store), and two writers would then corrupt
	// the database.
	db, err := sql.Open("sqlite", dsn(path, false, 2*time.Second))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory store: %w", err)
	}
	// The directory is 0700, so the file is never reachable by others
	// before this; the journal takes the database file's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path, now: time.Now}, nil
}

// OpenReadOnly opens an existing store without creating or changing
// anything. busy bounds how long a read waits for a writer.
func OpenReadOnly(path string, busy time.Duration) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoStore
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path, true, busy))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db, path: path, now: time.Now}, nil
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

// Validate normalizes n and enforces the size limits and the secret check.
func (n *NewNote) Validate() error {
	n.Title = strings.Join(strings.FieldsFunc(n.Title, unicode.IsSpace), " ")
	if n.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(n.Title) > MaxTitleChars {
		return fmt.Errorf("%w: title is longer than %d characters", ErrInvalidInput, MaxTitleChars)
	}
	if strings.TrimSpace(n.Body) == "" {
		return fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	if !utf8.ValidString(n.Body) {
		return fmt.Errorf("%w: body is not valid UTF-8", ErrInvalidInput)
	}
	if len(n.Body) > MaxBodyBytes {
		return fmt.Errorf("%w: body is larger than %d bytes (8 KiB); save a shorter summary", ErrInvalidInput, MaxBodyBytes)
	}
	if len(n.Tags) > MaxTags {
		return fmt.Errorf("%w: at most %d tags", ErrInvalidInput, MaxTags)
	}
	seen := map[string]bool{}
	tags := make([]string, 0, len(n.Tags))
	for _, t := range n.Tags {
		t = strings.Join(strings.FieldsFunc(t, unicode.IsSpace), " ")
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > MaxTagChars {
			return fmt.Errorf("%w: tag %q is longer than %d characters", ErrInvalidInput, t, MaxTagChars)
		}
		seen[t] = true
		tags = append(tags, t)
	}
	n.Tags = tags
	switch n.Source {
	case SourceClaudeCode, SourceCodex, SourceCLI:
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

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Save validates and stores n and returns its id. It refuses (and stores
// nothing) when the store is full; nothing is ever deleted automatically.
func (s *Store) Save(ctx context.Context, n NewNote) (string, error) {
	if err := n.Validate(); err != nil {
		return "", err
	}
	tags, _ := json.Marshal(n.Tags)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var count, size int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(title AS BLOB)) + length(CAST(body AS BLOB)) + length(CAST(tags AS BLOB))), 0) FROM memories`).Scan(&count, &size); err != nil {
		return "", err
	}
	add := int64(len(n.Title) + len(n.Body) + len(tags))
	if count >= MaxNotes || size+add > MaxStoreBytes {
		return "", ErrFull
	}
	id := newID()
	now := s.now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memories(id, created_at, updated_at, project_key, project_root, title, body, tags, source) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, now, now, n.ProjectKey, n.ProjectRoot, n.Title, n.Body, string(tags), n.Source); err != nil {
		return "", err
	}
	return id, tx.Commit()
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

// Recent returns the newest notes in sc, newest first (no bodies).
func (s *Store) Recent(ctx context.Context, sc Scope, limit int) ([]Entry, error) {
	w, args := sc.where("project_key")
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, tags, created_at FROM memories WHERE `+w+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, append(args, clampLimit(limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var tags string
		var ms int64
		if err := rows.Scan(&e.ID, &e.Title, &tags, &ms); err != nil {
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

// Search full-text searches title, body and tags within sc.
func (s *Store) Search(ctx context.Context, query string, sc Scope, limit int) ([]Entry, error) {
	fq, err := ftsQuery(query)
	if err != nil {
		return nil, err
	}
	w, args := sc.where("m.project_key")
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.title, m.tags, m.created_at, snippet(memories_fts, 1, '', '', '…', 24)
FROM memories_fts JOIN memories m ON m.rowid = memories_fts.rowid
WHERE memories_fts MATCH ? AND `+w+`
ORDER BY bm25(memories_fts, 5.0, 1.0, 2.0) LIMIT ?`, append(append([]any{fq}, args...), clampLimit(limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var tags string
		var ms int64
		if err := rows.Scan(&e.ID, &e.Title, &tags, &ms, &e.Snippet); err != nil {
			return nil, err
		}
		e.Tags, e.CreatedAt = decodeTags(tags), time.UnixMilli(ms).UTC()
		e.Snippet = Clip(strings.Join(strings.Fields(e.Snippet), " "), MaxSnippet)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns the notes with the given ids, in the order asked; missing
// ids are returned separately.
func (s *Store) Get(ctx context.Context, ids []string) (found []Note, missing []string, err error) {
	for _, id := range ids {
		var n Note
		var tags string
		var c, u int64
		err := s.db.QueryRowContext(ctx, `SELECT id, created_at, updated_at, project_key, project_root, title, body, tags, source FROM memories WHERE id = ?`, id).
			Scan(&n.ID, &c, &u, &n.ProjectKey, &n.ProjectRoot, &n.Title, &n.Body, &tags, &n.Source)
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		n.Tags, n.CreatedAt, n.UpdatedAt = decodeTags(tags), time.UnixMilli(c).UTC(), time.UnixMilli(u).UTC()
		found = append(found, n)
	}
	return found, missing, nil
}

// Forget hard-deletes one note (secure_delete overwrites its pages).
func (s *Store) Forget(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Count returns how many notes are stored.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n)
	return n, err
}

// Purge deletes the whole store: the database and its journal files. The
// memory directory is removed if it is then empty.
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
