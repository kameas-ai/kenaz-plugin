package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
)

// readOnlyPullDesk is a desk for a member of Platform who only reads Parent, with
// a file on Fleet in Parent.
func readOnlyPullDesk(t *testing.T) (*syncDesk, *contextlib.Library, string) {
	t.Helper()
	f := newSyncFleet()
	f.teams = syncTeamsWithInherited()
	f.put("n-up", "team-up", "guide.md", "the parent team's guide\n", "bob", 1)
	d := newSyncDesk(t, f)
	return d, contextlib.Team(contextlib.TeamDir(d.personal)), contextlib.TeamDir(d.personal)
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A folder that already holds the person's own files, under the name of a team
// they only read, is never taken for that team by a pull: their drafts stay in a
// folder that is still theirs, the team's files come down beside it, and the
// person is told which folder is which.
func TestContextSyncReadOnlyTeamNeverTakesAFolderWithFiles(t *testing.T) {
	alice := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	for name, draft := range map[string]func(t *testing.T, d *syncDesk, root string){
		// Made by context_write: bound to the account, with no team yet.
		"an unassigned folder of this account": func(t *testing.T, d *syncDesk, root string) {
			if _, err := contextlib.Open(d.personal, "").WithViewer(alice).Write("team/parent/draft.md", []byte("MY DRAFT\n"), contextlib.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		// Made by hand, or by an older release: no record at all.
		"a folder with no record": func(t *testing.T, d *syncDesk, root string) {
			if err := os.MkdirAll(filepath.Join(root, "parent"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "parent", "draft.md"), []byte("MY DRAFT\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, lib, root := readOnlyPullDesk(t)
			draft(t, d, root)
			before, wasBound := lib.FolderOwner("parent")
			if code := d.sync(true, "", "--team", "Parent"); code != ExitOK {
				t.Fatalf("exit %d:\n%s\n%s", code, d.out, d.errb)
			}
			// The person's folder is as it was: same owner record, same draft, and
			// nothing of the team's in it.
			after, bound := lib.FolderOwner("parent")
			if bound != wasBound || after != before || after.TeamID == "team-up" {
				t.Fatalf("the folder with the person's files was bound to the read-only team: %+v (was %+v)", after, before)
			}
			if mustRead(t, filepath.Join(root, "parent", "draft.md")) != "MY DRAFT\n" {
				t.Fatal("the draft changed")
			}
			if _, err := os.Stat(filepath.Join(root, "parent", "guide.md")); err == nil {
				t.Fatal("the team's file was put among the person's own")
			}
			// The team's files are in a folder of their own, bound to that team.
			const pulled = "parent-teamup"
			owner, ok := lib.FolderOwner(pulled)
			if !ok || owner.TeamID != "team-up" || mustRead(t, filepath.Join(root, pulled, "guide.md")) != "the parent team's guide\n" {
				t.Fatalf("%+v %v", owner, ok)
			}
			d.wantOut(syncNotMemberOfParent, usertext.SyncReadOnlyFolderBeside("parent", pulled, "Parent"))
			if strings.Contains(d.out.String(), "move it into") || len(d.f.pushes) != 0 {
				t.Fatalf("%s", d.out)
			}
			// Again: the same folders, nothing more said about a new one.
			if code := d.sync(true, "", "--team", "Parent"); code != ExitOK {
				t.Fatalf("exit %d", code)
			}
			if after2, _ := lib.FolderOwner("parent"); after2 != before {
				t.Fatalf("second run: %+v", after2)
			}
		})
	}
}

// The folder's notices and the exit code are the same for a team the person only
// reads: a record that can't be read is said, and is an error.
func TestContextSyncReadOnlyTeamKeepsFolderNotices(t *testing.T) {
	d, _, root := readOnlyPullDesk(t)
	if code := d.sync(true, "", "--team", "Parent"); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, d.out)
	}
	// An empty folder was free to take, so there is nothing to say about folders.
	if strings.Contains(d.out.String(), "folder") {
		t.Fatalf("%s", d.out)
	}
	if err := os.WriteFile(filepath.Join(root, "parent", contextlib.FolderRecord), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := d.sync(true, "", "--team", "Parent"); code != ExitError {
		t.Fatalf("exit %d with an unreadable folder record:\n%s", code, d.out)
	}
	d.wantOut(usertext.SyncFolderUnreadable("parent"), syncNotMemberOfParent)
	if strings.Contains(d.out.String(), "didn't come from") {
		t.Fatalf("a folder whose owner can't be read was said to hold other files:\n%s", d.out)
	}
}

// Every folder name the team could have is taken. Sync says so: it has nothing to
// do with reaching Kameas Fleet.
func TestContextSyncNoFolderLeft(t *testing.T) {
	d, _, root := readOnlyPullDesk(t)
	for _, name := range []string{"parent", "parent-teamup", "parent-team-up"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "mine.md"), []byte("MINE\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := d.sync(true, "", "--team", "Parent"); code != ExitError {
		t.Fatalf("exit %d:\n%s\n%s", code, d.out, d.errb)
	}
	want := "Kenaz has no folder to keep Parent's files in: parent, parent-teamup, parent-team-up are all in use, by another account or team or by files that aren't Parent's. Nothing was brought down. Rename or move one of those folders and run this again."
	if !strings.Contains(d.errb.String(), want) || strings.Contains(d.errb.String()+d.out.String(), "couldn't be reached") {
		t.Fatalf("%s\n%s", d.out, d.errb)
	}
	for _, name := range []string{"parent", "parent-teamup", "parent-team-up"} {
		if mustRead(t, filepath.Join(root, name, "mine.md")) != "MINE\n" {
			t.Fatal("a file changed")
		}
		if _, err := os.Stat(filepath.Join(root, name, "guide.md")); err == nil {
			t.Fatal("something was brought down")
		}
	}
}

// What does move out of a passed-over folder: a copy this account pulled from that
// same team, which goes with the team's other copies. The person's own file stays.
func TestContextSyncReadOnlyTeamMovesItsOwnCopiesOnly(t *testing.T) {
	d, lib, root := readOnlyPullDesk(t)
	// First pull: the folder is free, so the team's copy lands in parent/.
	if code := d.sync(true, "", "--team", "Parent"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if mustRead(t, filepath.Join(root, "parent", "guide.md")) == "" {
		t.Fatal("no copy")
	}
	// The folder loses its record (as an older release left folders), and the person
	// puts a file of their own in it.
	if err := os.Remove(filepath.Join(root, "parent", contextlib.FolderRecord)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "parent", "draft.md"), []byte("MY DRAFT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := d.sync(true, "", "--team", "Parent"); code != ExitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, d.out, d.errb)
	}
	if _, bound := lib.FolderOwner("parent"); bound {
		t.Fatal("the folder with the person's file was bound")
	}
	if mustRead(t, filepath.Join(root, "parent", "draft.md")) != "MY DRAFT\n" {
		t.Fatal("the draft changed")
	}
	if _, err := os.Stat(filepath.Join(root, "parent", "guide.md")); err == nil {
		t.Fatal("the team's copy stayed among the person's files")
	}
	if mustRead(t, filepath.Join(root, "parent-teamup", "guide.md")) != "the parent team's guide\n" {
		t.Fatal("the team's copy didn't move with the team's folder")
	}
	d.wantOut(usertext.SyncReadOnlyFolderBeside("parent", "parent-teamup", "Parent"))
}
