package consent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

// fakeFleet is an in-memory Fleet for the consent tests. Unused seam parts
// fall through to the stub.
type fakeFleet struct {
	fleetapi.Provider // stub for everything not overridden

	mu        sync.Mutex
	signedIn  bool
	meErr     error
	me        fleetapi.MeInfo
	caps      fleetapi.Capabilities
	optins    map[fleetapi.TelemetryClass]fleetapi.OptInState
	approvals []fleetapi.Approval
	// approvalsErr is returned by PendingApprovals (e.g. Fleet's 403
	// early-access refusal, which /me/pending-approvals answers for a plugin
	// token of an org outside the program).
	approvalsErr error
	puts         []fleetapi.OptInPut
	lockWaits    chan fleetapi.LockdownState
	// user / org override the signed-in session ("" = u1 / o1).
	user, org string
}

func newFake(available ...fleetapi.TelemetryClass) *fakeFleet {
	f := &fakeFleet{
		Provider:  stub.New(),
		signedIn:  true,
		me:        fleetapi.MeInfo{UserID: "u1", OrgID: "o1", OrgName: "Acme", Tier: "team"},
		caps:      fleetapi.Capabilities{Keys: map[string]bool{}},
		optins:    map[fleetapi.TelemetryClass]fleetapi.OptInState{},
		lockWaits: make(chan fleetapi.LockdownState, 4),
	}
	for _, ci := range Classes {
		f.optins[ci.Class] = fleetapi.OptInState{Class: ci.Class}
	}
	for _, c := range available {
		s := f.optins[c]
		s.Available = true
		f.optins[c] = s
	}
	return f
}

func (f *fakeFleet) Auth() fleetapi.Auth       { return fakeAuth{f} }
func (f *fakeFleet) Me() fleetapi.Me           { return fakeMe{f} }
func (f *fakeFleet) OptIns() fleetapi.OptIns   { return fakeOptIns{f} }
func (f *fakeFleet) Pollers() fleetapi.Pollers { return fakePollers{f} }

type fakeAuth struct{ f *fakeFleet }

func (a fakeAuth) Login(context.Context, fleetapi.LoginRequest) (fleetapi.Session, error) {
	return fleetapi.Session{}, fleetapi.ErrUnofficialBuild
}
func (a fakeAuth) Logout(context.Context) error { return nil }
func (a fakeAuth) Session(context.Context) (fleetapi.Session, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if !a.f.signedIn {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	u, o := a.f.user, a.f.org
	if u == "" {
		u = "u1"
	}
	if o == "" {
		o = "o1"
	}
	return fleetapi.Session{Env: fleetapi.EnvProd, UserID: u, OrgID: o}, nil
}
func (a fakeAuth) TokenStore() fleetapi.TokenStore { return stub.New().Auth().TokenStore() }

type fakeMe struct{ f *fakeFleet }

func (m fakeMe) Get(context.Context) (fleetapi.MeInfo, error) {
	m.f.mu.Lock()
	defer m.f.mu.Unlock()
	return m.f.me, m.f.meErr
}
func (m fakeMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	m.f.mu.Lock()
	defer m.f.mu.Unlock()
	return m.f.caps, nil
}

type fakeOptIns struct{ f *fakeFleet }

func (o fakeOptIns) Get(context.Context) ([]fleetapi.OptInState, error) {
	o.f.mu.Lock()
	defer o.f.mu.Unlock()
	var out []fleetapi.OptInState
	for _, ci := range Classes {
		out = append(out, o.f.optins[ci.Class])
	}
	return out, nil
}
func (o fakeOptIns) Put(_ context.Context, req fleetapi.OptInPut) error {
	o.f.mu.Lock()
	defer o.f.mu.Unlock()
	o.f.puts = append(o.f.puts, req)
	for _, u := range req.Updates {
		s := o.f.optins[u.Class]
		s.OptedIn = u.OptedIn
		// Simulates a Fleet that predates notice_version.
		o.f.optins[u.Class] = s
	}
	return nil
}

type fakePollers struct{ f *fakeFleet }

func (p fakePollers) PendingApprovals(context.Context) ([]fleetapi.Approval, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.f.approvalsErr != nil {
		return nil, p.f.approvalsErr
	}
	return append([]fleetapi.Approval(nil), p.f.approvals...), nil
}
func (p fakePollers) LockdownStatus(context.Context) (fleetapi.LockdownState, error) {
	return fleetapi.LockdownState{}, nil
}
func (p fakePollers) LockdownWait(ctx context.Context) (fleetapi.LockdownState, bool, error) {
	select {
	case s := <-p.f.lockWaits:
		return s, true, nil
	case <-ctx.Done():
		return fleetapi.LockdownState{}, false, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return fleetapi.LockdownState{}, false, nil
	}
}
func (p fakePollers) OrgPause(context.Context) (fleetapi.PauseState, error) {
	return fleetapi.PauseState{}, nil
}
func (p fakePollers) Configs(context.Context, fleetapi.ConfigsRequest) (fleetapi.ConfigBundle, error) {
	return fleetapi.ConfigBundle{NotModified: true}, nil
}
func (p fakePollers) AckConfigs(context.Context, string, []fleetapi.ConfigAck) error { return nil }

func (f *fakeFleet) set(fn func(f *fakeFleet)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeLanes records withdrawal calls.
type fakeLanes struct {
	mu      sync.Mutex
	stopped int
	purged  int
	classes []fleetapi.TelemetryClass
}

func (l *fakeLanes) StopAll() { l.mu.Lock(); l.stopped++; l.mu.Unlock() }
func (l *fakeLanes) PurgeAll() error {
	l.mu.Lock()
	l.purged++
	l.mu.Unlock()
	return nil
}
func (l *fakeLanes) PurgeClass(c fleetapi.TelemetryClass) error {
	l.mu.Lock()
	l.classes = append(l.classes, c)
	l.mu.Unlock()
	return nil
}

func newTestManager(t *testing.T, f *fakeFleet, lanes Lanes) *Manager {
	t.Helper()
	m := NewManager(Options{
		Provider:          f,
		Lanes:             lanes,
		ProjectionVersion: 1,
		Store:             NewStore(filepath.Join(t.TempDir(), "consent.json")),
	})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func allClasses() []fleetapi.TelemetryClass {
	var out []fleetapi.TelemetryClass
	for _, ci := range Classes {
		out = append(out, ci.Class)
	}
	return out
}

func optIn(t *testing.T, m *Manager, cs ...fleetapi.TelemetryClass) {
	t.Helper()
	sel := map[fleetapi.TelemetryClass]bool{}
	for _, c := range cs {
		sel[c] = true
	}
	if err := m.SetOptIns(context.Background(), sel); err != nil {
		t.Fatal(err)
	}
}
