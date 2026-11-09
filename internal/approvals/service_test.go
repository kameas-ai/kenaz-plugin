package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

var (
	alice = Identity{Env: "dev", OrgID: "org-1", UserID: "user-alice"}
	bob   = Identity{Env: "dev", OrgID: "org-1", UserID: "user-bob"}
)

type harness struct {
	t     *testing.T
	m     *Manager
	dir   string
	clk   *clock.Fake
	who   whoBox
	logs  *bytes.Buffer
	execs atomic.Int32
	// remote is the fake destination: idempotency key -> payload.
	remoteMu sync.Mutex
	remote   map[string][]byte
	execHook func(ctx context.Context, payload []byte, key string) ([]byte, error)
}

type whoBox struct {
	mu  sync.Mutex
	id  Identity
	out bool
}

func (w *whoBox) Store(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id, ok := v.(Identity); ok {
		w.id, w.out = id, false
	} else {
		w.out = true
	}
}

func (w *whoBox) Load() any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out {
		return signedOut{}
	}
	return w.id
}

type signedOut struct{}

var testKey = bytes.Repeat([]byte{0x42}, 32)

// renderTest is the page renderer for the test operations.
func renderTest(p []byte) (Rendered, error) {
	var v struct{ Title string }
	if err := json.Unmarshal(p, &v); err != nil {
		return Rendered{}, err
	}
	return Rendered{Summary: "Share a note titled " + v.Title, Targets: []string{"team:Platform"}}, nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), clk: clock.NewFake(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)), logs: &bytes.Buffer{}, remote: map[string][]byte{}}
	h.who.Store(alice)
	h.m = h.open()
	return h
}

func (h *harness) open() *Manager {
	h.t.Helper()
	opt := Options{
		Dir: h.dir, Clock: h.clk, BaseURL: func() string { return testOrigin }, StoreKey: testKey,
		Logger: slog.New(slog.NewTextHandler(h.logs, nil)),
		Identity: func(context.Context) (Identity, error) {
			if id, ok := h.who.Load().(Identity); ok {
				return id, nil
			}
			return Identity{}, errors.New("signed out")
		},
	}
	m, err := New(opt)
	if err != nil {
		h.t.Fatal(err)
	}
	m.Register("context.share", h.exec)
	m.RegisterRenderer("context.share", renderTest)
	return m
}

func (h *harness) exec(ctx context.Context, payload []byte, key string) ([]byte, error) {
	h.execs.Add(1)
	if h.execHook != nil {
		return h.execHook(ctx, payload, key)
	}
	h.remoteMu.Lock()
	defer h.remoteMu.Unlock()
	h.remote[key] = append([]byte(nil), payload...)
	return []byte(`{"id":"node-1"}`), nil
}

func (h *harness) signOut() { h.who.Store(signedOut{}) }

func req(id Identity, key, body string) Request {
	p, _ := json.Marshal(map[string]string{"title": "Flaky e2e", "body": body})
	return Request{Operation: "context.share", Env: id.Env, OrgID: id.OrgID, UserID: id.UserID, TargetIDs: []string{"team:Platform"}, Payload: p, IdempotencyKey: key, Summary: "Share a note with Platform"}
}

func (h *harness) prepare(r Request) string {
	h.t.Helper()
	id, st, err := h.m.Prepare(context.Background(), r)
	if err != nil || st != StatusPending {
		h.t.Fatalf("prepare: %v %s", err, st)
	}
	return id
}

func (h *harness) approve(id string) (Status, error) {
	h.t.Helper()
	return h.m.Approve(context.Background(), id)
}

