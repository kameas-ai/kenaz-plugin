package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
)

// syncFleet is a small Kameas Fleet: nodes with authors, and the refusals today's routes give.
type syncFleet struct {
	fleetapi.Provider
	org     string
	orgName string
	me      string
	teams   []fleetapi.TeamRef
	nodes   map[string]*fleetapi.NodeContent
	order   []string
	pushes  []fleetapi.PushRequest
	// failPush fails pushes of a short name this many times.
	failPush map[string]int
	// beforeRead runs once, before the next read of one node.
	beforeRead func()
}

func newSyncFleet() *syncFleet {
	return &syncFleet{Provider: stub.New(), org: "org-1", me: "alice", teams: []fleetapi.TeamRef{{ID: "team-1", Name: "Platform"}}, nodes: map[string]*fleetapi.NodeContent{}, failPush: map[string]int{}}
}

func (f *syncFleet) Flavor() fleetapi.Flavor   { return fleetapi.FlavorOfficial }
func (f *syncFleet) Me() fleetapi.Me           { return syncMe{f} }
func (f *syncFleet) Auth() fleetapi.Auth       { return syncAuth{f.Provider.Auth(), f} }
func (f *syncFleet) Context() fleetapi.Context { return syncCtx{f.Provider.Context(), f} }

// put is a file a teammate shared.
func (f *syncFleet) put(id, team, slug, body, owner string, version int) {
	if _, ok := f.nodes[id]; !ok {
		f.order = append(f.order, id)
	}
	f.nodes[id] = &fleetapi.NodeContent{Body: body, Node: fleetapi.ContextNode{ID: id, Title: slug, TeamID: team, Version: version, OwnerUserID: owner,
		Classification: fleetapi.ClassificationTeamShared, Metadata: map[string]any{contexttools.MetadataSlug: slug}}}
}

func (f *syncFleet) bySlug(slug string) *fleetapi.NodeContent {
	for _, n := range f.nodes {
		if n.Node.Metadata[contexttools.MetadataSlug] == slug {
			return n
		}
	}
	return nil
}

type syncMe struct{ f *syncFleet }

func (m syncMe) Get(context.Context) (fleetapi.MeInfo, error) {
	return fleetapi.MeInfo{UserID: m.f.me, OrgID: m.f.org, OrgName: m.f.orgName, Teams: m.f.teams}, nil
}
func (m syncMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{contexttools.CapTeamGraphSharing: true, fleetapi.CapContextFiles: true}}, nil
}

type syncAuth struct {
	fleetapi.Auth
	f *syncFleet
}

func (a syncAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: a.f.org, UserID: a.f.me}, nil
}

type syncCtx struct {
	fleetapi.Context
	f *syncFleet
}

func (c syncCtx) List(_ context.Context, r fleetapi.ListRequest) (fleetapi.ListPage, error) {
	var out fleetapi.ListPage
	if r.Since != "" {
		return out, nil
	}
	for _, id := range c.f.order {
		if n := c.f.nodes[id]; r.TeamID == "" || n.Node.TeamID == r.TeamID {
			out.Nodes = append(out.Nodes, *n)
		}
	}
	if len(out.Nodes) > 0 {
		out.Cursor = "end"
	}
	return out, nil
}

