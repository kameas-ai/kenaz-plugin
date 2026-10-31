package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"google.golang.org/protobuf/proto"
)

// sessionFleet is a fake Fleet whose credential store is shared by every
// process on the machine. Each process (sessionProc) loads the stored
// session once and keeps it, as the official provider does, so a relay
// that started signed out stays signed out until it reloads: the bug a
// sign-in by another process used to hit.
type sessionFleet struct {
	lane *laneFleet // Me, opt-ins, pollers and uploads

	mu      sync.Mutex
	stored  string // the signed-in user in the store; "" = signed out
	enrolls map[string]int
}

func newSessionFleet() *sessionFleet {
	return &sessionFleet{lane: &laneFleet{Provider: stub.New()}, enrolls: map[string]int{}}
}

func (f *sessionFleet) store(user string) { f.mu.Lock(); f.stored = user; f.mu.Unlock() }
func (f *sessionFleet) enrolled(user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enrolls[user]
}

// sessionProc is one process's provider. reload says whether it can
// reload its session in place (fleetapi.SessionReloader).
type sessionProc struct {
	*laneFleet
	f      *sessionFleet
	reload bool

	mu     sync.Mutex
	loaded bool
	user   string
}

func (f *sessionFleet) proc(reload bool) *sessionProc {
	return &sessionProc{laneFleet: f.lane, f: f, reload: reload}
}

func (p *sessionProc) Auth() fleetapi.Auth {
	if p.reload {
		return reloadingAuth{procAuth{p}}
	}
	return procAuth{p}
}
func (p *sessionProc) Enroller() fleetapi.Enroller { return procEnroller{p} }
func (p *sessionProc) Me() fleetapi.Me             { return procMe{laneMe{}, p} }

// procMe answers as Fleet does: only for a signed-in session.
type procMe struct {
	laneMe
	p *sessionProc
}

func (m procMe) Get(ctx context.Context) (fleetapi.MeInfo, error) {
	if _, err := m.p.session(); err != nil {
		return fleetapi.MeInfo{}, err
	}
	return m.laneMe.Get(ctx)
}
func (m procMe) Capabilities(ctx context.Context) (fleetapi.Capabilities, error) {
	if _, err := m.p.session(); err != nil {
		return fleetapi.Capabilities{}, err
	}
	return m.laneMe.Capabilities(ctx)
}

func (p *sessionProc) session() (fleetapi.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		p.f.mu.Lock()
		p.user, p.loaded = p.f.stored, true
		p.f.mu.Unlock()
	}
	if p.user == "" {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	return fleetapi.Session{Env: fleetapi.EnvDev, UserID: p.user, OrgID: "org-1"}, nil
}

type procAuth struct{ p *sessionProc }

func (a procAuth) Login(_ context.Context, r fleetapi.LoginRequest) (fleetapi.Session, error) {
	a.p.f.store("sub-1")
	a.p.mu.Lock()
	a.p.user, a.p.loaded = "sub-1", true
	a.p.mu.Unlock()
	return fleetapi.Session{Env: r.Env, UserID: "sub-1", OrgID: "org-1"}, nil
}
func (a procAuth) Logout(context.Context) error {
	a.p.f.store("")
	a.p.mu.Lock()
	a.p.user, a.p.loaded = "", true
	a.p.mu.Unlock()
	return nil
}
func (a procAuth) Session(context.Context) (fleetapi.Session, error) { return a.p.session() }
func (a procAuth) TokenStore() fleetapi.TokenStore                   { return nil }

type reloadingAuth struct{ procAuth }

func (a reloadingAuth) ReloadSession(context.Context) error {
	a.p.mu.Lock()
	a.p.loaded = false
	a.p.mu.Unlock()
	return nil
}

type procEnroller struct{ p *sessionProc }

func (e procEnroller) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	s, err := e.p.session()
	if err != nil {
		return fleetapi.EnrollResult{}, err
	}
	e.p.f.mu.Lock()
	e.p.f.enrolls[s.UserID]++
	e.p.f.mu.Unlock()
	return fleetapi.EnrollResult{NodeID: b.NodeID, Tier: "team"}, nil
}

// sessionMachine runs CLI commands and relays as separate "processes"
// (each with its own provider) on one machine.
type sessionMachine struct {
	t      *testing.T
	f      *sessionFleet
	reload bool
	home   string
	ks     *spool.Memory

	mu     sync.Mutex
	spawns int
	relays []chan struct{}
	cancel []context.CancelFunc
}

func newSessionMachine(t *testing.T, reload bool) *sessionMachine {
	old := stdinInteractive
	stdinInteractive = func() bool { return false }
	m := &sessionMachine{t: t, f: newSessionFleet(), reload: reload, home: t.TempDir(), ks: &spool.Memory{}}
	t.Cleanup(func() {
		m.mu.Lock()
		cancels, relays := m.cancel, m.relays
		m.mu.Unlock()
		for _, c := range cancels {
			c()
		}
		for _, r := range relays {
			select {
			case <-r:
			case <-time.After(5 * time.Second):
				t.Error("relay did not stop")
			}
		}
		stdinInteractive = old
	})
	return m
}