func (h *harness) status(id string) Status {
	h.t.Helper()
	st, _, err := h.m.Status(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) diskState() state {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, StateFile))
	if err != nil {
		h.t.Fatal(err)
	}
	var f sealedFile
	var s state
	if err := json.Unmarshal(b, &f); err != nil {
		h.t.Fatal(err)
	}
	if err := json.Unmarshal(f.State, &s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestHappyPathExecutesStoredPayloadOnce(t *testing.T) {
	h := newHarness(t)
	r := req(alice, "k1", "Retry with -count=3.")
	id := h.prepare(r)
	if h.execs.Load() != 0 {
		t.Fatal("prepare wrote")
	}
	// Same key and payload: the same request.
	id2, st, err := h.m.Prepare(context.Background(), r)
	if err != nil || id2 != id || st != StatusPending {
		t.Fatalf("idempotent prepare: %s %s %v", id2, st, err)
	}
	st, err = h.approve(id)
	if err != nil || st != StatusDone {
		t.Fatalf("approve: %s %v", st, err)
	}
	if h.execs.Load() != 1 || !bytes.Equal(h.remote["k1"], r.Payload) {
		t.Fatalf("executed %d times with %s", h.execs.Load(), h.remote["k1"])
	}
	st, res, _ := h.m.Status(context.Background(), id)
	if st != StatusDone || string(res) != `{"id":"node-1"}` {
		t.Fatalf("status %s %s", st, res)
	}
	// Content is purged after completion; only bounded metadata remains.
	rec := h.diskState().Records[id]
	if len(rec.Payload) != 0 || rec.Summary != "" {
		t.Fatalf("content kept after done: %+v", rec)
	}
	// A duplicate prepare returns the completed request, not a new write.
	id3, st, _ := h.m.Prepare(context.Background(), r)
	if id3 != id || st != StatusDone || h.execs.Load() != 1 {
		t.Fatalf("duplicate after done: %s %s execs=%d", id3, st, h.execs.Load())
	}
	// Logs never carry the payload or summary.
	if strings.Contains(h.logs.String(), "Retry with") || strings.Contains(h.logs.String(), "Share a note") {
		t.Fatalf("logs leak content: %s", h.logs.String())
	}
}

func TestIdempotencyKeyReuseWithDifferentPayloadRefused(t *testing.T) {
	h := newHarness(t)
	h.prepare(req(alice, "k1", "one"))
	if _, _, err := h.m.Prepare(context.Background(), req(alice, "k1", "two")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestPrepareValidation(t *testing.T) {
	h := newHarness(t)
	bad := []Request{
		{},
		func() Request { r := req(alice, "k", "x"); r.Payload = []byte("not json"); return r }(),
		func() Request { r := req(alice, "k", "x"); r.Payload = []byte(`["not","a","note"]`); return r }(), // renderer refuses
		func() Request { r := req(alice, "", "x"); return r }(),
		func() Request { r := req(alice, "k", "x"); r.Operation = "Bad Op"; return r }(),
	}
	for i, r := range bad {
		if _, _, err := h.m.Prepare(context.Background(), r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%d: got %v", i, err)
		}
	}
	r := req(alice, "k", "x")
	r.Operation = "skills.unregistered"
	if _, _, err := h.m.Prepare(context.Background(), r); !errors.Is(err, ErrUnknownOperation) {
		t.Errorf("unknown op: %v", err)
	}
	// A request for another account than the signed-in one.
	if _, _, err := h.m.Prepare(context.Background(), req(bob, "k", "x")); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("wrong account: %v", err)
	}
	h.signOut()
	if _, _, err := h.m.Prepare(context.Background(), req(alice, "k", "x")); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("signed out: %v", err)
	}
}

func TestSessionLossCancelsOpenRequests(t *testing.T) {
	h := newHarness(t)
	id := h.prepare(req(alice, "k1", "x"))
	h.signOut()
	h.m.Status(context.Background(), id)
	h.who.Store(alice)
	if st := h.status(id); st != StatusCanceled {
		t.Fatalf("got %s", st)
	}
}

func TestRemoteSuccessThenCrashReconciles(t *testing.T) {
	h := newHarness(t)
	applied := make(chan struct{})
	block := make(chan struct{})
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		h.remoteMu.Lock()
		h.remote[k] = p // the destination applied the write...
		h.remoteMu.Unlock()
		close(applied)
		<-block // ...and the relay dies before recording it
		return nil, errors.New("unreachable")
	}
	id := h.prepare(req(alice, "k1", "x"))
	crashed := make(chan struct{})
	go func() { defer close(crashed); h.approve(id) }()
	<-applied
	if h.diskState().Records[id].Status != StatusExecuting {
		t.Fatal("executing state not persisted before the remote call")
	}
	// Restart: a new Manager on the same store.
	h.execHook = nil
	m2 := h.open()
	if st, _, _ := m2.Status(context.Background(), id); st != StatusReconciling {
		t.Fatalf("after crash: %s", st)
	}
	// Without a reconciler, the write is never repeated blindly.
	m2.sweep(context.Background())
	if st, _, _ := m2.Status(context.Background(), id); st != StatusReconciling || h.execs.Load() != 1 {
		t.Fatalf("no reconciler: %s execs=%d", st, h.execs.Load())
	}
	// With a reconciler that finds the write, it finishes as done without
	// re-executing.
	m2.RegisterReconciler("context.share", func(ctx context.Context, p []byte, k string) ([]byte, bool, error) {
		h.remoteMu.Lock()
		defer h.remoteMu.Unlock()
		_, ok := h.remote[k]
		return []byte(`{"id":"node-1","reconciled":true}`), ok, nil
	})
	m2.sweep(context.Background())
	st, res, _ := m2.Status(context.Background(), id)
	if st != StatusDone || h.execs.Load() != 1 || !strings.Contains(string(res), "reconciled") {
		t.Fatalf("reconciled: %s %s execs=%d", st, res, h.execs.Load())
	}
	close(block)
	<-crashed // the dead process's goroutine, for test cleanup only
}

