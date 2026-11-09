package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	want := "github.com/acme/widget"
	for _, r := range []string{
		"git@github.com:acme/widget.git",
		"git@GitHub.com:Acme/Widget.git",
		"https://github.com/acme/widget",
		"https://github.com/acme/widget.git/",
		"https://user:tok" + "en@github.com/acme/widget.git",
		"ssh://git@github.com:22/acme/widget.git",
		"github.com:acme/widget",
		"tok:x@github.com:acme/widget.git",
	} {
		if got := NormalizeRemote(r); got != want {
			t.Errorf("%q: %q", r, got)
		}
	}
	for r, w := range map[string]string{
		"git@github.com:org/repo@v2.git": "github.com/org/repo@v2",
		"git@github.com:repo@v2.git":     "github.com/repo@v2",
		"tok:x@host.example:org/re@po":   "host.example/org/re@po",
		"user@host.example:repo":         "host.example/repo",
	} {
		if got := NormalizeRemote(r); got != w {
			t.Errorf("%q: %q, want %q", r, got, w)
		}
	}
	for _, r := range []string{"", "/srv/git/widget.git", "../widget", "file:///srv/widget.git", "./x:y", `C:\repos\widget`} {
		if got := NormalizeRemote(r); got != "" {
			t.Errorf("%q is local, got %q", r, got)
		}
	}
	if k := RemoteKey(want); len(k) != len(RemoteKeyPrefix)+32 || !IsPortableKey(k) {
		t.Errorf("key %q", k)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeRepo lays out a main checkout, with one linked worktree, as git does.
func makeRepo(t *testing.T, config string) (main, worktree string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, worktree = filepath.Join(base, "repo"), filepath.Join(base, "elsewhere", "wt")
	writeFile(t, filepath.Join(main, ".git", "config"), config)
	admin := filepath.Join(main, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(admin, "commondir"), "../..\n")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+admin+"\n")
	return main, worktree
}

const originConfig = `[core]
	bare = false
[remote "upstream"]
	url = https://example.com/other/thing.git
[remote "origin"]
	url = git@github.com:acme/widget.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[branch "main"]
	remote = origin
`

func TestProjectForWorktreesAndClonesAgree(t *testing.T) {
	main, wt := makeRepo(t, originConfig)
	a, err := ProjectFor(main)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProjectFor(filepath.Join(wt))
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != RemoteKey("github.com/acme/widget") || a.Key != b.Key || a.Root != main || b.Root != main || a.Checkout != main || b.Checkout != wt {
		t.Errorf("main %+v, worktree %+v", a, b)
	}
	// A clone elsewhere, on any device, has the same key.
	other, _ := makeRepo(t, "[remote \"origin\"]\n\turl = https://github.com/acme/widget\n")
	if c, _ := ProjectFor(other); c.Key != a.Key || c.Root == a.Root {
		t.Errorf("clone %+v", c)
	}
	// From a subdirectory too.
	sub := filepath.Join(wt, "pkg", "x")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if c, _ := ProjectFor(sub); c.Key != a.Key {
		t.Errorf("subdirectory %+v", c)
	}
}

func TestProjectForWithoutANetworkRemote(t *testing.T) {
	main, wt := makeRepo(t, "[core]\n\tbare = false\n")
	a, _ := ProjectFor(main)
	b, _ := ProjectFor(wt)
	if a.Key != ProjectKey(main) || IsPortableKey(a.Key) || a.Key != b.Key || a.Root != b.Root {
		t.Errorf("no remote: main %+v, worktree %+v", a, b)
	}
	// The first remote by name stands in for origin.
	withOther, _ := makeRepo(t, "[remote \"zeta\"]\n\turl = git@h.example:z/z.git\n[remote \"alpha\"]\n\turl = git@h.example:a/a.git\n")
	if c, _ := ProjectFor(withOther); c.Key != RemoteKey("h.example/a/a") {
		t.Errorf("no origin: %+v", c)
	}
	plain := t.TempDir()
	if c, _ := ProjectFor(plain); c.Key != ProjectKey(c.Root) {
		t.Errorf("not a repository: %+v", c)
	}
}

