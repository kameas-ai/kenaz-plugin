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

// fakeVerifier stands in for the browser-completed Fleet sign-in.
type fakeVerifier struct {
	mu   sync.Mutex
	who  Identity
	fail bool
	gate chan struct{} // if set, waits for it before answering
}

func (v *fakeVerifier) VerifyIdentity(ctx context.Context, open func(string) error, complete string) (Identity, error) {
	if !strings.HasPrefix(complete, "http://localhost:") || !strings.Contains(complete, PagePrefix+"enroll/complete?c=") {
		return Identity{}, errors.New("bad completion url")
	}
	open("https://id.example.test/authorize?client_id=x")
	if v.gate != nil {
		select {
		case <-v.gate:
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail {
		return Identity{}, errors.New("denied")
	}
	return v.who, nil
}

type harness struct {
	t     *testing.T
	m     *Manager
	dir   string
	clk   *clock.Fake
	who   whoBox
	ver   *fakeVerifier
	logs  *bytes.Buffer
	execs atomic.Int32
	// remote is the fake destination: idempotency key -> payload.
	remoteMu sync.Mutex
	remote   map[string][]byte
	execHook func(ctx context.Context, payload []byte, key string) ([]byte, error)
	binds    map[string]string
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

// bind returns the ceremony secret the browser that completed sign-in
// holds, completing the identity provider redirect once.
func (h *harness) bind(eid string) string {
	h.t.Helper()
	if s, ok := h.binds[eid]; ok {
		return s
	}
	h.m.mu.Lock()
	e := h.m.enrolls[eid]
	tok := ""
	if e != nil {
		tok = e.completeToken
	}
	h.m.mu.Unlock()
	_, sec, err := h.m.CompleteEnrollment(context.Background(), tok, testOrigin)
	if err != nil {
		return ""
	}
	h.binds[eid] = sec
	return sec
}

func newHarness(t *testing.T, withVerifier bool) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), clk: clock.NewFake(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)), logs: &bytes.Buffer{}, remote: map[string][]byte{}, binds: map[string]string{}}
	h.who.Store(alice)
	if withVerifier {
		h.ver = &fakeVerifier{who: alice}
	}
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
	if h.ver != nil {
		opt.Verifier = h.ver
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

// enroll runs a complete first-registration ceremony for a.
func (h *harness) enroll(a *vAuth) {
	h.t.Helper()
	ctx := context.Background()
	st, err := h.m.StartEnrollment(ctx, EnrollFirst, testOrigin)
	if err != nil {
		h.t.Fatal(err)
	}
	h.waitVerified(st.EnrollID)
	c, err := h.m.BeginRegistration(ctx, st.EnrollID, testOrigin, h.bind(st.EnrollID))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.m.FinishRegistration(ctx, st.EnrollID, testOrigin, h.bind(st.EnrollID), a.register(h.t, c)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) waitVerified(eid string) EnrollState {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := h.m.EnrollmentState(eid)
		if err != nil {
			h.t.Fatal(err)
		}
		if st.Verified || st.Error != "" {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("identity verification did not finish")
	return EnrollState{}
}

func (h *harness) approve(id string, a *vAuth) (Status, error) {
	h.t.Helper()
	opts, err := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	if err != nil {
		return "", err
	}
	return h.m.FinishApproval(context.Background(), id, testOrigin, "s1", a.assert(h.t, opts))
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
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
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
	st, err = h.approve(id, a)
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
	if len(rec.Payload) != 0 || rec.Summary != "" || rec.ReceiptSHA256 != "" {
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
	h := newHarness(t, true)
	h.prepare(req(alice, "k1", "one"))
	if _, _, err := h.m.Prepare(context.Background(), req(alice, "k1", "two")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestPrepareValidation(t *testing.T) {
	h := newHarness(t, true)
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

func TestNoAuthenticatorIsApprovalUnavailable(t *testing.T) {
	h := newHarness(t, false) // this build cannot enroll
	id, st, err := h.m.Prepare(context.Background(), req(alice, "k1", "x"))
	if err != nil || st != StatusApprovalUnavailable || id != "" {
		t.Fatalf("got %q %s %v", id, st, err)
	}
	if n := len(h.diskState().Records); n != 0 {
		t.Fatalf("unavailable request was stored: %d", n)
	}
	if _, err := h.m.StartEnrollment(context.Background(), EnrollFirst, testOrigin); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("enrollment without verifier: %v", err)
	}
	// With a verifier but nothing enrolled, the request waits and the
	// approval step asks for enrollment instead of approving.
	h2 := newHarness(t, true)
	id = h2.prepare(req(alice, "k1", "x"))
	if _, err := h2.m.BeginApproval(context.Background(), id, testOrigin, "s1"); !errors.Is(err, ErrEnrollmentRequired) {
		t.Fatalf("begin without credential: %v", err)
	}
	if h2.status(id) != StatusPending || h2.execs.Load() != 0 {
		t.Fatal("request moved without an authenticator")
	}
}

func TestForgedAssertionRejected(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k1", "x"))
	opts, err := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	if err != nil {
		t.Fatal(err)
	}
	// Same credential id, different key: a forged signature.
	forger := newVAuth(t)
	forger.credID, forger.userHandle, forger.count = a.credID, a.userHandle, a.count
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", forger.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("forged: %v", err)
	}
	// Tampered signature bytes on an otherwise valid assertion.
	opts, _ = h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	body := a.assert(t, opts)
	var m map[string]any
	json.Unmarshal(body, &m)
	resp := m["response"].(map[string]any)
	sig, _ := b64.DecodeString(resp["signature"].(string))
	sig[len(sig)-1] ^= 0xff
	resp["signature"] = b64.EncodeToString(sig)
	body, _ = json.Marshal(m)
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", body); !errors.Is(err, ErrAssertion) {
		t.Fatalf("tampered: %v", err)
	}
	if h.status(id) != StatusPending || h.execs.Load() != 0 {
		t.Fatal("forged assertion had an effect")
	}
}

func TestReplayedAssertionRejected(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k1", "x"))
	opts, _ := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	body := a.assert(t, opts)
	if st, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", body); err != nil || st != StatusDone {
		t.Fatalf("first: %s %v", st, err)
	}
	// Replaying the same assertion against the same request.
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", body); !errors.Is(err, ErrChallenge) {
		t.Fatalf("replay same request: %v", err)
	}
	// Replaying it against a new request after a fresh begin.
	id2 := h.prepare(req(alice, "k2", "y"))
	if _, err := h.m.BeginApproval(context.Background(), id2, testOrigin, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.FinishApproval(context.Background(), id2, testOrigin, "s1", body); !errors.Is(err, ErrAssertion) {
		t.Fatalf("replay other request: %v", err)
	}
	if h.execs.Load() != 1 || h.status(id2) != StatusPending {
		t.Fatalf("replay executed: %d", h.execs.Load())
	}
	// The challenge is single use even on failure: a correct assertion for
	// the consumed challenge no longer works.
	opts2, _ := h.m.BeginApproval(context.Background(), id2, testOrigin, "s1")
	bad := newVAuth(t)
	h.m.FinishApproval(context.Background(), id2, testOrigin, "s1", bad.assert(t, opts2))
	if _, err := h.m.FinishApproval(context.Background(), id2, testOrigin, "s1", a.assert(t, opts2)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("consumed challenge reused: %v", err)
	}
}

func TestMissingUVOrUPRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*vAuth)
	}{{"no UV", func(a *vAuth) { a.noUV = true }}, {"no UP", func(a *vAuth) { a.noUP = true }}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			a := newVAuth(t)
			h.enroll(a)
			id := h.prepare(req(alice, "k1", "x"))
			tc.set(a)
			if _, err := h.approve(id, a); !errors.Is(err, ErrAssertion) {
				t.Fatalf("got %v", err)
			}
			if h.status(id) != StatusPending || h.execs.Load() != 0 {
				t.Fatal("executed without UV/UP")
			}
		})
	}
	// Registration without UV is refused too.
	h := newHarness(t, true)
	a := newVAuth(t)
	a.noUV = true
	st, _ := h.m.StartEnrollment(context.Background(), EnrollFirst, testOrigin)
	h.waitVerified(st.EnrollID)
	c, _ := h.m.BeginRegistration(context.Background(), st.EnrollID, testOrigin, h.bind(st.EnrollID))
	if err := h.m.FinishRegistration(context.Background(), st.EnrollID, testOrigin, h.bind(st.EnrollID), a.register(t, c)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("registration without UV: %v", err)
	}
	if h.m.Enrolled(context.Background()) {
		t.Fatal("credential stored without UV")
	}
}

