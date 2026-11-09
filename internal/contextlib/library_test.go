package contextlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// repo makes a git repository and returns its root and project library folder.
func repo(t *testing.T) (string, string) {
	t.Helper()
	r := realTemp(t)
	if err := os.Mkdir(filepath.Join(r, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return r, filepath.Join(r, ".kenaz", "context")
}

func paths(files []Entry) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func listOf(t *testing.T, l *Library) []Entry {
	t.Helper()
	files, _, _, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type rules struct {
	Traversal      []string `json:"traversal"`
	BadExtensions  []string `json:"bad_extensions"`
	GoodExtensions []string `json:"good_extensions"`
	MaxFileBytes   int      `json:"max_file_bytes"`
	TrashTTLDays   int      `json:"trash_ttl_days"`
}

// The fixtures shared with the harness library.
func TestHarnessRules(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r rules
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.MaxFileBytes != MaxFileBytes || time.Duration(r.TrashTTLDays)*24*time.Hour != TrashTTL {
		t.Fatalf("limits differ from the fixtures: %+v", r)
	}
	dir := realTemp(t)
	l := Personal(dir)
	for _, p := range r.Traversal {
		if _, err := l.Read(p, 0, 100); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Read(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
	for _, name := range r.BadExtensions {
		write(t, filepath.Join(dir, name), "x")
		if _, err := l.Read(name, 0, 100); !errors.Is(err, ErrUnsupportedExt) {
			t.Errorf("Read(%q) = %v, want ErrUnsupportedExt", name, err)
		}
	}
	for _, name := range r.GoodExtensions {
		write(t, filepath.Join(dir, name), "x")
		if _, err := l.Read(name, 0, 100); err != nil {
			t.Errorf("Read(%q) = %v", name, err)
		}
	}
	got := map[string]bool{}
	for _, e := range listOf(t, l) {
		got[e.Rel()] = true
	}
	if runtime.GOOS == "linux" { // A.MD and a.md are one file on other systems
		for _, name := range r.GoodExtensions {
			if !got[name] {
				t.Errorf("List missed %s", name)
			}
		}
	}
	for _, name := range r.BadExtensions {
		if got[name] {
			t.Errorf("List showed %s", name)
		}
	}
}

// Beyond the harness's fixtures: other spellings of leaving the library, and
// anything hidden.
func TestPathsRefused(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, ".trash", "x.md"), "trashed")
	write(t, filepath.Join(dir, ".hidden.md"), "hidden")
	l := Personal(dir)
	for _, p := range []string{
		"", ".", "..", "./", `..\escape.md`, `foo\..\..\escape.md`, `\etc\passwd.md`, `\\srv\share\x.md`, `C:\x.md`, "C:x.md",
		"a.md:stream.md", "a\x00b.md", "a\nb.md", "a\u202eb.md", ".trash/x.md", ".hidden.md", "foo/.trash/x.md",
		"a\u2028b.md", "a\u2029b.md", "a\U000E0100b.md", "a\u3164b.md", "a<b.md", "a>b.md", "a[b.md", "a]b.md", "a`b.md",
	} {
		if _, err := l.Read(p, 0, 100); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Read(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
	set := Set{Personal: l}
	for _, p := range []string{"", "x.md", "other/x.md", "project/../personal/x.md", "personal/../../x.md"} {
		if _, err := set.Read(p, 0, 100); !errors.Is(err, ErrInvalidPath) && !errors.Is(err, ErrNoProject) {
			t.Errorf("Set.Read(%q) = %v", p, err)
		}
	}
}

func TestSymlinks(t *testing.T) {
	base := realTemp(t)
	dir := filepath.Join(base, "ctx")
	write(t, filepath.Join(dir, "real.md"), "real")
	write(t, filepath.Join(base, "secret.md"), "secret")
	// A sibling sharing the root's name as a prefix: the harness's HasPrefix
	// check would pass it.
	write(t, filepath.Join(base, "ctx-evil", "x.md"), "secret")
	symlink(t, "real.md", filepath.Join(dir, "inside.md"))
	symlink(t, "../secret.md", filepath.Join(dir, "file-out.md"))
	symlink(t, filepath.Join(base, "secret.md"), filepath.Join(dir, "abs-out.md"))
	symlink(t, base, filepath.Join(dir, "dir-out"))
	symlink(t, "../ctx-evil", filepath.Join(dir, "prefix-out"))
	symlink(t, ".", filepath.Join(dir, "cycle"))
	symlink(t, "missing.md", filepath.Join(dir, "dangling.md"))

	l := Personal(dir)
	for _, p := range []string{"file-out.md", "abs-out.md", "dir-out/secret.md", "prefix-out/x.md"} {
		if pg, err := l.Read(p, 0, 100); !errors.Is(err, ErrSymlinkEscape) {
			t.Errorf("Read(%q) = %q, %v; want ErrSymlinkEscape", p, pg.Content, err)
		}
	}
	if pg, err := l.Read("inside.md", 0, 100); err != nil || pg.Content != "real" {
		t.Errorf("a link inside the library: %q %v", pg.Content, err)
	}
	files, skipped, _, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(files), " "); got != "personal/inside.md personal/real.md" {
		t.Errorf("List = %s", got)
	}
	if skipped < 3 {
		t.Errorf("skipped = %d, want the escaping links counted", skipped)
	}
}

// A cloned repository cannot point its library at another folder.
func TestProjectRootSymlinkRefused(t *testing.T) {
	secrets := realTemp(t)
	write(t, filepath.Join(secrets, "id.md"), "secret")
	for name, link := range map[string]string{"context": ".kenaz/context", "dotdir": ".kenaz"} {
		t.Run(name, func(t *testing.T) {
			r, _ := repo(t)
			target := secrets
			if name == "dotdir" {
				write(t, filepath.Join(secrets, "context", "id.md"), "secret")
			}
			symlink(t, target, filepath.Join(r, filepath.FromSlash(link)))
			l := Project(r)
			if _, _, _, err := l.List(context.Background()); !errors.Is(err, ErrSymlinkEscape) {
				t.Errorf("List = %v", err)
			}
			if _, err := l.Read("id.md", 0, 100); !errors.Is(err, ErrSymlinkEscape) {
				t.Errorf("Read = %v", err)
			}
			if out := Index((Set{Project: l, Personal: Personal(filepath.Join(r, "none"))}).List(context.Background()), 1000); out != "" {
				t.Errorf("Index = %q", out)
			}
		})
	}
}

// Your own library may be a link you made.
func TestPersonalRootMayBeSymlink(t *testing.T) {
	base := realTemp(t)
	write(t, filepath.Join(base, "real", "a.md"), "x")
	symlink(t, filepath.Join(base, "real"), filepath.Join(base, "contexts"))
	if got := paths(listOf(t, Personal(filepath.Join(base, "contexts")))); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestProjectDetection(t *testing.T) {
	r, dir := repo(t)
	write(t, filepath.Join(dir, "a.md"), "x")
	sub := filepath.Join(r, "x", "y")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if l := Project(sub); l == nil || l.Dir() != dir {
		t.Fatalf("from a subfolder: %+v", l)
	}
	// .git may be a file, as in a git worktree.
	wt := realTemp(t)
	write(t, filepath.Join(wt, ".git"), "gitdir: elsewhere\n")
	if Project(wt) == nil {
		t.Fatal("a worktree is a repository")
	}
	if Project(realTemp(t)) != nil || Project("") != nil {
		t.Fatal("no repository, no project library")
	}
}

func TestSizeCap(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, "fits.md"), strings.Repeat("x", MaxFileBytes))
	write(t, filepath.Join(dir, "over.md"), strings.Repeat("x", MaxFileBytes+1))
	l := Personal(dir)
	if pg, err := l.Read("fits.md", 0, MaxFileBytes); err != nil || !pg.EOF || len(pg.Content) != MaxFileBytes {
		t.Fatalf("at the limit: %d %v", len(pg.Content), err)
	}
	if _, err := l.Read("over.md", 0, 100); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	files := listOf(t, l)
	if len(files) != 2 || files[0].TooLarge || !files[1].TooLarge {
		t.Fatalf("listing: %+v", files)
	}
}

func TestDirectoryAndMissing(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, "sub.md", "a.md"), "x")
	write(t, filepath.Join(dir, "file.md"), "x")
	l := Personal(dir)
	if _, err := l.Read("sub.md", 0, 100); !errors.Is(err, ErrIsDirectory) {
		t.Errorf("directory: %v", err)
	}
	for _, p := range []string{"nope.md", "file.md/under.md"} {
		if _, err := l.Read(p, 0, 100); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", p, err)
		}
	}
	missing := Personal(filepath.Join(dir, "none"))
	if _, err := missing.Read("a.md", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing library: %v", err)
	}
	if files := listOf(t, missing); len(files) != 0 {
		t.Errorf("missing library: %v", files)
	}
	if _, err := os.Stat(filepath.Join(dir, "none")); !os.IsNotExist(err) {
		t.Error("reading must not create the folder")
	}
}