func (c syncCtx) Pull(_ context.Context, r fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	if c.f.beforeRead != nil {
		hook := c.f.beforeRead
		c.f.beforeRead = nil
		hook()
	}
	var out []fleetapi.NodeContent
	for _, id := range r.IDs {
		if n, ok := c.f.nodes[id]; ok {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (c syncCtx) Push(_ context.Context, r fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	slug, _ := r.Metadata[contexttools.MetadataSlug].(string)
	if c.f.failPush[slug] > 0 {
		c.f.failPush[slug]--
		return fleetapi.ContextNode{}, errors.New("connection lost")
	}
	if n, ok := c.f.nodes[r.ID]; ok && n.Node.OwnerUserID != c.f.me {
		return fleetapi.ContextNode{}, &fleetapi.APIError{Status: 403, Code: "not_permitted"}
	}
	c.f.pushes = append(c.f.pushes, r)
	c.f.put(r.ID, r.TeamID, slug, r.Body, c.f.me, r.Version)
	return c.f.nodes[r.ID].Node, nil
}

type syncDesk struct {
	t         *testing.T
	f         *syncFleet
	env       Env
	out, errb *bytes.Buffer
	teamDir   string
	personal  string
}

func newSyncDesk(t *testing.T, f *syncFleet) *syncDesk {
	t.Helper()
	old := syncRetryWaits
	syncRetryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { syncRetryWaits = old })
	d := &syncDesk{t: t, f: f, out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	home := t.TempDir()
	t.Setenv("KENAZ_HOME", home)
	d.env = devEnv(home, f, d.out, d.errb)
	dir, err := d.env.contextPersonalDir()
	if err != nil {
		t.Fatal(err)
	}
	d.personal = dir
	d.teamDir = filepath.Join(contextlib.TeamDir(dir), "platform")
	// The folder as a first sync leaves it, so that a test can add files before running.
	if err := contextlib.Team(contextlib.TeamDir(dir)).BindFolder("platform", contextlib.FolderOwner{Env: "dev", OrgID: "org-1", UserID: f.me, TeamID: "team-1"}); err != nil {
		t.Fatal(err)
	}
	return d
}

// otherOrg is a second account on this device, in another org, with a team of the same name.
func otherOrg() *syncFleet {
	b := newSyncFleet()
	b.org, b.me, b.teams = "org-2", "bob", []fleetapi.TeamRef{{ID: "team-b", Name: "Platform"}}
	return b
}

func (d *syncDesk) sync(terminal bool, input string, args ...string) int {
	d.t.Helper()
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = strings.NewReader(input)
	defer func() { memoryStdinTerminal, memoryStdin = oldT, oldR }()
	d.out.Reset()
	d.errb.Reset()
	return runContext(d.env, append([]string{"sync"}, args...))
}

func (d *syncDesk) file(name, text string) {
	d.t.Helper()
	p := filepath.Join(d.teamDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		d.t.Fatal(err)
	}
}

func (d *syncDesk) read(name string) string {
	d.t.Helper()
	b, err := os.ReadFile(filepath.Join(d.teamDir, name))
	if err != nil {
		d.t.Fatal(err)
	}
	return string(b)
}

func (d *syncDesk) wantOut(subs ...string) {
	d.t.Helper()
	for _, s := range subs {
		if !strings.Contains(d.out.String(), s) {
			d.t.Errorf("no %q in:\n%s\n%s", s, d.out, d.errb)
		}
	}
}

func (d *syncDesk) ok(input string) {
	d.t.Helper()
	if code := d.sync(true, input); code != ExitOK {
		d.t.Fatalf("exit %d:\n%s\n%s", code, d.out, d.errb)
	}
}

// Both ways: a teammate's file comes down, yours goes up, and a second run does nothing.
func TestContextSyncBothWays(t *testing.T) {
	f := newSyncFleet()
	f.put("n-bob", "team-1", "from-bob.md", "bob's text\n", "bob", 1)
	d := newSyncDesk(t, f)
	d.file("mine.md", "# Mine\nmy text\n")
	d.ok("\n")
	if d.read("from-bob.md") != "bob's text\n" {
		t.Fatal("bob's file didn't come down")
	}
	d.wantOut("New:", "mine.md", "Share this file with Platform? Your team will be able to read its full text. Press v to view it, Enter or y to share, anything else to cancel: ", "Shared 1 file with Platform.")
	if len(f.pushes) != 1 || f.pushes[0].Body != "# Mine\nmy text\n" || f.pushes[0].Metadata[contexttools.MetadataSlug] != "mine.md" {
		t.Fatalf("%+v", f.pushes)
	}
	d.ok("")
	d.wantOut(usertext.SyncUpToDate, "2 unchanged.")
	if strings.Contains(d.out.String(), "Share these") || len(f.pushes) != 1 {
		t.Fatalf("%s %d", d.out, len(f.pushes))
	}
	// A change here goes up as the next version; a change there comes down.
	d.file("mine.md", "# Mine\nnew text\n")
	f.put("n-bob", "team-1", "from-bob.md", "bob's newer text\n", "bob", 2)
	d.ok("y\n")
	d.wantOut("Changed:", "mine.md")
	if d.read("from-bob.md") != "bob's newer text\n" || len(f.pushes) != 2 || f.pushes[1].Version != 2 {
		t.Fatalf("%q %+v", d.read("from-bob.md"), f.pushes)
	}
}

// Both changed: Fleet's text at the name, yours beside it, and yours goes up next time if you say so.
func TestContextSyncConflict(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "first\n")
	d.ok("\n")
	id := f.pushes[0].ID
	d.file("a.md", "my edit\n")
	f.put(id, "team-1", "a.md", "their edit\n", "alice", 2)
	d.ok("n\n")
	if d.read("a.md") != "their edit\n" || d.read("a (your version).md") != "my edit\n" {
		t.Fatalf("%q %q", d.read("a.md"), d.read("a (your version).md"))
	}
	d.wantOut(usertext.SyncConflictLine("a.md", "a (your version).md"), "New:", "a (your version).md")
	// Someone gets there first between the list and the send.
	d.file("b.md", "my new file\n")
	var theirs = contexttools.StableFileID("org-1", "team-1", "b.md")
	f.beforeRead = func() { f.put(theirs, "team-1", "b.md", "their new file\n", "alice", 1) }
	if code := d.sync(true, "\n"); code != ExitError {
		t.Fatalf("%d %s", code, d.out)
	}
	if d.read("b.md") != "their new file\n" || d.read("b (your version).md") != "my new file\n" {
		t.Fatalf("%q %q", d.read("b.md"), d.read("b (your version).md"))
	}
}

// A file that failed is sent by the next run, and only that one.
func TestContextSyncPartialFailureAndRerun(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "a\n")
	d.file("b.md", "b\n")
	d.file("c.md", "c\n")
	f.failPush["b.md"] = 100
	if code := d.sync(true, "\n"); code != ExitError {
		t.Fatalf("%d", code)
	}
	// Fleet stops answering at b: no more waiting, and c isn't tried.
	d.wantOut("b.md: "+usertext.SyncUnreachLine, usertext.SyncStopped(1), "Shared 1 of 3 files with Platform.")
	if len(f.pushes) != 1 {
		t.Fatalf("%d", len(f.pushes))
	}
	f.failPush["b.md"] = 0
	d.ok("\n")
	d.wantOut("Share these 2 files", "Shared 2 files", "1 unchanged.")
	if len(f.pushes) != 3 || f.pushes[1].Metadata[contexttools.MetadataSlug] != "b.md" {
		t.Fatalf("%+v", f.pushes)
	}
	// Fleet busy for a moment: the same run gets it there.
	d.file("d.md", "d\n")
	f.failPush["d.md"] = 1
	d.ok("\n")
	d.wantOut("Shared 1 file")
}

func TestContextSyncPrompt(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "the exact text\n")
	if code := d.sync(false, "\n"); code != ExitError || !strings.Contains(d.errb.String(), "context sync") || len(f.pushes) != 0 {
		t.Fatalf("no terminal: %d %s", code, d.errb)
	}
	if d.out.Len() != 0 {
		t.Fatalf("%s", d.out)
	}
	for _, no := range []string{"n\n", "no\n", "x\n", ""} {
		d.ok(no)
		d.wantOut(usertext.SyncCancelled)
		if len(f.pushes) != 0 {
			t.Fatalf("%q sent it", no)
		}
	}
	d.ok("v\nn\n")
	d.wantOut("--- a.md", "the exact text", usertext.SyncCancelled)
	if len(f.pushes) != 0 {
		t.Fatal("viewing sent it")
	}
	d.ok("v\ny\n")
	d.wantOut("Shared 1 file")
}

// The exact text, a file at a time, with nothing that can move the cursor.
func TestContextSyncViewEscapes(t *testing.T) {
	var out bytes.Buffer
	in := bufio.NewReader(strings.NewReader("\nq\n"))
	plan := contexttools.SyncPlan{Items: []contexttools.SyncItem{
		{Name: "a\x1b[2K.md", Text: "one\x1b[1A\x1b[2Khidden\n"}, {Name: "b.md", Text: "two\n"}, {Name: "c.md", Text: "three\n"},
	}}
	viewSyncItems(Env{Stdout: &out}, in, plan)
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("%q", out.String())
	}
	if !strings.Contains(out.String(), "one") || !strings.Contains(out.String(), "two") || strings.Contains(out.String(), "three") {
		t.Fatalf("q didn't stop it: %q", out.String())
	}
}

func TestContextSyncRefusedAmongGood(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("good.md", "fine\n")
	d.file("bad.md", "fine\n-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n")
	d.file("hidden.md", "fine\u200b text\n")
	d.ok("\n")
	d.wantOut("Not sent: bad.md: it looks like it holds a credential:", "line 2", "Not sent: hidden.md: it has characters a reader can't see:", "Share this file", "Shared 1 file")
	if len(f.pushes) != 1 || f.pushes[0].Metadata[contexttools.MetadataSlug] != "good.md" {
		t.Fatalf("%+v", f.pushes)
	}
	if strings.Contains(d.out.String(), "BEGIN RSA") {
		t.Fatal("the text was printed")
	}
}

