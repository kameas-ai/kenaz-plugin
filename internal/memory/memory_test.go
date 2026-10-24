package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kenaz state", DirName, FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func note(key, title, body string, tags ...string) NewNote {
	return NewNote{ProjectKey: key, ProjectRoot: "/p", Title: title, Body: body, Tags: tags, Source: SourceCLI}
}

func TestSaveSearchGetForget(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, err := s.Save(ctx, note("p1", "Build uses make check", "Run make check before pushing; it runs the hygiene gate.", "build", "ci"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, note("p2", "Other project", "make is not used here")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, note(GlobalProject, "Prefer table tests", "global convention")); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, "hygiene", ProjectScope("p1"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != id || !strings.Contains(hits[0].Snippet, "hygiene") {
		t.Fatalf("search = %+v", hits)
	}
	// Scope: current project + global, not other projects.
	if hits, _ := s.Search(ctx, "make", ProjectScope("p1"), 5); len(hits) != 1 {
		t.Fatalf("scoped search leaked across projects: %+v", hits)
	}
	if hits, _ := s.Search(ctx, "make", AllProjects, 5); len(hits) != 2 {
		t.Fatalf("all-projects search = %+v", hits)
	}
	// Prefix match on tags, and FTS operators in the query are inert.
	if hits, _ := s.Search(ctx, `bui AND NOT "x" NEAR(`, ProjectScope("p1"), 5); len(hits) != 1 {
		t.Fatalf("prefix/operator query = %+v", hits)
	}
	if _, err := s.Search(ctx, "  ** ", AllProjects, 5); err != ErrEmptyQuery {
		t.Fatalf("empty query err = %v", err)
	}

	rec, _ := s.Recent(ctx, ProjectScope("p1"), 10)
	if len(rec) != 2 {
		t.Fatalf("recent = %+v", rec)
	}

	got, missing, err := s.Get(ctx, []string{id, "nope"})
	if err != nil || len(got) != 1 || len(missing) != 1 || got[0].Body == "" || got[0].Tags[1] != "ci" || got[0].Source != SourceCLI {
		t.Fatalf("get = %+v %v %v", got, missing, err)
	}

	if err := s.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, id); err != ErrNotFound {
		t.Fatalf("second forget = %v", err)
	}
	if hits, _ := s.Search(ctx, "hygiene", AllProjects, 5); len(hits) != 0 {
		t.Fatalf("forgotten note still indexed: %+v", hits)
	}
}

func TestLimits(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	cases := map[string]NewNote{
		"long title":   note("p", strings.Repeat("t", MaxTitleChars+1), "b"),
		"big body":     note("p", "t", strings.Repeat("b", MaxBodyBytes+1)),
		"empty body":   note("p", "t", "  "),
		"many tags":    note("p", "t", "b", strings.Split("a b c d e f g h i j k", " ")...),
		"long tag":     note("p", "t", "b", strings.Repeat("x", MaxTagChars+1)),
		"bad source":   {ProjectKey: "p", Title: "t", Body: "b", Source: "other"},
		"secret title": note("p", "key "+"AKIA"+strings.Repeat("Q", 16), "b"),
	}
	for name, n := range cases {
		if _, err := s.Save(ctx, n); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	if _, err := s.Save(ctx, note("p", strings.Repeat("é", MaxTitleChars), strings.Repeat("b", MaxBodyBytes))); err != nil {
		t.Fatalf("at-limit note refused: %v", err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
}

func TestStoreCap(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	// Fill to just under 16 MiB of content directly, then one small note
	// still fits and a larger one does not.
	big := strings.Repeat("x", 4<<20)
	for i := 0; i < 4; i++ {
		if _, err := s.db.Exec(`INSERT INTO records(id, created_at, updated_at, project_key, title, content, content_hash, scope, source) VALUES (?, 0, 0, 'p', 't', ?, '', 'project', 'cli')`, fmt.Sprint("big", i), big[:len(big)-100]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Save(ctx, note("p", "n", "small")); err != nil {
		t.Fatalf("small note refused under the cap: %v", err)
	}
	if _, err := s.Save(ctx, note("p", "n", strings.Repeat("y", 1024))); err != ErrFull {
		t.Fatalf("over the byte cap: err = %v", err)
	}
	if !strings.Contains(ErrFull.Error(), "kenaz memory forget") || !strings.Contains(ErrFull.Error(), "kenaz memory purge") {
		t.Fatal("full message must point to forget and purge")
	}
	if n, _ := s.Count(ctx); n != 5 {
		t.Fatalf("a full store deleted notes: %d", n)
	}
}

func TestNoteCountCap(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.db.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < ?)
INSERT INTO records(id, created_at, updated_at, project_key, title, content, content_hash, scope, source) SELECT 'id'||i, 0, 0, 'p', 't', 'b', '', 'project', 'cli' FROM c`, MaxNotes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, note("p", "one more", "b")); err != ErrFull {
		t.Fatalf("note %d: err = %v", MaxNotes+1, err)
	}
}

func TestPermissionsAndPurge(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Save(ctx, note("p", "t", "b")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(s.Path())
		di, _ := os.Stat(filepath.Dir(s.Path()))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	path := s.Path()
	s.Close()
	if err := Purge(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("memory directory left behind: %v", err)
	}
	if _, err := OpenReadOnly(path, time.Second); err != ErrNoStore {
		t.Fatalf("read-only open after purge = %v", err)
	}
}

func TestReadOnlyOpenCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), DirName, FileName)
	if _, err := OpenReadOnly(path, time.Second); err != ErrNoStore {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("read-only open created the directory")
	}
}

func TestProjectKey(t *testing.T) {
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere\n"), 0o600) // worktree form
	p, err := ProjectFor(sub)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != root || len(p.Key) != 16 || p.Key != ProjectKey(root) {
		t.Fatalf("project = %+v (root %s)", p, root)
	}
	// Outside a repository the directory itself is the root.
	plain := t.TempDir()
	if q, _ := ProjectFor(plain); q.Key == p.Key || q.Key != ProjectKey(q.Root) {
		t.Fatalf("plain project = %+v", q)
	}
}

func TestSessionIndex(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := Project{Key: "p1", Root: "/p"}
	if got := SessionIndex(ctx, s.Path(), p); got != "" {
		t.Fatalf("empty store index = %q", got)
	}
	body := "BODYMARKER should never be in the index"
	for i := 0; i < 15; i++ {
		if _, err := s.Save(ctx, note("p1", strings.Repeat("long title ", 18), body)); err != nil {
			t.Fatal(err)
		}
	}
	got := SessionIndex(ctx, s.Path(), p)
	if got == "" || strings.Contains(got, "BODYMARKER") || len(got) > IndexMaxBytes {
		t.Fatalf("index (%d bytes): %q", len(got), got)
	}
	if lines := strings.Count(got, "\n- "); lines < 1 || lines > IndexMaxEntries {
		t.Fatalf("%d entries", lines)
	}
	if !strings.Contains(got, "memory_search") || !strings.Contains(got, "memory_get") || !strings.Contains(got, "memory_save") {
		t.Fatalf("missing guidance line: %q", got)
	}
	if got := SessionIndex(ctx, s.Path(), Project{Key: "other"}); got != "" {
		t.Fatalf("other project index = %q", got)
	}
}

func TestClaimDisclosureOnce(t *testing.T) {
	m := filepath.Join(t.TempDir(), DisclosureMarker)
	if !ClaimDisclosure(m, time.Now()) || ClaimDisclosure(m, time.Now()) {
		t.Fatal("disclosure must be claimed exactly once")
	}
}

func TestTexts(t *testing.T) {
	for _, s := range []string{Notice, Disclosure("/x/memory.db"), KeptLine("/x"), OffMessage} {
		if strings.Contains(strings.ToLower(s), "private") {
			t.Fatalf("memory must not be described as private: %q", s)
		}
	}
}
