package approvals

// Regression tests for the security review of the approval protocol. The
// threat is a local agent running as the same OS user, with a shell: it
// must not cause a write, an enrollment or a deletion without a real
// WebAuthn UV+UP assertion from the human.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// writeRawState replaces approvals.json with st as an agent would: it does
// not have the keychain key, so it can only write unauthenticated JSON or
// reuse an old MAC.
func writeRawState(t *testing.T, dir string, st state, mac string) {
	t.Helper()
	inner, _ := json.Marshal(st)
	var b []byte
	if mac == "" {
		b = inner // the pre-authentication format
	} else {
		b, _ = json.Marshal(sealedFile{Version: sealedVersion, State: inner, MAC: mac})
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSealed(t *testing.T, dir string) sealedFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var f sealedFile
	json.Unmarshal(b, &f)
	return f
}

// Finding 1 (PoC): with the relay stopped, an agent adds its own software
// credential to approvals.json. The file fails authentication on restart,
// is set aside, and the injected credential approves nothing.
func TestInjectedCredentialInStoreRejected(t *testing.T) {
	for _, keepMAC := range []bool{false, true} {
		h := newHarness(t, true)
		h.prepare(req(alice, "seed", "x"))
		old := readSealed(t, h.dir)
		s := h.diskState()
		agent := newVAuth(t)
		handle := make([]byte, 32)
		if s.UserHandles == nil {
			s.UserHandles = map[string][]byte{}
		}
		s.UserHandles[alice.key()] = handle
		agent.userHandle = handle
		s.Credentials = append(s.Credentials, storedCredential{Identity: alice, Credential: webauthn.Credential{
			ID: agent.credID, PublicKey: agent.coseKey(t), AttestationType: "none",
			Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true}}})
		mac := ""
		if keepMAC {
			mac = old.MAC
		}
		writeRawState(t, h.dir, s, mac)
		h.m = h.open() // relay restarted
		if _, err := os.Stat(filepath.Join(h.dir, StateFile+".rejected")); err != nil {
			t.Fatalf("tampered store not set aside: %v", err)
		}
		id := h.prepare(req(alice, "k", "agent payload"))
		if _, err := h.approve(id, agent); !errors.Is(err, ErrEnrollmentRequired) {
			t.Fatalf("approve with injected credential: %v", err)
		}
		if h.m.Enrolled(context.Background()) || h.execs.Load() != 0 {
			t.Fatal("injected credential trusted")
		}
	}
}

// Finding 1 (PoC): an agent writes a "reconciling" record with its own
// payload while the relay is stopped. It never executes.
func TestForgedReconcilingRecordNeverExecutes(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	s := h.diskState()
	p := []byte(`{"title":"forged","body":"never approved"}`)
	now := h.clk.Now()
	forged := &record{ID: "apr_aaaaaaaaaaaaaaaaaaaaaaaaaa", Operation: "context.share", Identity: alice, Payload: p,
		PayloadSHA256: payloadHash(p), IdempotencyKey: "forged", Status: StatusReconciling, ProfileGen: s.ProfileGen,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now}
	s.Records[forged.ID] = forged
	writeRawState(t, h.dir, s, readSealed(t, h.dir).MAC)
	h.m = h.open()
	h.m.RegisterReconciler("context.share", func(context.Context, []byte, string) ([]byte, bool, error) { return nil, false, nil })
	h.m.List(context.Background())
	if h.execs.Load() != 0 || h.remote["forged"] != nil {
		t.Fatalf("forged file record executed: %d", h.execs.Load())
	}
}

