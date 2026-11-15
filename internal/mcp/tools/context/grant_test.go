package contexttools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// keychain stands in for what the app binds a grant to.
type keychain struct {
	cur []byte
	n   byte
}

func (k *keychain) Current() []byte { return k.cur }
func (k *keychain) Renew() error    { k.n++; k.cur = bytes.Repeat([]byte{k.n}, 32); return nil }
func (k *keychain) Forget() error   { k.cur = nil; return nil }

var everyone = fleetapi.TeamRef{ID: "team-all", Name: "Everyone"}

// grantDesk is a desk whose shares go to a real approval service, as the relay runs
// it: the file executor and renderer, and the grant check.
type grantDesk struct {
	*desk
	mgr   *approvals.Manager
	grant contextlib.ShareGrant
	acct  contextlib.Account
}

func newGrantDesk(t *testing.T, g *fleetGraph, user string, teams ...fleetapi.TeamRef) *grantDesk {
	t.Helper()
	// The member is also in the organization's all-members team; shares go to the first
	// team the test names unless it says otherwise.
	d := newDesk(t, g, user, append([]fleetapi.TeamRef{everyone}, teams...)...)
	d.opts.DefaultTeam = func() string { return teams[0].ID }
	acct := contextlib.Account{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: user}
	dir := t.TempDir()
	m, err := approvals.New(approvals.Options{
		Dir: filepath.Join(dir, "approvals"), StoreKey: bytes.Repeat([]byte{7}, 32),
		Identity: func(context.Context) (approvals.Identity, error) {
			return approvals.Identity{Env: acct.Env, OrgID: acct.OrgID, UserID: acct.UserID}, nil
		},
		BaseURL: func() string { return "http://localhost:1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := contextlib.NewShareGrant(filepath.Join(dir, "grant"), bytes.Repeat([]byte{9}, 32), &keychain{})
	m.Register(ShareFileOperation, ExecuteShareFile(d.m, nil, d.led))
	m.RegisterRenderer(ShareFileOperation, RenderShareFile(nil))
	m.RegisterAutoApprover(ShareFileOperation, AutoApproveShareFile(ShareGrantOptions{
		Grant: grant, Ledger: d.led, Provider: d.m, Now: d.opts.Now,
	}))
	m.Register(ShareOperation, ExecuteShare(d.m, nil))
	m.RegisterRenderer(ShareOperation, RenderShare(nil))
	m.Register(PromoteOperation, ExecutePromote(d.m, nil))
	m.RegisterRenderer(PromoteOperation, RenderPromote(nil))
	return &grantDesk{desk: d, mgr: m, grant: grant, acct: acct}
}

func (d *grantDesk) on(t *testing.T) {
	t.Helper()
	if err := d.grant.Give(d.acct, fixedNow); err != nil {
		t.Fatal(err)
	}
}

// status is what the relay's status says about the grant.
func (d *grantDesk) status() fakeStatus {
	_, on := d.grant.On(d.acct)
	return fakeStatus{st: daemon.Status{ContextShareGrant: &daemon.ContextShareGrant{
		On: on, HourlyCap: contextlib.MaxGrantedSharesPerHour, UsedThisHour: d.grant.UsedThisHour(d.acct, fixedNow),
	}}}
}

func (d *grantDesk) share(in ShareFileInput) ShareFileOutput {
	return runShareFile(context.Background(), d.desk.m, d.mgr, d.status(), d.opts, in)
}

func (d *grantDesk) pushes() int {
	d.desk.m.graph.mu.Lock()
	defer d.desk.m.graph.mu.Unlock()
	return len(d.desk.m.pushes)
}

// Off, which is the default, a share waits for the person as it always has.
func TestShareGrantOffStaysPending(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.put(t, "a.md", "one\n")
	out := d.share(ShareFileInput{Path: "personal/a.md"})
	if out.Error != nil || out.Status != string(approvals.StatusPending) || out.ApproveURL == "" || out.SharedUnderGrant || strings.Contains(out.Message, "grant") {
		t.Fatalf("%+v", out)
	}
	if d.pushes() != 0 || len(d.grant.History(d.acct)) != 0 {
		t.Fatal("something was shared with the grant off")
	}
}

// On, the share runs through the person's approval path: the same executor, the same
// ledger link, and a result and a history entry that say what happened.
func TestShareGrantSharesThroughTheApprovalPath(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	d.on(t)
	text := "# Release\n\nShip on Tuesdays.\n"
	d.put(t, "release.md", text)
	out := d.share(ShareFileInput{Path: "personal/release.md"})
	if out.Error != nil || out.Status != string(approvals.StatusDone) || !out.SharedUnderGrant || out.ApproveURL != "" {
		t.Fatalf("%+v", out)
	}
	for _, want := range []string{"without a separate approval", "context share grant", "Platform", `"release.md"`, "kenaz context grant off"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("no %q in %q", want, out.Message)
		}
	}
	if n := onlyNode(t, g); n.req.Body != text || n.req.TeamID != platform.ID {
		t.Fatalf("%+v", n.req)
	}
	// The ledger was updated as an approved share updates it.
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("release.md")
	rec, _ := d.led.Get(key)
	if l, ok := rec.LinkFor(d.acct); !ok || l.NodeID != out.ID || !l.Authored || len(rec.Reservations) != 0 {
		t.Fatalf("%+v", rec)
	}
	// The request is on record as run by the grant, not by the person.
	st, res, err := d.mgr.Status(context.Background(), out.RequestID)
	if err != nil || st != approvals.StatusDone || !strings.Contains(string(res), `"approved_by":"grant"`) {
		t.Fatalf("%s %s %v", st, res, err)
	}
	if l := d.mgr.List(context.Background()); len(l) != 1 || l[0].ApprovedBy != approvals.ApprovedByGrant {
		t.Fatalf("%+v", l)
	}
	h := d.grant.History(d.acct)
	if len(h) != 1 || h[0].Slug != "release.md" || h[0].TeamID != platform.ID || h[0].TeamName != "Platform" ||
		h[0].SHA256 != contextlib.HashOf([]byte(text)) || !h[0].At.Equal(fixedNow) || h[0].Status != "done" || h[0].RequestID != out.RequestID {
		t.Fatalf("%+v", h)
	}
	if raw, _ := json.Marshal(h); strings.Contains(string(raw), "Tuesdays") {
		t.Fatalf("the history holds the text: %s", raw)
	}
	// An update goes the same way, to the same node.
	d.put(t, "release.md", text+"And Thursdays.\n")
	if up := d.share(ShareFileInput{Path: "personal/release.md"}); up.Status != string(approvals.StatusDone) || !up.Update || up.ID != out.ID || !up.SharedUnderGrant {
		t.Fatalf("%+v", up)
	}
	if n := onlyNode(t, g); !strings.Contains(n.req.Body, "Thursdays") || len(d.grant.History(d.acct)) != 2 {
		t.Fatalf("%+v", n.req)
	}
}

// Text drafted from memory always gets the person's own look: the tool's flag and
// the ledger's mark each keep it waiting.
func TestShareGrantNeverCoversMemoryDrafts(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.on(t)
	d.put(t, "a.md", "one\n")
	d.put(t, "b.md", "two\n")
	markDrafted(t, d.desk, "b.md", "two\n")
	for _, in := range []ShareFileInput{{Path: "personal/a.md", FromMemory: true}, {Path: "personal/b.md"}} {
		out := d.share(in)
		if out.Error != nil || out.Status != string(approvals.StatusPending) || out.SharedUnderGrant || out.ApproveURL == "" {
			t.Fatalf("%+v", out)
		}
		if !strings.Contains(out.Message, "doesn't cover text marked as drafted from memory") {
			t.Errorf("%q", out.Message)
		}
	}
	// A request straight to the relay that leaves the flag out, for a file the ledger
	// marks, waits too.
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("b.md")
	rec, _ := d.led.Get(key)
	held, _ := rec.ReservationFor(d.acct)
	node, err := BuildFileNode(ShareInput{Title: "b", Body: "two\n", Kind: "doc"}, "b.md", platform.ID, held.NodeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st := d.direct(t, shareFilePayload{Node: node, Path: "personal/b.md", LedgerKey: key}); st != approvals.StatusPending {
		t.Fatal(st)
	}
	if d.pushes() != 0 || len(d.grant.History(d.acct)) != 0 {
		t.Fatal("a memory draft was shared under the grant")
	}
}

// direct prepares a payload the way a caller of the control API could, skipping the tool.
func (d *grantDesk) direct(t *testing.T, sp shareFilePayload) approvals.Status {
	t.Helper()
	raw, _ := json.Marshal(sp)
	_, st, err := d.mgr.Prepare(context.Background(), approvals.Request{
		Operation: ShareFileOperation, Env: d.acct.Env, OrgID: d.acct.OrgID, UserID: d.acct.UserID,
		Payload: raw, IdempotencyKey: "direct:" + contextlib.HashOf(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Past the hour's cap a share waits for the person, and the result says why.
func TestShareGrantHourlyCap(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.on(t)
	for i := 0; i < contextlib.MaxGrantedSharesPerHour-1; i++ {
		if err := d.grant.Record(contextlib.ShareAudit{Account: d.acct, Slug: "x.md", At: fixedNow, Status: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	d.put(t, "a.md", "one\n")
	d.put(t, "b.md", "two\n")
	if out := d.share(ShareFileInput{Path: "personal/a.md"}); out.Status != string(approvals.StatusDone) || !out.SharedUnderGrant {
		t.Fatalf("the last one inside the cap: %+v", out)
	}
	out := d.share(ShareFileInput{Path: "personal/b.md"})
	if out.Error != nil || out.Status != string(approvals.StatusPending) || out.SharedUnderGrant || out.ApproveURL == "" {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(out.Message, "limit of 30 files per clock hour") || !strings.Contains(out.Message, "waits for the user's approval") {
		t.Errorf("%q", out.Message)
	}
	if d.pushes() != 1 {
		t.Fatalf("%d pushes", d.pushes())
	}
	// Losing the history doesn't hand the hour back while the relay runs.
	if err := d.grant.Wipe(); err != nil || d.grant.UsedThisHour(d.acct, fixedNow) != 0 {
		t.Fatal(err)
	}
	d.on(t)
	d.put(t, "c.md", "three\n")
	if out := d.share(ShareFileInput{Path: "personal/c.md"}); out.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", out)
	}
}

// The credential scan refuses under the grant as it does without: in the tool, and
// again in the relay for a request that never went through the tool.
func TestShareGrantStillScansForCredentials(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.on(t)
	secret := "pass" + "word = hunter2hunter2"
	d.put(t, "a.md", "one\n"+secret+"\n")
	out := d.share(ShareFileInput{Path: "personal/a.md"})
	if out.Error == nil || out.Error.Code != CodeCredentialDetected || out.SharedUnderGrant {
		t.Fatalf("%+v", out)
	}
	if l := d.mgr.List(context.Background()); len(l) != 0 {
		t.Fatalf("prepared anyway: %+v", l)
	}
	// A clean file reserves its node; the same node with a credential in the text,
	// sent straight to the relay, is left for the person.
	d.put(t, "b.md", "two\n")
	d.grant.Revoke()
	first := d.share(ShareFileInput{Path: "personal/b.md"})
	d.on(t)
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("b.md")
	node, err := BuildFileNode(ShareInput{Title: "b", Body: "two\n" + secret + "\n", Kind: "doc"}, "b.md", platform.ID, first.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st := d.direct(t, shareFilePayload{Node: node, Path: "personal/b.md", LedgerKey: key}); st != approvals.StatusPending {
		t.Fatal(st)
	}
	// Fleet's own lint still refuses what the device let through.
	d.put(t, "c.md", "LINTBLOCK\n")
	lint := d.share(ShareFileInput{Path: "personal/c.md"})
	if lint.Status != string(approvals.StatusFailed) || !lint.SharedUnderGrant || !strings.Contains(lint.Message, "nothing was shared") {
		t.Fatalf("%+v", lint)
	}
	if len(d.desk.m.graph.nodes) != 0 {
		t.Fatal("a credential reached Fleet")
	}
}

// The organization's switch, when it says no, turns a grant already given into nothing.
func TestShareGrantForbiddenByOrg(t *testing.T) {
	old := OrgAllowsShareGrant
	OrgAllowsShareGrant = func(context.Context, fleetapi.Provider) bool { return false }
	t.Cleanup(func() { OrgAllowsShareGrant = old })
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.on(t)
	d.put(t, "a.md", "one\n")
	if out := d.share(ShareFileInput{Path: "personal/a.md"}); out.Status != string(approvals.StatusPending) || out.SharedUnderGrant || d.pushes() != 0 {
		t.Fatalf("%+v", out)
	}
}

// Today the switch allows, and asks Fleet nothing.
func TestOrgAllowsShareGrantToday(t *testing.T) {
	f := newFake("team", "member", platform)
	if !OrgAllowsShareGrant(context.Background(), f) || f.networkCalls() != 0 {
		t.Fatal("the seam must allow without a Fleet call")
	}
}

// A grant belongs to the account that gave it: another account's is no grant, and is
// removed. A file written without the keychain key is no grant either.
func TestShareGrantIsPerAccount(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	other := d.acct
	other.UserID = "u2"
	if err := d.grant.Give(other, fixedNow); err != nil {
		t.Fatal(err)
	}
	d.put(t, "a.md", "one\n")
	if out := d.share(ShareFileInput{Path: "personal/a.md"}); out.Status != string(approvals.StatusPending) || d.pushes() != 0 {
		t.Fatalf("%+v", out)
	}
	if _, on := d.grant.On(other); on {
		t.Fatal("another account's grant was kept")
	}
	otherOrg := d.acct
	otherOrg.OrgID = "o2"
	d.grant.Give(otherOrg, fixedNow)
	if _, on := d.grant.On(d.acct); on {
		t.Fatal("a grant carried to another organization")
	}
}

// The existing refusals stand: a file stays with its team, and a request that names a
// node or team the ledger doesn't hold for this file is left for the person.
func TestShareGrantKeepsTeamRefusals(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform, payments)
	d.on(t)
	d.put(t, "a.md", "one\n")
	first := d.share(ShareFileInput{Path: "personal/a.md", Team: "Platform"})
	if first.Status != string(approvals.StatusDone) {
		t.Fatalf("%+v", first)
	}
	if out := d.share(ShareFileInput{Path: "personal/a.md", Team: "Payments"}); out.Error == nil || out.Error.Code != CodeTeamMismatch || out.SharedUnderGrant {
		t.Fatalf("%+v", out)
	}
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("a.md")
	// The same node moved to another team, and a new node the tool never reserved.
	moved, _ := BuildFileNode(ShareInput{Title: "a", Body: "moved\n", Kind: "doc"}, "a.md", payments.ID, first.ID, 2)
	fresh, _ := BuildFileNode(ShareInput{Title: "a", Body: "fresh\n", Kind: "doc"}, "a.md", payments.ID, "0192e4a0-0000-7000-8000-000000000001", 1)
	for _, sp := range []shareFilePayload{
		{Node: moved, Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: contextlib.HashOf([]byte("one\n"))},
		{Node: fresh, Path: "personal/a.md", LedgerKey: key},
	} {
		if st := d.direct(t, sp); st != approvals.StatusPending {
			t.Fatalf("%s for %+v", st, sp.Node)
		}
	}
	if n := onlyNode(t, g); n.req.TeamID != platform.ID || n.req.Body != "one\n" {
		t.Fatalf("%+v", n.req)
	}
}

// A share's result says who can read the file by what Kameas Fleet marks, never by
// the team's name or its place in the list: marked as the organization's
// all-members team, everyone in the organization can; another team is marked,
// only that team; none is marked (an older Fleet), the result can't tell and says
// what holds either way.
func TestShareGrantResultSaysWhoCanReadTheFile(t *testing.T) {
	const (
		shared = "Shared without a separate approval, under the context share grant the user turned on: "
		off    = "Tell the user it was shared. They can see what it shared with `kenaz context grant status` and turn it off with `kenaz context grant off`."
	)
	// Renamed, and not the first team listed: only the mark says which it is.
	allHands := fleetapi.TeamRef{ID: "team-all", Name: "All Hands", IsDefault: true, Role: "member"}
	plat := fleetapi.TeamRef{ID: platform.ID, Name: platform.Name, Role: "member"}
	for _, c := range []struct {
		name  string
		teams []fleetapi.TeamRef
		team  fleetapi.TeamRef
		want  string
	}{
		{"the marked team", []fleetapi.TeamRef{plat, allHands}, allHands,
			shared + `every member of All Hands can now read the file "a.md". All Hands is the organization's all-members team, so everyone in the organization can now read it: say so. ` + off},
		{"another team, listed first", []fleetapi.TeamRef{plat, allHands}, plat,
			shared + `every member of Platform can now read the file "a.md". ` + off},
		{"another team, listed last", []fleetapi.TeamRef{allHands, plat}, plat,
			shared + `every member of Platform can now read the file "a.md". ` + off},
		// An older Fleet marks no team: the first one listed is treated like any other.
		{"no team marked, first listed", []fleetapi.TeamRef{{ID: "team-all", Name: "All Hands"}, platform}, fleetapi.TeamRef{ID: "team-all", Name: "All Hands"},
			shared + `every member of All Hands can now read the file "a.md". If All Hands is the organization's all-members team, everyone in the organization can now read it: say so. ` + off},
		{"no team marked, listed last", []fleetapi.TeamRef{{ID: "team-all", Name: "All Hands"}, platform}, platform,
			shared + `every member of Platform can now read the file "a.md". If Platform is the organization's all-members team, everyone in the organization can now read it: say so. ` + off},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newGrantDesk(t, newGraph(), "u1", platform)
			d.desk.m.me.Teams = c.teams
			d.on(t)
			d.put(t, "a.md", "one\n")
			out := d.share(ShareFileInput{Path: "personal/a.md", Team: c.team.ID})
			if out.Error != nil || !out.SharedUnderGrant {
				t.Fatalf("%+v", out)
			}
			if out.Message != c.want {
				t.Errorf("got  %q\nwant %q", out.Message, c.want)
			}
		})
	}
}

// Without the grant the person approves the share, and both the tool's result and
// the approval say who will be able to read the file before they do: everyone in
// the organization, for the team Kameas Fleet marks as its all-members team.
// Nothing says the file can be read while it still waits.
func TestShareApprovalSaysWhenTheWholeOrganizationCanRead(t *testing.T) {
	allHands := fleetapi.TeamRef{ID: "team-all", Name: "All Hands", IsDefault: true, Role: "member"}
	plat := fleetapi.TeamRef{ID: platform.ID, Name: platform.Name, Role: "member"}
	const everyoneWill = "Everyone in your organization will be able to read this file: "
	for _, c := range []struct {
		name   string
		teams  []fleetapi.TeamRef
		team   fleetapi.TeamRef
		impact string // the approval's, with the team as the page shows it
		result string // added to the tool's result while the share waits
	}{
		{"the marked team", []fleetapi.TeamRef{plat, allHands}, allHands,
			everyoneWill + "All Hands (team-all) is your organization's all-members team.",
			" All Hands is the organization's all-members team: once the user approves, everyone in the organization will be able to read the file, so say so."},
		{"another team", []fleetapi.TeamRef{plat, allHands}, plat,
			"Every member of Platform (" + platform.ID + ") will be able to read this file.", ""},
		{"no team marked", []fleetapi.TeamRef{{ID: "team-all", Name: "All Hands"}, platform}, fleetapi.TeamRef{ID: "team-all", Name: "All Hands"},
			"Every member of All Hands (team-all) will be able to read this file.", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newGrantDesk(t, newGraph(), "u1", platform)
			d.desk.m.me.Teams = c.teams
			asked := 0
			d.mgr.RegisterRenderer(ShareFileOperation, RenderShareFile(func() []fleetapi.TeamRef { asked++; return c.teams }))
			d.put(t, "a.md", "one\n")
			in := ShareFileInput{Path: "personal/a.md", Team: c.team.ID}
			out := d.share(in)
			if out.Error != nil || out.Status != string(approvals.StatusPending) || out.SharedUnderGrant {
				t.Fatalf("%+v", out)
			}
			if strings.Contains(out.Message, "can now read") {
				t.Errorf("a share still waiting says it can be read: %q", out.Message)
			}
			if !strings.HasPrefix(out.Message, "Nothing has been written yet.") || !strings.HasSuffix(out.Message, "`kenaz approve "+out.RequestID+"`."+c.result) {
				t.Errorf("result %q\nwant it to end with %q", out.Message, c.result)
			}
			view, err := d.mgr.ViewRequest(context.Background(), out.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			want := c.impact + " Kameas Fleet stores it under the short name a.md, never its full path on your device."
			if view.Impact != want {
				t.Errorf("approval impact\ngot  %q\nwant %q", view.Impact, want)
			}
			if d.pushes() != 0 {
				t.Error("something was shared before the person approved")
			}
			// The approval asked Kameas Fleet for the teams once, not once for
			// the name and again for the mark.
			if asked != 1 {
				t.Errorf("the renderer asked for the teams %d times", asked)
			}
		})
	}
}

// The grant covers every team the member is in, the organization's all-members team
// included, and the result names the team. A team they aren't in is never shared with.
func TestShareGrantCoversTheEveryoneTeamButNoTeamTheMemberIsNotIn(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	d.on(t)
	d.put(t, "a.md", "one\n")
	out := d.share(ShareFileInput{Path: "personal/a.md", Team: everyone.ID})
	if out.Error != nil || out.Status != string(approvals.StatusDone) || !out.SharedUnderGrant || out.Team != "Everyone" {
		t.Fatalf("%+v", out)
	}
	// This Fleet marks no team as the all-members one, and the team can be renamed, so
	// the result doesn't go by its name: it says who can read the file if it is that team.
	if !strings.Contains(out.Message, `every member of Everyone can now read the file "a.md". If Everyone is the organization's all-members team, everyone in the organization can now read it: say so. Tell the user it was shared.`) {
		t.Errorf("%q", out.Message)
	}
	if n := onlyNode(t, g); n.req.TeamID != everyone.ID {
		t.Fatalf("%+v", n.req)
	}
	if h := d.grant.History(d.acct); len(h) != 1 || h[0].TeamID != everyone.ID {
		t.Fatalf("%+v", h)
	}
	// The tool refuses a team the member isn't in before anything is prepared.
	d.put(t, "b.md", "two\n")
	if out := d.share(ShareFileInput{Path: "personal/b.md", Team: payments.ID}); out.Error == nil || out.Error.Code != CodeTeamRequired || out.SharedUnderGrant {
		t.Fatalf("%+v", out)
	}
	// Straight to the relay, with a ledger reservation that names such a team, it
	// waits for the person; approved by them, the executor still refuses it.
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("b.md")
	node, err := BuildFileNode(ShareInput{Title: "b", Body: "two\n", Kind: "doc"}, "b.md", payments.ID, "0192e4a0-0000-7000-8000-000000000009", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = "personal/b.md"
		r.Reserve(contextlib.Reservation{Account: d.acct, NodeID: node.ID, TeamID: node.TeamID})
	}); err != nil {
		t.Fatal(err)
	}
	if st := d.direct(t, shareFilePayload{Node: node, Path: "personal/b.md", LedgerKey: key}); st != approvals.StatusPending {
		t.Fatal(st)
	}
	// Decided before anything was spent: no upload, no history line, no slot of the cap.
	if len(g.nodes) != 1 || d.pushes() != 1 || len(d.grant.History(d.acct)) != 1 || d.grant.UsedThisHour(d.acct, fixedNow) != 1 {
		t.Fatal("a team the member isn't in was shared with, or used up a share")
	}
}

// The grant covers context_share_file and nothing else: a shared note and a promotion
// still wait for the person.
func TestShareGrantCoversOnlyFileShares(t *testing.T) {
	d := newGrantDesk(t, newGraph(), "u1", platform)
	d.on(t)
	o := Options{DefaultTeam: d.opts.DefaultTeam, Now: d.opts.Now, Rand: d.opts.Rand}
	if out := runShare(context.Background(), d.desk.m, d.mgr, d.status(), o, ShareArgs{Title: "T", Body: "note", Kind: "explanation"}); out.Error != nil || out.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", out)
	}
	d.desk.m.me.Role, d.desk.m.caps.Keys[CapOrgGraphSharing] = "org_admin", true
	d.desk.m.graph.nodes["n1"] = &graphNode{owner: "u1", req: fleetapi.PushRequest{ID: "n1", Title: "Runbook", Body: "Restart the queue first.", TeamID: platform.ID, Classification: fleetapi.ClassificationTeamShared}}
	if out := runPromote(context.Background(), d.desk.m, d.mgr, d.status(), PromoteInput{NodeID: "n1"}); out.Error != nil || out.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", out)
	}
	if d.pushes() != 0 || len(d.desk.m.promotes) != 0 {
		t.Fatal("a note or a promotion ran under the grant")
	}
}

// The description an agent reads says what the grant does and that it can't give it.
func TestShareFileDescriptionTellsTheTruthAboutTheGrant(t *testing.T) {
	for _, want := range []string{"kenaz context grant on", "with no approval", "30 files per clock hour", "from_memory", "can't tell memory-derived text that isn't marked", "including the organization's all-members team", "readable by the whole organization", "Only the user can turn the grant on", "no tool can"} {
		if !strings.Contains(ShareGrantDescription, want) {
			t.Errorf("no %q in %q", want, ShareGrantDescription)
		}
	}
}

// gated is a member whose Fleet can be made slow: a read of who they are that waits
// out its caller's deadline, and an upload that waits to be let through.
type gated struct {
	*member
	slowMe  bool
	holdPut chan struct{}
}

func (g *gated) Me() fleetapi.Me           { return gatedMe{g.member.Me(), g} }
func (g *gated) Context() fleetapi.Context { return gatedCtx{g.member.Context(), g} }

type gatedMe struct {
	fleetapi.Me
	g *gated
}

func (m gatedMe) Get(ctx context.Context) (fleetapi.MeInfo, error) {
	// The tool's own read has no deadline; the relay's grant check has one.
	if _, bounded := ctx.Deadline(); bounded && m.g.slowMe {
		<-ctx.Done()
		return fleetapi.MeInfo{}, ctx.Err()
	}
	return m.Me.Get(ctx)
}

type gatedCtx struct {
	fleetapi.Context
	g *gated
}

func (c gatedCtx) Push(ctx context.Context, r fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	if c.g.holdPut != nil {
		<-c.g.holdPut
	}
	return c.Context.Push(ctx, r)
}

// newGatedDesk is a grant desk whose relay talks to a Fleet that can be slowed.
func newGatedDesk(t *testing.T, g *fleetGraph) (*grantDesk, *gated) {
	t.Helper()
	d := newGrantDesk(t, g, "u1", platform)
	p := &gated{member: d.desk.m}
	d.mgr.Register(ShareFileOperation, ExecuteShareFile(p, nil, d.led))
	d.mgr.RegisterAutoApprover(ShareFileOperation, AutoApproveShareFile(ShareGrantOptions{Grant: d.grant, Ledger: d.led, Provider: p, Now: d.opts.Now}))
	return d, p
}

// The result never says one thing while another happens. When the relay can't decide
// in time, the share waits for the person and stays waiting. When it decided yes and
// the upload is slow, the result says it is being shared, with no link to approve.
func TestShareGrantResultIsNeverFalse(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the relay's real time budget")
	}
	g := newGraph()
	d, p := newGatedDesk(t, g)
	d.on(t)
	d.put(t, "a.md", "one\n")
	p.slowMe = true
	start := time.Now()
	out := d.share(ShareFileInput{Path: "personal/a.md"})
	if took := time.Since(start); took > 4500*time.Millisecond {
		t.Fatalf("the tool call took %s: past the control client's five seconds", took)
	}
	if out.Error != nil || out.Status != string(approvals.StatusPending) || out.ApproveURL == "" || out.SharedUnderGrant || !strings.Contains(out.Message, "waits for the user's approval") {
		t.Fatalf("%+v", out)
	}
	p.slowMe = false
	time.Sleep(300 * time.Millisecond)
	if st, _, _ := d.mgr.Status(context.Background(), out.RequestID); st != approvals.StatusPending || d.pushes() != 0 || len(d.grant.History(d.acct)) != 0 {
		t.Fatalf("told it waits for approval, then: %s, %d pushes", st, d.pushes())
	}

	d.put(t, "b.md", "two\n")
	p.holdPut = make(chan struct{})
	start = time.Now()
	out = d.share(ShareFileInput{Path: "personal/b.md"})
	if took := time.Since(start); took > 4500*time.Millisecond {
		t.Fatalf("the tool call took %s", took)
	}
	if out.Error != nil || out.Status != string(approvals.StatusExecuting) || out.ApproveURL != "" || !out.SharedUnderGrant {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(out.Message, "Being shared with the team Platform") || !strings.Contains(out.Message, "hasn't finished") || strings.Contains(out.Message, "waits for the user's approval") {
		t.Fatalf("%q", out.Message)
	}
	close(p.holdPut)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _, _ := d.mgr.Status(context.Background(), out.RequestID)
		// The history is settled just after the request is.
		if h := d.grant.History(d.acct); st == approvals.StatusDone && len(h) == 1 && h[0].Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("told it is being shared, then: %s", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h := d.grant.History(d.acct); len(h) != 1 || h[0].Slug != "b.md" || h[0].Status != "done" {
		t.Fatalf("%+v", h)
	}
}

// Many shares at once never get past the cap between them, and none waits behind
// another's upload to be decided. A share under the grant answers "done" when its
// upload finishes within the time prepare waits, and "executing" when it is still
// going then: both are right at that moment, so the cap is counted there, and the
// outcome once every one of them has settled.
func TestShareGrantCapHoldsUnderConcurrency(t *testing.T) {
	t.Run("uploads that finish in time", func(t *testing.T) { shareGrantCapUnderConcurrency(t, 0) })
	// Fleet answers no upload until prepare has stopped waiting (4 s): every
	// share the grant took answers "executing", and settles afterwards.
	t.Run("uploads that outlast prepare", func(t *testing.T) { shareGrantCapUnderConcurrency(t, 4500*time.Millisecond) })
}

// shareGrantCapUnderConcurrency holds Fleet's answers back for hold.
func shareGrantCapUnderConcurrency(t *testing.T, hold time.Duration) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	d.on(t)
	if hold > 0 {
		g.mu.Lock()
		time.AfterFunc(hold, g.mu.Unlock)
	}
	const n = contextlib.MaxGrantedSharesPerHour + 10
	for i := 0; i < n; i++ {
		d.put(t, fmt.Sprintf("f%02d.md", i), fmt.Sprintf("file %d\n", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	by := map[string]int{}
	var granted []string // request ids the grant took
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := d.opts
			o.Rand = strings.NewReader(strings.Repeat(fmt.Sprintf("%02d-seed-\x5a", i), 64))
			out := runShareFile(context.Background(), d.desk.m, d.mgr, d.status(), o, ShareFileInput{Path: fmt.Sprintf("personal/f%02d.md", i)})
			mu.Lock()
			by[out.Status]++
			if out.Status != string(approvals.StatusPending) {
				granted = append(granted, out.RequestID)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	// As each call returned: ten wait for the person, and the grant took the rest,
	// never more than the cap, whether or not their uploads had finished.
	running := by[string(approvals.StatusDone)] + by[string(approvals.StatusExecuting)]
	if by[string(approvals.StatusPending)] != 10 || running != contextlib.MaxGrantedSharesPerHour || len(by) > 3 {
		t.Fatalf("as the calls returned: %v", by)
	}
	if hold > 0 && by[string(approvals.StatusExecuting)] != contextlib.MaxGrantedSharesPerHour {
		t.Fatalf("with Fleet held back, as the calls returned: %v", by)
	}
	// Settled: every one the grant took is done, nothing is left executing, and
	// exactly the cap is on Fleet.
	final := map[approvals.Status]int{}
	for _, id := range granted {
		var st approvals.Status
		for end := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var err error
			if st, _, err = d.mgr.Status(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if (st != approvals.StatusExecuting && st != approvals.StatusReconciling) || time.Now().After(end) {
				break
			}
		}
		final[st]++
	}
	if final[approvals.StatusDone] != contextlib.MaxGrantedSharesPerHour || len(final) != 1 || len(g.nodes) != contextlib.MaxGrantedSharesPerHour {
		t.Fatalf("settled: %v, %d on Fleet (as the calls returned: %v)", final, len(g.nodes), by)
	}
	if n := d.grant.UsedThisHour(d.acct, fixedNow); n != contextlib.MaxGrantedSharesPerHour {
		t.Fatalf("%d in the history", n)
	}
}

// The relay requires what the ledger holds for the file, one condition at a time. A
// payload that is right in every way but one waits for the person.
func TestShareGrantRelayChecksEachLedgerField(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform, payments)
	d.put(t, "a.md", "one\n")
	// With the grant off, the tool reserves a node for a.md and leaves it pending.
	first := d.share(ShareFileInput{Path: "personal/a.md"})
	if first.Status != string(approvals.StatusPending) {
		t.Fatalf("%+v", first)
	}
	d.on(t)
	key, _ := contextlib.Personal(d.opts.PersonalDir).Key("a.md")
	node := func(body, slug, team, id string, version int) NodeInput {
		n, err := BuildFileNode(ShareInput{Title: "a", Body: body, Kind: "doc"}, slug, team, id, version)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	const otherID = "0192e4a0-0000-7000-8000-00000000000a"
	for name, sp := range map[string]shareFilePayload{
		"another node":       {Node: node("x\n", "a.md", platform.ID, otherID, 1), Path: "personal/a.md", LedgerKey: key},
		"another team":       {Node: node("x\n", "a.md", payments.ID, first.ID, 1), Path: "personal/a.md", LedgerKey: key},
		"another short name": {Node: node("x\n", "b.md", platform.ID, first.ID, 1), Path: "personal/a.md", LedgerKey: key},
		"another path":       {Node: node("x\n", "a.md", platform.ID, first.ID, 1), Path: "personal/b.md", LedgerKey: key},
		"a later version":    {Node: node("x\n", "a.md", platform.ID, first.ID, 2), Path: "personal/a.md", LedgerKey: key},
		"no ledger record":   {Node: node("x\n", "a.md", platform.ID, first.ID, 1), Path: "personal/a.md", LedgerKey: "nokey"},
	} {
		if st := d.direct(t, sp); st != approvals.StatusPending {
			t.Errorf("%s: %s", name, st)
		}
	}
	if d.pushes() != 0 || len(d.grant.History(d.acct)) != 0 {
		t.Fatalf("%d pushes", d.pushes())
	}
	// Right in every way: it runs. So each case above failed for its one difference.
	if st := d.direct(t, shareFilePayload{Node: node("one\n", "a.md", platform.ID, first.ID, 1), Path: "personal/a.md", LedgerKey: key}); st != approvals.StatusDone {
		t.Fatal(st)
	}

	// Updates: the link must be this member's own, over the text last shared, at the
	// next version.
	base := contextlib.HashOf([]byte("one\n"))
	for name, sp := range map[string]shareFilePayload{
		"another base text": {Node: node("two\n", "a.md", platform.ID, first.ID, 2), Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: contextlib.HashOf([]byte("zero\n"))},
		"a skipped version": {Node: node("two\n", "a.md", platform.ID, first.ID, 3), Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: base},
		"another node":      {Node: node("two\n", "a.md", platform.ID, otherID, 2), Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: base},
		"another team":      {Node: node("two\n", "a.md", payments.ID, first.ID, 2), Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: base},
	} {
		if st := d.direct(t, sp); st != approvals.StatusPending {
			t.Errorf("update, %s: %s", name, st)
		}
	}
	// A link to a teammate's file, which this member didn't author.
	if err := d.led.Update(key, func(r *contextlib.FileRecord) {
		l, _ := r.LinkFor(d.acct)
		l.Authored, l.OwnerUserID = false, "someone-else"
		r.SetLink(l)
	}); err != nil {
		t.Fatal(err)
	}
	mine := shareFilePayload{Node: node("two\n", "a.md", platform.ID, first.ID, 2), Path: "personal/a.md", LedgerKey: key, Update: true, BaseSHA256: base}
	if st := d.direct(t, mine); st != approvals.StatusPending {
		t.Errorf("not the author: %s", st)
	}
	if d.pushes() != 1 {
		t.Fatalf("%d pushes", d.pushes())
	}
	d.led.Update(key, func(r *contextlib.FileRecord) {
		l, _ := r.LinkFor(d.acct)
		l.Authored = true
		r.SetLink(l)
	})
	mine.Node.Body = "three\n" // a new request, the same in every checked way
	if st := d.direct(t, mine); st != approvals.StatusDone {
		t.Fatal(st)
	}
}