// A bare repository has no main checkout, so its worktree is its own root.
func TestProjectForBareWorktree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare, wt := filepath.Join(base, "bare.git"), filepath.Join(base, "wt")
	writeFile(t, filepath.Join(bare, "config"), originConfig)
	writeFile(t, filepath.Join(bare, "worktrees", "wt", "commondir"), "../..\n")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(bare, "worktrees", "wt")+"\n")
	p, err := ProjectFor(wt)
	if err != nil || p.Key != RemoteKey("github.com/acme/widget") || p.Root != wt || p.Checkout != wt {
		t.Errorf("%+v %v", p, err)
	}
}

// A submodule is its own project, with its own remote and root.
func TestProjectForSubmodule(t *testing.T) {
	main, _ := makeRepo(t, originConfig)
	sub := filepath.Join(main, "vendor", "lib")
	gitdir := filepath.Join(main, ".git", "modules", "lib")
	writeFile(t, filepath.Join(gitdir, "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/lib.git\n")
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
	p, err := ProjectFor(sub)
	if err != nil {
		t.Fatal(err)
	}
	if p.Key != RemoteKey("github.com/acme/lib") || p.Root != sub {
		t.Errorf("submodule %+v", p)
	}
}

// Notes under the earlier path keys move to the project's key, once, and notes
// from a checkout that is gone keep theirs.
func TestMigrateProjectKeys(t *testing.T) {
	ctx := context.Background()
	main, wt := makeRepo(t, originConfig)
	gone := filepath.Join(t.TempDir(), "gone")
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	save := func(key, root, title string) string {
		id, err := s.SaveNote(ctx, NewNote{ProjectKey: key, ProjectRoot: root, Title: title, Body: "b", Source: SourceCLI}, SaveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return id.ID
	}
	fromMain := save(ProjectKey(main), main, "from main")
	fromWT := save(ProjectKey(wt), wt, "from worktree")
	fromGone := save(ProjectKey(gone), gone, "from gone")
	global := save(GlobalProject, "", "global")
	// The flag was set on the first open, when the store was empty: a store
	// from an earlier version has no flag.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, metaProjectKeys); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := RemoteKey("github.com/acme/widget")
	es, err := s.Recent(ctx, Scope{Keys: []string{key}}, 10)
	if err != nil || len(es) != 2 {
		t.Fatalf("project notes %d, err %v", len(es), err)
	}
	notes, _, err := s.Get(ctx, []string{fromMain, fromWT, fromGone, global})
	if err != nil || len(notes) != 4 {
		t.Fatalf("notes %d %v", len(notes), err)
	}
	if notes[0].ProjectKey != key || notes[0].ProjectRoot != main || notes[1].ProjectKey != key || notes[1].ProjectRoot != main {
		t.Errorf("moved: %+v %+v", notes[0], notes[1])
	}
	if notes[2].ProjectKey != ProjectKey(gone) || notes[3].ProjectKey != GlobalProject {
		t.Errorf("should stay: %+v %+v", notes[2], notes[3])
	}
	// It runs once: a later change of remote does not move notes again.
	if v, _ := readMeta(ctx, s.db, metaProjectKeys); v == "" {
		t.Error("no flag")
	}
	// A read-only open of an unmigrated store migrates first.
	if _, err := s.db.ExecContext(ctx, `UPDATE records SET project_key = ?, project_root = ? WHERE id = ?`, ProjectKey(main), main, fromMain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, metaProjectKeys); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, _, _ := r.Get(ctx, []string{fromMain})
	if len(got) != 1 || got[0].ProjectKey != key {
		t.Errorf("read-only open did not migrate: %+v", got)
	}
}