func TestAlteredPayloadRejected(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k1", "original"))
	opts, _ := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	// Something rewrites the stored payload or destination after the page
	// was shown.
	h.m.st.update(func(s *state) error {
		s.Records[id].Payload = []byte(`{"title":"Flaky e2e","body":"exfiltrate"}`)
		s.Records[id].TargetIDs = []string{"team:Everyone"}
		return nil
	})
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", a.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("altered payload: %v", err)
	}
	if h.execs.Load() != 0 {
		t.Fatal("altered payload executed")
	}
	// A rewrite that also updates the stored hash still fails: the
	// challenge is bound to the hash shown when it was issued.
	id2 := h.prepare(req(alice, "k2", "original"))
	opts, _ = h.m.BeginApproval(context.Background(), id2, testOrigin, "s1")
	h.m.st.update(func(s *state) error {
		p := []byte(`{"body":"exfiltrate"}`)
		s.Records[id2].Payload, s.Records[id2].PayloadSHA256 = p, payloadHash(p)
		return nil
	})
	if _, err := h.m.FinishApproval(context.Background(), id2, testOrigin, "s1", a.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("altered payload+hash: %v", err)
	}
	if h.execs.Load() != 0 {
		t.Fatal("altered payload executed")
	}
}

func TestWrongCredentialAccountOrigin(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	// Bob enrolls his own key on the same machine.
	h.who.Store(bob)
	h.ver.mu.Lock()
	h.ver.who = bob
	h.ver.mu.Unlock()
	b := newVAuth(t)
	h.enroll(b)
	h.who.Store(alice)
	id := h.prepare(req(alice, "k1", "x"))

	// Wrong credential: Bob's key cannot approve Alice's request.
	opts, _ := h.m.BeginApproval(ctx, id, testOrigin, "s1")
	if _, err := h.m.FinishApproval(ctx, id, testOrigin, "s1", b.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("wrong credential: %v", err)
	}
	// Wrong origin in clientData.
	opts, _ = h.m.BeginApproval(ctx, id, testOrigin, "s1")
	a.origin = "http://evil.example"
	if _, err := h.m.FinishApproval(ctx, id, testOrigin, "s1", a.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("wrong clientData origin: %v", err)
	}
	a.origin = ""
	// Finishing from a different page origin than the one that began.
	opts, _ = h.m.BeginApproval(ctx, id, testOrigin, "s1")
	if _, err := h.m.FinishApproval(ctx, id, "http://localhost:9999", "s1", a.assert(t, opts)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("wrong page origin: %v", err)
	}
	// Wrong RP ID hashed by the authenticator.
	opts, _ = h.m.BeginApproval(ctx, id, testOrigin, "s1")
	a.rpID = "evil.example"
	if _, err := h.m.FinishApproval(ctx, id, testOrigin, "s1", a.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("wrong rp id: %v", err)
	}
	a.rpID = ""
	if h.execs.Load() != 0 || h.status(id) != StatusPending {
		t.Fatal("rejected assertion had an effect")
	}
	// Wrong account: the session switches to Bob between begin and finish.
	opts, _ = h.m.BeginApproval(ctx, id, testOrigin, "s1")
	h.who.Store(bob)
	if _, err := h.m.FinishApproval(ctx, id, testOrigin, "s1", a.assert(t, opts)); err == nil {
		t.Fatal("account switch: approved")
	}
	h.who.Store(alice)
	if st := h.status(id); st != StatusCanceled || h.execs.Load() != 0 {
		t.Fatalf("account switch: %s execs=%d", st, h.execs.Load())
	}
	if len(h.diskState().Records[id].Payload) != 0 {
		t.Fatal("canceled request kept its payload")
	}
}

