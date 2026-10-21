package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
)

const canary = "CANARYHOST9f3e"

type fakeEnroller struct {
	mu     sync.Mutex
	bodies [][]byte
	errs   []error // consumed in order; nil = success
}

func (f *fakeEnroller) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(b)
	f.bodies = append(f.bodies, raw)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return fleetapi.EnrollResult{}, err
		}
	}
	return fleetapi.EnrollResult{NodeID: b.NodeID, OrgID: "org", Tier: "team"}, nil
}

func (f *fakeEnroller) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }
func (f *fakeEnroller) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.bodies[len(f.bodies)-1])
}

func host() Host {
	return Host{GOOS: "darwin", GOARCH: "arm64", Hostname: func() (string, error) { return canary + ".local", nil }}
}

func TestNodeIDStable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateNodeID(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(a) {
		t.Fatalf("not a ULID: %q", a)
	}
	b, _ := LoadOrCreateNodeID(dir, time.Now())
	if a != b {
		t.Fatalf("node id changed: %s → %s", a, b)
	}
}

// The canary hostname never leaves unless the member opts in.
func TestDeviceDisplayName(t *testing.T) {
	dir := t.TempDir()
	body := func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "01JNODEID", "1.0.0", host()) }
	f := &fakeEnroller{}
	hb := &Heartbeat{Enroller: f, Body: body}
	ctx := context.Background()

	if _, err := hb.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.last(), canary) || !regexp.MustCompile(`"hostname":"macOS device [0-9a-f]{4}"`).MatchString(f.last()) {
		t.Fatalf("default body leaked or wrong: %s", f.last())
	}
	if !strings.Contains(f.last(), `"client_kind":"relay"`) {
		t.Fatalf("client_kind: %s", f.last())
	}
	device.Rename(dir, "Work laptop")
	hb.Once(ctx)
	if !strings.Contains(f.last(), `"hostname":"Work laptop"`) || strings.Contains(f.last(), canary) {
		t.Fatalf("renamed body: %s", f.last())
	}
	device.UseComputerName(dir)
	hb.Once(ctx)
	if !strings.Contains(f.last(), canary) {
		t.Fatalf("opt-in body should carry hostname: %s", f.last())
	}
}

// The body's keys are exactly the allowlisted ones.
func TestBodyKeysAllowlisted(t *testing.T) {
	b, err := BuildBody(t.TempDir(), "01JNODEID", "1.0.0", Host{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	var m map[string]any
	json.Unmarshal(raw, &m)
	allowed := map[string]bool{}
	for _, k := range []string{"node_id", "platform", "version", "hostname", "arch", "os_version", "in_vm", "vm_id", "client_kind"} {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

func TestBuildBodyRejectsUnknownPlatform(t *testing.T) {
	if _, err := BuildBody(t.TempDir(), "n", "1", Host{GOOS: "plan9"}); err == nil {
		t.Fatal("want validation error")
	}
}

// Enroll at start and every 6 h ±10%; node_removed stops the loop.
func TestHeartbeatSchedule(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	f := &fakeEnroller{errs: []error{nil, nil, &fleetapi.APIError{Status: 410, Code: fleetapi.CodeNodeRemoved}}}
	var states []State
	var smu sync.Mutex
	hb := &Heartbeat{
		Enroller: f,
		Body:     func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "n1", "1", host()) },
		Clock:    clk,
		Rand:     func() float64 { return 1 }, // max jitter: +10%
		OnState:  func(s State, _ fleetapi.EnrollResult) { smu.Lock(); states = append(states, s); smu.Unlock() },
	}
	done := make(chan error, 1)
	go func() { done <- hb.Run(context.Background()) }()

	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	clk.Advance(6*time.Hour + 30*time.Minute) // < 6.6h: no beat
	time.Sleep(10 * time.Millisecond)
	if f.calls() != 1 {
		t.Fatalf("beat too early: %d calls", f.calls())
	}
	clk.Advance(7 * time.Minute) // past 6.6h
	waitFor(t, func() bool { return f.calls() == 2 && clk.Waiters() == 1 })
	clk.Advance(7 * time.Hour)
	select {
	case err := <-done:
		if !isRemoved(err) {
			t.Fatalf("Run err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on node_removed")
	}
	if st, _ := hb.State(); st != StateRemoved {
		t.Fatalf("state = %s", st)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(states) != 2 || states[0] != StateEnrolled || states[1] != StateRemoved {
		t.Fatalf("transitions = %v", states)
	}
}

func TestHeartbeatJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		hb := &Heartbeat{Rand: func() float64 { return r }}
		d := hb.next()
		if d < 324*time.Minute || d > 396*time.Minute {
			t.Fatalf("r=%v delay %v outside 6h±10%%", r, d)
		}
	}
}

func TestHeartbeatStopsOnUnofficialBuild(t *testing.T) {
	f := &fakeEnroller{errs: []error{fleetapi.ErrUnofficialBuild}}
	hb := &Heartbeat{Enroller: f, Body: func() (fleetapi.EnrollBody, error) { return BuildBody(t.TempDir(), "n", "1", host()) }, Clock: clock.NewFake(time.Now())}
	if err := hb.Run(context.Background()); !errors.Is(err, fleetapi.ErrUnofficialBuild) {
		t.Fatalf("err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeartbeatKickAndSignedOutRetry(t *testing.T) {
	clk := clock.NewFake(time.Now())
	kick := make(chan struct{}, 1)
	f := &fakeEnroller{errs: []error{fleetapi.ErrNotSignedIn, nil}}
	hb := &Heartbeat{Enroller: f, Kick: kick, Clock: clk,
		Body: func() (fleetapi.EnrollBody, error) { return BuildBody(t.TempDir(), "n", "1", host()) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Run(ctx)
	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	if st, _ := hb.State(); st != StateSignedOut {
		t.Fatalf("state = %s", st)
	}
	kick <- struct{}{} // login happened
	waitFor(t, func() bool { st, _ := hb.State(); return st == StateEnrolled })
}

// Early access: an org outside the program is a distinct state, the
// loop keeps going at the normal interval (not the 5 min error retry), and
// admission is picked up on the next beat.
func TestHeartbeatEarlyAccessKeepsBeating(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	ea := &fleetapi.APIError{Status: 403, Code: fleetapi.CodeEarlyAccessRequired}
	f := &fakeEnroller{errs: []error{ea, nil}}
	hb := &Heartbeat{
		Enroller: f,
		Body:     func() (fleetapi.EnrollBody, error) { return BuildBody(dir, "n1", "1", host()) },
		Clock:    clk,
		Rand:     func() float64 { return 0.5 }, // no jitter: exactly 6 h
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hb.Run(ctx) }()

	waitFor(t, func() bool { return f.calls() == 1 && clk.Waiters() == 1 })
	if st, _ := hb.State(); st != StateEarlyAccess {
		t.Fatalf("state = %s, want %s", st, StateEarlyAccess)
	}
	clk.Advance(retryDelay + time.Minute) // the error retry would fire here
	time.Sleep(10 * time.Millisecond)
	if f.calls() != 1 {
		t.Fatalf("early access must not use the 5 min error retry: %d calls", f.calls())
	}
	clk.Advance(6 * time.Hour)
	waitFor(t, func() bool { st, _ := hb.State(); return f.calls() == 2 && st == StateEnrolled })
}