func TestReconcileNotAppliedRetriesIdempotentlyOnce(t *testing.T) {
	h := newHarness(t)
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return nil, ErrOutcomeUnknown // timeout before the destination answered
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id); st != StatusReconciling {
		t.Fatalf("got %s", st)
	}
	h.execHook = nil
	h.m.RegisterReconciler("context.share", func(ctx context.Context, p []byte, k string) ([]byte, bool, error) {
		return nil, false, nil
	})
	h.m.sweep(context.Background())
	if st := h.status(id); st != StatusDone || h.execs.Load() != 2 {
		t.Fatalf("retry: %s execs=%d", st, h.execs.Load())
	}
	if _, ok := h.remote["k1"]; !ok {
		t.Fatal("retry did not use the stored idempotency key")
	}
}

// A request nothing can resolve (no reconciler) does not stay
// reconciling forever: after a week it ends failed with an unknown
// outcome, its content dropped, and it is never re-executed.
func TestUnresolvedReconcilingEndsFailed(t *testing.T) {
	h := newHarness(t)
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return nil, ErrOutcomeUnknown
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id); st != StatusReconciling {
		t.Fatalf("got %s", st)
	}
	h.m.sweep(context.Background())
	if st := h.status(id); st != StatusReconciling {
		t.Fatalf("resolved too early: %s", st)
	}
	h.clk.Advance(retainUnresolved + time.Minute)
	h.m.sweep(context.Background())
	st, res, _ := h.m.Status(context.Background(), id)
	if st != StatusFailed || string(res) != `{"error":"outcome_unknown"}` || h.execs.Load() != 1 {
		t.Fatalf("after a week: %s %s execs=%d", st, res, h.execs.Load())
	}
	if r := h.diskState().Records[id]; len(r.Payload) != 0 {
		t.Fatal("content kept")
	}
}

func TestExecutorRefusalIsFailedWithCodeOnly(t *testing.T) {
	h := newHarness(t)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) {
		return nil, errors.Join(ErrorCode("credential_detected"), errors.New("body echo SECRET"))
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id); st != StatusFailed {
		t.Fatalf("got %s", st)
	}
	_, res, _ := h.m.Status(context.Background(), id)
	if string(res) != `{"error":"credential_detected"}` {
		t.Fatalf("result %s", res)
	}
}