func TestExpiredChallengeAndRequest(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k1", "x"))
	opts, _ := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	h.clk.Advance(DefaultTTL + time.Second)
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", a.assert(t, opts)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("expired challenge: %v", err)
	}
	if st := h.status(id); st != StatusExpired || h.execs.Load() != 0 {
		t.Fatalf("expired: %s", st)
	}
	if len(h.diskState().Records[id].Payload) != 0 {
		t.Fatal("expired request kept its payload")
	}
	if _, err := h.m.BeginApproval(context.Background(), id, testOrigin, "s1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("begin on expired: %v", err)
	}
}

func TestSimultaneousExecuteRunsOnce(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	release := make(chan struct{})
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		<-release
		return []byte(`{}`), nil
	}
	id := h.prepare(req(alice, "k1", "x"))
	opts, _ := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	body := a.assert(t, opts)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", body)
			results <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 || h.execs.Load() != 1 {
		t.Fatalf("successes=%d execs=%d", ok, h.execs.Load())
	}
	// Direct concurrent execute calls with a forged receipt do nothing.
	id2 := h.prepare(req(alice, "k2", "y"))
	var wg2 sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg2.Add(1)
		go func() { defer wg2.Done(); h.m.execute(context.Background(), id2, []byte("forged-receipt")) }()
	}
	wg2.Wait()
	if h.execs.Load() != 1 {
		t.Fatalf("execute without a receipt ran: %d", h.execs.Load())
	}
}