func TestContextSyncMemoryCount(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("drawn.md", "from notes\n")
	d.file("plain.md", "plain\n")
	set := contextlib.Open(d.personal, t.TempDir())
	key, err := set.Key("team/platform/drawn.md")
	if err != nil {
		t.Fatal(err)
	}
	led := contextlib.NewLedger(d.env.contextLedgerDir())
	if err := led.Update(key, func(r *contextlib.FileRecord) {
		r.Path, r.DraftedFromMemory, r.DraftedSHA256 = "team/platform/drawn.md", true, contextlib.HashOf([]byte("from notes\n"))
	}); err != nil {
		t.Fatal(err)
	}
	d.ok("\n")
	d.wantOut("Share these 2 files with Platform? Your team will be able to read their full text. 1 is drawn from your private memory. Press v", "drawn.md", "drawn from your private memory")
	if r, _ := led.Get(key); r.DraftedFromMemory {
		t.Fatal("the mark stayed after sharing that text")
	}
}

// What's already here stays where it is: a pulled copy keeps its file, and your own files stay yours.
func TestContextSyncExistingFolders(t *testing.T) {
	f := newSyncFleet()
	f.put("old-id", "team-1", "pulled.md", "pulled text\n", "alice", 3)
	d := newSyncDesk(t, f)
	d.file("pulled.md", "pulled text\nedited\n")
	set := contextlib.Open(d.personal, t.TempDir())
	key, err := set.Key("team/platform/pulled.md")
	if err != nil {
		t.Fatal(err)
	}
	led := contextlib.NewLedger(d.env.contextLedgerDir())
	acct := contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if err := led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = "team/platform/pulled.md"
		r.SetLink(contextlib.Link{Account: acct, NodeID: "old-id", TeamID: "team-1", Version: 3, BaseSHA256: contextlib.HashOf([]byte("pulled text\n"))})
	}); err != nil {
		t.Fatal(err)
	}
	// Your own files and a repository's never go.
	if err := os.WriteFile(filepath.Join(mustDir(t, d.personal), "private.md"), []byte("mine alone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	mustDir(t, filepath.Join(repo, ".git"))
	if err := os.WriteFile(filepath.Join(mustDir(t, filepath.Join(repo, ".kenaz", "context")), "repo.md"), []byte("repo text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	d.ok("\n")
	if len(f.pushes) != 1 || f.pushes[0].ID != "old-id" || f.pushes[0].Version != 4 {
		t.Fatalf("%+v", f.pushes)
	}
	if strings.Contains(d.out.String(), "private.md") || strings.Contains(d.out.String(), "repo.md") {
		t.Fatalf("%s", d.out)
	}
}

func mustDir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContextSyncNeverSends(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file(".hidden.md", "no\n")
	d.file(".trash/old.md", "no\n")
	d.file("sub/nested.md", "no\n")
	d.ok("")
	d.wantOut("1 in subfolders", usertext.SyncUpToDate)
	if len(f.pushes) != 0 {
		t.Fatalf("%+v", f.pushes)
	}
}

// A teammate's file: Fleet refuses, in plain words, the text stays, and it's tried again next time.
func TestContextSyncTeammateRefusal(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	f.put("n-bob", "team-1", "bobs.md", "bob's text\n", "bob", 1)
	d.ok("")
	d.file("bobs.md", "my change to it\n")
	if code := d.sync(true, "\n"); code != ExitError {
		t.Fatalf("%d", code)
	}
	d.wantOut("bobs.md: "+usertext.SyncNotYoursLine, "Shared 0 of 1 file")
	if d.read("bobs.md") != "my change to it\n" || f.nodes["n-bob"].Body != "bob's text\n" {
		t.Fatal("something was lost or overwritten")
	}
	if code := d.sync(true, "\n"); code != ExitError {
		t.Fatalf("%d", code)
	}
	d.wantOut("Changed:", "bobs.md")
}

func TestContextSyncRemovals(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("kept.md", "kept\n")
	d.file("gone.md", "gone\n")
	d.file("unsafe.md", "unsafe\n")
	d.ok("\n")
	// Removed here: listed, nothing sent, and Fleet still has it.
	if err := os.Remove(filepath.Join(d.teamDir, "gone.md")); err != nil {
		t.Fatal(err)
	}
	pushes := len(f.pushes)
	d.ok("")
	d.wantOut("still on Kameas Fleet", "gone.md")
	if len(f.pushes) != pushes || f.bySlug("gone.md") == nil {
		t.Fatal("a removal changed Fleet")
	}
	// Removed on Fleet: reported, and the copy stays.
	n := f.bySlug("unsafe.md")
	n.Node.Deleted, n.Node.RemovalReason, n.Body = true, fleetapi.RemovalUnsafeContent, ""
	d.ok("")
	d.wantOut("unsafe.md: " + contexttools.UnsafeRemovalMessage)
	if d.read("unsafe.md") != "unsafe\n" || len(f.pushes) != pushes {
		t.Fatal("the copy went, or was sent back")
	}
}

// Two people adding a file of one name meet at one id, and it says nothing about them.
func TestContextSyncStableID(t *testing.T) {
	a := contexttools.StableFileID("org-1", "team-1", "x.md")
	if a != contexttools.StableFileID("org-1", "team-1", "x.md") || a == contexttools.StableFileID("org-1", "team-2", "x.md") || a == contexttools.StableFileID("org-2", "team-1", "x.md") {
		t.Fatal(a)
	}
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("x.md", "text\n")
	d.ok("\n")
	if f.pushes[0].ID != a {
		t.Fatalf("%s %s", f.pushes[0].ID, a)
	}
	// A second device with the same text just links up, and sends nothing.
	second := newSyncDesk(t, f)
	second.file("x.md", "text\n")
	second.ok("")
	if len(f.pushes) != 1 {
		t.Fatalf("%d", len(f.pushes))
	}
}

func TestContextSyncTeams(t *testing.T) {
	f := newSyncFleet()
	f.teams = []fleetapi.TeamRef{{ID: "team-1", Name: "Platform"}, {ID: "team-2", Name: "Sales"}}
	d := newSyncDesk(t, f)
	d.file("a.md", "a\n")
	d.ok("1\n\n")
	d.wantOut("1. Platform", "2. Sales", usertext.SyncPickTeam, "with Platform")
	if len(f.pushes) != 1 || f.pushes[0].TeamID != "team-1" {
		t.Fatalf("%+v", f.pushes)
	}
	if code := d.sync(true, "nope\n"); code != ExitError {
		t.Fatalf("%d", code)
	}
	if code := d.sync(true, "", "--team", "Nobody"); code != ExitError || !strings.Contains(d.errb.String(), "No team named") {
		t.Fatalf("%d %s", code, d.errb)
	}
	if code := d.sync(true, "", "--team", "Sales"); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
}

// No word about how it works underneath, in any line a person sees.
func TestContextSyncSaysNothingOfGit(t *testing.T) {
	f := newSyncFleet()
	f.put("n", "team-1", "t.md", "theirs\n", "alice", 1)
	d := newSyncDesk(t, f)
	d.file("t.md", "mine\n")
	d.file("new.md", "new\n")
	d.ok("\n")
	help := &bytes.Buffer{}
	contextHelp(Env{Stdout: help})
	all := strings.ToLower(d.out.String() + d.errb.String())
	for _, w := range []string{"git", "commit", "branch", "rebase", "remote", "merge"} {
		if strings.Contains(all, w) {
			t.Errorf("%q in %s", w, all)
		}
	}
	if !strings.HasPrefix(help.String(), "usage:\n  "+"kenaz"[:0]) || !strings.Contains(strings.SplitN(help.String(), "\n", 3)[1], "context sync") {
		t.Errorf("help doesn't lead with sync: %s", help)
	}
}

// One account's text never goes out under another, with a team of the same name.
func TestContextSyncOtherOrgTextStays(t *testing.T) {
	a := newSyncFleet()
	d := newSyncDesk(t, a)
	a.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "ORG A CONFIDENTIAL\n", "carol", 1)
	d.file("draft.md", "meant for org A\n")
	d.ok("n\n")
	b := otherOrg()
	b.put("22222222-2222-7222-8222-222222222222", "team-b", "runbook.md", "ORG B CONFIDENTIAL\n", "dave", 1)
	d.env.Provider = b
	d.ok("\n")
	if len(b.pushes) != 0 {
		t.Fatalf("sent to the other org: %+v", b.pushes)
	}
	if strings.Contains(d.out.String(), "draft.md") {
		t.Fatalf("listed: %s", d.out)
	}
	// Org B's text is in its own folder, and org A's is as it was.
	if d.read("runbook.md") != "ORG A CONFIDENTIAL\n" {
		t.Fatal(d.read("runbook.md"))
	}
	bDir := filepath.Join(filepath.Dir(d.teamDir), "platform-teamb")
	if got, err := os.ReadFile(filepath.Join(bDir, "runbook.md")); err != nil || string(got) != "ORG B CONFIDENTIAL\n" {
		t.Fatalf("%q %v", got, err)
	}
	// Back again: nothing of org B's reads as an edit.
	d.env.Provider = a
	a.pushes = nil
	d.ok("n\n")
	if strings.Contains(d.out.String(), "runbook") || len(a.pushes) != 0 {
		t.Fatalf("%s", d.out)
	}
}

// Two teams whose names make one folder name get two folders.
func TestContextSyncTeamsSharingAName(t *testing.T) {
	f := newSyncFleet()
	f.teams = []fleetapi.TeamRef{{ID: "team-1", Name: "Platform"}, {ID: "team-2", Name: "platform"}}
	d := newSyncDesk(t, f)
	d.file("a.md", "for team one\n")
	if code := d.sync(true, "\n", "--team", "team-1"); code != ExitOK {
		t.Fatalf("%d %s %s", code, d.out, d.errb)
	}
	id := f.pushes[0].ID
	d.file("a.md", "edited, for team one\n")
	if code := d.sync(true, "\n", "--team", "team-2"); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	if len(f.pushes) != 1 || f.nodes[id].Node.TeamID != "team-1" {
		t.Fatalf("%+v", f.pushes)
	}
}

// A file whose link says another team isn't sent to this one.
func TestContextSyncLinkToAnotherTeam(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "text\n")
	set := contextlib.Open(d.personal, t.TempDir())
	key, _ := set.Key("team/platform/a.md")
	led := contextlib.NewLedger(d.env.contextLedgerDir())
	acct := contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if err := led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = "team/platform/a.md"
		r.SetLink(contextlib.Link{Account: acct, NodeID: "n", TeamID: "team-9"})
	}); err != nil {
		t.Fatal(err)
	}
	d.ok("")
	d.wantOut("Not sent: a.md: this copy belongs to another of your teams.")
	if len(f.pushes) != 0 {
		t.Fatalf("%+v", f.pushes)
	}
}