func TestStoreFileIsOwnerOnly(t *testing.T) {
	h := newHarness(t)
	h.prepare(req(alice, "k1", "x"))
	fi, err := os.Stat(filepath.Join(h.dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// ctxVerifier blocks until its context ends and reports that it did.
// A lookup that keeps failing is tried less and less often rather than on every
// 30-second sweep, and the request still finishes when the lookup starts working.
func TestReconcileFailureBacksOff(t *testing.T) {
	h := newHarness(t)
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return nil, ErrOutcomeUnknown
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id); st != StatusReconciling {
		t.Fatalf("got %s", st)
	}
	calls, works := 0, false
	h.m.RegisterReconciler("context.share", func(ctx context.Context, p []byte, k string) ([]byte, bool, error) {
		calls++
		if works {
			return []byte(`{"id":"n"}`), true, nil
		}
		return nil, false, errors.New("the lookup failed")
	})
	sweeps := func(n int) {
		for i := 0; i < n; i++ {
			h.m.sweep(context.Background())
			h.clk.Advance(30 * time.Second)
		}
	}
	sweeps(240) // two hours
	if calls > 10 {
		t.Fatalf("%d lookups in two hours", calls)
	}
	if calls < 3 {
		t.Fatalf("only %d lookups in two hours", calls)
	}
	works = true
	h.clk.Advance(reconcileBackoffMax)
	h.m.sweep(context.Background())
	if st := h.status(id); st != StatusDone {
		t.Fatalf("got %s", st)
	}
	h.m.sweep(context.Background())
	if len(h.m.backoff) != 0 {
		t.Fatalf("%v", h.m.backoff)
	}
}

const testOrigin = "http://localhost:4318"

// Each press races to one write.
func TestSimultaneousApproveRunsOnce(t *testing.T) {
	h := newHarness(t)
	id := h.prepare(req(alice, "k1", "x"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.m.Approve(context.Background(), id)
		}()
	}
	wg.Wait()
	if h.execs.Load() != 1 || h.status(id) != StatusDone {
		t.Fatalf("execs=%d status=%s", h.execs.Load(), h.status(id))
	}
}

// A request can't run without an approval: not by preparing, sweeping or restarting.
func TestNothingRunsWithoutApproval(t *testing.T) {
	h := newHarness(t)
	id := h.prepare(req(alice, "k1", "x"))
	h.m.sweep(context.Background())
	h.m = h.open()
	h.m.sweep(context.Background())
	if h.execs.Load() != 0 || h.status(id) != StatusPending {
		t.Fatalf("execs=%d status=%s", h.execs.Load(), h.status(id))
	}
}

// Expiry, sign-out, another account and a decline all leave nothing to approve.
func TestApproveRefusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.prepare(req(alice, "expires", "x"))
	h.clk.Advance(DefaultTTL + time.Second)
	if st, err := h.approve(id); err == nil && st != StatusExpired {
		t.Fatalf("expired: %s %v", st, err)
	}
	id = h.prepare(req(alice, "decline", "x"))
	if err := h.m.Deny(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st, err := h.approve(id); err == nil && st != StatusDenied {
		t.Fatalf("declined: %s %v", st, err)
	}
	id = h.prepare(req(alice, "signout", "x"))
	h.signOut()
	if _, err := h.approve(id); err == nil {
		t.Fatal("approved signed out")
	}
	h.who.Store(alice)
	id = h.prepare(req(alice, "switch", "x"))
	h.who.Store(bob)
	if st, err := h.approve(id); err == nil && st != StatusCanceled {
		t.Fatalf("other account: %s %v", st, err)
	}
	if h.execs.Load() != 0 {
		t.Fatalf("%d writes", h.execs.Load())
	}
}

// A stored payload altered after it was prepared is refused: the hash binds it.
func TestAlteredPayloadRefused(t *testing.T) {
	h := newHarness(t)
	id := h.prepare(req(alice, "k1", "x"))
	if err := h.m.st.update(func(s *state) error {
		s.Records[id].Payload = []byte(`{"title":"swapped","body":"y"}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.approve(id); !errors.Is(err, ErrPayloadChanged) {
		t.Fatalf("got %v", err)
	}
	if h.execs.Load() != 0 {
		t.Fatal("wrote")
	}
}