func TestLogoutDuringApproval(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k1", "secret note"))
	opts, _ := h.m.BeginApproval(context.Background(), id, testOrigin, "s1")
	h.m.SignOut() // kenaz logout reaches the daemon lane's Purge
	if _, err := h.m.FinishApproval(context.Background(), id, testOrigin, "s1", a.assert(t, opts)); err == nil {
		t.Fatal("approved after logout")
	}
	if st := h.status(id); st != StatusCanceled || h.execs.Load() != 0 {
		t.Fatalf("after logout: %s", st)
	}
	raw, _ := os.ReadFile(filepath.Join(h.dir, StateFile))
	if bytes.Contains(raw, []byte("secret note")) {
		t.Fatal("payload survived logout")
	}
	// Signed out (session gone) without a daemon purge also cancels.
	id2 := h.prepare(req(alice, "k2", "x"))
	h.signOut()
	h.who.Store(alice)
	h.m.SignOut()
	if st := h.status(id2); st != StatusCanceled {
		t.Fatalf("second: %s", st)
	}
	// And the offline path used by `kenaz logout` while no daemon runs.
	id3 := h.prepare(req(alice, "k3", "offline secret"))
	if err := CancelAllOffline(h.dir, testKey, h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	m2 := h.open()
	if st, _, _ := m2.Status(context.Background(), id3); st != StatusCanceled {
		t.Fatalf("offline cancel: %s", st)
	}
}

func TestSessionLossCancelsOpenRequests(t *testing.T) {
	h := newHarness(t, true)
	id := h.prepare(req(alice, "k1", "x"))
	h.signOut()
	h.m.Status(context.Background(), id)
	h.who.Store(alice)
	if st := h.status(id); st != StatusCanceled {
		t.Fatalf("got %s", st)
	}
}

func TestBootstrapRegistrationRace(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	s1, _ := h.m.StartEnrollment(ctx, EnrollFirst, testOrigin)
	s2, _ := h.m.StartEnrollment(ctx, EnrollFirst, testOrigin)
	h.waitVerified(s1.EnrollID)
	h.waitVerified(s2.EnrollID)
	c1, err1 := h.m.BeginRegistration(ctx, s1.EnrollID, testOrigin, h.bind(s1.EnrollID))
	c2, err2 := h.m.BeginRegistration(ctx, s2.EnrollID, testOrigin, h.bind(s2.EnrollID))
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	a1, a2 := newVAuth(t), newVAuth(t)
	if err := h.m.FinishRegistration(ctx, s1.EnrollID, testOrigin, h.bind(s1.EnrollID), a1.register(t, c1)); err != nil {
		t.Fatal(err)
	}
	if err := h.m.FinishRegistration(ctx, s2.EnrollID, testOrigin, h.bind(s2.EnrollID), a2.register(t, c2)); !errors.Is(err, ErrEnrollmentRace) {
		t.Fatalf("second registration: %v", err)
	}
	if n := len(h.diskState().Credentials); n != 1 {
		t.Fatalf("credentials = %d", n)
	}
	// Once enrolled, a new ceremony cannot silently register another
	// authenticator: "first" is not offered, and "add" needs an assertion
	// from the existing one.
	if _, err := h.m.StartEnrollment(ctx, EnrollFirst, testOrigin); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("first after enrolled: %v", err)
	}
	s3, err := h.m.StartEnrollment(ctx, EnrollAdd, testOrigin)
	if err != nil || !s3.NeedsAssertion {
		t.Fatalf("add: %+v %v", s3, err)
	}
	if _, err := h.m.BeginRegistration(ctx, s3.EnrollID, testOrigin, h.bind(s3.EnrollID)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("add without assertion: %v", err)
	}
	opts, _ := h.m.BeginEnrollAssertion(ctx, s3.EnrollID, testOrigin)
	if _, err := h.m.FinishEnrollAssertion(ctx, s3.EnrollID, testOrigin, a2.assert(t, opts)); !errors.Is(err, ErrAssertion) {
		t.Fatalf("add with unenrolled key: %v", err)
	}
	opts, _ = h.m.BeginEnrollAssertion(ctx, s3.EnrollID, testOrigin)
	sec, err := h.m.FinishEnrollAssertion(ctx, s3.EnrollID, testOrigin, a1.assert(t, opts))
	if err != nil {
		t.Fatal(err)
	}
	h.binds[s3.EnrollID] = sec
	c3, err := h.m.BeginRegistration(ctx, s3.EnrollID, testOrigin, h.bind(s3.EnrollID))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.FinishRegistration(ctx, s3.EnrollID, testOrigin, h.bind(s3.EnrollID), a2.register(t, c3)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.diskState().Credentials); n != 2 {
		t.Fatalf("credentials after add = %d", n)
	}
}

