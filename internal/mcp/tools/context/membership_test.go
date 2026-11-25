package contexttools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// The teams of fleetapitest.SampleTeamsWithInherited: the member belongs to
// Everyone (the all-members team) and Payments, and can only read Platform (above
// Payments) and Billing and Ledgers (below it).
const (
	mEveryone = "0b8f6f0e-0000-4000-8000-000000000001"
	mPlatform = "0b8f6f0e-0000-4000-8000-000000000002"
	mPayments = "0b8f6f0e-0000-4000-8000-000000000003"
	mBilling  = "0b8f6f0e-0000-4000-8000-000000000004"
	mLedgers  = "0b8f6f0e-0000-4000-8000-000000000005"
)

var (
	withInherited    = fleetapitest.SampleTeamsWithInherited
	defaultInherited = fleetapitest.SampleTeamsDefaultInherited
	// inheritedTargets are ways to name each team the member can only read.
	inheritedTargets = []string{mPlatform, "Platform", "platform", mBilling, "Billing", mLedgers, " Ledgers "}
)

func wantNotMember(t *testing.T, what string, e *ToolError, own ...string) {
	t.Helper()
	if e == nil || e.Code != CodeNotTeamMember {
		t.Fatalf("%s: want %s, got %+v", what, CodeNotTeamMember, e)
	}
	if !strings.Contains(e.Message, "not a member of") || !strings.Contains(e.Message, "can't share to it") || !strings.Contains(e.Message, "Nothing was shared.") {
		t.Errorf("%s: message doesn't say why: %q", what, e.Message)
	}
	// The choices are the ids of the member's own teams, in the order the message
	// names them.
	got := append([]string(nil), e.Choices...)
	sort.Strings(got)
	sort.Strings(own)
	if strings.Join(got, ",") != strings.Join(own, ",") {
		t.Errorf("%s: choices %v, want the member's own teams %v", what, e.Choices, own)
	}
	for _, id := range own {
		if !strings.Contains(e.Message, "(id "+id+")") {
			t.Errorf("%s: message doesn't name the team %s the member can share to: %q", what, id, e.Message)
		}
	}
}

// The exact words of the refusal, since agents and people act on them.
func TestNotMemberRefusalText(t *testing.T) {
	_, e := TeamsOf(withInherited()).ShareTarget("Platform", "")
	want := "You can read the context of Platform (id " + mPlatform + "), but you are not a member of that team, so you can't share to it. Nothing was shared. " +
		"You can share to: Everyone (id " + mEveryone + "); Payments (id " + mPayments + ")."
	if e == nil || e.Message != want || e.Remedy != "Pass team with the id of one of your own teams; team_members lists them." {
		t.Fatalf("%+v", e)
	}
	_, e = TeamsOf(withInherited()).ShareTarget("", mBilling)
	want = "Your default_team setting names Billing (id " + mBilling + "). You can read that team's context, but you are not a member of it, so you can't share to it. Nothing was shared. " +
		"You can share to: Everyone (id " + mEveryone + "); Payments (id " + mPayments + ")."
	if e == nil || e.Message != want || e.Remedy != "Pass team with the id of one of your own teams, or change default_team in the Kameas Fleet config." {
		t.Fatalf("%+v", e)
	}
	// A member of no team at all, who can read some.
	only := []fleetapi.TeamRef{withInherited()[1], withInherited()[3]}
	_, e = TeamsOf(only).ShareTarget(mPlatform, "")
	want = "You can read the context of Platform (id " + mPlatform + "), but you are not a member of that team, so you can't share to it. Nothing was shared. " +
		"You are not a member of any team, so there is no team you can share to."
	if e == nil || e.Code != CodeNotTeamMember || e.Message != want || len(e.Choices) != 0 {
		t.Fatalf("%+v", e)
	}
	if _, e = TeamsOf(only).ShareTarget("", ""); e == nil || e.Code != CodeTeamRequired || !strings.Contains(e.Message, "no team you can share to") {
		t.Fatalf("%+v", e)
	}
	// Many teams: the list is bounded.
	many := []fleetapi.TeamRef{{ID: "ro", Name: "Read only", Membership: fleetapi.TeamMembershipInherited}}
	for i := 0; i < 40; i++ {
		many = append(many, fleetapi.TeamRef{ID: fmt.Sprintf("t%02d", i), Name: fmt.Sprintf("Team %02d", i), Membership: fleetapi.TeamMembershipDirect})
	}
	if _, e = TeamsOf(many).ShareTarget("ro", ""); e == nil || !strings.Contains(e.Message, "and 28 more.") || len(e.Choices) != 40 || len(e.Message) > 1000 {
		t.Fatalf("%+v", e)
	}
}

// A share target is only ever a team the member belongs to: by id, by name, as the
// configured default, and as "your only team".
func TestShareTargetIsATeamTheMemberBelongsTo(t *testing.T) {
	ts := TeamsOf(withInherited())
	for _, c := range []struct{ arg, def, want string }{
		{mPayments, "", mPayments},
		{"payments", "", mPayments},
		{"Everyone", "", mEveryone},
		{mEveryone, "Payments", mEveryone},
		{"", "Payments", mPayments},
		{"", mEveryone, mEveryone},
	} {
		if got, e := ts.ShareTarget(c.arg, c.def); e != nil || got.ID != c.want {
			t.Errorf("ShareTarget(%q, %q) = %+v, %+v", c.arg, c.def, got, e)
		}
	}
	for _, name := range inheritedTargets {
		got, e := ts.ShareTarget(name, "")
		wantNotMember(t, "asked for "+name, e, mEveryone, mPayments)
		if got.ID != "" {
			t.Errorf("%s resolved to %+v", name, got)
		}
		// A good default doesn't rescue a call that names a read-only team, and a
		// default that names one is refused, never skipped for another team.
		_, e = ts.ShareTarget(name, "Payments")
		wantNotMember(t, "asked for "+name+" over a good default", e, mEveryone, mPayments)
		_, e = ts.ShareTarget("", name)
		wantNotMember(t, "default_team "+name, e, mEveryone, mPayments)
		if !strings.Contains(e.Message, "default_team setting") {
			t.Errorf("default %s: %q", name, e.Message)
		}
	}
	// Two own teams and nothing said: the choices are the member's own teams only.
	if _, e := ts.ShareTarget("", ""); e == nil || e.Code != CodeTeamRequired || strings.Join(e.Choices, ",") != "Everyone,Payments" || e.Message != "You belong to more than one team; say which one." {
		t.Fatalf("%+v", e)
	}
	if _, e := ts.ShareTarget("Nope", ""); e == nil || e.Code != CodeTeamRequired || strings.Join(e.Choices, ",") != "Everyone,Payments" {
		t.Fatalf("%+v", e)
	}
	// A stale default still falls through, to the member's own teams.
	if _, e := ts.ShareTarget("", "Gone"); e == nil || e.Code != CodeTeamRequired || len(e.Choices) != 2 {
		t.Fatalf("%+v", e)
	}

	// The default team itself inherited: the member has exactly one team, Payments,
	// however many they can read, and can't share to the all-members team.
	ts = TeamsOf(defaultInherited())
	if got, e := ts.ShareTarget("", ""); e != nil || got.ID != mPayments {
		t.Fatalf("the only-team rule with four read-only teams: %+v %+v", got, e)
	}
	if got, e := ts.ShareTarget("", "Gone"); e != nil || got.ID != mPayments {
		t.Fatalf("stale default, one own team: %+v %+v", got, e)
	}
	for _, name := range append([]string{mEveryone, "Everyone"}, inheritedTargets...) {
		_, e := ts.ShareTarget(name, "")
		wantNotMember(t, "default team inherited, asked for "+name, e, mPayments)
		_, e = ts.ShareTarget("", name)
		wantNotMember(t, "default team inherited, default_team "+name, e, mPayments)
	}
	// It is still known to be the all-members team, for wording about who reads.
	if everyone, known := ts.WholeOrg(mEveryone); !everyone || !known {
		t.Fatal("the inherited default team is still the all-members team")
	}
	if everyone, known := ts.WholeOrg(mPayments); everyone || !known {
		t.Fatal("Payments is not the all-members team")
	}
	if ts.Member(mEveryone) || !ts.Member(mPayments) || ts.Member(mPlatform) || ts.Member("") || ts.Member("nope") {
		t.Fatal("Member")
	}
	if e := ts.ShareRefusal(mEveryone); e == nil || e.Code != CodeNotTeamMember {
		t.Fatalf("%+v", e)
	}
	if e := ts.ShareRefusal("gone"); e == nil || e.Code != failScopeChanged {
		t.Fatalf("%+v", e)
	}
	if e := ts.ShareRefusal(mPayments); e != nil {
		t.Fatalf("%+v", e)
	}
	// A Teams nobody filled in belongs to nothing.
	if (Teams{}).Member(mPayments) || (Teams{}).ShareRefusal(mPayments) == nil {
		t.Fatal("an empty Teams allowed a share")
	}
}