// An older folder: what can be shown to be this account's and team's stays usable, and
// the rest isn't sent, and is reported.
func TestContextSyncOlderFolders(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	teams := contextlib.TeamDir(d.personal)
	if err := os.RemoveAll(d.teamDir); err != nil {
		t.Fatal(err)
	}
	f.put("old-id", "team-1", "pulled.md", "pulled text\n", "alice", 3)
	d.file("pulled.md", "pulled text\nedited\n")
	d.file("theirs.md", "someone else's\n")
	d.file("whose.md", "whose is this\n")
	set := contextlib.Open(d.personal, t.TempDir())
	key, _ := set.Key("team/platform/pulled.md")
	led := contextlib.NewLedger(d.env.contextLedgerDir())
	acct := contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if err := led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = "team/platform/pulled.md"
		r.SetLink(contextlib.Link{Account: acct, NodeID: "old-id", TeamID: "team-1", Version: 3, BaseSHA256: contextlib.HashOf([]byte("pulled text\n"))})
	}); err != nil {
		t.Fatal(err)
	}
	// A link to someone else proves the folder isn't all this account's.
	theirKey, _ := set.Key("team/platform/theirs.md")
	if err := led.Update(theirKey, func(r *contextlib.FileRecord) {
		r.Path = "team/platform/theirs.md"
		r.SetLink(contextlib.Link{Account: contextlib.Account{Env: "dev", OrgID: "org-9", UserID: "carol"}, NodeID: "n-c", TeamID: "team-9"})
	}); err != nil {
		t.Fatal(err)
	}
	d.ok("\n")
	if len(f.pushes) != 1 || f.pushes[0].ID != "old-id" || f.pushes[0].Version != 4 {
		t.Fatalf("%+v", f.pushes)
	}
	newDir := filepath.Join(teams, "platform-team1")
	if _, err := os.Stat(filepath.Join(newDir, "pulled.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.teamDir, "whose.md")); err != nil {
		t.Fatal(err)
	}
	d.wantOut(usertext.SyncLegacyLine(2, "platform", "platform-team1"))
	if strings.Contains(d.out.String(), "New:") {
		t.Fatalf("%s", d.out)
	}
	// An older folder of nothing but this team's files is simply taken.
	f2 := newSyncFleet()
	d2 := newSyncDesk(t, f2)
	if err := os.Remove(filepath.Join(d2.teamDir, contextlib.FolderRecord)); err != nil {
		t.Fatal(err)
	}
	d2.ok("")
	if owner, bound := contextlib.Team(contextlib.TeamDir(d2.personal)).FolderOwner("platform"); !bound || owner.TeamID != "team-1" {
		t.Fatalf("%+v %v", owner, bound)
	}
}