func TestEnrollmentAccountMismatchAndRecovery(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	h.ver.who = bob // the browser signs in as someone else
	st, _ := h.m.StartEnrollment(ctx, EnrollFirst, testOrigin)
	if got := h.waitVerified(st.EnrollID); got.Error != "account_mismatch" || got.Verified {
		t.Fatalf("mismatch: %+v", got)
	}
	if _, err := h.m.BeginRegistration(ctx, st.EnrollID, testOrigin, h.bind(st.EnrollID)); err == nil {
		t.Fatal("registration after mismatch")
	}
	h.ver.who = alice
	old := newVAuth(t)
	h.enroll(old)
	id := h.prepare(req(alice, "k1", "x"))
	// Recovery: identity re-verified; old credentials and open requests
	// are revoked.
	s2, err := h.m.StartEnrollment(ctx, EnrollRecover, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	h.waitVerified(s2.EnrollID)
	c, _ := h.m.BeginRegistration(ctx, s2.EnrollID, testOrigin, h.bind(s2.EnrollID))
	fresh := newVAuth(t)
	if err := h.m.FinishRegistration(ctx, s2.EnrollID, testOrigin, h.bind(s2.EnrollID), fresh.register(t, c)); err != nil {
		t.Fatal(err)
	}
	if st := h.status(id); st != StatusCanceled {
		t.Fatalf("open request survived recovery: %s", st)
	}
	id2 := h.prepare(req(alice, "k2", "y"))
	if _, err := h.approve(id2, old); !errors.Is(err, ErrAssertion) {
		t.Fatalf("revoked key approved: %v", err)
	}
	if st, err := h.approve(id2, fresh); err != nil || st != StatusDone {
		t.Fatalf("new key: %s %v", st, err)
	}
}

func TestRemoteSuccessThenCrashReconciles(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
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
	go func() { defer close(crashed); h.approve(id, a) }()
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
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return nil, ErrOutcomeUnknown // timeout before the destination answered
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id, a); st != StatusReconciling {
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
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	h.execHook = func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return nil, ErrOutcomeUnknown
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id, a); st != StatusReconciling {
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
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) {
		return nil, errors.Join(ErrorCode("credential_detected"), errors.New("body echo SECRET"))
	}
	id := h.prepare(req(alice, "k1", "x"))
	if st, _ := h.approve(id, a); st != StatusFailed {
		t.Fatalf("got %s", st)
	}
	_, res, _ := h.m.Status(context.Background(), id)
	if string(res) != `{"error":"credential_detected"}` {
		t.Fatalf("result %s", res)
	}
}