// A read can name any team the member can read, and with nothing named "your only
// team" is still the one they belong to.
func TestReadTargetTakesInheritedTeams(t *testing.T) {
	ts := TeamsOf(withInherited())
	for want, names := range map[string][]string{
		mEveryone: {mEveryone, "everyone"}, mPayments: {mPayments, "Payments"},
		mPlatform: {mPlatform, "Platform"}, mBilling: {mBilling, "billing"}, mLedgers: {mLedgers, " Ledgers "},
	} {
		for _, name := range names {
			if got, e := ts.ReadTarget(name, ""); e != nil || got.ID != want {
				t.Errorf("ReadTarget(%q) = %+v, %+v", name, got, e)
			}
			if got, e := ts.ReadTarget("", name); e != nil || got.ID != want {
				t.Errorf("ReadTarget(default %q) = %+v, %+v", name, got, e)
			}
		}
	}
	_, e := ts.ReadTarget("", "")
	if e == nil || e.Code != CodeTeamRequired || strings.Join(e.Choices, ",") != "Billing,Everyone,Ledgers,Payments,Platform" ||
		e.Message != "You belong to more than one team, and can read context from others; say which one." {
		t.Fatalf("%+v", e)
	}
	if _, e = ts.ReadTarget("Nope", ""); e == nil || e.Message != `No team named "Nope" among your teams or the teams whose context you can read.` || len(e.Choices) != 5 {
		t.Fatalf("%+v", e)
	}
	ts = TeamsOf(defaultInherited())
	if got, e := ts.ReadTarget("", ""); e != nil || got.ID != mPayments {
		t.Fatalf("the only-team rule for a read: %+v %+v", got, e)
	}
	if got, e := ts.ReadTarget("Everyone", ""); e != nil || got.ID != mEveryone {
		t.Fatalf("%+v %+v", got, e)
	}
	// A member of no team who can read two.
	only := []fleetapi.TeamRef{withInherited()[1], withInherited()[3]}
	if _, e = TeamsOf(only).ReadTarget("", ""); e == nil || e.Message != "You can read context from more than one team; say which one." {
		t.Fatalf("%+v", e)
	}
	if got, e := TeamsOf(only[:1]).ReadTarget("", ""); e != nil || got.ID != mPlatform {
		t.Fatalf("%+v %+v", got, e)
	}
}

// asFake is the relay's context for a request the fake's account approved.
func asFake() context.Context {
	return approvals.ContextWithIdentity(context.Background(), approvals.Identity{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: "sub-1"})
}

// seeded gives a desk a new source of ids, as each call has in the app.
func seeded(d *desk, seed string) {
	d.opts.Rand = strings.NewReader(strings.Repeat(seed+"-seed-\x5a", 64))
}

// memberDesk is a desk for the member of the inherited fixture, with a client that
// lists a team's files.
func memberDesk(t *testing.T, g *fleetGraph, teams []fleetapi.TeamRef) (*desk, listing) {
	t.Helper()
	d := newDesk(t, g, "u1", teams...)
	return d, listing{d.m}
}

// ledgerTouched reports whether the ledger holds anything for a personal file.
func ledgerTouched(t *testing.T, d *desk, rel string) bool {
	t.Helper()
	key, err := contextlib.Personal(d.opts.PersonalDir).Key(rel)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := d.led.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	return len(rec.Links) > 0 || len(rec.Reservations) > 0 || rec.Path != ""
}

// writeTools is every tool of the context groups that isn't marked read-only, with
// how it stands to an inherited team. A new tool that writes must be added here,
// with a case below that shows it refuses such a team before preparing anything:
// TestEveryWriteOperationRefusesAnInheritedTeam fails until it is.
var writeTools = map[string]string{
	ToolShare:     "refuses",
	ToolShareFile: "refuses",
	// Takes a node id and no team; Fleet's own roles decide (owners and admins).
	ToolPromote: "no team target",
	// Writes working copies on this device and nothing on Fleet: a read of Fleet.
	ToolPullFiles: "reads",
}

func TestEveryWriteOperationRefusesAnInheritedTeam(t *testing.T) {
	// The tools as served, so that one added later can't be missed.
	f := newFake("enterprise", "org_admin", withInherited()...)
	d := mcp.Deps{Provider: f, Env: fleetapi.EnvDev, Approvals: f.approvals}
	srv := mcp.NewServer("test", d, NewGroup(Options{DefaultTeam: func() string { return "" }}), NewFilesGroup(FilesOptions{PersonalDir: t.TempDir(), LedgerDir: t.TempDir()}))
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, tl := range listed.Tools {
		readOnly := tl.Annotations != nil && tl.Annotations.ReadOnlyHint
		served[tl.Name] = true
		if _, known := writeTools[tl.Name]; !readOnly && !known {
			t.Errorf("%s writes and isn't in writeTools: say how it treats a team the member can only read, and test it here", tl.Name)
		}
		raw, _ := json.Marshal(tl.InputSchema)
		takesTeam := strings.Contains(string(raw), `"team"`)
		if writeTools[tl.Name] == "no team target" && takesTeam {
			t.Errorf("%s now takes a team: it must refuse an inherited one", tl.Name)
		}
		if writeTools[tl.Name] == "refuses" && !takesTeam {
			t.Errorf("%s no longer takes a team", tl.Name)
		}
	}
	for name := range writeTools {
		if !served[name] {
			t.Errorf("%s is in writeTools and isn't served", name)
		}
	}

	for _, fixture := range []struct {
		name  string
		teams []fleetapi.TeamRef
		own   []string
		ro    []string
	}{
		{"default team direct", withInherited(), []string{mEveryone, mPayments}, inheritedTargets},
		{"default team inherited", defaultInherited(), []string{mPayments}, append([]string{mEveryone, "Everyone"}, inheritedTargets...)},
	} {
		for _, target := range fixture.ro {
			what := fixture.name + ", " + strings.TrimSpace(target)

			// context_share, through MCP.
			t.Run("context_share/"+what, func(t *testing.T) {
				f := newFake("team", "member", fixture.teams...)
				cs := session(t, f, nil, "")
				var out ShareOutput
				isErr := call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc", "team": target}, &out)
				wantNotMember(t, "context_share", out.Error, fixture.own...)
				if !isErr || len(f.approvals.reqs) != 0 || f.networkCalls() != 0 || out.RequestID != "" || out.ApproveURL != "" {
					t.Fatalf("something was prepared or sent: %+v, %d requests, %d calls", out, len(f.approvals.reqs), f.networkCalls())
				}
				// As the configured default, with no team in the call.
				cs = session(t, f, nil, target)
				out = ShareOutput{}
				call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc"}, &out)
				wantNotMember(t, "context_share by default_team", out.Error, fixture.own...)
				if len(f.approvals.reqs) != 0 || f.networkCalls() != 0 {
					t.Fatal("something was prepared or sent for the default team")
				}
			})

			// context_share_file, with no grant.
			t.Run("context_share_file/"+what, func(t *testing.T) {
				d := newDesk(t, newGraph(), "u1", fixture.teams...)
				d.put(t, "a.md", "one\n")
				out := d.share(ShareFileInput{Path: "personal/a.md", Team: target})
				wantNotMember(t, "context_share_file", out.Error, fixture.own...)
				if len(d.m.approvals.reqs) != 0 || len(d.m.pushes) != 0 || out.ID != "" || out.RequestID != "" || ledgerTouched(t, d, "a.md") {
					t.Fatalf("something was prepared, reserved or sent: %+v", out)
				}
				d.opts.DefaultTeam = func() string { return target }
				out = d.share(ShareFileInput{Path: "personal/a.md"})
				wantNotMember(t, "context_share_file by default_team", out.Error, fixture.own...)
				if len(d.m.approvals.reqs) != 0 || ledgerTouched(t, d, "a.md") {
					t.Fatal("something was prepared or reserved for the default team")
				}
			})

			// context_share_file, with the grant on: nothing of the grant's is used.
			t.Run("context_share_file under the grant/"+what, func(t *testing.T) {
				if strings.EqualFold(strings.TrimSpace(target), everyone.Name) {
					t.Skip("the grant desk adds a team of its own with this name, which the member is in")
				}
				g := newGraph()
				d := newGrantDesk(t, g, "u1", fixture.teams...)
				d.opts.DefaultTeam = func() string { return "" }
				d.on(t)
				d.put(t, "a.md", "one\n")
				out := d.share(ShareFileInput{Path: "personal/a.md", Team: target})
				wantNotMember(t, "context_share_file under the grant", out.Error, append([]string{everyone.ID}, fixture.own...)...)
				if out.SharedUnderGrant || out.RequestID != "" || d.pushes() != 0 || len(g.nodes) != 0 || ledgerTouched(t, d.desk, "a.md") {
					t.Fatalf("something was prepared, reserved or sent: %+v", out)
				}
				if h := d.grant.History(d.acct); len(h) != 0 || d.grant.UsedThisHour(d.acct, fixedNow) != 0 {
					t.Fatalf("the grant wrote a history line or used a slot: %+v", h)
				}
			})
		}

		// A direct team behaves as it always has, with the inherited rows there.
		t.Run("direct team unchanged/"+fixture.name, func(t *testing.T) {
			f := newFake("team", "member", fixture.teams...)
			cs := session(t, f, nil, "")
			var out ShareOutput
			if call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc", "team": "Payments"}, &out); out.Error != nil || out.Status != "pending" || out.Team != "Payments" {
				t.Fatalf("%+v", out)
			}
			req := f.approvals.last()
			if !strings.Contains(req.Summary, `titled "T" with the team "Payments" in Kameas Fleet.`) || req.Impact != "Every member of Payments will be able to read this note." {
				t.Fatalf("%+v", req)
			}
			if _, err := ExecuteShare(f, nil)(asFake(), req.Payload, ""); err != nil || len(f.pushes) != 1 || f.pushes[0].TeamID != mPayments {
				t.Fatalf("%v %+v", err, f.pushes)
			}

			g := newGraph()
			d := newDesk(t, g, "u1", fixture.teams...)
			d.put(t, "a.md", "one\n")
			fout := d.share(ShareFileInput{Path: "personal/a.md", Team: mPayments})
			if fout.Error != nil || fout.Team != "Payments" {
				t.Fatalf("%+v", fout)
			}
			if _, _, err := d.approve(t); err != nil || onlyNode(t, g).req.TeamID != mPayments {
				t.Fatalf("%v", err)
			}
			// And under the grant it is shared at once, and counted.
			gd := newGrantDesk(t, newGraph(), "u1", fixture.teams...)
			gd.opts.DefaultTeam = func() string { return "" }
			gd.on(t)
			gd.put(t, "a.md", "one\n")
			if out := gd.share(ShareFileInput{Path: "personal/a.md", Team: "Payments"}); out.Error != nil || !out.SharedUnderGrant || out.Status != string(approvals.StatusDone) {
				t.Fatalf("%+v", out)
			}
			if gd.grant.UsedThisHour(gd.acct, fixedNow) != 1 {
				t.Fatal("a granted share wasn't counted")
			}
		})
	}
}

