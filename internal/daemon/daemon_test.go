package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// fakeProvider is an "official" provider with controllable session and
// counted enrolls. Unimplemented methods fall through to the stub.
type fakeProvider struct {
	fleetapi.Provider
	signedIn  atomic.Bool
	enrolls   atomic.Int32
	enrollErr error
}

func newFake(signedIn bool) *fakeProvider {
	p := &fakeProvider{Provider: stub.New()}
	p.signedIn.Store(signedIn)
	return p
}

func (p *fakeProvider) Flavor() fleetapi.Flavor     { return fleetapi.FlavorOfficial }
func (p *fakeProvider) Auth() fleetapi.Auth         { return fakeAuth{p} }
func (p *fakeProvider) Enroller() fleetapi.Enroller { return fakeEnroller{p} }
func (p *fakeProvider) Me() fleetapi.Me             { return fakeMe{} }

type fakeAuth struct{ p *fakeProvider }

func (a fakeAuth) Login(context.Context, fleetapi.LoginRequest) (fleetapi.Session, error) {
	a.p.signedIn.Store(true)
	return fleetapi.Session{Env: fleetapi.EnvDev}, nil
}
func (a fakeAuth) Logout(context.Context) error { a.p.signedIn.Store(false); return nil }
func (a fakeAuth) Session(context.Context) (fleetapi.Session, error) {
	if a.p.signedIn.Load() {
		return fleetapi.Session{Env: fleetapi.EnvDev, UserID: "u", OrgID: "o"}, nil
	}
	return fleetapi.Session{}, fleetapi.ErrNotSignedIn
}
func (a fakeAuth) TokenStore() fleetapi.TokenStore { return nil }

type fakeEnroller struct{ p *fakeProvider }

func (e fakeEnroller) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	e.p.enrolls.Add(1)
	if e.p.enrollErr != nil {
		return fleetapi.EnrollResult{}, e.p.enrollErr
	}
	if !e.p.signedIn.Load() {
		return fleetapi.EnrollResult{}, fleetapi.ErrNotSignedIn
	}
	return fleetapi.EnrollResult{NodeID: b.NodeID, Tier: "team"}, nil
}

type fakeMe struct{}

func (fakeMe) Get(context.Context) (fleetapi.MeInfo, error) {
	return fleetapi.MeInfo{UserID: "u", OrgID: "o", Tier: "team"}, nil
}
func (fakeMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{"context_sync": true}}, nil
}

func testOpts(t *testing.T, p fleetapi.Provider, dir string) Options {
	return Options{
		Provider: p, Env: fleetapi.EnvDev, StateDir: dir, Version: "test",
		Logger: logx.Discard(), Ports: []int{0},
		Host: enroll.Host{GOOS: "linux", GOARCH: "amd64"},
	}
}