// Finding 1, defense in depth: even a record inside the authenticated
// store needs a verified assertion over its exact payload before any
// transition into executing. A record without proof, or with a real proof
// moved onto another payload, never runs.
func TestReconcileRequiresApprovalProof(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { return nil, ErrOutcomeUnknown }
	id := h.prepare(req(alice, "k1", "approved text"))
	if st, _ := h.approve(id, a); st != StatusReconciling {
		t.Fatalf("got %s", st)
	}
	h.execHook = nil
	h.m.RegisterReconciler("context.share", func(context.Context, []byte, string) ([]byte, bool, error) { return nil, false, nil })
	execs := h.execs.Load()

	// The real proof on a different payload.
	h.m.st.update(func(s *state) error {
		r := s.Records[id]
		r.Payload = []byte(`{"title":"Flaky e2e","body":"swapped"}`)
		r.PayloadSHA256 = payloadHash(r.Payload)
		return nil
	})
	h.m.sweep(context.Background())
	if h.execs.Load() != execs || h.status(id) != StatusReconciling {
		t.Fatal("proof accepted for another payload")
	}
	// No proof at all.
	p := []byte(`{"title":"x","body":"no proof"}`)
	h.m.st.update(func(s *state) error {
		s.Records["apr_bbbbbbbbbbbbbbbbbbbbbbbbbb"] = &record{ID: "apr_bbbbbbbbbbbbbbbbbbbbbbbbbb", Operation: "context.share", Identity: alice,
			Payload: p, PayloadSHA256: payloadHash(p), IdempotencyKey: "x", Status: StatusReconciling, ProfileGen: s.ProfileGen}
		return nil
	})
	h.m.sweep(context.Background())
	if h.execs.Load() != execs {
		t.Fatal("record without proof executed")
	}
	// An approved record without proof is canceled, not executed.
	id2 := h.prepare(req(alice, "k2", "y"))
	receipt := []byte("0123456789abcdef0123456789abcdef")
	h.m.st.update(func(s *state) error { s.Records[id2].Status = StatusApproved; return nil })
	h.m.mu.Lock()
	h.m.receipts[id2] = receipt
	h.m.mu.Unlock()
	if st := h.m.execute(context.Background(), id2, receipt); st != StatusCanceled || h.execs.Load() != execs {
		t.Fatalf("approved without proof: %s", st)
	}
}

// Finding 1: with no OS keychain there is no key outside user-writable
// files, so approvals are unavailable and an existing file is not trusted.
func TestNoStoreKeyIsApprovalUnavailable(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	m, err := New(Options{Dir: h.dir, Identity: func(context.Context) (Identity, error) { return alice, nil }, Verifier: h.ver})
	if err != nil {
		t.Fatal(err)
	}
	m.Register("context.share", h.exec)
	m.RegisterRenderer("context.share", renderTest)
	if id, st, err := m.Prepare(context.Background(), req(alice, "k", "x")); st != StatusApprovalUnavailable || id != "" || err != nil {
		t.Fatalf("prepare: %q %s %v", id, st, err)
	}
	if _, err := m.StartEnrollment(context.Background(), EnrollFirst, testOrigin); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("enroll: %v", err)
	}
	if m.Enrolled(context.Background()) {
		t.Fatal("keyless manager trusted the file's credentials")
	}
}

// Finding 3: a local process that starts the ceremony through the API
// cannot register a key: registration needs the HttpOnly secret that only
// the browser redirected back by the identity provider receives, and the
// API never returns the sign-in URL or the completion token.
func TestEnrollmentBoundToSignedInBrowser(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{EnrollFirst, EnrollRecover} {
		h := newHarness(t, true)
		human := newVAuth(t)
		if mode == EnrollRecover {
			h.enroll(human)
		}
		st, err := h.m.StartEnrollment(ctx, mode, testOrigin)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(h.waitVerified(st.EnrollID))
		if strings.Contains(string(raw), "id.example.test") || strings.Contains(string(raw), "complete") {
			t.Fatalf("state leaks the sign-in or completion URL: %s", raw)
		}
		for _, bind := range []string{"", "guessed-secret"} {
			if _, err := h.m.BeginRegistration(ctx, st.EnrollID, testOrigin, bind); err == nil {
				t.Fatalf("%s: registration began without the browser binding", mode)
			}
		}
		if _, _, err := h.m.CompleteEnrollment(ctx, "guessed-token", testOrigin); err == nil {
			t.Fatal("completion with a guessed token")
		}
		// The agent's key cannot be registered even after the human signed in.
		agent := newVAuth(t)
		c, err := h.m.BeginRegistration(ctx, st.EnrollID, testOrigin, h.bind(st.EnrollID))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.m.FinishRegistration(ctx, st.EnrollID, testOrigin, "", agent.register(t, c)); err == nil {
			t.Fatalf("%s: finished without the binding", mode)
		}
		if mode == EnrollRecover {
			id := h.prepare(req(alice, "k", "x"))
			if st, err := h.approve(id, human); err != nil || st != StatusDone {
				t.Fatalf("recover without binding wiped the human's key: %s %v", st, err)
			}
		}
		// The completion token is one use.
		h.m.mu.Lock()
		tok := h.m.enrolls[st.EnrollID].completeToken
		h.m.mu.Unlock()
		if tok != "" {
			t.Fatal("completion token not consumed")
		}
	}
}