// Another account can't see, read or write a bound folder, and neither can nobody.
func TestContextFoldersAreSeparated(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "alice's team's text\n")
	mine := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	other := &contextlib.Account{Env: "dev", OrgID: "org-2", UserID: "bob"}
	cwd := t.TempDir()
	for _, tc := range []struct {
		name string
		who  *contextlib.Account
		see  bool
	}{{"owner", mine, true}, {"other", other, false}, {"signed out", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			set := contextlib.Open(d.personal, cwd).WithViewer(tc.who)
			listed := len(set.List(context.Background()).Files) > 0
			_, rerr := set.Read("team/platform/a.md", 0, 100)
			_, werr := set.Write("team/platform/new.md", []byte("x\n"), contextlib.WriteOptions{})
			if listed != tc.see || (rerr == nil) != tc.see || (werr == nil) != tc.see {
				t.Fatalf("listed %v, read %v, write %v", listed, rerr, werr)
			}
			if !tc.see && !errors.Is(rerr, contextlib.ErrOtherOwner) && !errors.Is(rerr, contextlib.ErrFolderNeedsSignIn) {
				t.Fatal(rerr)
			}
			idx := contextlib.SessionContext(context.Background(), set, contextlib.Options{Index: true, IndexBytes: 1536})
			if strings.Contains(idx, "a.md") != tc.see {
				t.Fatal(idx)
			}
		})
	}
}

// Two files on Fleet with one short name: each keeps a name of its own, the same every run.
func TestContextSyncDuplicateShortNames(t *testing.T) {
	f := newSyncFleet()
	f.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "carol's\n", "carol", 1)
	f.put("22222222-2222-7222-8222-222222222222", "team-1", "runbook.md", "dave's\n", "dave", 1)
	d := newSyncDesk(t, f)
	var counts []int
	for i := 0; i < 4; i++ {
		d.ok("n\n")
		es, _ := os.ReadDir(d.teamDir)
		counts = append(counts, len(es))
		if strings.Contains(d.out.String(), "your version") || strings.Contains(d.out.String(), "New:") || strings.Contains(d.out.String(), "Share these") {
			t.Fatalf("run %d: %s", i, d.out)
		}
	}
	if counts[0] != counts[3] {
		t.Fatalf("%v", counts)
	}
	if d.read("runbook.md") != "carol's\n" || d.read("runbook-22222222.md") != "dave's\n" && d.read("runbook-11111111.md") == "" {
		t.Fatal("not one file each")
	}
	// An edit of the second goes to its own node, under its own short name.
	d.file("runbook-11111111.md", "changed\n")
	d.ok("\n")
}

func TestContextSyncRefusesLinks(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	other := filepath.Join(filepath.Dir(d.teamDir), "secret-team")
	mustDir(t, other)
	if err := os.WriteFile(filepath.Join(other, "plan.md"), []byte("OTHER TEAM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret-team/plan.md", filepath.Join(d.teamDir, "link.md")); err != nil {
		t.Fatal(err)
	}
	d.file("ok.md", "fine\n")
	d.ok("\n")
	d.wantOut("Not sent: link.md: it's a link")
	for _, p := range f.pushes {
		if strings.Contains(p.Body, "OTHER TEAM") {
			t.Fatal("a link sent another team's copy")
		}
	}
	if len(f.pushes) != 1 {
		t.Fatalf("%+v", f.pushes)
	}
	// Coming down doesn't write through one either.
	f.put("n-link", "team-1", "link.md", "from fleet\n", "carol", 1)
	d.ok("")
	if got, _ := os.ReadFile(filepath.Join(other, "plan.md")); string(got) != "OTHER TEAM\n" {
		t.Fatalf("%q", got)
	}
}

func TestContextSyncTeamNameIsEscaped(t *testing.T) {
	f := newSyncFleet()
	f.teams = []fleetapi.TeamRef{{ID: "team-1", Name: "Platform\x1b[2K\r"}}
	d := newSyncDesk(t, f)
	d.file("a.md", "x\n")
	d.ok("\n")
	if strings.Contains(d.out.String(), "\x1b") || strings.Contains(d.out.String(), "\r") {
		t.Fatalf("%q", d.out.String())
	}
}

func TestContextSyncNotesAreJustCounted(t *testing.T) {
	f := newSyncFleet()
	f.nodes["n1"] = &fleetapi.NodeContent{Body: "a note\n", Node: fleetapi.ContextNode{ID: "n1", Title: "A shared note", TeamID: "team-1", Version: 1, OwnerUserID: "carol", Classification: fleetapi.ClassificationTeamShared}}
	f.order = append(f.order, "n1")
	d := newSyncDesk(t, f)
	d.ok("")
	d.wantOut(usertext.SyncNotesLine(1))
	if strings.Contains(d.out.String(), "couldn't be saved") {
		t.Fatalf("%s", d.out)
	}
}

func TestContextSyncRepeatedConflictsKeepEveryVersion(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "first\n")
	d.ok("\n")
	id := f.pushes[0].ID
	for i := 2; i <= 4; i++ {
		d.file("a.md", fmt.Sprintf("my edit %d\n", i))
		f.put(id, "team-1", "a.md", fmt.Sprintf("their edit %d\n", i), "alice", i)
		d.ok("n\n")
	}
	for n, w := range map[string]string{"a (your version).md": "my edit 2\n", "a (your version 2).md": "my edit 3\n", "a (your version 3).md": "my edit 4\n", "a.md": "their edit 4\n"} {
		if d.read(n) != w {
			t.Errorf("%s = %q", n, d.read(n))
		}
	}
}

// What's sent is what was listed, and a later edit isn't lost.
func TestContextSyncEditBetweenPromptAndSend(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("a.md", "listed text\n")
	f.beforeRead = func() { d.file("a.md", "edited after the prompt\n") }
	d.ok("\n")
	if f.pushes[0].Body != "listed text\n" || d.read("a.md") != "edited after the prompt\n" {
		t.Fatalf("%q %q", f.pushes[0].Body, d.read("a.md"))
	}
	d.ok("n\n")
	d.wantOut("Changed:", "a.md")
}

