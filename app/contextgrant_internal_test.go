package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
)

// grantCLI drives `kenaz context grant` for one signed-in account on a device with a
// keychain.
type grantCLI struct {
	t    *testing.T
	f    *syncFleet
	env  Env
	out  *bytes.Buffer
	errb *bytes.Buffer
}

func newGrantCLI(t *testing.T) *grantCLI {
	t.Helper()
	old := features.ContextLibrary
	features.ContextLibrary = true
	t.Cleanup(func() { features.ContextLibrary = old })
	c := &grantCLI{t: t, f: newSyncFleet(), out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	home := t.TempDir()
	t.Setenv("KENAZ_HOME", home)
	c.env = devEnv(home, c.f, c.out, c.errb)
	c.env.cred = &credStore{store: credstore.Keychain}
	return c
}

func (c *grantCLI) run(terminal bool, input string, args ...string) int {
	c.t.Helper()
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = strings.NewReader(input)
	defer func() { memoryStdinTerminal, memoryStdin = oldT, oldR }()
	c.out.Reset()
	c.errb.Reset()
	return runContext(c.env, append([]string{"grant"}, args...))
}

// on reports whether the signed-in account holds the grant, as the relay reads it.
func (c *grantCLI) on() bool {
	c.t.Helper()
	r, err := c.env.resolve("")
	if err != nil {
		c.t.Fatal(err)
	}
	g, acct := c.env.accountShareGrant(context.Background(), r, false)
	if acct == nil {
		return false
	}
	_, on := g.On(*acct)
	return on
}

// The grant is off by default, and only a person at a terminal who types y turns it
// on. Enter alone, any other answer, and a call with no terminal change nothing.
func TestContextGrantNeedsAPersonAtATerminal(t *testing.T) {
	c := newGrantCLI(t)
	if c.on() {
		t.Fatal("on by default")
	}
	if code := c.run(true, "", "status"); code != ExitOK || !strings.Contains(c.out.String(), "Off:") {
		t.Fatalf("%d %s %s", code, c.out, c.errb)
	}
	// No terminal: refused before anything is shown or asked, whatever is piped in or
	// set in the environment.
	t.Setenv("KENAZ_CONTEXT_GRANT", "on")
	t.Setenv("KENAZ_CONTEXT_SHARE_GRANT", "1")
	for _, in := range []string{"y\n", "yes\n", ""} {
		if code := c.run(false, in, "on"); code != ExitError || !strings.Contains(c.errb.String(), "this needs you at a terminal") || c.out.Len() != 0 || c.on() {
			t.Fatalf("no terminal, input %q: %d %s %s", in, code, c.out, c.errb)
		}
	}
	for _, in := range []string{"\n", "n\n", "", "sure\n"} {
		if code := c.run(true, in, "on"); code != ExitOK || !strings.Contains(c.out.String(), usertext.NothingChanged) || c.on() {
			t.Fatalf("answer %q: %d %s %s", in, code, c.out, c.errb)
		}
	}
	if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
		t.Fatalf("%d %s %s", code, c.out, c.errb)
	}
	out := c.out.String()
	for _, want := range []string{"without asking you to approve each one", "You won't see a file's text before your team can read it", "30 files per clock hour (the count starts again on\n    the hour)", "marked as drafted from your memory", "can't tell memory-derived text", "including the all-members team", "whole organization can read", "context grant off", "[y/N]", contextGrantOn} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %s", want, out)
		}
	}
	for _, bad := range []string{"I agree", "Accept", "accept", "Consent", "consent", "legal", "approved by"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q in %s", bad, out)
		}
	}
	if code := c.run(false, "", "status"); code != ExitOK || !strings.Contains(c.out.String(), "On since") || !strings.Contains(c.out.String(), "Used this hour: 0 of 30") {
		t.Fatalf("%d %s", code, c.out)
	}
	// Turning it off needs no terminal: it only reduces what leaves the device.
	if code := c.run(false, "", "off"); code != ExitOK || c.on() || !strings.Contains(c.out.String(), contextGrantOff) {
		t.Fatalf("%d %s %s", code, c.out, c.errb)
	}
	for _, args := range [][]string{{}, {"enable"}, {"on", "--yes"}, {"on", "-y"}} {
		if code := c.run(true, "y\n", args...); code != ExitUsage || c.on() {
			t.Fatalf("%v: %d", args, code)
		}
	}
}