func startDaemon(t *testing.T, opt Options) (*Daemon, context.CancelFunc, <-chan error) {
	t.Helper()
	d := New(opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { _, err := Connect(context.Background(), opt.StateDir); return err == nil })
	return d, cancel, done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// concurrent starters yield exactly one daemon and one enroll.
func TestSingletonRace(t *testing.T) {
	dir := t.TempDir()
	p := newFake(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var spawned atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	spawn := func() error {
		spawned.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- New(testOpts(t, p, dir)).Run(ctx)
		}()
		return nil
	}
	var cwg sync.WaitGroup
	clients := make([]*Client, 5)
	for i := range clients {
		cwg.Add(1)
		go func(i int) {
			defer cwg.Done()
			c, err := EnsureRunning(ctx, dir, spawn, 3*time.Second)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			clients[i] = c
		}(i)
	}
	cwg.Wait()
	for _, c := range clients[1:] {
		if c != nil && clients[0] != nil && c.Info.Nonce != clients[0].Info.Nonce {
			t.Fatal("clients connected to different daemons")
		}
	}
	waitFor(t, func() bool { return p.enrolls.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := p.enrolls.Load(); n != 1 {
		t.Fatalf("enroll calls = %d, want 1", n)
	}
	losers := 0
	for i := 0; i < int(spawned.Load())-1; i++ {
		if err := <-errs; errors.Is(err, ErrAlreadyRunning) {
			losers++
		}
	}
	if losers != int(spawned.Load())-1 {
		t.Fatalf("losers = %d of %d spawns", losers, spawned.Load())
	}
	cancel()
	wg.Wait()
}

// idle exit after 15 min with no client.
func TestIdle(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	opt := testOpts(t, newFake(true), dir)
	opt.Clock = clk
	d := New(opt)
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	waitFor(t, func() bool { _, err := Connect(context.Background(), dir); return err == nil })
	c, _ := Connect(context.Background(), dir)
	c.Ping(context.Background(), "mcp-1")

	// A live client keeps the daemon up past the idle timeout.
	for i := 0; i < 2; i++ {
		clk.Advance(time.Minute)
		c.Ping(context.Background(), "mcp-1")
	}
	c.Bye(context.Background(), "mcp-1")
	for i := 0; i < 14; i++ {
		clk.Advance(time.Minute)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("exited before 15 idle minutes")
	default:
	}
	for i := 0; i < 3; i++ {
		clk.Advance(time.Minute)
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		c.Shutdown(context.Background(), false)
		<-done
		t.Fatal("daemon did not idle-exit")
	}
}

// a foreign listener on the preferred port is skipped, and a stale
// daemon.json pointing at it is not trusted.
func TestForeignPort(t *testing.T) {
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	fport := foreign.Addr().(*net.TCPAddr).Port
	go http.Serve(foreign, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"collector","proof":"deadbeef"}`))
	}))
	dir := t.TempDir()
	WriteInfo(dir, Info{Port: fport, Nonce: "stale-nonce"})
	if _, err := Connect(context.Background(), dir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("foreign listener trusted: %v", err)
	}
	opt := testOpts(t, newFake(true), dir)
	opt.Ports = []int{fport, 0}
	d, _, _ := startDaemon(t, opt)
	if got := d.Info().Port; got == fport || got == 0 {
		t.Fatalf("bound port %d", got)
	}
}

func TestControlAuthAndSessionGate(t *testing.T) {
	dir := t.TempDir()
	p := newFake(false)
	opt := testOpts(t, p, dir)
	d := New(opt)
	d.Handle("POST /v1/logs", d.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { _, err := Connect(context.Background(), dir); return err == nil })
	base := "http://127.0.0.1:" + strconv.Itoa(d.Info().Port)

	resp, _ := http.Get(base + "/control/status")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
	resp, _ = http.Post(base+"/v1/logs", "application/x-protobuf", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("signed-out ingest = %d, want 503", resp.StatusCode)
	}
	p.signedIn.Store(true)
	resp, _ = http.Post(base+"/v1/logs", "application/x-protobuf", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed-in ingest = %d", resp.StatusCode)
	}
	c, _ := Connect(context.Background(), dir)
	c.Kick(context.Background())
	waitFor(t, func() bool { s, _ := c.Status(context.Background()); return s.Enroll == string(enroll.StateEnrolled) })
	s, err := c.Status(context.Background())
	if err != nil || s.Session != SessionSignedIn || s.Tier != "team" || !s.Daemon.Running {
		t.Fatalf("status = %+v err=%v", s, err)
	}
}

type fakeLane struct {
	mu                       sync.Mutex
	started, stopped, purged bool
}

func (l *fakeLane) Name() string { return "telemetry" }
func (l *fakeLane) Start(context.Context) error {
	l.mu.Lock()
	l.started = true
	l.mu.Unlock()
	return nil
}
func (l *fakeLane) Stop() error        { l.mu.Lock(); l.stopped = true; l.mu.Unlock(); return nil }
func (l *fakeLane) Hold(string)        {}
func (l *fakeLane) Resume()            {}
func (l *fakeLane) Purge() error       { l.mu.Lock(); l.purged = true; l.mu.Unlock(); return nil }
func (l *fakeLane) Status() LaneStatus { return LaneStatus{Name: "telemetry", State: "running"} }

func TestShutdownPurge(t *testing.T) {
	dir := t.TempDir()
	key, _ := spool.GetOrCreateKey(&spool.Memory{}, "dev")
	sp, _ := spool.Open(dir+"/spool", key, spool.Options{})
	sp.Append("telemetry", []byte("x"))
	lane := &fakeLane{}
	opt := testOpts(t, newFake(true), dir)
	opt.Spool, opt.Lanes = sp, []Lane{lane}
	d := New(opt)
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	waitFor(t, func() bool { _, err := Connect(context.Background(), dir); return err == nil })
	c, _ := Connect(context.Background(), dir)
	if s, _ := c.Status(context.Background()); len(s.Lanes) != 1 || s.Spool == nil || s.Spool.Records != 1 {
		t.Fatalf("status lanes/spool = %+v", s)
	}
	if err := c.Shutdown(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if !lane.started || !lane.stopped || !lane.purged || !sp.Empty() {
		t.Fatalf("lane=%+v spoolEmpty=%v", lane, sp.Empty())
	}
	if _, err := ReadInfo(dir); !errors.Is(err, ErrNotRunning) {
		t.Fatal("daemon.json not removed")
	}
}

func TestNodeRemovedPurges(t *testing.T) {
	dir := t.TempDir()
	p := newFake(true)
	p.enrollErr = &fleetapi.APIError{Status: 410, Code: fleetapi.CodeNodeRemoved}
	lane := &fakeLane{}
	opt := testOpts(t, p, dir)
	opt.Lanes = []Lane{lane}
	d, _, _ := startDaemon(t, opt)
	waitFor(t, func() bool { return d.Status(context.Background()).Session == SessionRemoved })
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if !lane.purged {
		t.Fatal("lane not purged on removal")
	}
}

// Plan required: status carries the approved plan-required text (owned
// by the plugin, not Fleet's message) and lanes are not purged.
func TestPlanRequiredRefusalInStatus(t *testing.T) {
	for _, code := range []string{fleetapi.CodeCapabilityNotInTier, fleetapi.CodeLegacyEarlyAccessRequired} {
		t.Run(code, func(t *testing.T) { testPlanRequiredRefusalInStatus(t, code) })
	}
}

func testPlanRequiredRefusalInStatus(t *testing.T, code string) {
	dir := t.TempDir()
	p := newFake(true)
	p.enrollErr = &fleetapi.APIError{Status: 403, Code: code, Message: "server text", Details: map[string]string{"capability": fleetapi.PluginCapability}}
	lane := &fakeLane{}
	opt := testOpts(t, p, dir)
	opt.Lanes = []Lane{lane}
	d, _, _ := startDaemon(t, opt)
	waitFor(t, func() bool { return d.Status(context.Background()).Enroll == "plan_required" })
	s := d.Status(context.Background())
	found := false
	for _, n := range s.Notes {
		if n == "Kenaz for Claude Code and Codex needs a Kameas Fleet plan that includes it. Ask your organization's Kameas Fleet administrator." {
			found = true
		}
		if strings.Contains(strings.ToLower(n), "early access") {
			t.Fatalf("early-access text in status: %q", n)
		}
		if n == "server text" {
			t.Fatal("status must not show Fleet's message")
		}
	}
	if !found {
		t.Fatalf("notes = %v", s.Notes)
	}
	if s.Session == SessionRemoved {
		t.Fatal("plan required is not a node removal")
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.purged {
		t.Fatal("lanes must not be purged on a plan-required refusal")
	}
}

// Every build uses the same state directory for a Fleet environment, so a
// relay another build runs there is refused by Connect (with its name) and
// never replaced by EnsureRunning; ConnectAny still reaches it (to stop
// it).
func TestOtherBuildRelayRefused(t *testing.T) {
	dir := t.TempDir()
	startDaemon(t, testOpts(t, newFake(true), dir))
	in, err := ReadInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if in.Build() != variant.Current.Name || (variant.Current.Name == variant.Kenaz.Name) != (in.Variant == "") {
		t.Fatalf("daemon.json variant %q", in.Variant)
	}
	other := variant.KenazDev.Name
	if other == variant.Current.Name {
		other = variant.Kenaz.Name
	}
	in.Variant = other
	if other == variant.Kenaz.Name {
		in.Variant = "" // how kenaz records itself
	}
	if err := WriteInfo(dir, in); err != nil {
		t.Fatal(err)
	}
	var ob *OtherBuildError
	if _, err := Connect(context.Background(), dir); !errors.As(err, &ob) || ob.Variant != other || !strings.Contains(err.Error(), other+" is using this Fleet environment") {
		t.Fatalf("Connect: %v", err)
	}
	spawned := false
	if _, err := EnsureRunning(context.Background(), dir, func() error { spawned = true; return nil }, time.Second); !errors.As(err, &ob) || spawned {
		t.Fatalf("EnsureRunning: %v spawned=%v", err, spawned)
	}
	if _, err := ConnectAny(context.Background(), dir); err != nil {
		t.Fatalf("ConnectAny: %v", err)
	}
}

// reloadFake keeps the session it loaded until ReloadSession, as a
// provider that loads the stored session once per process does.
type reloadFake struct {
	*fakeProvider
	stored  atomic.Bool // what the credential store holds
	reloads atomic.Int32
	fail    atomic.Bool // ReloadSession fails (the store could not be read)
}

func (p *reloadFake) Auth() fleetapi.Auth { return reloadAuth{fakeAuth{p.fakeProvider}, p} }

type reloadAuth struct {
	fakeAuth
	r *reloadFake
}

func (a reloadAuth) ReloadSession(context.Context) error {
	a.r.reloads.Add(1)
	if a.r.fail.Load() {
		return errors.New("credential store unavailable")
	}
	a.r.signedIn.Store(a.r.stored.Load())
	return nil
}

// POST /control/session-changed needs the nonce, reloads the session and
// enrolls at once; a provider that cannot reload answers reloaded=false.
func TestSessionChangedEndpoint(t *testing.T) {
	p := &reloadFake{fakeProvider: newFake(false)}
	opt := testOpts(t, p, t.TempDir())
	opt.SessionCheckPeriod = time.Hour
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	bad := *c
	bad.Info.Nonce = "wrong"
	if _, err := bad.SessionChanged(context.Background()); err == nil {
		t.Fatal("session-changed accepted without the nonce")
	}
	if p.reloads.Load() != 0 {
		t.Fatal("reloaded for an unauthenticated caller")
	}
	p.stored.Store(true)
	if ok, err := c.SessionChanged(context.Background()); err != nil || !ok {
		t.Fatalf("SessionChanged = %v, %v", ok, err)
	}
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedIn && st.Enroll == string(enroll.StateEnrolled) && st.Tier == "team"
	})

	plain := newFake(false)
	opt2 := testOpts(t, plain, t.TempDir())
	startDaemon(t, opt2)
	c2, err := Connect(context.Background(), opt2.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c2.SessionChanged(context.Background()); err != nil || ok {
		t.Fatalf("no reload support: SessionChanged = %v, %v; want false", ok, err)
	}
}

// A reload that fails for any reason other than "unsupported" answers
// reloaded=false, so `kenaz login` restarts the relay rather than leave it
// on the old session.
func TestSessionChangedReloadErrorIsNotReloaded(t *testing.T) {
	p := &reloadFake{fakeProvider: newFake(false)}
	opt := testOpts(t, p, t.TempDir())
	opt.SessionCheckPeriod = time.Hour
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	p.stored.Store(true)
	p.fail.Store(true)
	if ok, err := c.SessionChanged(context.Background()); err != nil || ok {
		t.Fatalf("failed reload: SessionChanged = %v, %v; want false", ok, err)
	}
	if p.reloads.Load() != 1 {
		t.Fatalf("reloads = %d, want 1", p.reloads.Load())
	}
	p.fail.Store(false)
	if ok, err := c.SessionChanged(context.Background()); err != nil || !ok {
		t.Fatalf("after the store recovers: SessionChanged = %v, %v; want true", ok, err)
	}
}

// The session watch reloads on its own and acts only on a change.
func TestSessionWatchSelfHeals(t *testing.T) {
	p := &reloadFake{fakeProvider: newFake(false)}
	opt := testOpts(t, p, t.TempDir())
	opt.SessionCheckPeriod = 10 * time.Millisecond
	startDaemon(t, opt)
	c, err := Connect(context.Background(), opt.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	p.stored.Store(true)
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedIn && st.Enroll == string(enroll.StateEnrolled)
	})
	enrolls := p.enrolls.Load()
	time.Sleep(100 * time.Millisecond)
	if n := p.enrolls.Load(); n != enrolls {
		t.Fatalf("an unchanged session re-enrolled %d times", n-enrolls)
	}
	p.stored.Store(false)
	waitFor(t, func() bool {
		st, _ := c.Status(context.Background())
		return st.Session == SessionSignedOut && st.Enroll == string(enroll.StateSignedOut)
	})
}
