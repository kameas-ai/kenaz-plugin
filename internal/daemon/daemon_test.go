package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
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

// Early access: status carries the approved early-access text (owned
// by the plugin, not Fleet's message) and lanes are not purged.
func TestEarlyAccessRefusalInStatus(t *testing.T) {
	dir := t.TempDir()
	p := newFake(true)
	p.enrollErr = &fleetapi.APIError{Status: 403, Code: fleetapi.CodeEarlyAccessRequired, Message: "server text"}
	lane := &fakeLane{}
	opt := testOpts(t, p, dir)
	opt.Lanes = []Lane{lane}
	d, _, _ := startDaemon(t, opt)
	waitFor(t, func() bool { return d.Status(context.Background()).Enroll == "early_access_required" })
	s := d.Status(context.Background())
	found := false
	for _, n := range s.Notes {
		if n == "Kenaz for Claude Code and Codex is in early access. Contact sales@kameas.ai to join." {
			found = true
		}
		if n == "server text" {
			t.Fatal("status must not show Fleet's message")
		}
	}
	if !found {
		t.Fatalf("notes = %v", s.Notes)
	}
	if s.Session == SessionRemoved {
		t.Fatal("early access is not a node removal")
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.purged {
		t.Fatal("lanes must not be purged on an early-access refusal")
	}
}