// It can't be turned on where the grant couldn't be kept safely or isn't allowed:
// with no keychain, signed in to an unofficial build, or when the organization's
// switch says no. That switch also turns off a grant already given.
func TestContextGrantRefusals(t *testing.T) {
	c := newGrantCLI(t)
	c.env.cred = &credStore{store: credstore.Files}
	if code := c.run(true, "y\n", "on"); code != ExitError || !strings.Contains(c.errb.String(), "keychain") || c.on() {
		t.Fatalf("no keychain: %d %s %s", code, c.out, c.errb)
	}
	c.env.cred = &credStore{store: credstore.Keychain}
	if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
		t.Fatalf("%d %s", code, c.errb)
	}
	old := contexttools.OrgAllowsShareGrant
	contexttools.OrgAllowsShareGrant = func(context.Context, fleetapi.Provider) bool { return false }
	t.Cleanup(func() { contexttools.OrgAllowsShareGrant = old })
	if code := c.run(true, "", "status"); code != ExitOK || !strings.Contains(c.out.String(), "Off: your organization doesn't allow") {
		t.Fatalf("%d %s", code, c.out)
	}
	r, _ := c.env.resolve("")
	g, acct := c.env.accountShareGrant(context.Background(), r, false)
	if st := contextShareGrantStatus(context.Background(), g, c.f, *acct); st.On {
		t.Fatal("status shows a forbidden grant as on")
	}
	c.run(false, "", "off")
	if code := c.run(true, "y\n", "on"); code != ExitError || !strings.Contains(c.errb.String(), "organization doesn't allow") || c.on() {
		t.Fatalf("forbidden: %d %s %s", code, c.out, c.errb)
	}
}

// Signing out ends the grant, and so does another account signing in: it doesn't come
// back when the first account returns.
func TestContextGrantEndsWithTheSession(t *testing.T) {
	c := newGrantCLI(t)
	if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
		t.Fatalf("%d %s", code, c.errb)
	}
	c.f.me = "bob"
	if code := c.run(false, "", "status"); code != ExitOK || !strings.Contains(c.out.String(), "Off:") || c.on() {
		t.Fatalf("another account: %d %s", code, c.out)
	}
	c.f.me = "alice"
	if c.on() {
		t.Fatal("the grant came back with the first account")
	}
	if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
		t.Fatalf("%d %s", code, c.errb)
	}
	c.f.org = "org-2"
	if c.on() {
		t.Fatal("the grant carried to another organization")
	}
	c.f.org = "org-1"
	if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
		t.Fatalf("%d %s", code, c.errb)
	}
	// The history holds file and team names: it goes at logout too.
	hr, _ := c.env.resolve("")
	hg, hacct := c.env.accountShareGrant(context.Background(), hr, false)
	if err := hg.Record(contextlib.ShareAudit{Account: *hacct, Slug: "a.md", TeamName: "Platform", At: c.env.now()}); err != nil {
		t.Fatal(err)
	}
	c.out.Reset()
	c.errb.Reset()
	runLogout(c.env, nil)
	if c.on() {
		t.Fatalf("still on after logout: %s %s", c.out, c.errb)
	}
	r, _ := c.env.resolve("")
	for _, name := range []string{"grant.json", "history.json"} {
		if _, err := os.Stat(filepath.Join(contextShareGrantDir(r.StateDir), name)); !os.IsNotExist(err) {
			t.Fatalf("%s outlived logout: %v", name, err)
		}
	}
}

// `kenaz status` says whether it is on.
func TestStatusShowsContextGrant(t *testing.T) {
	c := newGrantCLI(t)
	if code := runStatus(c.env, nil); code != ExitOK || !strings.Contains(c.out.String(), "context share grant: off") {
		t.Fatalf("%d %s %s", code, c.out, c.errb)
	}
	c.run(true, "y\n", "on")
	c.out.Reset()
	if code := runStatus(c.env, nil); code != ExitOK || !strings.Contains(c.out.String(), "context share grant: on") || !strings.Contains(c.out.String(), "without asking, up to 30 per clock hour (turn off: ") || !strings.Contains(c.out.String(), "context grant off") {
		t.Fatalf("%d %s %s", code, c.out, c.errb)
	}
	c.out.Reset()
	runStatus(c.env, []string{"--json"})
	var js struct {
		Grant *daemon.ContextShareGrant `json:"context_share_grant"`
	}
	if err := json.Unmarshal(c.out.Bytes(), &js); err != nil || js.Grant == nil || !js.Grant.On || js.Grant.HourlyCap != contextlib.MaxGrantedSharesPerHour {
		t.Fatalf("%v %s", err, c.out)
	}
}