// A name whose file was removed from Fleet can't be used again, and the words say what to do.
func TestContextSyncRemovedNameCantBeReused(t *testing.T) {
	f := newSyncFleet()
	id := contexttools.StableFileID("org-1", "team-1", "a.md")
	f.put(id, "team-1", "a.md", "", "carol", 2)
	f.nodes[id].Node.Deleted = true
	d := newSyncDesk(t, f)
	d.file("a.md", "new text\n")
	if code := d.sync(true, "\n"); code != ExitError {
		t.Fatalf("%d %s", code, d.out)
	}
	d.wantOut("a.md: " + usertext.SyncNameGone)
	if len(f.pushes) != 0 {
		t.Fatalf("%+v", f.pushes)
	}
}

// Paths that clean to another account's folder are that account's folder.
func TestContextPathsAroundTheBinding(t *testing.T) {
	a := newSyncFleet()
	d := newSyncDesk(t, a)
	d.file("secret.md", "ORG A\n")
	d.ok("\n")
	bob := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "bob"}
	for name, set := range map[string]contextlib.Set{"other": contextlib.Open(d.personal, "").WithViewer(bob), "signed out": contextlib.Open(d.personal, "").WithViewer(nil)} {
		for _, ref := range []string{"team/platform/secret.md", "team/./platform/secret.md", "team/nothing/../platform/secret.md", `team\platform\secret.md`, "team/platform//secret.md", "team/././platform/./secret.md"} {
			if pg, err := set.Read(ref, 0, 100); err == nil || strings.Contains(pg.Content, "ORG A") {
				t.Errorf("%s read past the binding with %q: %v", name, ref, err)
			}
			if _, err := set.Locate(ref); err == nil {
				t.Errorf("%s located %q", name, ref)
			}
			if _, err := set.Key(ref); err == nil {
				t.Errorf("%s got a key for %q", name, ref)
			}
			if _, err := set.Write(strings.Replace(ref, "secret", "planted", 1), []byte("from another account\n"), contextlib.WriteOptions{}); err == nil {
				t.Errorf("%s wrote past the binding with %q", name, ref)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(d.teamDir, "planted.md")); err == nil {
		t.Fatal("a file was planted")
	}
}

// First use: a file put in a team's folder before any sync is listed and goes, with one prompt.
func TestContextSyncFirstUse(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	if err := os.Remove(filepath.Join(d.teamDir, contextlib.FolderRecord)); err != nil {
		t.Fatal(err)
	}
	d.file("notes.md", "my first note\n")
	d.ok("\n")
	d.wantOut("New:", "notes.md", "Share this file with Platform in org-1?", "Shared 1 file")
	if strings.Contains(d.out.String(), "older file") || len(f.pushes) != 1 {
		t.Fatalf("%s", d.out)
	}
	if o, bound := contextlib.Team(contextlib.TeamDir(d.personal)).FolderOwner("platform"); !bound || o.TeamID != "team-1" || o.UserID != "alice" {
		t.Fatalf("%+v %v", o, bound)
	}
}

// context_write makes a new folder this account's, and the first sync adds the team.
func TestContextWriteClaimsANewFolder(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	lib := contextlib.Team(contextlib.TeamDir(d.personal))
	if err := os.RemoveAll(d.teamDir); err != nil {
		t.Fatal(err)
	}
	alice := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	bob := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "bob"}
	if _, err := contextlib.Open(d.personal, "").WithViewer(alice).Write("team/platform/x.md", []byte("drafted\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if o, bound := lib.FolderOwner("platform"); !bound || !o.Unassigned || o.UserID != "alice" {
		t.Fatalf("%+v %v", o, bound)
	}
	if _, err := contextlib.Open(d.personal, "").WithViewer(bob).Read("team/platform/x.md", 0, 10); !errors.Is(err, contextlib.ErrOtherOwner) {
		t.Fatal(err)
	}
	d.ok("\n")
	d.wantOut("New:", "x.md", "Shared 1 file")
	if o, _ := lib.FolderOwner("platform"); o.Unassigned || o.TeamID != "team-1" {
		t.Fatalf("%+v", o)
	}
	// Another account's unassigned folder isn't taken.
	if err := lib.BindFolder("sales", contextlib.FolderOwner{Env: "dev", OrgID: "org-1", UserID: "bob", Unassigned: true}); err != nil {
		t.Fatal(err)
	}
	f2 := newSyncFleet()
	f2.teams = []fleetapi.TeamRef{{ID: "team-s", Name: "Sales"}}
	d.env.Provider = f2
	d.ok("")
	if o, _ := lib.FolderOwner("sales"); o.UserID != "bob" {
		t.Fatalf("%+v", o)
	}
	if o, bound := lib.FolderOwner("sales-teams"); !bound || o.TeamID != "team-s" {
		t.Fatalf("%+v %v", o, bound)
	}
}

// A record nobody can read: it says which folder, it doesn't say all is well, and nothing goes.
func TestContextSyncUnreadableRecord(t *testing.T) {
	for name, body := range map[string]string{"corrupt": "not json", "empty": "{}"} {
		t.Run(name, func(t *testing.T) {
			f := newSyncFleet()
			d := newSyncDesk(t, f)
			d.file("secret.md", "TEXT\n")
			d.ok("\n")
			f.pushes = nil
			if err := os.WriteFile(filepath.Join(d.teamDir, contextlib.FolderRecord), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if code := d.sync(true, "\n"); code != ExitError {
				t.Fatalf("%d %s", code, d.out)
			}
			d.wantOut(usertext.SyncFolderUnreadable("platform"))
			if strings.Contains(d.out.String(), usertext.SyncUpToDate) || len(f.pushes) != 0 {
				t.Fatalf("%s", d.out)
			}
		})
	}
	// Deleted, with files linked to their owner: another account still doesn't get them.
	a := newSyncFleet()
	d := newSyncDesk(t, a)
	d.file("secret.md", "ORG A TEXT\n")
	d.ok("\n")
	if err := os.Remove(filepath.Join(d.teamDir, contextlib.FolderRecord)); err != nil {
		t.Fatal(err)
	}
	b := otherOrg()
	d.env.Provider = b
	d.ok("\n")
	if len(b.pushes) != 0 {
		t.Fatalf("%+v", b.pushes)
	}
	// By the tools, the record can't be written, and no Fleet name can be it.
	if _, err := contextlib.Open(d.personal, "").Write("team/platform/"+contextlib.FolderRecord, []byte("{}"), contextlib.WriteOptions{Replace: true}); err == nil {
		t.Error("a tool path wrote the record")
	}
	if contextlib.ValidSlug(contextlib.FolderRecord) {
		t.Error("a short name can be the record")
	}
}

func TestContextPulledFileNamedLikeTheRecord(t *testing.T) {
	f := newSyncFleet()
	f.put("11111111-1111-7111-8111-111111111111", "team-1", contextlib.FolderRecord, `{"env":"x","org_id":"y","user_id":"z"}`, "carol", 1)
	d := newSyncDesk(t, f)
	d.ok("\n")
	b, _ := os.ReadFile(filepath.Join(d.teamDir, contextlib.FolderRecord))
	if strings.Contains(string(b), `"user_id":"z"`) {
		t.Errorf("a teammate wrote the record: %s", b)
	}
}

// The real tools, through kenaz mcp, as the owner, another account and nobody.
func TestContextToolsWithASession(t *testing.T) {
	old := features.ContextLibrary
	features.ContextLibrary = true
	t.Cleanup(func() { features.ContextLibrary = old })
	a := newSyncFleet()
	d := newSyncDesk(t, a)
	d.file("secret.md", "ORG A\n")
	d.ok("\n")
	run := func(p fleetapi.Provider) (list, read, write map[string]any) {
		env := d.env
		env.Provider = p
		env.Stdout, env.Stderr = d.out, d.errb
		env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
			ct, st := sdk.NewInMemoryTransports()
			if _, err := srv.Connect(ctx, st, nil); err != nil {
				return err
			}
			cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				return err
			}
			defer cs.Close()
			call := func(name string, args map[string]any) map[string]any {
				res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					return map[string]any{"not_served": err.Error()}
				}
				raw, _ := json.Marshal(res.StructuredContent)
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				return m
			}
			list = call("context_list", map[string]any{})
			read = call("context_read", map[string]any{"path": "team/platform/secret.md"})
			write = call("context_write", map[string]any{"path": "team/platform/planted.md", "content": "x\n"})
			return nil
		}
		if code := RunEnv(env, []string{"mcp"}); code != ExitOK {
			t.Fatalf("mcp exit %d: %s", code, d.errb)
		}
		return
	}
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	l, r, _ := run(a)
	if !strings.Contains(asJSON(l), "team/platform/secret.md") || r["content"] != "ORG A\n" {
		t.Errorf("the owner lost their copy: %v %v", l, r)
	}
	bob := newSyncFleet()
	bob.me = "bob"
	l, r, w := run(bob)
	if strings.Contains(asJSON(l), "secret.md") || r["content"] != nil || w["error"] == nil || !strings.Contains(asJSON(r), "another account") {
		t.Errorf("another account: %s %s %s", asJSON(l), asJSON(r), asJSON(w))
	}
	if l["sign_in"] != nil {
		t.Errorf("another account was told to sign in: %s", asJSON(l))
	}
	// Signed out, with no context_files to hear, the local tools still work and
	// refuse only the team copy.
	l, r, w = run(signedOutFleet{a})
	if r["content"] != nil || strings.Contains(asJSON(r), "another account") || !strings.Contains(asJSON(r), "kenaz login") || !strings.Contains(asJSON(w), "kenaz login") {
		t.Errorf("signed out: %s %s", asJSON(r), asJSON(w))
	}
	if l["sign_in"] != usertext.TeamFilesNeedSignIn || l["not_served"] != nil {
		t.Errorf("signed out list: %s", asJSON(l))
	}
	idx := contextFilesIndex{env: func() Env { e := d.env; e.Provider = bob; return e }(), dir: d.personal}.Index(context.Background(), t.TempDir())
	if strings.Contains(idx, "secret") {
		t.Errorf("index: %s", idx)
	}
}