func TestPaging(t *testing.T) {
	dir := realTemp(t)
	text := strings.Repeat("héllo wörld 世界 ", 500)
	write(t, filepath.Join(dir, "a.md"), text)
	l := Personal(dir)
	var got strings.Builder
	var off int64
	for i := 0; ; i++ {
		pg, err := l.Read("a.md", off, 97)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(pg.Content) {
			t.Fatalf("page %d is not valid UTF-8", i)
		}
		got.WriteString(pg.Content)
		if pg.EOF {
			break
		}
		if pg.NextOffset <= off {
			t.Fatalf("no progress at %d", off)
		}
		off = pg.NextOffset
	}
	if got.String() != text {
		t.Fatal("pages do not add up to the file")
	}
	// An offset inside a character moves to the next one; past the end is empty.
	if pg, _ := l.Read("a.md", 2, 3); !utf8.ValidString(pg.Content) || pg.Offset != 3 {
		t.Fatalf("mid-character: %+v", pg)
	}
	if pg, err := l.Read("a.md", int64(len(text)+10), 10); err != nil || pg.Content != "" || !pg.EOF {
		t.Fatalf("past the end: %+v %v", pg, err)
	}
	if _, err := l.Read("a.md", -1, 10); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("negative: %v", err)
	}
}

func TestTrash(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, "doomed.md"), "bye")
	// Last edited long ago. A rename keeps that time, and the 30 days must
	// still run from the deletion.
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "doomed.md"), past, past); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l := Personal(dir)
	l.now = func() time.Time { return now }
	if err := l.Delete("doomed.md"); err != nil {
		t.Fatal(err)
	}
	if got := listOf(t, l); len(got) != 0 {
		t.Fatalf("a trashed file is listed: %v", got)
	}
	ents, err := os.ReadDir(filepath.Join(dir, ".trash"))
	if err != nil || len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), "-doomed.md") {
		t.Fatalf("trash: %v %v", ents, err)
	}
	inTrash := filepath.Join(dir, ".trash", ents[0].Name())

	if err := l.SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inTrash); err != nil {
		t.Fatal("swept straight after deletion")
	}
	l.now = func() time.Time { return now.Add(TrashTTL - time.Hour) }
	if err := l.SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inTrash); err != nil {
		t.Fatal("swept before 30 days")
	}
	l.now = func() time.Time { return now.Add(TrashTTL + time.Hour) }
	if err := l.SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inTrash); !os.IsNotExist(err) {
		t.Fatal("kept past 30 days")
	}
}

