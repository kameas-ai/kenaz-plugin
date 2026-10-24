package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// verifying is the official fake whose Auth can also run a browser
// identity verification (what enrollment needs).
type verifying struct{ *official }

func (p verifying) Auth() fleetapi.Auth {
	return verifyingAuth{auth{p.official}, &fleetapitest.IdentityVerifier{}}
}

type verifyingAuth struct {
	auth
	*fleetapitest.IdentityVerifier
}

// The CLI can never approve, cancel, enroll or export: every such verb is
// refused before anything is contacted.
func TestApprovalsCLIRefusesApprove(t *testing.T) {
	h := newHarness(t, stub.New())
	for _, verb := range []string{"approve", "APPROVE", "accept", "confirm", "deny", "cancel", "register", "export", "receipt", "token"} {
		code, out, errb := h.run(t, "approvals", verb, "apr_aaaaaaaaaaaaaaaaaaaaaaaaaa")
		if code != app.ExitUsage || out != "" || !strings.Contains(errb, "cannot approve") {
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
	h := newHarness(t, verifying{p})
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
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "skills.publish") || !strings.Contains(out, "pending") || !strings.Contains(out, "approvals enroll") {
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

// Without an enrolled authenticator and without a way to enroll one, a
// write tool reports approval_unavailable and stores nothing.
func TestApprovalsUnavailableWithoutVerifier(t *testing.T) {
	p := &official{Provider: stub.New()}
	h := newHarness(t, p) // Auth has no VerifyIdentity
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
	if got["status"] != "approval_unavailable" || got["request_id"] != nil {
		t.Fatalf("got %v", got)
	}
	cancel()
	<-ddone
}