type signedOutFleet struct{ *syncFleet }

func (p signedOutFleet) Auth() fleetapi.Auth { return signedOutFleetAuth{p.syncFleet.Auth()} }

type signedOutFleetAuth struct{ fleetapi.Auth }

func (signedOutFleetAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{}, fleetapi.ErrNotSignedIn
}

// No churn: two files with one name, a teammate's edit, a rename on Fleet, a second device.
func TestContextSyncNoChurnAcrossDevicesAndRename(t *testing.T) {
	f := newSyncFleet()
	one, two := "11111111-1111-7111-8111-111111111111", "22222222-2222-7222-8222-222222222222"
	f.put(one, "team-1", "runbook.md", "carol's\n", "carol", 1)
	f.put(two, "team-1", "runbook.md", "dave's\n", "dave", 1)
	d := newSyncDesk(t, f)
	offered := func(d *syncDesk) bool {
		return strings.Contains(d.out.String(), "New:") || strings.Contains(d.out.String(), "Changed:")
	}
	for i := 0; i < 3; i++ {
		d.ok("n\n")
		if offered(d) {
			t.Fatalf("%s", d.out)
		}
	}
	f.put(two, "team-1", "runbook.md", "dave's edit\n", "dave", 2)
	d.ok("n\n")
	got := map[string]bool{}
	es, _ := os.ReadDir(d.teamDir)
	for _, e := range es {
		b, _ := os.ReadFile(filepath.Join(d.teamDir, e.Name()))
		got[string(b)] = true
	}
	if !got["dave's edit\n"] || !got["carol's\n"] || got["dave's\n"] {
		t.Fatalf("%v", got)
	}
	f.put(one, "team-1", "ops.md", "carol's\n", "carol", 2)
	d.ok("n\n")
	if offered(d) {
		t.Fatalf("a rename offers files: %s", d.out)
	}
	d2 := newSyncDesk(t, f)
	for i := 0; i < 3; i++ {
		d2.ok("n\n")
		if offered(d2) {
			t.Fatalf("second device: %s", d2.out)
		}
	}
}

func TestContextSyncLines(t *testing.T) {
	if got := usertext.SyncStopped(1); !strings.Contains(got, "1 file wasn't sent") {
		t.Error(got)
	}
	if got := usertext.SyncStopped(2); !strings.Contains(got, "2 files weren't sent") {
		t.Error(got)
	}
	if got := usertext.SyncNotesLine(1); !strings.Contains(got, "1 shared note stays") {
		t.Error(got)
	}
}

func syncTree(d *syncDesk) string {
	root := filepath.Dir(d.teamDir)
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.Contains(p, ".trash") {
			c, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			b.WriteString(rel + " = " + strings.TrimSpace(string(c)) + "\n")
		}
		return nil
	})
	return b.String()
}

func (d *syncDesk) unbind() {
	d.t.Helper()
	if err := os.Remove(filepath.Join(d.teamDir, contextlib.FolderRecord)); err != nil {
		d.t.Fatal(err)
	}
}