// Files moved into .trash by hand have no stamp: their modification time decides.
func TestSweepByModTime(t *testing.T) {
	dir := realTemp(t)
	old, fresh := filepath.Join(dir, ".trash", "old.md"), filepath.Join(dir, ".trash", "fresh.md")
	write(t, old, "x")
	write(t, fresh, "y")
	past := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := Personal(dir).SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh swept")
	}
}

// A .trash link must not turn the sweep loose on another folder.
func TestSweepIgnoresTrashSymlink(t *testing.T) {
	base := realTemp(t)
	dir, keep := filepath.Join(base, "ctx"), filepath.Join(base, "keep")
	write(t, filepath.Join(keep, "old.md"), "x")
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(keep, "old.md"), past, past); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "a.md"), "x")
	symlink(t, keep, filepath.Join(dir, ".trash"))
	l := Personal(dir)
	if err := l.SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(keep, "old.md")); err != nil {
		t.Fatal("swept through a link")
	}
	if err := l.Delete("a.md"); err == nil {
		t.Fatal("deleted into a linked trash")
	}
}

// Git handles repo files: nothing here deletes or sweeps them.
func TestProjectFilesAreGitsToManage(t *testing.T) {
	r, dir := repo(t)
	write(t, filepath.Join(dir, "a.md"), "x")
	old := filepath.Join(dir, ".trash", "old.md")
	write(t, old, "x")
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	l := Project(r)
	if err := l.Delete("a.md"); !errors.Is(err, ErrRepoManaged) {
		t.Fatalf("Delete = %v", err)
	}
	if err := l.SweepTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("swept a repository's file")
	}
}