func (m *sessionMachine) env(ctx context.Context, p fleetapi.Provider, out, errb *bytes.Buffer) Env {
	return Env{Provider: p, Stdout: out, Stderr: errb, Ctx: ctx, KeyStore: m.ks,
		Getenv: func(k string) string {
			return map[string]string{variant.Shared.HomeVar: m.home, variant.Shared.EnvVar: "dev"}[k]
		},
		OpenBrowser: func(string) error { return nil },
		Spawn: func(string, fleetapi.Env) error {
			m.mu.Lock()
			m.spawns++
			m.mu.Unlock()
			m.startRelay()
			return nil
		},
	}
}

// startRelay starts a relay process (a fresh provider) in the background.
func (m *sessionMachine) startRelay() chan struct{} {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.relays = append(m.relays, done)
	m.cancel = append(m.cancel, cancel)
	m.mu.Unlock()
	go func() {
		var out, errb bytes.Buffer
		defer close(done)
		RunEnv(m.env(ctx, m.f.proc(m.reload), &out, &errb), []string{"daemon"})
	}()
	return done
}

// run runs one CLI command as its own process.
func (m *sessionMachine) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunEnv(m.env(context.Background(), m.f.proc(m.reload), &out, &errb), args)
	return code, out.String(), errb.String()
}

func (m *sessionMachine) spawnCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.spawns }

func (m *sessionMachine) stateDir() string { return m.home + "/plugin/dev" }

// relayStatus is the running relay's status (ok false when none answers).
func (m *sessionMachine) relayStatus() (daemon.Status, daemon.Info, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, m.stateDir())
	if err != nil {
		return daemon.Status{}, daemon.Info{}, false
	}
	st, err := c.Status(ctx)
	return st, c.Info, err == nil
}