// A share to the all-members team still says the whole organization reads it, and
// only a member of that team gets that far.
func TestShareToTheDefaultTeamNeedsMembershipOfIt(t *testing.T) {
	d := newDesk(t, newGraph(), "u1", withInherited()...)
	d.put(t, "a.md", "one\n")
	out := d.share(ShareFileInput{Path: "personal/a.md", Team: "Everyone"})
	if out.Error != nil || !strings.Contains(d.m.approvals.last().Impact, "Everyone in your organization will be able to read this file") {
		t.Fatalf("%+v", out)
	}
	d = newDesk(t, newGraph(), "u1", defaultInherited()...)
	d.put(t, "a.md", "one\n")
	out = d.share(ShareFileInput{Path: "personal/a.md", Team: "Everyone"})
	wantNotMember(t, "the inherited default team", out.Error, mPayments)
	// A share to the member's own team says nothing of the whole organization: the
	// all-members team is known, and it is another team.
	out = d.share(ShareFileInput{Path: "personal/a.md"})
	if out.Error != nil || out.Team != "Payments" || d.m.approvals.last().Impact != "Every member of Payments will be able to read this file." {
		t.Fatalf("%+v %q", out, d.m.approvals.last().Impact)
	}
}

// Every read works for a team the member can only read.
func TestReadsWorkForAnInheritedTeam(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		teams []fleetapi.TeamRef
	}{{"default team direct", withInherited()}, {"default team inherited", defaultInherited()}} {
		for want, names := range map[string][]string{mPlatform: {mPlatform, "Platform"}, mBilling: {mBilling, "billing"}, mLedgers: {mLedgers, "Ledgers"}} {
			for _, name := range names {
				t.Run(fixture.name+"/"+name, func(t *testing.T) {
					f := newFake("team", "member", fixture.teams...)
					f.search = []fleetapi.SearchResult{{Node: fleetapi.ContextNode{ID: "n1", Title: "Runbook", Kind: "doc"}, Snippet: "s"}}
					f.members = []fleetapi.Member{{DisplayName: "Ada", Role: "lead"}}
					cs := session(t, f, nil, "")
					var so SearchOutput
					if call(t, cs, ToolSearch, map[string]any{"query": "q", "team": name}, &so); so.Error != nil || len(so.Results) != 1 || f.searches[0].TeamID != want {
						t.Fatalf("context_search: %+v %+v", so, f.searches)
					}
					var po PullOutput
					if call(t, cs, ToolPull, map[string]any{"ids": []string{"n1"}, "team": name}, &po); po.Error != nil || len(po.Nodes) != 1 || f.pulls[0].TeamID != want {
						t.Fatalf("context_pull: %+v %+v", po, f.pulls)
					}
					var mo TeamMembersOutput
					if call(t, cs, ToolTeamMembers, map[string]any{"team": name}, &mo); mo.Error != nil || len(mo.Members) != 1 {
						t.Fatalf("team_members: %+v", mo)
					}
					// As the configured default too.
					cs = session(t, f, nil, name)
					so = SearchOutput{}
					if call(t, cs, ToolSearch, map[string]any{"query": "q"}, &so); so.Error != nil || f.searches[1].TeamID != want {
						t.Fatalf("context_search by default_team: %+v", so)
					}

					// context_pull_files: a teammate of that team shared a file there.
					g := newGraph()
					author := newDesk(t, g, "u2", fleetapi.TeamRef{ID: want, Name: "Theirs", Membership: fleetapi.TeamMembershipDirect})
					author.put(t, "guide.md", "their guide\n")
					if out := author.share(ShareFileInput{Path: "personal/guide.md"}); out.Error != nil {
						t.Fatalf("%+v", out)
					}
					if _, _, err := author.approve(t); err != nil {
						t.Fatal(err)
					}
					d, p := memberDesk(t, g, fixture.teams)
					got := runPullFiles(context.Background(), p, nil, d.opts, PullFilesInput{Team: name})
					if got.Error != nil || len(got.Files) != 1 || got.Files[0].Status != PullSaved {
						t.Fatalf("context_pull_files by team: %+v", got)
					}
					ref := got.Files[0].Path
					if d.readLocal(t, ref) != "their guide\n" {
						t.Fatal("the file didn't come down")
					}
					// By id as well.
					d2, p2 := memberDesk(t, g, fixture.teams)
					if got := runPullFiles(context.Background(), p2, nil, d2.opts, PullFilesInput{IDs: []string{onlyNode(t, g).req.ID}}); got.Error != nil || len(got.Files) != 1 || got.Files[0].Status != PullSaved {
						t.Fatalf("context_pull_files by id: %+v", got)
					}
				})
			}
		}
	}
}