// Finding 4: the page shows only what the daemon derives from the stored
// payload; caller-supplied summary, impact and targets are ignored.
func TestPageTextComesFromPayload(t *testing.T) {
	h := newHarness(t, true)
	r := req(alice, "k", "x")
	r.Summary, r.Impact, r.TargetIDs = "Totally harmless", "Nothing at all", []string{"nowhere"}
	id := h.prepare(r)
	v, err := h.m.ViewRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Summary != "Share a note titled Flaky e2e" || v.Impact != "" || len(v.Targets) != 1 || v.Targets[0] != "team:Platform" {
		t.Fatalf("view: %+v", v)
	}
	// No renderer: Prepare refuses.
	h.m.Register("context.unrendered", h.exec)
	r.Operation = "context.unrendered"
	if _, _, err := h.m.Prepare(context.Background(), r); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("unrendered: %v", err)
	}
}

// Finding 5 (PoC): a denied or expired request does not block preparing
// the same idempotency key again.
func TestDeniedOrExpiredKeyCanBePreparedAgain(t *testing.T) {
	h := newHarness(t, true)
	r := req(alice, "skills.publish:abc", "x")
	id := h.prepare(r)
	if err := h.m.Deny(context.Background(), id, "s1"); err != nil {
		t.Fatal(err)
	}
	id2, st, err := h.m.Prepare(context.Background(), r)
	if err != nil || st != StatusPending || id2 == id {
		t.Fatalf("after deny: %s %s %v", id2, st, err)
	}
	h.clk.Advance(DefaultTTL + 1)
	id3, st, err := h.m.Prepare(context.Background(), r)
	if err != nil || st != StatusPending || id3 == id2 {
		t.Fatalf("after expiry: %s %s %v", id3, st, err)
	}
	// A pending request is still deduplicated.
	if id4, _, _ := h.m.Prepare(context.Background(), r); id4 != id3 {
		t.Fatal("pending request not deduplicated")
	}
}

// Finding 6: begin calls from other page sessions never evict a session's
// challenge.
func TestOtherSessionsCannotEvictChallenge(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	id := h.prepare(req(alice, "k", "x"))
	opts, err := h.m.BeginApproval(context.Background(), id, testOrigin, "human")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSessionsPerRequest+10; i++ {
		h.m.BeginApproval(context.Background(), id, testOrigin, "agent-"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
	}
	if st, err := h.m.FinishApproval(context.Background(), id, testOrigin, "human", a.assert(t, opts)); err != nil || st != StatusDone {
		t.Fatalf("human approval after begin flood: %s %v", st, err)
	}
	// Another session's assertion slot is not this session's.
	id2 := h.prepare(req(alice, "k2", "y"))
	opts, _ = h.m.BeginApproval(context.Background(), id2, testOrigin, "human")
	if _, err := h.m.FinishApproval(context.Background(), id2, testOrigin, "agent", a.assert(t, opts)); !errors.Is(err, ErrChallenge) {
		t.Fatalf("cross-session finish: %v", err)
	}
}

// Finding 7: a reconciling request does not survive sign-out: it closes
// as outcome_unknown, its content is purged and it never runs later.
func TestSignOutClosesReconciling(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { return nil, ErrOutcomeUnknown }
	id := h.prepare(req(alice, "k1", "secret text"))
	if st, _ := h.approve(id, a); st != StatusReconciling {
		t.Fatal("not reconciling")
	}
	h.execHook = nil
	h.m.SignOut()
	h.m.RegisterReconciler("context.share", func(context.Context, []byte, string) ([]byte, bool, error) { return nil, false, nil })
	h.m.sweep(context.Background())
	if st := h.status(id); st != StatusOutcomeUnknown || h.execs.Load() != 1 {
		t.Fatalf("after sign-out: %s execs=%d", st, h.execs.Load())
	}
	raw, _ := os.ReadFile(filepath.Join(h.dir, StateFile))
	if strings.Contains(string(raw), "secret text") {
		t.Fatal("payload kept after sign-out")
	}
}

// Finding 8: the executor runs with the approved identity in its context
// so it can compare it with the live session right before writing.
func TestExecutorSeesApprovedIdentity(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	h.enroll(a)
	var got Identity
	h.execHook = func(ctx context.Context, _ []byte, _ string) ([]byte, error) {
		got, _ = RecordIdentity(ctx)
		if err := CheckSession(ctx, bob.Env, bob.OrgID, bob.UserID); err == nil {
			t.Error("CheckSession accepted another account")
		}
		return []byte(`{}`), CheckSession(ctx, alice.Env, alice.OrgID, alice.UserID)
	}
	id := h.prepare(req(alice, "k", "x"))
	if st, err := h.approve(id, a); err != nil || st != StatusDone || got != alice {
		t.Fatalf("%s %v %+v", st, err, got)
	}
	if err := CheckSession(context.Background(), alice.Env, alice.OrgID, alice.UserID); err == nil {
		t.Fatal("CheckSession without an approved identity")
	}
}
