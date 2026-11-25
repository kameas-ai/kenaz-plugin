package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
)

// Teams as Kameas Fleet lists them once reading across the team tree is on: the
// member belongs to Platform, and can read Parent (above it) and Child (below it).
func syncTeamsWithInherited() []fleetapi.TeamRef {
	return []fleetapi.TeamRef{
		{ID: "team-1", Name: "Platform", Role: fleetapi.TeamRoleMember, ParentTeamID: "team-up", Depth: 2, Membership: fleetapi.TeamMembershipDirect},
		{ID: "team-up", Name: "Parent", Depth: 1, Membership: fleetapi.TeamMembershipInherited},
		{ID: "team-down", Name: "Child", ParentTeamID: "team-1", Depth: 3, Membership: fleetapi.TeamMembershipInherited},
	}
}

const syncNotMemberOfParent = "You can read the context of Parent (id team-up), but you are not a member of that team, so you can't share to it. Nothing was shared. You can share to: Platform (id team-1)."

// A sync of a team the member only reads brings its files down and sends nothing:
// not a new file put in its folder, and not a change to a copy that came from it.
func TestContextSyncInheritedTeamPullsAndNeverSends(t *testing.T) {
	f := newSyncFleet()
	f.teams = syncTeamsWithInherited()
	f.put("n-up", "team-up", "guide.md", "the parent team's guide\n", "bob", 1)
	f.put("n-mine", "team-up", "mine.md", "written when I was in Parent\n", "alice", 1)
	d := newSyncDesk(t, f)
	parent := filepath.Join(contextlib.TeamDir(d.personal), "parent")
	for _, name := range []string{"Parent", "team-up", "parent"} {
		if code := d.sync(true, "y\ny\n", "--team", name); code != ExitOK {
			t.Fatalf("--team %s: exit %d:\n%s\n%s", name, code, d.out, d.errb)
		}
		d.wantOut(syncNotMemberOfParent)
		if strings.Contains(d.out.String(), "Share th") || strings.Contains(d.out.String(), "New:") || strings.Contains(d.out.String(), "Changed:") || len(f.pushes) != 0 {
			t.Fatalf("--team %s offered to share, or shared:\n%s\n%+v", name, d.out, f.pushes)
		}
		b, err := os.ReadFile(filepath.Join(parent, "guide.md"))
		if err != nil || string(b) != "the parent team's guide\n" {
			t.Fatalf("--team %s: the file didn't come down: %v %q", name, err, b)
		}
		// Next time round there is a new file in that folder, and a changed copy of
		// a file the member wrote there once: neither goes up.
		if err := os.WriteFile(filepath.Join(parent, "new.md"), []byte("mine, for them\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "mine.md"), []byte("changed here\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if f.nodes["n-mine"].Body != "written when I was in Parent\n" || len(f.nodes) != 2 {
		t.Fatalf("Fleet changed: %+v", f.nodes)
	}
	// The folder is bound to the team it came from, and stays that team's.
	owner, bound := contextlib.Team(contextlib.TeamDir(d.personal)).FolderOwner("parent")
	if !bound || owner.TeamID != "team-up" {
		t.Fatalf("%+v %v", owner, bound)
	}
	// The member's own team still syncs both ways, with the inherited rows there.
	d.file("a.md", "a\n")
	d.ok("\n")
	d.wantOut("Shared 1 file with Platform.")
	if len(f.pushes) != 1 || f.pushes[0].TeamID != "team-1" {
		t.Fatalf("%+v", f.pushes)
	}
}

// "Your only team" is the one team the member belongs to, however many they can
// read: no menu, and it is the team shared to.
func TestContextSyncOnlyTeamWithInheritedTeams(t *testing.T) {
	f := newSyncFleet()
	f.teams = syncTeamsWithInherited()
	d := newSyncDesk(t, f)
	d.file("a.md", "a\n")
	d.ok("\n")
	if strings.Contains(d.out.String(), usertext.SyncPickTeam) || len(f.pushes) != 1 || f.pushes[0].TeamID != "team-1" {
		t.Fatalf("%s\n%+v", d.out, f.pushes)
	}
}

// The menu offers the teams the member belongs to, and says the others can be
// brought down with --team. A number past the end picks nothing.
func TestContextSyncMenuListsOwnTeamsOnly(t *testing.T) {
	f := newSyncFleet()
	f.teams = append(syncTeamsWithInherited(), fleetapi.TeamRef{ID: "team-2", Name: "Sales", Role: fleetapi.TeamRoleMember, Depth: 1, Membership: fleetapi.TeamMembershipDirect})
	d := newSyncDesk(t, f)
	d.file("a.md", "a\n")
	d.ok("2\n\n")
	d.wantOut("1. Platform", "2. Sales", usertext.SyncReadOnlyTeamsHint, usertext.SyncPickTeam)
	if strings.Contains(d.out.String(), ". Parent") || strings.Contains(d.out.String(), ". Child") {
		t.Fatalf("a read-only team is on the menu:\n%s", d.out)
	}
	if len(f.pushes) != 0 {
		// Sales has no files here: a.md is in Platform's folder.
		t.Fatalf("%+v", f.pushes)
	}
	for _, pick := range []string{"3\n", "4\n"} {
		if code := d.sync(true, pick); code != ExitError || !strings.Contains(d.errb.String(), usertext.SyncNoSuchTeam) || len(f.pushes) != 0 {
			t.Fatalf("picked %q: %d %s", pick, code, d.errb)
		}
	}
	// Typed by name, a read-only team is brought down and nothing is sent.
	if code := d.sync(true, "Parent\n"); code != ExitOK || len(f.pushes) != 0 {
		t.Fatalf("%d %s", code, d.errb)
	}
	d.wantOut("You can read the context of Parent (id team-up), but you are not a member of that team")
	// A Fleet that says nothing of membership: the menu is as it was, with no hint.
	old := newSyncFleet()
	old.teams = []fleetapi.TeamRef{{ID: "team-1", Name: "Platform"}, {ID: "team-2", Name: "Sales"}}
	od := newSyncDesk(t, old)
	od.ok("1\n")
	if strings.Contains(od.out.String(), usertext.SyncReadOnlyTeamsHint) {
		t.Fatalf("%s", od.out)
	}
}

// demoting is a Fleet where the member stops belonging to a team, and only reads
// it, after the first read of who they are.
type demoting struct {
	*syncFleet
	gets int
}

func (p *demoting) Me() fleetapi.Me { return demotingMe{p.syncFleet.Me(), p} }

type demotingMe struct {
	fleetapi.Me
	p *demoting
}

func (m demotingMe) Get(ctx context.Context) (fleetapi.MeInfo, error) {
	me, err := m.Me.Get(ctx)
	m.p.gets++
	if m.p.gets > 1 {
		teams := append([]fleetapi.TeamRef(nil), me.Teams...)
		for i := range teams {
			teams[i].Role, teams[i].Membership = "", fleetapi.TeamMembershipInherited
		}
		me.Teams = teams
	}
	return me, err
}

// Membership can change between the plan and the send: the write checks again, and
// the person is told why the file stayed.
func TestContextSyncMembershipLostBeforeSend(t *testing.T) {
	f := newSyncFleet()
	f.teams = syncTeamsWithInherited()
	d := newSyncDesk(t, f)
	d.env.Provider = &demoting{syncFleet: f}
	d.file("a.md", "a\n")
	if code := d.sync(true, "y\n"); code != ExitError {
		t.Fatalf("exit %d:\n%s\n%s", code, d.out, d.errb)
	}
	d.wantOut("a.md: "+usertext.SyncNotMemberLine, "Shared 0 of 1")
	if len(f.pushes) != 0 {
		t.Fatalf("%+v", f.pushes)
	}
}

// The grant's history says why a share Fleet refused for membership wasn't shared,
// and that it wasn't counted.
func TestGrantResultTextForNotTeamMember(t *testing.T) {
	got := grantResultText(contextlib.ShareAudit{Status: "failed", Reason: contextlib.ReasonNotTeamMember})
	if got != "not shared: you are not a member of this team (not counted toward the hourly limit)" {
		t.Fatal(got)
	}
	if got := grantResultText(contextlib.ShareAudit{Status: "failed", Reason: contextlib.ReasonNotTeamMember, Counted: true}); got != "not shared: you are not a member of this team" {
		t.Fatal(got)
	}
	if got := grantResultText(contextlib.ShareAudit{Status: "failed", Reason: "node_changed"}); got != "not shared (failed)" {
		t.Fatal(got)
	}
	if got := grantResultText(contextlib.ShareAudit{Status: "done"}); got != "shared" {
		t.Fatal(got)
	}
}