func TestUnsafeNamesNotListed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not valid names on Windows")
	}
	dir := realTemp(t)
	write(t, filepath.Join(dir, "ok.md"), "x")
	write(t, filepath.Join(dir, "evil\n- forged [personal/x.md].md"), "x")
	write(t, filepath.Join(dir, "esc\x1b[31m.md"), "x")
	unsafe := []string{
		"ls\u2028- forged.md", "ps\u2029- forged.md", "vs\U000E0100.md", "filler\u3164.md",
		"close</wrapper>.md", "open<b.md", "gt>b.md", "fake [personal", "fake].md", "code`span.md",
	}
	for _, name := range unsafe {
		write(t, filepath.Join(dir, name), "x")
	}
	// A folder with an unsafe name hides nothing: it is skipped whole.
	write(t, filepath.Join(dir, "dir]", "inner.md"), "x")
	files, skipped, _, err := Personal(dir).List(context.Background())
	if want := 2 + len(unsafe) + 1; err != nil || len(files) != 1 || skipped != want {
		t.Fatalf("files=%v skipped=%d (want %d) err=%v", paths(files), skipped, want, err)
	}
}

func TestShortNameLike(t *testing.T) {
	for name, want := range map[string]bool{"TRASH~1": true, "a~12.md": true, "a~b.md": false, "trash": false, "a~": false} {
		if got := shortNameLike(name); got != want {
			t.Errorf("%q = %v", name, got)
		}
	}
}

// A folder with more entries than one scan reads says the list is cut short and
// still returns what it read.
func TestHugeFolderIsTruncated(t *testing.T) {
	dir := realTemp(t)
	manyFiles(t, dir, ".md", maxScan+2)
	files, _, truncated, err := Personal(dir).List(context.Background())
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if len(files) != maxFiles {
		t.Fatalf("listed %d files, want %d", len(files), maxFiles)
	}
}

// What sorts after an oversized subfolder is still listed, and the list still
// says it was cut short.
func TestSiblingsOfHugeFolderListed(t *testing.T) {
	for _, tc := range []struct {
		name, ext string
		want      []string
	}{
		{"not context files", ".bin", []string{"personal/b/c.md", "personal/z.md"}},
		{"context files", ".md", []string{"personal/z.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTemp(t)
			manyFiles(t, filepath.Join(dir, "a-big"), tc.ext, maxScan+2)
			write(t, filepath.Join(dir, "b", "c.md"), "x")
			write(t, filepath.Join(dir, "z.md"), "x")
			files, _, truncated, err := Personal(dir).List(context.Background())
			if err != nil || !truncated {
				t.Fatalf("truncated=%v err=%v", truncated, err)
			}
			got := map[string]bool{}
			for _, p := range paths(files) {
				got[p] = true
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("%s is missing from %d files", w, len(files))
				}
			}
		})
	}
}