// A working copy pulled from a team the member can only read never goes back up:
// not as an update of a file that is theirs there, and not by naming that team for
// a new copy. As a new file of their own it goes to one of their own teams.
func TestPulledCopyFromAnInheritedTeamIsNeverPushedBack(t *testing.T) {
	g := newGraph()
	// The member wrote this file while they were in Platform.
	was := []fleetapi.TeamRef{{ID: mPlatform, Name: "Platform", Membership: fleetapi.TeamMembershipDirect}}
	then := newDesk(t, g, "u1", was...)
	then.put(t, "guide.md", "one\n")
	if out := then.share(ShareFileInput{Path: "personal/guide.md"}); out.Error != nil {
		t.Fatalf("%+v", out)
	}
	if _, _, err := then.approve(t); err != nil {
		t.Fatal(err)
	}
	node := onlyNode(t, g)

	// Now they are in Payments, and read Platform from below it.
	d, p := memberDesk(t, g, withInherited())
	d.opts.Rand = strings.NewReader(strings.Repeat("another-device\x5a", 512))
	got := runPullFiles(context.Background(), p, nil, d.opts, PullFilesInput{Team: "Platform"})
	if got.Error != nil || len(got.Files) != 1 || !got.Files[0].Yours {
		t.Fatalf("%+v", got)
	}
	ref := got.Files[0].Path
	if !strings.HasPrefix(ref, "team/platform/") {
		t.Fatalf("pulled to %s", ref)
	}
	// The folder is bound to Platform, the team it came from.
	owner, bound := d.opts.set().Team.FolderOwner("platform")
	if !bound || owner.TeamID != mPlatform {
		t.Fatalf("binding: %+v %v", owner, bound)
	}
	if _, err := d.opts.set().Write(ref, []byte("two\n"), contextlib.WriteOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	share := func(in ShareFileInput) ShareFileOutput {
		return runShareFile(context.Background(), p, d.m.approvals, nil, d.opts, in)
	}
	for _, in := range []ShareFileInput{{Path: ref}, {Path: ref, Team: "Platform"}, {Path: ref, Team: mPlatform, AsNew: true}, {Path: ref, Team: "Payments"}} {
		out := share(in)
		wantNotMember(t, fmt.Sprintf("%+v", in), out.Error, mEveryone, mPayments)
		if len(d.m.approvals.reqs) != 0 || node.req.Body != "one\n" || len(g.nodes) != 1 {
			t.Fatalf("%+v: something was prepared or sent", in)
		}
	}
	if out := share(ShareFileInput{Path: ref}); !strings.Contains(out.Error.Remedy, "as_new") {
		t.Fatalf("%+v", out.Error)
	}
	// As a new file of their own, in a team of theirs: a different node, and the
	// one in Platform is untouched.
	out := share(ShareFileInput{Path: ref, Team: "Payments", AsNew: true})
	if out.Error != nil || out.Update || out.ID == node.req.ID || out.Team != "Payments" {
		t.Fatalf("%+v", out)
	}
	req := d.m.approvals.last()
	if _, err := ExecuteShareFile(p, nil, d.led)(d.ctx(), req.Payload, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if len(g.nodes) != 2 || g.nodes[node.req.ID].req.Body != "one\n" || g.nodes[out.ID].req.TeamID != mPayments {
		t.Fatalf("%+v", g.nodes)
	}
}

// The executors check again, when the person has approved: a team the member has
// come to only read since is refused as not_team_member, and one they left as
// scope_changed, with nothing pushed either way.
func TestExecutorsRefuseATeamTheMemberOnlyReads(t *testing.T) {
	demote := func(teams []fleetapi.TeamRef, id string) []fleetapi.TeamRef {
		out := append([]fleetapi.TeamRef(nil), teams...)
		for i := range out {
			if out[i].ID == id {
				out[i].Role, out[i].Membership = "", fleetapi.TeamMembershipInherited
			}
		}
		return out
	}
	without := func(teams []fleetapi.TeamRef, id string) []fleetapi.TeamRef {
		var out []fleetapi.TeamRef
		for _, tm := range teams {
			if tm.ID != id {
				out = append(out, tm)
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		then func([]fleetapi.TeamRef, string) []fleetapi.TeamRef
		code string
	}{{"only reads it now", demote, CodeNotTeamMember}, {"left it", without, failScopeChanged}} {
		t.Run("note/"+c.name, func(t *testing.T) {
			f := newFake("team", "member", withInherited()...)
			cs := session(t, f, nil, "")
			var out ShareOutput
			if call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc", "team": "Payments"}, &out); out.Error != nil {
				t.Fatalf("%+v", out)
			}
			f.me.Teams = c.then(f.me.Teams, mPayments)
			_, err := ExecuteShare(f, nil)(asFake(), f.approvals.last().Payload, "")
			if approvals.CodeOf(err) != c.code || f.writeCalls() != 0 {
				t.Fatalf("%v, %d writes", err, f.writeCalls())
			}
		})
		t.Run("file/"+c.name, func(t *testing.T) {
			g := newGraph()
			d := newDesk(t, g, "u1", withInherited()...)
			d.put(t, "a.md", "one\n")
			if out := d.share(ShareFileInput{Path: "personal/a.md", Team: "Payments"}); out.Error != nil {
				t.Fatalf("%+v", out)
			}
			d.m.me.Teams = c.then(d.m.me.Teams, mPayments)
			req := d.m.approvals.last()
			_, err := ExecuteShareFile(d.m, nil, d.led)(d.ctx(), req.Payload, req.IdempotencyKey)
			if approvals.CodeOf(err) != c.code || len(d.m.pushes) != 0 || len(g.nodes) != 0 {
				t.Fatalf("%v, %d pushes", err, len(d.m.pushes))
			}
		})
	}
}

// The relay's grant check never takes a request for a team the member only reads,
// whatever the caller prepared: it waits for the person, and no slot or history
// line is used.
func TestShareGrantNeverTakesAnInheritedTeam(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", withInherited()...)
	d.on(t)
	d.put(t, "b.md", "two\n")
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("b.md")
	for i, team := range []string{mPlatform, mBilling, mLedgers} {
		node, err := BuildFileNode(ShareInput{Title: "b", Body: "two\n", Kind: "doc"}, "b.md", team, fmt.Sprintf("0192e4a0-0000-7000-8000-00000000000%d", i), 1)
		if err != nil {
			t.Fatal(err)
		}
		// Even with a ledger reservation that names the team, as a caller who can
		// write the ledger could leave.
		if err := d.led.Update(key, func(r *contextlib.FileRecord) {
			r.Path = "personal/b.md"
			r.Reserve(contextlib.Reservation{Account: d.acct, NodeID: node.ID, TeamID: node.TeamID})
		}); err != nil {
			t.Fatal(err)
		}
		if st := d.direct(t, shareFilePayload{Node: node, Path: "personal/b.md", LedgerKey: key}); st != approvals.StatusPending {
			t.Fatalf("%s: %s", team, st)
		}
	}
	if len(g.nodes) != 0 || d.pushes() != 0 || len(d.grant.History(d.acct)) != 0 || d.grant.UsedThisHour(d.acct, fixedNow) != 0 {
		t.Fatal("a team the member only reads was shared with, or used up a share")
	}
}

// refusing is a Fleet that refuses pushes with err while it is set.
type refusing struct {
	*member
	err func() error
}

func (r *refusing) Context() fleetapi.Context { return refusingCtx{r.member.Context(), r} }

type refusingCtx struct {
	fleetapi.Context
	r *refusing
}

func (c refusingCtx) Push(ctx context.Context, req fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	if err := c.r.err(); err != nil {
		return fleetapi.ContextNode{}, err
	}
	return c.Context.Push(ctx, req)
}

// What counts toward the grant's hourly cap: every attempt, but one Kameas Fleet
// refused because the member isn't in the team. That can still happen when a
// membership changes between the tool's check and the upload; it shares nothing
// and hands its slot back. Any other failure keeps its slot, and the cap is exact.
func TestShareGrantDoesNotCountANotTeamMemberRefusal(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	var refuse error
	p := &refusing{member: d.desk.m, err: func() error { return refuse }}
	d.mgr.Register(ShareFileOperation, ExecuteShareFile(p, nil, d.led))
	d.mgr.RegisterAutoApprover(ShareFileOperation, AutoApproveShareFile(ShareGrantOptions{Grant: d.grant, Ledger: d.led, Provider: p, Now: d.opts.Now, FailureCode: d.mgr.FailureCode}))
	d.on(t)
	settled := func(n int, status string) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			h := d.grant.History(d.acct)
			if len(h) == n && h[n-1].Status == status {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("history: %+v", h)
			}
		}
	}

	// Fleet says the member isn't in the team, five times.
	refuse = &fleetapi.APIError{Status: 403, Code: "not_team_member", Message: "You can only share to teams you belong to."}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("r%d.md", i)
		d.put(t, name, fmt.Sprintf("refused %d\n", i))
		seeded(d.desk, name)
		out := d.share(ShareFileInput{Path: "personal/" + name})
		if out.Error != nil || out.Status != string(approvals.StatusFailed) || !out.SharedUnderGrant {
			t.Fatalf("%+v", out)
		}
		want := "The context share grant ran this without a separate approval, and it was refused because the user is not a member of Platform: nothing was shared. The user can share only to teams they belong to; team_members lists them. "
		if !strings.HasPrefix(out.Message, want) || strings.Contains(out.Message, "admin") {
			t.Fatalf("%q", out.Message)
		}
		if _, res, _ := d.mgr.Status(context.Background(), out.RequestID); !strings.Contains(string(res), `"error":"not_team_member"`) {
			t.Fatalf("%s", res)
		}
		settled(i+1, "failed")
	}
	h := d.grant.History(d.acct)
	if len(h) != 5 || h[4].Reason != contextlib.ReasonNotTeamMember || h[4].Counts() {
		t.Fatalf("%+v", h)
	}
	if n := d.grant.UsedThisHour(d.acct, fixedNow); n != 0 || len(g.nodes) != 0 {
		t.Fatalf("%d slots used by refusals, %d nodes", n, len(g.nodes))
	}

	// A genuine failure is an attempt, and keeps its slot.
	refuse = &fleetapi.APIError{Status: 422, Code: "lint_blocked"}
	d.put(t, "lint.md", "blocked\n")
	seeded(d.desk, "lint")
	if out := d.share(ShareFileInput{Path: "personal/lint.md"}); out.Status != string(approvals.StatusFailed) || strings.Contains(out.Message, "doesn't count") {
		t.Fatalf("%+v", out)
	}
	settled(6, "failed")
	if n := d.grant.UsedThisHour(d.acct, fixedNow); n != 1 {
		t.Fatalf("a failed share that wasn't a membership refusal used %d slots, want 1", n)
	}

	// The rest of the hour: exactly the cap, the failed one included, by the history
	// and by the relay's own count (which the refusals gave back).
	refuse = nil
	done, pending := 0, 0
	for i := 0; i < contextlib.MaxGrantedSharesPerHour+3; i++ {
		name := fmt.Sprintf("ok%02d.md", i)
		d.put(t, name, fmt.Sprintf("fine %d\n", i))
		seeded(d.desk, name)
		switch out := d.share(ShareFileInput{Path: "personal/" + name}); out.Status {
		case string(approvals.StatusDone):
			done++
		case string(approvals.StatusPending):
			pending++
		default:
			t.Fatalf("%+v", out)
		}
	}
	if done != contextlib.MaxGrantedSharesPerHour-1 || pending != 4 || len(g.nodes) != done {
		t.Fatalf("%d done, %d pending, %d on Fleet", done, pending, len(g.nodes))
	}
	if n := d.grant.UsedThisHour(d.acct, fixedNow); n != contextlib.MaxGrantedSharesPerHour {
		t.Fatalf("%d used", n)
	}
}

