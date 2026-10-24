package memory

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := filepath.Join(t.TempDir(), "kenaz state", DirName, FileName)
	os.MkdirAll(filepath.Dir(dst), 0o700)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return dst
}

// testdata/v1-memory.db was written by the shipped v1 store (five notes:
// project, global, unicode, and two deliberate identical saves). The
// upgrade keeps every note, id, title, body, tag, project, source and
// timestamp, keeps them searchable, and queues nothing for Fleet.
func TestUpgradeV1Lossless(t *testing.T) {
	ctx := context.Background()
	path := copyFixture(t, "v1-memory.db")
	// Read-only opens (session index, search) upgrade transparently.
	ro, err := OpenReadOnly(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := ro.Count(ctx); n != 5 {
		t.Fatalf("count after upgrade = %d", n)
	}
	ro.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != SchemaVersion {
		t.Fatalf("user_version = %d", v)
	}
	es, err := s.Recent(ctx, AllProjects, 20)
	if err != nil || len(es) != 5 {
		t.Fatalf("recent = %v %v", es, err)
	}
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
		if len(e.ID) != 16 {
			t.Fatalf("v1 id changed: %q", e.ID)
		}
	}
	notes, missing, err := s.Get(ctx, ids)
	if err != nil || len(missing) != 0 {
		t.Fatal(missing, err)
	}
	byTitle := map[string]Note{}
	for _, n := range notes {
		byTitle[n.Title] = n
		if n.SyncState != StateLocal || n.Origin != "device" {
			t.Fatalf("upgraded note not local: %+v", n)
		}
	}
	b := byTitle["Build uses make check"]
	if b.Body != "Run make check before pushing; it runs the hygiene gate." || b.ProjectKey != "0123456789abcdef" ||
		b.ProjectRoot != "/home/dev/app" || strings.Join(b.Tags, ",") != "build,ci" || b.Source != SourceClaudeCode || b.Scope != ScopeProject ||
		!b.CreatedAt.Equal(time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("project note = %+v", b)
	}
	if g := byTitle["Prefer table tests"]; g.Scope != ScopeGlobal || g.ProjectKey != GlobalProject || g.Source != SourceCodex {
		t.Fatalf("global note = %+v", g)
	}
	if u := byTitle["Ünïcode title ✓"]; u.Body != "Body with emoji 🚀 and quotes \"x\"." {
		t.Fatalf("unicode = %+v", u)
	}
	if hits, _ := s.Search(ctx, "hygiene", AllProjects, 5); len(hits) != 1 {
		t.Fatalf("search after upgrade = %v", hits)
	}
	if hits, _ := s.Search(ctx, "duplicate", AllProjects, 5); len(hits) != 2 {
		t.Fatalf("deliberate duplicates merged: %v", hits)
	}
	var queued int
	s.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&queued)
	if queued != 0 {
		t.Fatal("upgrade queued notes for upload")
	}
	var hash string
	s.db.QueryRow(`SELECT content_hash FROM records WHERE title = 'Prefer table tests'`).Scan(&hash)
	if hash != ContentHash("Global convention: table-driven tests with t.Run.") {
		t.Fatal("content hash not filled")
	}
	// The upgraded store still takes new notes and deletes old ones.
	if _, err := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "new", Body: "after upgrade", Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "hygiene", AllProjects, 5); len(hits) != 0 {
		t.Fatal("forgotten upgraded note still indexed")
	}
}

// A v1 store is the device notebook: opening it as an account profile is
// refused, so v1 notes can never be adopted into a synced account by
// accident.
func TestUpgradeBindsDeviceProfile(t *testing.T) {
	path := copyFixture(t, "v1-memory.db")
	if _, err := OpenProfile(path, Profile{Env: "dev", OrgID: "o", UserID: "u"}); !errors.Is(err, ErrProfileMismatch) {
		t.Fatalf("err = %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DirName, FileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`PRAGMA user_version = 99`)
	s.Close()
	if _, err := Open(path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("open = %v", err)
	}
	if _, err := OpenReadOnly(path, time.Second); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("ro open = %v", err)
	}
}

func TestCorruptStoreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), DirName, FileName)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(strings.Repeat("not a database ", 400)), 0o600)
	if _, err := Open(path); err == nil {
		t.Fatal("opened garbage")
	}
	before, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(before), "not a database") {
		t.Fatal("a corrupt store was overwritten")
	}
}