func TestOfflineApprovalWithEnrolledAuthenticator(t *testing.T) {
	// Approval needs no network: the identity comes from the local session
	// and verification is local. A local executor (memory) completes.
	h := newHarness(t, false) // no verifier: offline, cannot enroll now
	a := newVAuth(t)
	// Enroll earlier while online.
	online := newHarness(t, true)
	online.dir = h.dir
	online.m = online.open()
	online.enroll(a)
	h.m = h.open()
	h.m.Register("memory.save", func(ctx context.Context, p []byte, k string) ([]byte, error) {
		return []byte(`{"saved":true}`), nil
	})
	h.m.RegisterRenderer("memory.save", renderTest)
	r := req(alice, "m1", "remember this")
	r.Operation = "memory.save"
	id := h.prepare(r)
	if st, err := h.approve(id, a); err != nil || st != StatusDone {
		t.Fatalf("offline approve: %s %v", st, err)
	}
}

func TestRestartCancelsApprovedButNotExecuted(t *testing.T) {
	h := newHarness(t, true)
	id := h.prepare(req(alice, "k1", "x"))
	h.m.st.update(func(s *state) error { s.Records[id].Status = StatusApproved; return nil })
	m2 := h.open()
	if st, _, _ := m2.Status(context.Background(), id); st != StatusCanceled {
		t.Fatalf("got %s", st)
	}
}

func TestStoreFileIsOwnerOnly(t *testing.T) {
	h := newHarness(t, true)
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
type ctxVerifier struct{ ended chan error }

func (v ctxVerifier) VerifyIdentity(ctx context.Context, open func(string) error, _ string) (Identity, error) {
	open("https://id.example.test/authorize?client_id=x")
	<-ctx.Done()
	v.ended <- ctx.Err()
	return Identity{}, ctx.Err()
}

// Signing out drops enrollment ceremonies and cancels their identity
// sign-in, instead of leaving it waiting for its full lifetime.
func TestSignOutCancelsEnrollmentSignIn(t *testing.T) {
	h := newHarness(t, false)
	v := ctxVerifier{ended: make(chan error, 1)}
	h.ver = nil
	m, err := New(Options{Dir: t.TempDir(), Clock: h.clk, BaseURL: func() string { return testOrigin }, StoreKey: testKey, Verifier: v,
		Identity: func(context.Context) (Identity, error) { return alice, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartEnrollment(context.Background(), EnrollFirst, testOrigin); err != nil {
		t.Fatal(err)
	}
	m.SignOut()
	select {
	case err := <-v.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sign-in ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dropped ceremony's sign-in was not canceled")
	}
}

// The completion redirect waits for a slow identity verification (the
// verifier may redirect the browser before exchanging the code), up to
// completeWait, and stops at once when the browser goes away.
func TestCompleteEnrollmentWaitsForSlowVerification(t *testing.T) {
	if completeWait < 60*time.Second {
		t.Fatalf("completion waits only %v", completeWait)
	}
	h := newHarness(t, true)
	gate := make(chan struct{})
	h.ver.gate = gate
	st, err := h.m.StartEnrollment(context.Background(), EnrollFirst, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	tok := completeToken(h.m, st.EnrollID)

	// The browser leaves: the wait ends with its context.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, _, err := h.m.CompleteEnrollment(ctx, tok, testOrigin); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("canceled wait: %v after %v", err, time.Since(start))
	}

	// Verification finishes only after the browser's request started
	// waiting: the wait picks it up.
	go func() { time.Sleep(300 * time.Millisecond); close(gate) }()
	id, sec, err := h.m.CompleteEnrollment(context.Background(), tok, testOrigin)
	if err != nil || id != st.EnrollID || sec == "" {
		t.Fatalf("slow verification: %q %q %v", id, sec, err)
	}
}

// When verification never answers, the wait gives up at completeWait.
func TestCompleteEnrollmentGivesUp(t *testing.T) {
	old := completeWait
	completeWait = 200 * time.Millisecond
	t.Cleanup(func() { completeWait = old })
	h := newHarness(t, true)
	h.ver.gate = make(chan struct{})
	st, err := h.m.StartEnrollment(context.Background(), EnrollFirst, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.m.CompleteEnrollment(context.Background(), completeToken(h.m, st.EnrollID), testOrigin); !errors.Is(err, ErrChallenge) {
		t.Fatalf("got %v", err)
	}
}
