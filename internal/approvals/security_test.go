package approvals

// Regression tests for the security review of the approval protocol. The
// threat is a tool or a process that can only write files: it must not cause a write or
// a deletion without the person's approval.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// Finding 1 (PoC): an agent writes a "reconciling" record with its own
// payload while the relay is stopped. It never executes.
func TestForgedReconcilingRecordNeverExecutes(t *testing.T) {
	h := newHarness(t)
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

// Finding 1: with no OS keychain there is no key outside user-writable
// files, so approvals are unavailable and an existing file is not trusted.
func TestNoStoreKeyIsApprovalUnavailable(t *testing.T) {
	h := newHarness(t)
	m, err := New(Options{Dir: h.dir, Identity: func(context.Context) (Identity, error) { return alice, nil }})
	if err != nil {
		t.Fatal(err)
	}
	m.Register("context.share", h.exec)
	m.RegisterRenderer("context.share", renderTest)
	if id, st, err := m.Prepare(context.Background(), req(alice, "k", "x")); st != StatusApprovalUnavailable || id != "" || err != nil {
		t.Fatalf("prepare: %q %s %v", id, st, err)
	}
	if _, err := m.Approve(context.Background(), "apr_aaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("approve: %v", err)
	}
}

// Finding 4: the page shows only what the daemon derives from the stored
// payload; caller-supplied summary, impact and targets are ignored.
func TestPageTextComesFromPayload(t *testing.T) {
	h := newHarness(t)
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
	h := newHarness(t)
	r := req(alice, "skills.publish:abc", "x")
	id := h.prepare(r)
	if err := h.m.Deny(context.Background(), id); err != nil {
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

// Finding 7: a reconciling request does not survive sign-out: it closes
// as outcome_unknown, its content is purged and it never runs later.
func TestSignOutClosesReconciling(t *testing.T) {
	h := newHarness(t)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { return nil, ErrOutcomeUnknown }
	id := h.prepare(req(alice, "k1", "secret text"))
	if st, _ := h.approve(id); st != StatusReconciling {
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
	h := newHarness(t)
	var got Identity
	h.execHook = func(ctx context.Context, _ []byte, _ string) ([]byte, error) {
		got, _ = RecordIdentity(ctx)
		if err := CheckSession(ctx, bob.Env, bob.OrgID, bob.UserID); err == nil {
			t.Error("CheckSession accepted another account")
		}
		return []byte(`{}`), CheckSession(ctx, alice.Env, alice.OrgID, alice.UserID)
	}
	id := h.prepare(req(alice, "k", "x"))
	if st, err := h.approve(id); err != nil || st != StatusDone || got != alice {
		t.Fatalf("%s %v %+v", st, err, got)
	}
	if err := CheckSession(context.Background(), alice.Env, alice.OrgID, alice.UserID); err == nil {
		t.Fatal("CheckSession without an approved identity")
	}
}
