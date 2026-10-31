package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

const (
	canaryToken = "CANARY-TOKEN-5c1e"
	canaryEmail = "canary.member@example.com"
)

// official is a fake official provider; unimplemented methods hit the stub.
type official struct {
	fleetapi.Provider
	signedIn atomic.Bool
	// refuseCode, when set, is the code Fleet refuses enroll with because
	// the org's plan does not include the plugin.
	refuseCode atomic.Value
	mu         sync.Mutex
	bodies     []string
}

func (p *official) Flavor() fleetapi.Flavor     { return fleetapi.FlavorOfficial }
func (p *official) Auth() fleetapi.Auth         { return auth{p} }
func (p *official) Enroller() fleetapi.Enroller { return enr{p} }
func (p *official) Me() fleetapi.Me             { return me{} }

type auth struct{ p *official }

func (a auth) Login(_ context.Context, r fleetapi.LoginRequest) (fleetapi.Session, error) {
	r.OpenURL("https://login.example/authorize?token_hint=" + canaryToken)
	a.p.signedIn.Store(true)
	return fleetapi.Session{Env: r.Env, UserID: canaryEmail}, nil
}
func (a auth) Logout(context.Context) error { a.p.signedIn.Store(false); return nil }
func (a auth) Session(context.Context) (fleetapi.Session, error) {
	if a.p.signedIn.Load() {
		return fleetapi.Session{Env: fleetapi.EnvDev, UserID: canaryEmail}, nil
	}
	return fleetapi.Session{}, fleetapi.ErrNotSignedIn
}
func (a auth) TokenStore() fleetapi.TokenStore { return nil }

// ReloadSession: Session always reads the shared state, so there is
// nothing to reload.
func (a auth) ReloadSession(context.Context) error { return nil }

type enr struct{ p *official }

func (e enr) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	raw, _ := json.Marshal(b)
	e.p.mu.Lock()
	e.p.bodies = append(e.p.bodies, string(raw))
	e.p.mu.Unlock()
	if !e.p.signedIn.Load() {
		return fleetapi.EnrollResult{}, fleetapi.ErrNotSignedIn
	}
	if c, _ := e.p.refuseCode.Load().(string); c != "" {
		return fleetapi.EnrollResult{}, &fleetapi.APIError{Status: 403, Code: c, Message: "fleet text", Details: map[string]string{"capability": fleetapi.PluginCapability}}
	}
	return fleetapi.EnrollResult{NodeID: b.NodeID, Tier: "team"}, nil
}

type me struct{}

func (me) Get(context.Context) (fleetapi.MeInfo, error) {
	return fleetapi.MeInfo{Tier: "team", UserID: canaryEmail}, nil
}
func (me) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{"context_sync": true}}, nil
}

type harness struct {
	home string
	ks   *spool.Memory
	p    fleetapi.Provider
}

func newHarness(t *testing.T, p fleetapi.Provider) *harness {
	return &harness{home: t.TempDir(), ks: &spool.Memory{}, p: p}
}

func (h *harness) env(ctx context.Context, out, errb *bytes.Buffer) app.Env {
	return app.Env{
		Provider: h.p, Stdout: out, Stderr: errb, Ctx: ctx,
		Getenv: func(k string) string {
			return map[string]string{variant.Shared.HomeVar: h.home, variant.Shared.EnvVar: "dev"}[k]
		},
		KeyStore:    h.ks,
		OpenBrowser: func(string) error { return nil },
		Spawn:       func(string, fleetapi.Env) error { return nil },
	}
}

func (h *harness) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := app.RunEnv(h.env(context.Background(), &out, &errb), args)
	return code, out.String(), errb.String()
}

func (h *harness) state() string { return filepath.Join(h.home, "plugin", "dev") }

func TestStatusJSONUnofficial(t *testing.T) {
	h := newHarness(t, stub.New())
	code, out, _ := h.run(t, "status", "--json")
	var st daemon.Status
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if st.SchemaVersion != 1 || st.Session != daemon.SessionUnofficial || st.Env != "dev" || st.Daemon.Running {
		t.Fatalf("status = %+v", st)
	}
	if !regexp.MustCompile(`^(macOS|Linux|Windows) device [0-9a-f]{4}$`).MatchString(st.DeviceName) {
		t.Fatalf("device name %q", st.DeviceName)
	}
}