// The wrong account says no: the folder goes back, and the right one still gets the file.
func TestContextSyncNoUndoesTheBinding(t *testing.T) {
	a := newSyncFleet()
	d := newSyncDesk(t, a)
	d.unbind()
	d.file("draft.md", "A DRAFT\n")
	b := otherOrg()
	b.orgName = "Other Org"
	d.env.Provider = b
	d.ok("n\n")
	d.wantOut("Share this file with Platform in Other Org?", usertext.SyncCancelled)
	if len(b.pushes) != 0 {
		t.Fatalf("%+v", b.pushes)
	}
	lib := contextlib.Team(contextlib.TeamDir(d.personal))
	if _, bound := lib.FolderOwner("platform"); bound {
		t.Fatal("still bound after no")
	}
	d.env.Provider = a
	d.ok("\n")
	d.wantOut("draft.md", "Shared 1 file")
	if o, _ := lib.FolderOwner("platform"); o.UserID != "alice" {
		t.Fatalf("%+v", o)
	}
}

// No, after something came down into it: it can't go back, so it says whose it now is.
func TestContextSyncNoAfterAPullSaysWhoseItIs(t *testing.T) {
	f := newSyncFleet()
	f.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "CAROL\n", "carol", 1)
	d := newSyncDesk(t, f)
	d.unbind()
	d.file("draft.md", "A DRAFT\n")
	d.ok("n\n")
	d.wantOut(usertext.SyncFolderBound("platform", "Platform in org-1"))
	if strings.Contains(d.out.String(), usertext.SyncUpToDate) {
		t.Fatalf("%s", d.out)
	}
	if _, bound := contextlib.Team(contextlib.TeamDir(d.personal)).FolderOwner("platform"); !bound {
		t.Fatal("not bound")
	}
}

// An unassigned folder goes back to unassigned on no.
func TestContextSyncNoKeepsAnUnassignedFolder(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	if err := os.RemoveAll(d.teamDir); err != nil {
		t.Fatal(err)
	}
	alice := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if _, err := contextlib.Open(d.personal, "").WithViewer(alice).Write("team/platform/x.md", []byte("DRAFT\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	d.ok("n\n")
	if o, bound := contextlib.Team(contextlib.TeamDir(d.personal)).FolderOwner("platform"); !bound || !o.Unassigned {
		t.Fatalf("%+v %v", o, bound)
	}
}

// Doing as the message says after a corrupt record: the file syncs.
func TestContextSyncCorruptThenDeleteAsTold(t *testing.T) {
	f := newSyncFleet()
	f.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "CAROL\n", "carol", 1)
	d := newSyncDesk(t, f)
	d.file("mine.md", "MINE\n")
	d.ok("\n")
	if err := os.WriteFile(filepath.Join(d.teamDir, contextlib.FolderRecord), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = d.sync(true, "n\n")
	d.wantOut(usertext.SyncFolderUnreadable("platform"))
	d.unbind()
	d.file("mine.md", "MINE EDITED\n")
	f.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "CAROL 2\n", "carol", 2)
	before := len(f.pushes)
	_ = d.sync(true, "\n")
	tree := syncTree(d)
	if !strings.Contains(tree, "MINE EDITED") || !strings.Contains(tree, "CAROL 2") {
		t.Fatalf("%s", tree)
	}
	if len(f.pushes) != before+1 || f.pushes[before].Body != "MINE EDITED\n" {
		t.Fatalf("%s %+v", d.out, f.pushes[before:])
	}
}

// A file written for no team of yours is mentioned, rather than all being up to date.
func TestContextSyncUnclaimedFolder(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	alice := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if _, err := contextlib.Open(d.personal, "").WithViewer(alice).Write("team/platform-team/x.md", []byte("DRAFT\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	d.ok("")
	d.wantOut(usertext.SyncUnclaimed("platform-team"))
	if strings.Contains(d.out.String(), usertext.SyncUpToDate) || len(f.pushes) != 0 {
		t.Fatalf("%s", d.out)
	}
}

func TestContextSyncPromptWording(t *testing.T) {
	for _, tc := range []struct {
		n, m int
		want string
	}{
		{1, 0, "Share this file with T? Your team will be able to read its full text. Press v to view it, Enter or y to share, anything else to cancel: "},
		{1, 1, "Share this file with T? Your team will be able to read its full text. It's drawn from your private memory. Press v to view it, Enter or y to share, anything else to cancel: "},
		{3, 0, "Share these 3 files with T? Your team will be able to read their full text. Press v to view them, Enter or y to share, anything else to cancel: "},
		{3, 2, "Share these 3 files with T? Your team will be able to read their full text. 2 are drawn from your private memory. Press v to view them, Enter or y to share, anything else to cancel: "},
	} {
		if got := usertext.PromptSyncShare(tc.n, "T", tc.m); got != tc.want {
			t.Errorf("%q", got)
		}
	}
}

// A refused save leaves no record behind, and nobody signed in can start a team's folder.
func TestContextWriteClaims(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	lib := contextlib.Team(contextlib.TeamDir(d.personal))
	alice := &contextlib.Account{Env: "dev", OrgID: "org-1", UserID: "alice"}
	if _, err := contextlib.Open(d.personal, "").WithViewer(alice).Write("team/sales/x.exe", []byte("x"), contextlib.WriteOptions{}); err == nil {
		t.Fatal("saved")
	}
	if _, bound := lib.FolderOwner("sales"); bound {
		t.Fatal("a refused save claimed the folder")
	}
	if _, err := contextlib.Open(d.personal, "").WithViewer(nil).Write("team/new/x.md", []byte("x\n"), contextlib.WriteOptions{}); !errors.Is(err, contextlib.ErrFolderNeedsSignIn) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(d.teamDir), "new", "x.md")); err == nil {
		t.Fatal("saved signed out")
	}
	// Your own files still save.
	if _, err := contextlib.Open(d.personal, "").WithViewer(nil).Write("personal/x.md", []byte("x\n"), contextlib.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A hand-made file with a teammate's file's name: both texts survive.
func TestContextSyncFirstUseNameCollision(t *testing.T) {
	f := newSyncFleet()
	f.put("11111111-1111-7111-8111-111111111111", "team-1", "runbook.md", "CAROL\n", "carol", 1)
	d := newSyncDesk(t, f)
	d.unbind()
	d.file("runbook.md", "MY TEXT\n")
	d.ok("n\n")
	if tree := syncTree(d); !strings.Contains(tree, "MY TEXT") || !strings.Contains(tree, "CAROL") || len(f.pushes) != 0 {
		t.Fatalf("%s", tree)
	}
}

// Files linked to someone else keep a folder from being taken, whatever else is in it.
func TestContextSyncLinkedFolderNotTaken(t *testing.T) {
	f := newSyncFleet()
	d := newSyncDesk(t, f)
	d.file("mine.md", "ORG A\n")
	d.ok("\n")
	d.unbind()
	d.file("stray.md", "ORG A STRAY\n")
	b := otherOrg()
	d.env.Provider = b
	d.ok("\n")
	if len(b.pushes) != 0 {
		t.Fatalf("%+v", b.pushes)
	}
}