// The relay's wiring: the grant check is registered for file shares only, the status
// carries the grant, and a purge (sign-out, node removal) ends it.
func TestRelayContextGrantWiring(t *testing.T) {
	c := newGrantCLI(t)
	key := bytes.Repeat([]byte{5}, 32)
	opt := &daemon.Options{Provider: c.f, Env: fleetapi.EnvDev, StateDir: filepath.Join(t.TempDir(), "dev"), Logger: logx.Discard()}
	g := c.env.shareGrant(opt.StateDir, opt.Env, true)
	m, err := approvals.New(approvals.Options{Dir: approvalsDir(opt.StateDir), StoreKey: key, Identity: sessionIdentity(c.f)})
	if err != nil {
		t.Fatal(err)
	}
	register, purge := installContextShareGrant(opt, m, g)
	registerFleetWriteExecutors(m, c.f, opt.StateDir, nil)
	registerContextFileExecutors(m, c.f, opt.StateDir, nil)
	register()
	acct := contextlib.Account{Env: "dev", OrgID: c.f.org, UserID: c.f.me}
	if err := g.Give(acct, c.env.now()); err != nil {
		t.Fatal(err)
	}
	var st daemon.Status
	for _, cb := range opt.Contributors {
		cb.ContributeStatus(&st)
	}
	if st.ContextShareGrant == nil || !st.ContextShareGrant.On {
		t.Fatalf("%+v", st.ContextShareGrant)
	}
	// A file share the tool reserved runs; a shared note doesn't.
	led := contextlib.NewLedger(contextLedgerIn(filepath.Dir(opt.StateDir)))
	node, err := contexttools.BuildFileNode(contexttools.ShareInput{Title: "a", Body: "one\n", Kind: "doc"}, "a.md", "team-1", "0192e4a0-0000-7000-8000-000000000001", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Update("k1", func(r *contextlib.FileRecord) {
		r.Path = "personal/a.md"
		r.Reserve(contextlib.Reservation{Account: acct, NodeID: node.ID, TeamID: node.TeamID})
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"node": node, "path": "personal/a.md", "ledger_key": "k1"})
	prep := func(op string, p []byte) approvals.Status {
		_, s, err := m.Prepare(context.Background(), approvals.Request{Operation: op, Env: "dev", OrgID: c.f.org, UserID: c.f.me, Payload: p, IdempotencyKey: op + ":1"})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return s
	}
	if s := prep(contexttools.ShareFileOperation, payload); s != approvals.StatusDone || len(c.f.pushes) != 1 || c.f.pushes[0].Body != "one\n" {
		t.Fatalf("file share: %s, %d pushes", s, len(c.f.pushes))
	}
	if h := g.History(acct); len(h) != 1 || h[0].Slug != "a.md" || h[0].TeamName != "Platform" || h[0].Status != "done" {
		t.Fatalf("%+v", h)
	}
	shared, err := contexttools.BuildShareNode(contexttools.ShareInput{Title: "T", Body: "note", Kind: "explanation"}, "team-1", c.env.now(), bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	note, _ := json.Marshal(shared)
	if s := prep(contexttools.ShareOperation, note); s != approvals.StatusPending || len(c.f.pushes) != 1 {
		t.Fatalf("shared note: %s, %d pushes", s, len(c.f.pushes))
	}
	purge()
	if _, on := g.On(acct); on || len(g.History(acct)) != 0 {
		t.Fatal("the grant or its history outlived a purge")
	}
}

// The relay's own purge (sign-out, node removal) ends the grant: through the hook the
// relay installs and the lane it purges, not a function called by hand.
func TestRelayPurgeEndsContextGrant(t *testing.T) {
	c := newGrantCLI(t)
	opt := &daemon.Options{Provider: c.f, Env: fleetapi.EnvDev, StateDir: filepath.Join(t.TempDir(), "dev"), Logger: logx.Discard()}
	if _, err := approvalsDaemonHook(c.env)(opt); err != nil {
		t.Fatal(err)
	}
	acct := contextlib.Account{Env: "dev", OrgID: c.f.org, UserID: c.f.me}
	g := c.env.shareGrant(opt.StateDir, opt.Env, false)
	if err := g.Give(acct, c.env.now()); err != nil {
		t.Fatal(err)
	}
	g.Record(contextlib.ShareAudit{Account: acct, Slug: "a.md", At: c.env.now()})
	if _, on := g.On(acct); !on {
		t.Fatal("not on")
	}
	purged := 0
	for _, l := range opt.Lanes {
		if l.Name() == "approvals" {
			purged++
			if err := l.Purge(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, on := g.On(acct); purged != 1 || on || len(g.History(acct)) != 0 {
		t.Fatalf("purged %d lanes; the grant or its history is still there", purged)
	}
}

// Ending the grant holds against someone who kept a copy of its file: put back after
// `grant off`, after a sign-out, or after a sign-out by an older release that knows
// nothing of the grant, it is not a grant. Turning the grant on again doesn't bring
// the old file back either.
func TestContextGrantRestoredFileIsNoGrant(t *testing.T) {
	c := newGrantCLI(t)
	r, err := c.env.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(contextShareGrantDir(r.StateDir), "grant.json")
	turnOn := func() []byte {
		t.Helper()
		if code := c.run(true, "y\n", "on"); code != ExitOK || !c.on() {
			t.Fatalf("%d %s", code, c.errb)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	restore := func(b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	saved := turnOn()
	c.run(false, "", "off")
	restore(saved)
	if c.on() {
		t.Fatal("restored after off: read as a grant")
	}
	if code := c.run(false, "", "status"); code != ExitOK || !strings.Contains(c.out.String(), "Off:") {
		t.Fatalf("%s", c.out)
	}

	second := turnOn()
	restore(saved)
	if c.on() {
		t.Fatal("an earlier grant's file was read as the new grant")
	}
	restore(second)
	if !c.on() {
		t.Fatal("the grant in force stopped being one")
	}

	// This release's logout.
	runLogout(c.env, nil)
	restore(second)
	if c.on() {
		t.Fatal("restored after logout: read as a grant")
	}
	// The same account signs in again and a relay starts: still no grant.
	if _, err := spool.GetOrCreateKey(c.env.keyStore(), string(r.Env)); err != nil {
		t.Fatal(err)
	}
	if c.on() {
		t.Fatal("restored after logout and sign-in: read as a grant")
	}

	// An older release's logout: it deletes the session's spool key, as every release
	// has, and leaves the grant's file and its keychain id where they are.
	third := turnOn()
	if err := c.env.keyStore().Delete(string(r.Env)); err != nil {
		t.Fatal(err)
	}
	if c.on() {
		t.Fatal("the grant outlived an older release's logout")
	}
	if _, err := spool.GetOrCreateKey(c.env.keyStore(), string(r.Env)); err != nil {
		t.Fatal(err)
	}
	restore(third)
	if c.on() {
		t.Fatal("the grant came back at the next sign-in after an older release's logout")
	}
}

// Nothing an agent can reach gives the grant: no MCP tool is named for it or takes
// an input for it, no control route mentions it, and the one call that records a
// grant is in the terminal command.
func TestNoToolOrControlRouteGivesTheGrant(t *testing.T) {
	c := newGrantCLI(t)
	// Every tool `kenaz mcp` serves to a signed-in member whose Fleet offers context files.
	var names []string
	c.env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		for tool, err := range cs.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			names = append(names, tool.Name)
			schema, _ := json.Marshal(tool.InputSchema)
			if strings.Contains(strings.ToLower(tool.Name), "grant") || strings.Contains(strings.ToLower(string(schema)), "grant") {
				t.Errorf("tool %s can be asked about a grant: %s", tool.Name, schema)
			}
		}
		return nil
	}
	if code := runMCP(c.env, nil); code != ExitOK {
		t.Fatalf("mcp: %d %s", code, c.errb)
	}
	served := strings.Join(names, " ")
	for _, want := range []string{"context_share_file", "context_share", "context_promote", "skills_publish", "memory_save", "fleet_status", "approval_status"} {
		if !strings.Contains(" "+served+" ", " "+want+" ") {
			t.Fatalf("%s isn't served, so this test saw too little: %s", want, served)
		}
	}

	// Every control route in the program, by its path as written in the source.
	known := map[string]bool{
		"/control/status": true, "/control/clients/": true, "/control/clients/{id}": true, "/control/kick": true,
		"/control/shutdown": true, "/control/session-changed": true, "/control/session-end": true,
		"/control/approvals": true, "/control/approvals/": true, "/control/approvals/prepare": true,
	}
	routeRe := regexp.MustCompile(`"(?:[A-Z]+ )?(/control/[^"\s?]*)`)
	gives := map[string]int{}
	root := ".."
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range routeRe.FindAllStringSubmatch(string(b), -1) {
			if !known[m[1]] {
				t.Errorf("%s: control route %s isn't on this test's list; check it can't give or use the grant, then add it", p, m[1])
			}
		}
		if n := strings.Count(string(b), ".Give("); n > 0 {
			rel, _ := filepath.Rel(root, p)
			gives[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gives) != 1 || gives["app/contextgrant.go"] != 1 {
		t.Fatalf("the grant is given from: %v; only `kenaz context grant on` may", gives)
	}
}