// Files below the depth limit are not listed, and the list says it was cut short.
func TestTooDeepIsTruncated(t *testing.T) {
	dir := realTemp(t)
	deep := dir
	for i := range maxDepth - 1 {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	write(t, filepath.Join(deep, "last.md"), "x")
	files, _, truncated, err := Personal(dir).List(context.Background())
	if err != nil || len(files) != 1 || truncated {
		t.Fatalf("at the limit: files=%v truncated=%v err=%v", paths(files), truncated, err)
	}
	write(t, filepath.Join(deep, "below", "lost.md"), "x")
	files, _, truncated, err = Personal(dir).List(context.Background())
	if err != nil || len(files) != 1 || !truncated {
		t.Fatalf("past the limit: files=%v truncated=%v err=%v", paths(files), truncated, err)
	}
}

// Hidden files count toward the limit on a whole scan, though none is listed.
func TestHiddenFilesCountTowardTheScan(t *testing.T) {
	dir := realTemp(t)
	for i := range maxTotalScan/maxScan + 1 {
		d := filepath.Join(dir, fmt.Sprintf("d%d", i))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for j := range maxScan { // exactly maxScan, so no single folder overflows
			if err := os.WriteFile(filepath.Join(d, fmt.Sprintf(".x%05d", j)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, truncated, err := Personal(dir).List(context.Background()); err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
}

// A whole scan stays bounded however many folders there are.
func TestScanBounded(t *testing.T) {
	dir := realTemp(t)
	for i := range maxTotalScan/maxScan + 1 {
		manyFiles(t, filepath.Join(dir, fmt.Sprintf("d%d", i)), ".bin", maxScan)
	}
	write(t, filepath.Join(dir, "z-last", "late.md"), "x")
	files, _, truncated, err := Personal(dir).List(context.Background())
	if err != nil || !truncated || len(files) != 0 {
		t.Fatalf("files=%v truncated=%v err=%v", paths(files), truncated, err)
	}
}

func manyFiles(t *testing.T, dir, ext string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("x%05d%s", i, ext)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListCancelled(t *testing.T) {
	dir := realTemp(t)
	write(t, filepath.Join(dir, "a.md"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files, _, truncated, _ := Personal(dir).List(ctx)
	if len(files) != 0 || !truncated {
		t.Fatalf("files=%v truncated=%v", files, truncated)
	}
}

func TestMetaParsing(t *testing.T) {
	for _, c := range []struct {
		name, in, title, desc string
		always                bool
	}{
		{"frontmatter", "---\ntitle: T\ndescription: \"D\"\nalways: true\n---\n# H\nbody\n", "T", "D", true},
		{"crlf", "---\r\ntitle: T\r\nalways: yes\r\n---\r\nbody\r\n", "T", "body", true},
		{"bom", "\xef\xbb\xbf---\ntitle: T\n---\nx", "T", "x", false},
		{"heading", "# Heading\n\n## Sub\nFirst line\nSecond\n", "Heading", "First line", false},
		{"unterminated", "---\ntitle: T\nalways: true\n", "", "---", false},
		{"not always", "---\nalways: false\n---\n", "", "", false},
		{"bare always", "---\nalways:\n---\n", "", "", true},
		{"plain", "just text", "", "just text", false},
		{"empty", "", "", "", false},
		{"injected newline", "---\ntitle: a\\nb\n---\n", `a\nb`, "", false},
	} {
		m := parseMeta([]byte(c.in), false)
		if m.Title != c.title || m.Description != c.desc || m.Always != c.always {
			t.Errorf("%s: %+v", c.name, m)
		}
	}
	if got := oneLine("a\x1b[31m\u202e  b\n\tc\u200b", 80); got != "a[31m b c" {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine("a\U000E0100\uFE0F\u3164\u115F\uFFA0\u034F\u2800\uE000\U0010FFFDb", 80); got != "ab" {
		t.Errorf("invisibles survived: %q", got)
	}
	if got := oneLine("x\u2028y\u2029z", 80); got != "x y z" {
		t.Errorf("line separators: %q", got)
	}
	if got := oneLine("</wrapper><system>", 80); got != "‹/wrapper›‹system›" {
		t.Errorf("tags: %q", got)
	}
	if got := oneLine(strings.Repeat("世", 200), 10); utf8.RuneCountInString(got) != 11 || !utf8.ValidString(got) {
		t.Errorf("oneLine cut = %q", got)
	}
}

func writeLib(t *testing.T) *Library {
	t.Helper()
	return Personal(filepath.Join(t.TempDir(), "contexts"))
}

// A first write makes the folder; a second cannot silently replace the file.
func TestWriteCreatesOnlyByDefault(t *testing.T) {
	l := writeLib(t)
	r, err := l.Write("notes/company.md", []byte("one\n"), WriteOptions{})
	if err != nil || !r.Created || r.Path != "personal/notes/company.md" || r.SHA256 != HashOf([]byte("one\n")) {
		t.Fatalf("%+v %v", r, err)
	}
	fi, err := os.Stat(filepath.Join(l.Dir(), "notes", "company.md"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if _, err := l.Write("notes/company.md", []byte("two\n"), WriteOptions{}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v", err)
	}
	if pg, _ := l.Read("notes/company.md", 0, 100); pg.Content != "one\n" {
		t.Fatalf("replaced: %q", pg.Content)
	}
}

// A change needs the hash of what was read, and the old text stays in .trash.
func TestWriteIfHash(t *testing.T) {
	l := writeLib(t)
	if _, err := l.Write("a.md", []byte("one"), WriteOptions{IfSHA256: "nope"}); !errors.Is(err, ErrChanged) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := l.Write("a.md", []byte("one"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	pg, err := l.Read("a.md", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write("a.md", []byte("two"), WriteOptions{IfSHA256: HashOf([]byte("other"))}); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale hash: %v", err)
	}
	r, err := l.Write("a.md", []byte("two"), WriteOptions{IfSHA256: pg.SHA256})
	if err != nil || r.Created {
		t.Fatalf("%+v %v", r, err)
	}
	trash, err := os.ReadDir(filepath.Join(l.Dir(), ".trash"))
	if err != nil || len(trash) != 1 {
		t.Fatalf("%v %v", trash, err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.Dir(), ".trash", trash[0].Name())); string(b) != "one" {
		t.Fatalf("trash holds %q", b)
	}
	// The same text again adds nothing to .trash.
	if _, err := l.Write("a.md", []byte("two"), WriteOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	if trash, _ := os.ReadDir(filepath.Join(l.Dir(), ".trash")); len(trash) != 1 {
		t.Fatalf("trash grew: %d", len(trash))
	}
	entries, _ := os.ReadDir(l.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".write-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestWriteRefuses(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Project(repo).Write("a.md", []byte("x"), WriteOptions{}); !errors.Is(err, ErrRepoManaged) {
		t.Fatalf("project: %v", err)
	}
	if entries, _ := os.ReadDir(repo); len(entries) != 1 {
		t.Fatalf("project write touched the repo: %v", entries)
	}
	l := writeLib(t)
	deep := strings.Repeat("d/", maxDepth) + "a.md"
	for _, c := range []struct {
		path    string
		content []byte
		want    error
	}{
		{"../a.md", []byte("x"), ErrInvalidPath},
		{"/etc/a.md", []byte("x"), ErrInvalidPath},
		{".trash/a.md", []byte("x"), ErrInvalidPath},
		{"a[1].md", []byte("x"), ErrInvalidPath},
		{deep, []byte("x"), ErrInvalidPath},
		{"a.sh", []byte("x"), ErrUnsupportedExt},
		{"a.md", make([]byte, MaxFileBytes+1), ErrFileTooLarge},
		{"a.md", []byte{0xff, 0xfe}, ErrNotText},
		{"a.md", []byte("a\x00b"), ErrNotText},
	} {
		if _, err := l.Write(c.path, c.content, WriteOptions{}); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.path, err, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), "a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write left a file: %v", err)
	}
}

// A file at the deepest level List shows can be written and is then listed.
func TestWriteDeepestIsListed(t *testing.T) {
	l := writeLib(t)
	rel := strings.Repeat("d/", maxDepth-1) + "a.md"
	if _, err := l.Write(rel, []byte("x"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	files, _, _, err := l.List(context.Background())
	if err != nil || len(files) != 1 || files[0].Rel() != rel {
		t.Fatalf("%+v %v", files, err)
	}
}

// Links stay as they are: never replaced, never followed out of the library.
func TestWriteRefusesLinks(t *testing.T) {
	l := writeLib(t)
	if err := os.MkdirAll(l.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(l.Dir(), "link.md")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.Dir(), "out")); err != nil {
		t.Skip(err)
	}
	if _, err := l.Write("link.md", []byte("x"), WriteOptions{Replace: true}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("file link: %v", err)
	}
	if _, err := l.Write("out/new.md", []byte("x"), WriteOptions{}); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("folder link: %v", err)
	}
	if _, err := l.Write("out/secret.md", []byte("x"), WriteOptions{Replace: true}); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("folder link to a file: %v", err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "keep" {
		t.Fatalf("followed: %q", b)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("wrote outside: %v", entries)
	}
}