// waitFor polls the relay's status until cond holds.
func (m *sessionMachine) waitFor(what string, wait time.Duration, cond func(daemon.Status) bool) (daemon.Status, daemon.Info) {
	m.t.Helper()
	deadline := time.Now().Add(wait)
	var last daemon.Status
	for {
		if st, in, ok := m.relayStatus(); ok {
			last = st
			if cond(st) {
				return st, in
			}
		}
		if time.Now().After(deadline) {
			b, _ := json.Marshal(last)
			m.t.Fatalf("relay never %s; last status %s", what, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func signedOutAndWithdrawn(st daemon.Status) bool {
	return st.Session == daemon.SessionSignedOut && st.Enroll == string(enroll.StateSignedOut) && laneHeld(st, "withdrawn")
}

func signedInAndEnrolled(st daemon.Status) bool {
	return st.Session == daemon.SessionSignedIn && st.Enroll == string(enroll.StateEnrolled)
}

func laneHeld(st daemon.Status, reason string) bool {
	for _, l := range st.Lanes {
		if l.Name == telemetryLaneName {
			return l.State == "held" && l.Reason == reason
		}
	}
	return false
}

func withSessionCheck(t *testing.T, d time.Duration) {
	old := sessionCheckEvery
	sessionCheckEvery = d
	t.Cleanup(func() { sessionCheckEvery = old })
}

// A relay that started signed out picks up `kenaz login` from another
// process at once, without a restart: status says signed in and it
// enrolls. Nothing is uploaded, since the member has not opted in.
func TestLoginReachesRunningRelay(t *testing.T) {
	withSessionCheck(t, time.Hour) // only the login notice can do it
	m := newSessionMachine(t, true)
	m.startRelay()
	_, before := m.waitFor("reported signed out", 5*time.Second, signedOutAndWithdrawn)

	code, out, errb := m.run("login")
	if code != ExitOK {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	st, after := m.waitFor("signed in and enrolled", 3*time.Second, signedInAndEnrolled)
	if after.Nonce != before.Nonce || after.PID != before.PID {
		t.Fatal("the relay was restarted; it should reload in place")
	}
	if n := m.spawnCount(); n != 0 {
		t.Fatalf("login started %d relays next to the running one", n)
	}
	if m.f.enrolled("sub-1") == 0 {
		t.Fatal("the relay never enrolled the new session")
	}
	if st.Tier != "team" {
		t.Fatalf("tier = %q: the capability refresh did not follow the sign-in", st.Tier)
	}
	// The member has not opted in: the lane stays held and nothing is sent.
	if !laneHeld(st, "withdrawn") || m.f.lane.postedCount() != 0 {
		t.Fatalf("lane not held before opt-in: %+v posted=%d", st.Lanes, m.f.lane.postedCount())
	}
	if code, out, _ := m.run("status"); code != ExitOK || !bytes.Contains([]byte(out), []byte("session:      signed in")) || !bytes.Contains([]byte(out), []byte("enrollment:   enrolled")) {
		t.Fatalf("status after login: %s", out)
	}
}

// A provider that cannot reload in place (or a relay from a release
// without the notice) is restarted by login, and the new relay enrolls.
func TestLoginRestartsRelayThatCannotReload(t *testing.T) {
	m := newSessionMachine(t, false)
	first := m.startRelay()
	_, before := m.waitFor("reported signed out", 5*time.Second, signedOutAndWithdrawn)

	if code, out, errb := m.run("login"); code != ExitOK {
		t.Fatalf("login exit=%d out=%s err=%s", code, out, errb)
	}
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the stale relay was not stopped")
	}
	_, after := m.waitFor("signed in and enrolled", 5*time.Second, signedInAndEnrolled)
	if after.Nonce == before.Nonce {
		t.Fatal("still the stale relay")
	}
	if n := m.spawnCount(); n != 1 {
		t.Fatalf("spawns = %d, want 1", n)
	}
}

// Self-heal: a sign-in whose notice never reached the relay (here the
// store changes with no notice at all) is picked up on the relay's next
// session check.
func TestRelaySelfHealsSignIn(t *testing.T) {
	withSessionCheck(t, 50*time.Millisecond)
	m := newSessionMachine(t, true)
	m.startRelay()
	_, before := m.waitFor("reported signed out", 5*time.Second, signedOutAndWithdrawn)

	m.f.store("sub-1")
	_, after := m.waitFor("signed in and enrolled", 5*time.Second, signedInAndEnrolled)
	if after.Nonce != before.Nonce {
		t.Fatal("the relay was restarted")
	}
	if m.f.lane.postedCount() != 0 {
		t.Fatal("uploaded before the member opted in")
	}
}

// Self-heal for sign-out: a sign-out the relay was not told about (logout
// could not reach it) stops uploads, purges and shows signed out on the
// next session check.
func TestRelaySelfHealsSignOut(t *testing.T) {
	withSessionCheck(t, 50*time.Millisecond)
	// The org is admitted before the fixture's events, so they are kept.
	oldNow := laneNow
	laneNow = func() time.Time { return fixtures.Base.Add(-time.Hour) }
	t.Cleanup(func() { laneNow = oldNow })
	m := newSessionMachine(t, true)
	m.f.store("sub-1")
	// Fleet is down, so what the member's tool sends waits in the spool.
	m.f.lane.set(func(f *laneFleet) { f.optedIn = true; f.postErr = &fleetapi.APIError{Status: 503} })
	m.startRelay()
	_, in := m.waitFor("signed in and enrolled", 5*time.Second, signedInAndEnrolled)

	spoolDir := filepath.Join(m.stateDir(), "spool")
	deadline := time.Now().Add(5 * time.Second)
	for spoolEmpty(t, spoolDir) {
		if time.Now().After(deadline) {
			t.Fatalf("nothing was queued for the signed-in account (upload attempts %d)", m.f.lane.attemptCount())
		}
		postRelaySession(t, in)
		time.Sleep(50 * time.Millisecond)
	}

	m.f.store("")
	st, _ := m.waitFor("reported signed out", 5*time.Second, signedOutAndWithdrawn)
	if st.Tier != "" || st.OrgName != "" {
		t.Fatalf("signed-out status still names the old account's plan: tier=%q", st.Tier)
	}
	if !spoolEmpty(t, spoolDir) {
		t.Fatal("the sign-out did not purge what was queued for the old account")
	}
}

// postRelaySession sends a Claude Code session's logs to the running
// relay's OTLP receiver and ends the session, so the relay builds a batch.
func postRelaySession(t *testing.T, in daemon.Info) {
	t.Helper()
	body, err := proto.Marshal(fixtures.ClaudeLogs())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/logs", in.Port), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("relay OTLP receiver: http %d", resp.StatusCode)
	}
	c := &daemon.Client{Info: in, HTTP: http.DefaultClient}
	if err := c.SessionEnd(ctx, string(canon.ClaudeCode), canon.LocalSessionHash(fixtures.ClaudeSessionID)); err != nil {
		t.Fatal(err)
	}
}

// spoolEmpty reports whether no spool segment (*.seg) in dir holds data.
func spoolEmpty(t *testing.T, dir string) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".seg") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return false
		}
	}
	return true
}

// `kenaz logout` reaches a running relay: it purges and stops, and status
// then reports signed out.
func TestLogoutReachesRunningRelay(t *testing.T) {
	withSessionCheck(t, time.Hour)
	m := newSessionMachine(t, true)
	m.f.store("sub-1")
	relay := m.startRelay()
	m.waitFor("signed in and enrolled", 5*time.Second, signedInAndEnrolled)

	if code, out, errb := m.run("logout"); code != ExitOK {
		t.Fatalf("logout exit=%d out=%s err=%s", code, out, errb)
	}
	select {
	case <-relay:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not stop on logout")
	}
	code, out, _ := m.run("status", "--json")
	var st daemon.Status
	if code != ExitOK || json.Unmarshal([]byte(out), &st) != nil || st.Session != daemon.SessionSignedOut {
		t.Fatalf("status after logout: %s", out)
	}
}
