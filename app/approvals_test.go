package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// `kenaz approvals` lists and opens. These verbs point at the commands that decide, or
// are refused, before anything is contacted.
func TestApprovalsCLIRefusesApprove(t *testing.T) {
	h := newHarness(t, stub.New())
	for _, verb := range []string{"approve", "APPROVE", "accept", "confirm", "deny", "cancel", "decline", "enroll", "register", "export", "receipt", "token"} {
		code, out, errb := h.run(t, "approvals", verb, "apr_aaaaaaaaaaaaaaaaaaaaaaaaaa")
		if code != app.ExitUsage || out != "" || !strings.Contains(errb, "kenaz approve") {
			t.Errorf("%s: exit=%d out=%q err=%q", verb, code, out, errb)
		}
	}
	if code, _, _ := h.run(t, "approvals", "frobnicate"); code != app.ExitUsage {
		t.Errorf("unknown subcommand exit=%d", code)
	}
}

// End to end: skills_publish over MCP prepares a request in the daemon and
// uploads nothing; `kenaz approvals` lists it and opens the page; logout
// cancels it and purges its content.
func TestApprovalsEndToEnd(t *testing.T) {
	p := &official{Provider: stub.New()}
	h := newHarness(t, p)
	if code, _, errb := h.run(t, "login"); code != 0 {
		t.Fatalf("login: %s", errb)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dout, derr bytes.Buffer
	ddone := make(chan int, 1)
	go func() { ddone <- app.RunEnv(h.env(ctx, &dout, &derr), []string{"daemon"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := daemon.Connect(ctx, h.state()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never started: %s", derr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: e2e-skill\ndescription: E2E.\n---\nsecret-body-text\n"), 0o644)
	var got map[string]any
	var mout, merr bytes.Buffer
	menv := h.env(ctx, &mout, &merr)
	menv.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		tools, err := cs.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tl := range tools.Tools {
			n := strings.ToLower(tl.Name)
			if strings.Contains(n, "approve") || strings.Contains(n, "enroll") || strings.Contains(n, "receipt") {
				t.Errorf("MCP exposes %s", tl.Name)
			}
		}
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "skills_publish", Arguments: map[string]any{"path": dir, "version": "1.0.0"}})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, &got)
	}
	if code := app.RunEnv(menv, []string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit=%d err=%s", code, merr.String())
	}
	id, _ := got["request_id"].(string)
	if got["status"] != "pending" || !approvals.ValidID(id) || !strings.HasPrefix(got["approve_url"].(string), "http://localhost:") {
		t.Fatalf("skills_publish = %v", got)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "receipt") {
		t.Fatalf("tool output mentions a receipt: %s", raw)
	}

	code, out, errb := h.run(t, "approvals")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "skills.publish") || !strings.Contains(out, "pending") {
		t.Fatalf("list exit=%d out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "secret-body-text") {
		t.Fatal("list shows content")
	}
	var opened string
	oenv := h.env(ctx, &bytes.Buffer{}, &bytes.Buffer{})
	oenv.OpenBrowser = func(u string) error { opened = u; return nil }
	if code := app.RunEnv(oenv, []string{"approvals", "open", id}); code != 0 || !strings.HasSuffix(opened, "/approvals/r/"+id) || !strings.HasPrefix(opened, "http://localhost:") {
		t.Fatalf("open: %d %q", code, opened)
	}
	if code, out, _ := h.run(t, "approvals", "status", id); code != 0 || strings.TrimSpace(out) != "pending" {
		t.Fatalf("status: %d %s", code, out)
	}
	if code, _, _ := h.run(t, "approvals", "approve", id); code != app.ExitUsage {
		t.Fatal("approve accepted")
	}
	if code, out, _ := h.run(t, "approvals", "status", id); code != 0 || strings.TrimSpace(out) != "pending" {
		t.Fatalf("status after refused approve: %s", out)
	}

	if code, _, _ := h.run(t, "logout"); code != 0 {
		t.Fatal("logout")
	}
	select {
	case <-ddone:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
	raw, err := os.ReadFile(filepath.Join(h.state(), "approvals", approvals.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret-body-text")) || bytes.Contains(raw, []byte(`"status":"pending"`)) || !bytes.Contains(raw, []byte(`"status":"canceled"`)) {
		t.Fatalf("after logout: %s", raw)
	}
	// The daemon log never carries the payload.
	logs, _ := os.ReadFile(filepath.Join(h.state(), "logs", "relay.log"))
	if bytes.Contains(logs, []byte("secret-body-text")) {
		t.Fatal("relay.log carries the payload")
	}
}

// No setup: on a device with a keychain, a write tool's first request is ready to approve.
func TestApprovalsNeedNoSetup(t *testing.T) {
	p := &official{Provider: stub.New()}
	h := newHarness(t, p)
	if code, _, errb := h.run(t, "login"); code != 0 {
		t.Fatalf("login: %s", errb)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ddone := make(chan int, 1)
	go func() { ddone <- app.RunEnv(h.env(ctx, &bytes.Buffer{}, &bytes.Buffer{}), []string{"daemon"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := daemon.Connect(ctx, h.state()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: x\ndescription: X.\n---\nx\n"), 0o644)
	var got map[string]any
	menv := h.env(ctx, &bytes.Buffer{}, &bytes.Buffer{})
	menv.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		srv.Connect(ctx, st, nil)
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "skills_publish", Arguments: map[string]any{"path": dir, "version": "1"}})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, &got)
	}
	app.RunEnv(menv, []string{"mcp"})
	if got["status"] != "pending" || got["request_id"] == nil || !strings.Contains(fmt.Sprint(got["message"]), "kenaz approve") {
		t.Fatalf("got %v", got)
	}
	cancel()
	<-ddone
}

// teamMember is a signed-in member of one team on a plan with team sharing.
type teamMember struct{ *official }

func (teamMember) Me() fleetapi.Me { return teamMe{} }

// Auth is signed in to the organization once logged in.
func (m teamMember) Auth() fleetapi.Auth { return acctAuth{auth{m.official}} }

type teamMe struct{}

func (teamMe) Get(context.Context) (fleetapi.MeInfo, error) {
	return fleetapi.MeInfo{UserID: "u1", OrgID: "o1", Tier: "team", Teams: []fleetapi.TeamRef{{ID: "t1", Name: "Platform"}}}, nil
}
func (teamMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{"team_graph_sharing": true, "shared_team_graph": true, fleetapi.CapContextFiles: true}}, nil
}

// Sharing a context file through the real relay and MCP server: the request is
// prepared, the relay renders it for the page and nothing is uploaded.
func TestShareContextFileThroughRelay(t *testing.T) {
	contextFeature(t, true)
	p := &official{Provider: stub.New()}
	h := newHarness(t, teamMember{p})
	if code, _, errb := h.run(t, "login"); code != 0 {
		t.Fatalf("login: %s", errb)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ddone := make(chan int, 1)
	var derr bytes.Buffer
	go func() { ddone <- app.RunEnv(h.env(ctx, &bytes.Buffer{}, &derr), []string{"daemon"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := daemon.Connect(ctx, h.state()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never started: %s", derr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFile(t, filepath.Join(h.home, "contexts", "company.md"), "# Company\nsecret-body-text\n")

	var got map[string]any
	var merr bytes.Buffer
	menv := h.env(ctx, &bytes.Buffer{}, &merr)
	menv.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "context_share_file", Arguments: map[string]any{"path": "personal/company.md"}})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, &got)
	}
	if code := app.RunEnv(menv, []string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit=%d err=%s", code, merr.String())
	}
	id, _ := got["request_id"].(string)
	if got["status"] != "pending" || !approvals.ValidID(id) || got["slug"] != "company.md" {
		t.Fatalf("context_share_file = %v", got)
	}
	code, out, errb := h.run(t, "approvals")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "context.share_file") {
		t.Fatalf("list exit=%d out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "secret-body-text") {
		t.Fatal("list shows content")
	}
	if code, _, _ := h.run(t, "logout"); code != 0 {
		t.Fatal("logout")
	}
	select {
	case <-ddone:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
	logs, _ := os.ReadFile(filepath.Join(h.state(), "logs", "relay.log"))
	if bytes.Contains(logs, []byte("secret-body-text")) {
		t.Fatal("relay.log carries the file")
	}
}