// An unofficial build's doctor reports the stub flavor first.
func TestDoctorFlavorFirst(t *testing.T) {
	h := newHarness(t, stub.New())
	code, out, _ := h.run(t, "doctor")
	if code != 0 || !strings.HasPrefix(out, "build flavor: unofficial (stub)\n") {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	h = newHarness(t, &official{Provider: stub.New()})
	app.BuildCommit, app.PrivateModuleVersion = "abc1234", "v0.3.0"
	defer func() { app.BuildCommit, app.PrivateModuleVersion = "", "" }()
	_, out, _ = h.run(t, "doctor")
	if !strings.HasPrefix(out, "build flavor: official abc1234+v0.3.0\n") {
		t.Fatalf("official doctor: %s", out)
	}
}

func TestDeviceCommands(t *testing.T) {
	h := newHarness(t, stub.New())
	if code, out, _ := h.run(t, "device"); code != 0 || !strings.Contains(out, " device ") {
		t.Fatalf("show: %d %s", code, out)
	}
	if code, out, _ := h.run(t, "device", "rename", "Work laptop"); code != 0 || !strings.Contains(out, "Work laptop") {
		t.Fatalf("rename: %d %s", code, out)
	}
	host, _ := os.Hostname()
	if code, out, _ := h.run(t, "device", "rename", "--use-computer-name"); code != 0 || (host != "" && !strings.Contains(out, host[:min(len(host), 10)])) {
		t.Fatalf("use-computer-name: %d %s", code, out)
	}
	if code, out, _ := h.run(t, "device", "reset"); code != 0 || !strings.Contains(out, " device ") {
		t.Fatalf("reset: %d %s", code, out)
	}
	if code, _, _ := h.run(t, "device", "rename"); code != app.ExitUsage {
		t.Fatalf("bad rename exit %d", code)
	}
}

func TestLogoutPurgesEvenUnofficial(t *testing.T) {
	h := newHarness(t, stub.New())
	key, _ := spool.GetOrCreateKey(h.ks, "dev")
	sp, _ := spool.Open(filepath.Join(h.state(), "spool"), key, spool.Options{})
	sp.Append("telemetry", []byte("x"))
	sp.Close()
	code, out, _ := h.run(t, "logout")
	if code != app.ExitUnofficial || !strings.Contains(out, "Local data purged") {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	ents, _ := os.ReadDir(filepath.Join(h.state(), "spool"))
	if len(ents) != 0 {
		t.Fatalf("spool not empty: %v", ents)
	}
	if _, err := h.ks.Get("dev"); err != spool.ErrNoKey {
		t.Fatal("spool key not deleted")
	}
}

// End to end: login → daemon enrolls → MCP fleet_status over the daemon
// link → logout stops and purges. Also: no canary in any log.
func TestDaemonMCPEndToEnd(t *testing.T) {
	p := &official{Provider: stub.New()}
	h := newHarness(t, p)
	if code, out, errb := h.run(t, "login"); code != 0 || !strings.Contains(out, "Signed in") {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dout, derr bytes.Buffer
	ddone := make(chan int, 1)
	go func() { ddone <- app.RunEnv(h.env(ctx, &dout, &derr), []string{"daemon"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, err := daemon.Connect(ctx, h.state()); err == nil {
			if st, _ := c.Status(ctx); st.Enroll == "enrolled" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never enrolled; stderr=%s", derr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

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
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fleet_status"})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, &got)
	}
	if code := app.RunEnv(menv, []string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit=%d err=%s", code, merr.String())
	}
	if mout.Len() != 0 {
		t.Fatalf("mcp wrote to stdout outside the protocol: %q", mout.String())
	}
	if got["session"] != daemon.SessionSignedIn || got["enroll"] != "enrolled" || got["tier"] != "team" {
		t.Fatalf("fleet_status = %v", got)
	}
	// fleet_status says whether reporting works
	// (sign-in, consent, queue, receiver port) and per-host setup state.
	rep, _ := got["reporting"].(map[string]any)
	if rep == nil || rep["signed_in"] != true || rep["state"] == "" || rep["receiver_port"] == nil || rep["queued"] == nil {
		t.Fatalf("fleet_status reporting = %v", got["reporting"])
	}
	if hosts, _ := got["hosts"].([]any); len(hosts) < 2 {
		t.Fatalf("fleet_status hosts = %v", got["hosts"])
	}
	if got["dashboard_url"] != "https://dev.fleet.kameas.ai/" {
		t.Fatalf("fleet_status dashboard_url = %v (env dev)", got["dashboard_url"])
	}

	if code, out, _ := h.run(t, "status"); code != 0 || !strings.Contains(out, "running on 127.0.0.1:") || !strings.Contains(out, "reporting:") || !strings.Contains(out, "receiver 127.0.0.1:") || !strings.Contains(out, "setup claude-code:") {
		t.Fatalf("status text: %s", out)
	}
	if code, out, _ := h.run(t, "doctor"); !strings.Contains(out, "reporting") || !strings.Contains(out, "setup codex") {
		t.Fatalf("doctor exit=%d: %s", code, out)
	}
	if code, _, _ := h.run(t, "logout"); code != 0 {
		t.Fatalf("logout exit=%d", code)
	}
	select {
	case code := <-ddone:
		if code != 0 {
			t.Fatalf("daemon exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop on logout")
	}

	// enroll bodies carry the display name, never the OS hostname.
	host, _ := os.Hostname()
	p.mu.Lock()
	for _, b := range p.bodies {
		if host != "" && strings.Contains(b, host) {
			t.Fatalf("hostname in enroll body: %s", b)
		}
	}
	p.mu.Unlock()

	// Neither canary appears in any log file.
	filepath.Walk(filepath.Join(h.state(), "logs"), func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, c := range []string{canaryToken, canaryEmail} {
			if bytes.Contains(b, []byte(c)) {
				t.Fatalf("%s leaked into %s", c, path)
			}
		}
		return nil
	})
}

// Plan required: an org whose plan does not include the plugin sees the
// approved text in login (non-zero exit), status and setup.
func TestPlanRequiredRefusalShownInLoginStatusSetup(t *testing.T) {
	testPlanRequiredRefusal(t, fleetapi.CodeCapabilityNotInTier)
}

// Older Fleet servers send their own refusal code for the same case. It
// shows the same plan-required text and state; no early-access text ever
// reaches the member.
func TestLegacyRefusalShownAsPlanRequired(t *testing.T) {
	testPlanRequiredRefusal(t, fleetapi.CodeLegacyEarlyAccessRequired)
}

func testPlanRequiredRefusal(t *testing.T, refuseCode string) {
	const msg = "Kenaz for Claude Code and Codex needs a Kameas Fleet plan that includes it. Ask your organization's Kameas Fleet administrator."
	const kept = "Nothing is being collected or held on this device until your organization's plan includes Kenaz for Claude Code and Codex."
	p := &official{Provider: stub.New()}
	p.refuseCode.Store(refuseCode)
	h := newHarness(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dout, derr bytes.Buffer
	ddone := make(chan int, 1)
	go func() { ddone <- app.RunEnv(h.env(ctx, &dout, &derr), []string{"daemon"}) }()
	waitDaemon := time.Now().Add(3 * time.Second)
	for {
		if _, err := daemon.Connect(ctx, h.state()); err == nil {
			break
		}
		if time.Now().After(waitDaemon) {
			t.Fatalf("daemon never started; stderr=%s", derr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	code, out, errb := h.run(t, "login")
	if code == 0 || !strings.Contains(out, "Signed in") || !strings.Contains(errb, msg) {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	if strings.Contains(errb, "fleet text") {
		t.Fatal("login must show the plugin's text, not Fleet's message")
	}
	code, out, _ = h.run(t, "status")
	if code != 0 || !strings.Contains(out, "plan_required") || !strings.Contains(out, msg) || !strings.Contains(out, kept) {
		t.Fatalf("status: exit=%d out=%s", code, out)
	}
	_, sout, serr := h.run(t, "setup", "doctor")
	if !strings.Contains(serr, msg) {
		t.Fatalf("setup stderr = %s", serr)
	}
	for _, s := range []string{errb, out, sout, serr} {
		if strings.Contains(strings.ToLower(s), "early access") || strings.Contains(s, "early_access") {
			t.Fatalf("early-access text shown: %s", s)
		}
	}

	if code, _, _ := h.run(t, "logout"); code != 0 {
		t.Fatalf("logout exit=%d", code)
	}
	select {
	case <-ddone:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop on logout")
	}
}

// The startup self-check: a plugin registration whose launcher is gone
// (brew upgrade removed the version a pre-0.6.0 setup registered) is
// reported by user-facing commands, with the fix, and never by hook or
// mcp (the hosts' hot path) or version.
func TestStartupReportsBrokenRegistration(t *testing.T) {
	h := newHarness(t, stub.New())
	os.MkdirAll(h.state(), 0o700)
	gone := filepath.Join(h.home, "opt", "homebrew", "Caskroom", variant.Current.CaskToken, "0.5.0", "kenaz_0.5.0_darwin_arm64")
	st := `{"version":1,"variant":"` + variant.Current.Name + `","claude":{"path":"x","written_sha256":"","created_container":false,"keys":{},
		"bundle":{"root":` + strconv.Quote(gone) + `,"version":"0.5.0","rendered":"r","digest":"d"}}}`
	if err := os.WriteFile(filepath.Join(h.state(), "setup-state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, errb := h.run(t, "status", "--json")
	if !strings.Contains(errb, "Claude Code points at a "+variant.Current.Binary+" that no longer exists") || !strings.Contains(errb, "`"+variant.Current.Binary+" init`") {
		t.Fatalf("status stderr: %q", errb)
	}
	for _, args := range [][]string{{"version"}, {"hook", "session-end"}} {
		if _, _, errb := h.run(t, args...); strings.Contains(errb, "no longer exists") {
			t.Fatalf("%v reported it: %q", args, errb)
		}
	}
}