// Without the relay's own count handing the slot back, a refusal would still hold
// one until the relay restarts; and losing the history must not hand back more.
func TestShareGrantRefundIsOnlyTheRefusedSlot(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	var refuse error
	p := &refusing{member: d.desk.m, err: func() error { return refuse }}
	d.mgr.Register(ShareFileOperation, ExecuteShareFile(p, nil, d.led))
	d.mgr.RegisterAutoApprover(ShareFileOperation, AutoApproveShareFile(ShareGrantOptions{Grant: d.grant, Ledger: d.led, Provider: p, Now: d.opts.Now, FailureCode: d.mgr.FailureCode}))
	d.on(t)
	for i := 0; i < contextlib.MaxGrantedSharesPerHour; i++ {
		name := fmt.Sprintf("ok%02d.md", i)
		d.put(t, name, fmt.Sprintf("fine %d\n", i))
		seeded(d.desk, name)
		if out := d.share(ShareFileInput{Path: "personal/" + name}); out.Status != string(approvals.StatusDone) {
			t.Fatalf("%+v", out)
		}
	}
	// The cap is spent: a refusal can't be taken, so there is nothing to hand back.
	refuse = &fleetapi.APIError{Status: 403, Code: "not_team_member"}
	d.put(t, "late.md", "late\n")
	seeded(d.desk, "late")
	if out := d.share(ShareFileInput{Path: "personal/late.md"}); out.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", out)
	}
	// With the history gone, the relay's count still holds the hour.
	if err := d.grant.Wipe(); err != nil {
		t.Fatal(err)
	}
	d.on(t)
	refuse = nil
	d.put(t, "more.md", "more\n")
	seeded(d.desk, "more")
	if out := d.share(ShareFileInput{Path: "personal/more.md"}); out.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", out)
	}
}