// A failed migration leaves v1 intact (transactional).
func TestMigrationIsTransactional(t *testing.T) {
	path := copyFixture(t, "v1-memory.db")
	db, err := sql.Open("sqlite", dsn(path, false, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Sabotage: a table the v2 migration creates already exists.
	if _, err := db.Exec(`CREATE TABLE aliases (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("migration succeeded over a conflicting table")
	}
	db, _ = sql.Open("sqlite", dsn(path, true, time.Second))
	defer db.Close()
	var v, n int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n)
	if v != 1 || n != 5 {
		t.Fatalf("after failed upgrade: version %d, %d v1 notes", v, n)
	}
}

func TestProfileIsolation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a := Profile{Env: "dev", OrgID: "o1", UserID: "u1"}
	b := Profile{Env: "dev", OrgID: "o1", UserID: "u2"}
	if ProfilePath(root, a) == ProfilePath(root, b) || ProfilePath(root, Local) != PathIn(root) {
		t.Fatal("profiles share a path")
	}
	if strings.Contains(ProfilePath(root, a), "u1") || strings.Contains(ProfilePath(root, a), "o1") {
		t.Fatal("account ids in a path")
	}
	sa, err := OpenProfile(ProfilePath(root, a), a)
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	if _, err := sa.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "a", Body: "only a", Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenProfile(ProfilePath(root, a), b); !errors.Is(err, ErrProfileMismatch) {
		t.Fatalf("other profile opened a's store: %v", err)
	}
	if _, err := OpenProfile(ProfilePath(root, a), Profile{Env: "dev"}); err == nil {
		t.Fatal("incomplete profile accepted")
	}
}

// Request keys make retries idempotent; deliberate identical saves stay
// separate notes.
func TestIdempotentSaves(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	n := NewNote{ProjectKey: GlobalProject, Title: "t", Body: "same", Source: SourceCLI}
	r1, err := s.SaveNote(ctx, n, SaveOptions{RequestKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.SaveNote(ctx, n, SaveOptions{RequestKey: "k1"})
	if err != nil || r2.ID != r1.ID || !r2.Duplicate {
		t.Fatalf("replay = %+v %v", r2, err)
	}
	r3, _ := s.SaveNote(ctx, n, SaveOptions{RequestKey: "k2"})
	r4, _ := s.SaveNote(ctx, n, SaveOptions{})
	if r3.ID == r1.ID || r4.ID == r1.ID || r3.ID == r4.ID {
		t.Fatal("deliberate saves were deduplicated by text")
	}
	if c, _ := s.Count(ctx); c != 3 {
		t.Fatalf("count = %d", c)
	}
	// A replayed forget after the note is gone returns the first result.
	f1, err := s.ForgetNote(ctx, r1.ID, MutateOptions{RequestKey: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if f2, err := s.ForgetNote(ctx, r1.ID, MutateOptions{RequestKey: "f1"}); err != nil || f2 != f1 {
		t.Fatalf("forget replay = %+v %v", f2, err)
	}
	// And a replayed save never resurrects it.
	if r, _ := s.SaveNote(ctx, n, SaveOptions{RequestKey: "k1"}); !r.Duplicate {
		t.Fatal("replayed save after forget stored a new note")
	}
	if c, _ := s.Count(ctx); c != 2 {
		t.Fatalf("count after forget = %d", c)
	}
}

// The device notebook never queues anything, whatever happens to it.
func TestDeviceNotebookNeverQueues(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, _ := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "t", Body: "b", Source: SourceCLI})
	nb := "b2"
	if _, err := s.UpdateNote(ctx, id, Update{Body: &nb}, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableSync(ctx, "v", MutateOptions{}); !errors.Is(err, ErrLocalProfile) {
		t.Fatalf("enable on device notebook = %v", err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n)
	if n != 0 {
		t.Fatal("device notebook queued work")
	}
}

// A shipped v1 binary opening a v2 store runs its own DDL (the verbatim
// schemaV1: an empty memories table, its index, FTS and triggers), saves
// notes there and sets user_version back to 1. The next v2 open treats the
// store as v2 because its tables exist, merges the stray v1 notes into
// records once (no loss, no duplicates), drops the v1 tables and restores
// user_version 2.
func TestV1BinaryOverV2StoreReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DirName, FileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "v2 note", Body: "saved by v2", Source: SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// What the v1 binary does.
	db, err := sql.Open("sqlite", dsn(path, false, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO memories(id, created_at, updated_at, project_key, title, body, source) VALUES (?,?,?,?,?,?,?)`
	if _, err := db.Exec(ins, "01JV1NOTE0000000000000000A", 1, 1, "proj0123456789ab", "from v1", "saved by the old binary", "cli"); err != nil {
		t.Fatal(err)
	}
	// An id the v2 store already holds is not duplicated.
	if _, err := db.Exec(ins, kept, 1, 1, GlobalProject, "dup", "duplicate of a v2 note", "cli"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // the repair is idempotent
		s, err = Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		var v, stray int
		s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
		s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'memories%'`).Scan(&stray)
		if v != SchemaVersion || stray != 0 {
			t.Fatalf("reopen %d: user_version %d, %d v1 objects left", i, v, stray)
		}
		if n, _ := s.Count(ctx); n != 2 {
			t.Fatalf("reopen %d: count = %d, want 2", i, n)
		}
		got, err := s.Search(ctx, "old binary", AllProjects, 10)
		if err != nil || len(got) != 1 {
			t.Fatalf("reopen %d: merged note not searchable: %v %d", i, err, len(got))
		}
		f, _, _ := s.Get(ctx, []string{kept})
		if len(f) != 1 || f[0].Body != "saved by v2" {
			t.Fatalf("reopen %d: v2 note changed: %+v", i, f)
		}
		s.Close()
	}

	// The v1 binary leaving user_version at 2 (tables only) is repaired
	// too.
	db, _ = sql.Open("sqlite", dsn(path, false, time.Second))
	db.Exec(schemaV1)
	db.Exec(ins, "01JV1NOTE0000000000000000B", 1, 1, GlobalProject, "again", "second stray", "cli")
	db.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, _ := s.Count(ctx); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
}