// Kameas Fleet's refusal reads as what it is, wherever it surfaces: not a matter
// of role, and nothing an admin lifts.
func TestMapErrorNotTeamMember(t *testing.T) {
	// As the official client delivers it: the route's 403 with its code.
	err := &fleetapi.APIError{Status: 403, Code: "not_team_member", Message: "You can only share to teams you belong to.", Details: map[string]string{"node_id": "n1"}}
	got := MapError(err)
	if got.Code != CodeNotTeamMember || got.Need != "not_team_member" ||
		got.Message != "Nothing was shared: Kameas Fleet refused this because you are not a member of that team." ||
		got.Remedy != "You can share only to teams you belong to; team_members lists them, and the teams whose context you can only read." {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(got.Message+got.Remedy, "role") || strings.Contains(got.Message+got.Remedy, "admin") || strings.Contains(got.Message, "n1") {
		t.Fatalf("%+v", got)
	}
	if code := approvals.CodeOf(writeErr(err)); code != CodeNotTeamMember {
		t.Fatalf("executor code %q", code)
	}
	// Any other 403 is still about the role, as before.
	if got := MapError(&fleetapi.APIError{Status: 403, Code: "permission_denied"}); got.Code != CodeRoleRequired {
		t.Fatalf("%+v", got)
	}
	// The reasons Fleet gives per item only (owned_by_another_member, team_not_found,
	// previously_rejected) don't reach the plugin through today's client, which
	// reads the response's status, conflicts[] and rejected[] and not results[]: a
	// node Fleet refuses for one of them arrives as not_permitted, or as an error
	// with no code at all. Nothing is mapped for them here.

	// A note share that Fleet refuses this way fails with the code.
	f := newFake("team", "member", platform)
	f.opErr = err
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc"}, &out)
	f.nodes = nil
	if _, xerr := ExecuteShare(f, nil)(asFake(), f.approvals.last().Payload, ""); approvals.CodeOf(xerr) != CodeNotTeamMember {
		t.Fatalf("%v", xerr)
	}
	// And a read that Fleet refuses this way says so too.
	var mo TeamMembersOutput
	call(t, cs, ToolTeamMembers, map[string]any{}, &mo)
	if mo.Error == nil || mo.Error.Code != CodeNotTeamMember {
		t.Fatalf("%+v", mo.Error)
	}
}

// team_members tells the member's own teams from those they only read, in each
// team's membership and in words; with a Fleet that says nothing of membership the
// output has neither.
func TestTeamMembersTellsOwnTeamsFromReadOnlyOnes(t *testing.T) {
	f := newFake("team", "member", withInherited()...)
	cs := session(t, f, nil, "")
	raw := rawCall(t, cs, ToolTeamMembers, map[string]any{"team": "Payments"})
	var got struct {
		Structured TeamMembersOutput `json:"structured"`
		Text       []string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	out := got.Structured
	if out.Error != nil || len(out.Teams) != 5 {
		t.Fatalf("%+v", out)
	}
	want := map[string]string{mEveryone: "direct", mPlatform: "inherited", mPayments: "direct", mBilling: "inherited", mLedgers: "inherited"}
	for _, tm := range out.Teams {
		if tm.Membership != want[tm.ID] {
			t.Errorf("%s: membership %q", tm.Name, tm.Membership)
		}
		if tm.Membership == "inherited" && tm.Role != "" {
			t.Errorf("%s: an inherited team has no role, got %q", tm.Name, tm.Role)
		}
	}
	note := "Your teams (you belong to these and can share to them): Everyone (id " + mEveryone + "); Payments (id " + mPayments + "). " +
		"Teams whose context you can read but can't share to, because you are not a member of them: Platform (id " + mPlatform + "); Billing (id " + mBilling + "); Ledgers (id " + mLedgers + ")."
	if out.Note != note {
		t.Fatalf("note:\n%s\nwant:\n%s", out.Note, note)
	}
	// The text an app shows is the same answer.
	if len(got.Text) != 1 || !strings.Contains(got.Text[0], `"membership":"inherited"`) || !strings.Contains(got.Text[0], "Teams whose context you can read but can't share to") {
		t.Fatalf("%v", got.Text)
	}

	// Every team direct, as Fleet sends it today: membership on each, and no note.
	direct := fleetapitest.SampleTeams()
	for i := range direct {
		direct[i].Membership = fleetapi.TeamMembershipDirect
	}
	cs = session(t, newFake("team", "member", direct...), nil, "")
	var all TeamMembersOutput
	call(t, cs, ToolTeamMembers, map[string]any{"team": "Payments"}, &all)
	if all.Note != "" || all.Teams[0].Membership != "direct" {
		t.Fatalf("%+v", all)
	}

	// A member of no team: the note says so.
	only := []fleetapi.TeamRef{withInherited()[1]}
	if note := teamsNote(TeamsOf(only)); !strings.HasPrefix(note, "You are not a member of any team, so there is no team you can share to. Teams whose context you can read") {
		t.Fatal(note)
	}

	// An older Fleet: no membership and no note in the output at all.
	cs = session(t, newFake("team", "member", fleetapitest.SampleTeams()...), nil, "")
	text := string(rawCall(t, cs, ToolTeamMembers, map[string]any{"team": "Payments"}))
	if strings.Contains(text, "membership") || strings.Contains(text, "note") {
		t.Fatalf("%s", text)
	}
}

// The descriptions agents read say that some teams can be read and not shared to.
func TestDescriptionsSayReadOnlyTeams(t *testing.T) {
	f := newFake("team", "member", withInherited()...)
	srv := mcp.NewServer("test", mcp.Deps{Provider: f, Env: fleetapi.EnvDev}, NewGroup(Options{}), NewFilesGroup(FilesOptions{PersonalDir: t.TempDir(), LedgerDir: t.TempDir()}))
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		ToolShare:       "You can share only with a team the user is a member of.",
		ToolShareFile:   "You can share only with a team the user is a member of.",
		ToolSearch:      "including one they are not a member of",
		ToolPull:        "including one they are not a member of",
		ToolPullFiles:   "it can't be shared back to that team",
		ToolTeamMembers: `"inherited" is a team whose context you can read without being a member of it`,
	}
	for _, tl := range listed.Tools {
		if want, ok := wants[tl.Name]; ok {
			if !strings.Contains(tl.Description, want) {
				t.Errorf("%s: %q", tl.Name, tl.Description)
			}
			delete(wants, tl.Name)
		}
	}
	if len(wants) != 0 {
		t.Fatalf("not served: %v", wants)
	}
	if !strings.Contains(ShareGrantDescription, "It never covers a team whose context the user can only read") {
		t.Fatal(ShareGrantDescription)
	}
}

// meTeamsReaders is every function that may read the Teams field of a
// fleetapi.MeInfo, by package path and function name. There is one: the place
// that splits the rows. The seam's own package may read it nowhere; a helper
// added there would be a way round the split.
var meTeamsReaders = map[string]bool{
	"github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context.TeamsOfMe": true,
}

// teamsFieldTagSets are the build-tag sets the module is type-checked under, so
// that a file compiled only into one build is read too.
var teamsFieldTagSets = []string{"", "kenaz_dev", "kenaz_opencode", "kenaz_memory_capture,kenaz_memory_project_scope,kenaz_context_library,kenaz_claude_desktop_chat"}

// meTeamsReads type-checks every package under dir (tests left out) and returns
// each use of the Teams field of fleetapi.MeInfo with the function it is in, as
// "pkgpath.Func at file:line". It goes by the field itself (the object the
// selection resolves to), so it makes no difference what the variable is called,
// whether it shadows an import, whether MeInfo is reached through an alias, or
// whether Teams is promoted from a MeInfo embedded in another struct. A
// conversion of a MeInfo to another struct type is reported too ("converts"):
// the new type's Teams is another field, and would carry the rows away unseen.
//
// What it cannot see: rows read by reflection, or by marshalling a MeInfo and
// reading the result (JSON, gob), or through unsafe. Nothing in the module does
// any of those with a MeInfo. It needs the go command, and runs wherever this
// package's tests run; the Windows CI jobs don't run this package, which loses
// nothing, since what it reads is the same source under each build-tag set.
func meTeamsReads(t *testing.T, dir, tags string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,CgoFiles,Export,Standard,Module,Error", "-tags", tags, "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list (tags %q): %v\n%s", tags, err, stderr.String())
	}
	type pkg struct {
		ImportPath, Dir, Export string
		GoFiles, CgoFiles       []string
		Standard                bool
		Module                  *struct {
			Path string
			Main bool
		}
		Error *struct{ Err string }
	}
	exports := map[string]string{}
	var own []pkg
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Error != nil {
			t.Fatalf("%s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Main {
			own = append(own, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	var reads []string
	for _, p := range own {
		if len(p.CgoFiles) > 0 {
			t.Fatalf("%s uses cgo, which this check doesn't read", p.ImportPath)
		}
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}}
		checked, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info)
		if err != nil {
			t.Fatalf("%s (tags %q): %v", p.ImportPath, tags, err)
		}
		// The field, as this package sees it: its own declaration in the seam's
		// package, the imported one anywhere else. A package that can't reach
		// fleetapi can't hold a MeInfo.
		const seam = "github.com/kameas-ai/kenaz-plugin/fleetapi"
		seamPkg := checked
		if p.ImportPath != seam {
			if exports[seam] == "" {
				continue
			}
			if seamPkg, err = imp.Import(seam); err != nil {
				t.Fatal(err)
			}
		}
		var meInfo *types.Named
		var meTeams *types.Var
		if obj, ok := seamPkg.Scope().Lookup("MeInfo").(*types.TypeName); ok {
			meInfo, _ = types.Unalias(obj.Type()).(*types.Named)
		}
		if meInfo != nil {
			if st, ok := meInfo.Underlying().(*types.Struct); ok {
				for i := 0; i < st.NumFields(); i++ {
					if st.Field(i).Name() == "Teams" {
						meTeams = st.Field(i)
					}
				}
			}
		}
		if meTeams == nil {
			t.Fatalf("%s: fleetapi.MeInfo has no Teams field", p.ImportPath)
		}
		// isMeInfo reports whether t is MeInfo or a pointer to it, aliases resolved.
		isMeInfo := func(t types.Type) bool {
			t = types.Unalias(t)
			if ptr, ok := t.(*types.Pointer); ok {
				t = types.Unalias(ptr.Elem())
			}
			return types.Identical(t, meInfo)
		}
		for _, f := range files {
			for _, decl := range f.Decls {
				// The function a read is in: methods as Type.Method, and anything
				// outside a function (a package-level initializer) as "".
				fn := ""
				if fd, ok := decl.(*ast.FuncDecl); ok {
					fn = fd.Name.Name
					if fd.Recv != nil && len(fd.Recv.List) == 1 {
						recv := fd.Recv.List[0].Type
						if star, ok := recv.(*ast.StarExpr); ok {
							recv = star.X
						}
						if id, ok := recv.(*ast.Ident); ok {
							fn = id.Name + "." + fn
						}
					}
				}
				report := func(what string, at token.Pos) {
					pos := fset.Position(at)
					rel, _ := filepath.Rel(dir, pos.Filename)
					reads = append(reads, fmt.Sprintf("%s.%s at %s:%d%s", p.ImportPath, fn, filepath.ToSlash(rel), pos.Line, what))
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.SelectorExpr:
						// The field itself, however it was reached: promoted
						// selections resolve to the same object.
						if s := info.Selections[n]; s != nil && s.Kind() == types.FieldVal && s.Obj() == meTeams {
							report("", n.Pos())
						}
					case *ast.CallExpr:
						// T(me): a MeInfo turned into another type.
						if len(n.Args) != 1 {
							return true
						}
						to, from := info.Types[n.Fun], info.Types[n.Args[0]]
						if to.IsType() && from.Type != nil && isMeInfo(from.Type) && !isMeInfo(to.Type) {
							report(" converts", n.Pos())
						}
					}
					return true
				})
			}
		}
	}
	return reads
}

// Every team that the plugin picks, checks or shows as the person's comes through
// contexttools.Teams, which is what keeps a team the member only reads out of
// every write and off every approval page. This type-checks the whole module,
// under each set of build tags, and fails on any read of the Teams field of a
// fleetapi.MeInfo outside the one function that splits the rows. It goes by the
// field, not by names: a variable that shadows an import, or a helper put in the
// seam's own package, is still a read.
func TestTeamRowsAreReadThroughTeams(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the module root: %v", err)
	}
	for _, tags := range teamsFieldTagSets {
		reads := meTeamsReads(t, root, tags)
		allowed := 0
		for _, r := range reads {
			if meTeamsReaders[r[:strings.Index(r, " at ")]] {
				allowed++
				continue
			}
			t.Errorf("tags %q: %s reads MeInfo.Teams.\n\tBuild a contexttools.Teams with TeamsOfMe (or Session.Teams) and ask it: ShareTarget, Member, ShareRefusal or OwnName for a team the person shares to or is said to be in; ReadTarget, Readable or Name for a read.", tags, r)
		}
		if allowed != 1 {
			t.Errorf("tags %q: %d reads in the allowed functions, want the one in TeamsOfMe: %v", tags, allowed, reads)
		}
	}
	// The old resolver took any row for any purpose; it must not come back.
	for _, dir := range []string{".", filepath.Join(root, "app")} {
		names, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, name := range names {
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(name, "_test.go") && regexp.MustCompile(`\bResolveTeam\(`).Match(src) {
				t.Errorf("%s: ResolveTeam is gone: use Teams.ShareTarget for a share and Teams.ReadTarget for a read", name)
			}
		}
	}
}

// The check itself: it finds a read however the variable is named, in the seam's
// own package too, through an alias, through a MeInfo embedded by value or by
// pointer, and a conversion of a MeInfo to another struct type; and takes a
// method call, another type's Teams field, a composite literal and a conversion
// to MeInfo itself for what they are.
func TestTeamRowsCheckGoesByTheField(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module github.com/kameas-ai/kenaz-plugin\n\ngo 1.26.0\n")
	write("fleetapi/me.go", `package fleetapi

type TeamRef struct{ ID string }
type MeInfo struct{ Teams []TeamRef }

// A helper in the seam's own package is a read like any other.
func Rows(me MeInfo) []TeamRef { return me.Teams }
`)
	write("app/a.go", `package app

import (
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

type other struct{ Teams []string }

func (o other) List() []string { return o.Teams }

type holder struct{ me *fleetapi.MeInfo }

func (h holder) Teams() int { return 0 }

func shadowStrings(me fleetapi.MeInfo) int {
	strings := me
	return len(strings.Teams)
}

func shadowSeam(me fleetapi.MeInfo) int {
	fleetapi := me
	return len(fleetapi.Teams)
}

func throughPointer(h holder) int { return len(h.me.Teams) + h.Teams() }

// Promoted from an embedded MeInfo, by value and by pointer.
type wrapValue struct{ fleetapi.MeInfo }
type wrapPointer struct{ *fleetapi.MeInfo }

func promotedValue(w wrapValue) int     { return len(w.Teams) }
func promotedPointer(w wrapPointer) int { return len(w.Teams) }

// Through an alias: the same type under another name.
type aliased = fleetapi.MeInfo

func throughAlias(me aliased) int { return len(me.Teams) }

// Converted to a type of the program's own with the same fields: its Teams is
// another field, so it is the conversion that is found.
type copied struct{ Teams []fleetapi.TeamRef }

func converted(me fleetapi.MeInfo) int        { return len(copied(me).Teams) }
func convertedPointer(me *fleetapi.MeInfo) int { return len((*copied)(me).Teams) }

// Not reads: a literal, a conversion to MeInfo itself or its alias, a call with
// a MeInfo argument.
func literal() fleetapi.MeInfo             { return fleetapi.MeInfo{Teams: nil} }
func same(me fleetapi.MeInfo) aliased      { return aliased(fleetapi.MeInfo(me)) }
func call(me fleetapi.MeInfo) int          { return throughAliasNot(me) }
func throughAliasNot(fleetapi.MeInfo) int  { return 0 }

var atPackageLevel = len(fleetapi.MeInfo{}.Teams)

var _ = strings.ToUpper
`)
	got := meTeamsReads(t, dir, "")
	var names []string
	for _, r := range got {
		name := r[:strings.Index(r, " at ")]
		if strings.HasSuffix(r, " converts") {
			name += " converts"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"github.com/kameas-ai/kenaz-plugin/app.",
		"github.com/kameas-ai/kenaz-plugin/app.converted converts",
		"github.com/kameas-ai/kenaz-plugin/app.convertedPointer converts",
		"github.com/kameas-ai/kenaz-plugin/app.promotedPointer",
		"github.com/kameas-ai/kenaz-plugin/app.promotedValue",
		"github.com/kameas-ai/kenaz-plugin/app.shadowSeam",
		"github.com/kameas-ai/kenaz-plugin/app.shadowStrings",
		"github.com/kameas-ai/kenaz-plugin/app.throughAlias",
		"github.com/kameas-ai/kenaz-plugin/app.throughPointer",
		"github.com/kameas-ai/kenaz-plugin/fleetapi.Rows",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("found:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
}

// A sync of a team the member only reads never plans or sends, and one with no
// Teams at all sends nothing either.
func TestSyncerNeverSendsToAnInheritedTeam(t *testing.T) {
	g := newGraph()
	d, p := memberDesk(t, g, withInherited())
	acct := contextlib.Account{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: "u1"}
	set := d.opts.set().WithViewer(&acct)
	for _, c := range []struct {
		name  string
		team  fleetapi.TeamRef
		teams Teams
		code  string
	}{
		{"inherited", withInherited()[1], TeamsOf(withInherited()), CodeNotTeamMember},
		{"no teams given", withInherited()[2], Teams{}, failScopeChanged},
		{"row passed as direct by mistake", fleetapi.TeamRef{ID: mPlatform, Name: "Platform"}, TeamsOf(withInherited()), CodeNotTeamMember},
	} {
		s := &Syncer{Store: NodeStore(p, nil, d.led), Set: set, Led: d.led, Acct: acct, Team: c.team, Teams: c.teams}
		if _, err := s.Pull(context.Background()); err != nil {
			t.Fatalf("%s: pull: %v", c.name, err)
		}
		_, err := s.Plan(context.Background())
		te, ok := err.(*ToolError)
		if !ok || te.Code != c.code {
			t.Fatalf("%s: plan: %v", c.name, err)
		}
		res, err := s.Send(context.Background(), SyncItem{Name: "x.md", put: SyncPut{TeamID: c.team.ID, Path: "x.md", Title: "x", Body: "x\n"}})
		if err != nil || res.Outcome != SyncNotMember || len(d.m.pushes) != 0 {
			t.Fatalf("%s: send: %+v %v", c.name, res, err)
		}
	}
}

// declared records whether each read of /me carried the declaration that the
// caller handles inherited team rows.
type declared struct {
	*fakeFleet
	got []bool
}

func (d *declared) Me() fleetapi.Me { return declaredMe{d.fakeFleet.Me(), d} }

type declaredMe struct {
	fleetapi.Me
	d *declared
}

func (m declaredMe) Get(ctx context.Context) (fleetapi.MeInfo, error) {
	m.d.got = append(m.d.got, fleetapi.InheritedTeamsRequested(ctx))
	return m.Me.Get(ctx)
}

// The context tools tell the provider, call by call, that they handle inherited
// rows; nothing carries that by default.
func TestContextToolsDeclareTheyHandleInheritedTeams(t *testing.T) {
	if fleetapi.InheritedTeamsRequested(context.Background()) {
		t.Fatal("on by default")
	}
	if !fleetapi.InheritedTeamsRequested(fleetapi.WithInheritedTeams(context.Background())) {
		t.Fatal("not carried")
	}
	for _, op := range []Op{OpSearchTeam, OpSearchOrg, OpPull, OpShare, OpPromote, OpTeamMembers} {
		p := &declared{fakeFleet: newFake("enterprise", "org_admin", withInherited()...)}
		if _, terr := Check(context.Background(), p, nil, op); terr != nil {
			t.Fatalf("%v: %+v", op, terr)
		}
		if len(p.got) != 1 || !p.got[0] {
			t.Errorf("op %v read /me with the declaration: %v", op, p.got)
		}
	}
	// The grant's own read, in the relay.
	d := newGrantDesk(t, newGraph(), "u1", platform)
	p := &declared{fakeFleet: d.desk.m.fakeFleet}
	auto := AutoApproveShareFile(ShareGrantOptions{Grant: d.grant, Ledger: d.led, Provider: meOf{d.desk.m, p}, Now: d.opts.Now})
	d.on(t)
	auto(context.Background(), approvals.Identity{Env: d.acct.Env, OrgID: d.acct.OrgID, UserID: d.acct.UserID}, "apr_x", []byte(`{}`))
	if len(p.got) != 1 || !p.got[0] {
		t.Fatalf("the grant read /me with the declaration: %v", p.got)
	}
}

// meOf is a member whose /me goes through a recording wrapper.
type meOf struct {
	*member
	d *declared
}

func (m meOf) Me() fleetapi.Me { return m.d.Me() }

// context_pull_files for a team the member only reads never takes a folder that
// already holds files: the person's own stay where they are, in a folder that is
// still theirs, and the team's copies go beside it.
func TestPullFilesReadOnlyTeamNeverTakesAFolderWithFiles(t *testing.T) {
	g := newGraph()
	author := newDesk(t, g, "u2", fleetapi.TeamRef{ID: mPlatform, Name: "Platform", Membership: fleetapi.TeamMembershipDirect})
	author.put(t, "guide.md", "their guide\n")
	if out := author.share(ShareFileInput{Path: "personal/guide.md"}); out.Error != nil {
		t.Fatalf("%+v", out)
	}
	if _, _, err := author.approve(t); err != nil {
		t.Fatal(err)
	}
	d, p := memberDesk(t, g, withInherited())
	acct := contextlib.Account{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: "u1"}
	d.opts.Viewer = func() *contextlib.Account { return &acct }
	if _, err := d.opts.set().Write("team/platform/draft.md", []byte("MY DRAFT\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := d.opts.set().Team.FolderOwner("platform")
	got := runPullFiles(context.Background(), p, nil, d.opts, PullFilesInput{Team: "Platform"})
	if got.Error != nil || len(got.Files) != 1 || got.Files[0].Status != PullSaved {
		t.Fatalf("%+v", got)
	}
	ref := got.Files[0].Path
	if strings.HasPrefix(ref, "team/platform/") || !strings.HasPrefix(ref, "team/platform-") {
		t.Fatalf("pulled to %s, among the person's own files", ref)
	}
	if after, _ := d.opts.set().Team.FolderOwner("platform"); after != before || !after.Unassigned {
		t.Fatalf("the person's folder was bound: %+v", after)
	}
	if d.readLocal(t, "team/platform/draft.md") != "MY DRAFT\n" || d.readLocal(t, ref) != "their guide\n" {
		t.Fatal("texts")
	}
	// A team the member belongs to takes its unassigned folder, as it always has.
	own := newDesk(t, newGraph(), "u2", fleetapi.TeamRef{ID: mPayments, Name: "Payments", Membership: fleetapi.TeamMembershipDirect})
	own.put(t, "pay.md", "pay\n")
	g2 := own.m.graph
	if out := own.share(ShareFileInput{Path: "personal/pay.md"}); out.Error != nil {
		t.Fatalf("%+v", out)
	}
	if _, _, err := own.approve(t); err != nil {
		t.Fatal(err)
	}
	d2, p2 := memberDesk(t, g2, withInherited())
	d2.opts.Viewer = func() *contextlib.Account { return &acct }
	if _, err := d2.opts.set().Write("team/payments/draft.md", []byte("MY DRAFT\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	got = runPullFiles(context.Background(), p2, nil, d2.opts, PullFilesInput{Team: "Payments"})
	if got.Error != nil || len(got.Files) != 1 || !strings.HasPrefix(got.Files[0].Path, "team/payments/") {
		t.Fatalf("%+v", got)
	}
}

// When the provider says Fleet sent teams it could not hand over, a member who
// seems to be in no team is told that, and not that they are in none; and
// team_members says how many are hidden. Counts only.
func TestHiddenTeamsAreSaid(t *testing.T) {
	const two = "Kameas Fleet sent 2 teams this version of Kenaz could not read; they are hidden. Updating Kenaz may fix this."
	hidden := fleetapi.MeInfo{TeamsDegraded: fleetapi.TeamsDegraded{Skipped: 1, Withheld: 1}}
	for name, resolve := range map[string]func(string, string) (fleetapi.TeamRef, *ToolError){"share": TeamsOfMe(hidden).ShareTarget, "read": TeamsOfMe(hidden).ReadTarget} {
		for _, arg := range []string{"", "Platform"} {
			_, e := resolve(arg, "")
			if e == nil || e.Code != CodeTeamRequired || e.Message != two+" Until then Kenaz can't tell which teams you are in." || strings.Contains(e.Message, "not a member of any team") || strings.Contains(e.Remedy, "add you to a team") {
				t.Errorf("%s %q: %+v", name, arg, e)
			}
		}
	}
	// With nothing hidden the words are the ones they were.
	if _, e := TeamsOfMe(fleetapi.MeInfo{}).ShareTarget("", ""); e == nil || e.Message != "You are not a member of any team in Kameas Fleet." {
		t.Fatalf("%+v", e)
	}
	// Only read-only teams came through, and others were hidden.
	some := fleetapi.MeInfo{Teams: []fleetapi.TeamRef{withInherited()[1]}, TeamsDegraded: fleetapi.TeamsDegraded{Withheld: 1}}
	const one = "Kameas Fleet sent 1 team this version of Kenaz could not read; it is hidden. Updating Kenaz may fix this."
	if _, e := TeamsOfMe(some).ShareTarget("", ""); e == nil || !strings.HasPrefix(e.Message, one+" None of the teams Kenaz can read is one you belong to") {
		t.Fatalf("%+v", e)
	}
	if _, e := TeamsOfMe(some).ShareTarget("Platform", ""); e == nil || e.Code != CodeNotTeamMember || !strings.Contains(e.Message, one) || strings.Contains(e.Message, "You are not a member of any team,") {
		t.Fatalf("%+v", e)
	}
	// The whole list unreadable.
	if _, e := TeamsOfMe(fleetapi.MeInfo{TeamsDegraded: fleetapi.TeamsDegraded{NotAList: true}}).ReadTarget("", ""); e == nil || !strings.HasPrefix(e.Message, "Kameas Fleet sent your teams in a form this version of Kenaz could not read; they are hidden.") {
		t.Fatalf("%+v", e)
	}

	// team_members: the teams that did come through, and the sentence.
	f := newFake("team", "member", platform)
	f.me.TeamsDegraded = fleetapi.TeamsDegraded{Skipped: 2, TreeDropped: 1}
	cs := session(t, f, nil, "")
	var out TeamMembersOutput
	call(t, cs, ToolTeamMembers, map[string]any{}, &out)
	if out.Error != nil || len(out.Teams) != 1 || out.TeamsHidden != two+" Kameas Fleet sent 1 team whose place in the team tree this version of Kenaz could not read, so Kenaz can't say which is your organization's all-members team. Updating Kenaz may fix this." {
		t.Fatalf("%+v", out)
	}
	// And through a tool, for a member with nothing listed.
	f = newFake("team", "member")
	f.me.TeamsDegraded = fleetapi.TeamsDegraded{Withheld: 2}
	cs = session(t, f, nil, "")
	var so ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "T", "body": "B", "kind": "doc"}, &so)
	if so.Error == nil || !strings.HasPrefix(so.Error.Message, two) || len(f.approvals.reqs) != 0 {
		t.Fatalf("%+v", so.Error)
	}
	out = TeamMembersOutput{}
	call(t, cs, ToolTeamMembers, map[string]any{}, &out)
	if out.TeamsHidden != two || out.Error == nil || !strings.HasPrefix(out.Error.Message, two) {
		t.Fatalf("%+v", out)
	}
	// Nothing degraded: no such field in the output at all.
	cs = session(t, newFake("team", "member", platform), nil, "")
	if text := string(rawCall(t, cs, ToolTeamMembers, map[string]any{})); strings.Contains(text, "teams_hidden") {
		t.Fatal(text)
	}
	if (fleetapi.TeamsDegraded{}).Any() || !(fleetapi.TeamsDegraded{TreeDropped: 1}).Any() || (fleetapi.TeamsDegraded{Skipped: -3}).Hidden() != 0 {
		t.Fatal("TeamsDegraded")
	}
}

// With every folder name a team could have holding files, there is no folder for
// it: the error says so with its own code, which callers go by, and names the
// folders.
func TestNoFolderLeftHasItsOwnCode(t *testing.T) {
	d := newDesk(t, newGraph(), "u1", withInherited()...)
	acct := contextlib.Account{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: "u1"}
	set := d.opts.set().WithViewer(&acct)
	owner := contextlib.FolderOwner{Env: acct.Env, OrgID: acct.OrgID, UserID: acct.UserID, TeamID: "team-up"}
	root := contextlib.TeamDir(d.opts.PersonalDir)
	for _, name := range []string{"parent", "parent-teamup", "parent-team-up"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "mine.md"), []byte("MINE\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	folder, _, err := ResolveTeamFolder(set, d.led, owner, "Parent", true)
	te, ok := err.(*ToolError)
	if !ok || te.Code != CodeNoFolder || CodeNoFolder != "no_folder" || folder != "" {
		t.Fatalf("%q %v", folder, err)
	}
	for _, name := range []string{"parent", "parent-teamup", "parent-team-up"} {
		if !strings.Contains(te.Message, name) {
			t.Errorf("the message doesn't name %s: %q", name, te.Message)
		}
	}
	// The same through a sync of that team.
	s := &Syncer{Store: NodeStore(listing{d.m}, nil, d.led), Set: set, Led: d.led, Acct: acct,
		Team: fleetapi.TeamRef{ID: "team-up", Name: "Parent", Membership: fleetapi.TeamMembershipInherited}, Teams: TeamsOf(nil)}
	if _, err := s.Pull(d.ctx()); err == nil || err.(*ToolError).Code != CodeNoFolder {
		t.Fatalf("%v", err)
	}
}
