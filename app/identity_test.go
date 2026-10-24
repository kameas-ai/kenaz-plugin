package app

import (
	"bytes"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

func blockingApproval() []fleetapi.Approval {
	return []fleetapi.Approval{{ID: "a1", Kind: "legal_acceptance", Title: "Terms", Blocking: true, Required: true}}
}

// queueHeldSession signs in as the fake's user, holds the lane with a
// blocking approval and queues one session's telemetry on the device.
func queueHeldSession(t *testing.T, f *laneFleet, l *telemetryLane) {
	t.Helper()
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) && l.dec.HoldReason() != "" }) {
		t.Fatal("lane never signed in, admitted and held")
	}
	postClaudeSession(t, l, l.rx)
	if !waitFor(t, func() bool { return !l.sp.Empty() }) {
		t.Fatalf("nothing queued; status %+v", l.Status())
	}
}

// assertNothingFrom fails if any uploaded batch carries user's identity.
func assertNothingFrom(t *testing.T, f *laneFleet, user string) {
	t.Helper()
	for _, b := range f.batches() {
		if bytes.Contains(b.Body, []byte(user)) {
			t.Fatalf("a batch stamped for %s was uploaded", user)
		}
	}
}

// Account switching: telemetry queued for one
// account is purged when another account signs in (no logout in between)
// and is never uploaded with the new account's credentials.
func TestTelemetryLaneAccountSwitchDropsQueue(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
	l, _, _ := newTestLane(t, f)
	queueHeldSession(t, f, l)

	f.set(func(f *laneFleet) { f.user = "sub-2" })
	if !waitFor(t, func() bool { s, ok := l.cm.Session(); return ok && s.UserID == "sub-2" && l.sp.Empty() }) {
		t.Fatalf("queue of the previous account not purged; status %+v", l.Status())
	}
	f.set(func(f *laneFleet) { f.approvals = nil })
	time.Sleep(200 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches uploaded after the account switch", n)
	}
	assertNothingFrom(t, f, "sub-1")
}

// The same across a relay restart: the new daemon process finds another
// account signed in and purges the previous account's spool first.
func TestTelemetryLaneAccountSwitchAcrossRestart(t *testing.T) {
	dir, ks := t.TempDir(), &spool.Memory{}
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
	l1, _, stop1 := newTestLaneIn(t, f, dir, ks, nil)
	queueHeldSession(t, f, l1)
	stop1()

	f.set(func(f *laneFleet) { f.user = "sub-2"; f.approvals = nil })
	l2, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { s, ok := l2.cm.Session(); return ok && s.UserID == "sub-2" }) {
		t.Fatal("restarted lane never signed in")
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches of the previous account uploaded after restart", n)
	}
	if !l2.sp.Empty() {
		t.Fatal("previous account's spool kept")
	}
}

// Same account across a restart (e.g. an upgrade): the queue is kept and
// drains once the hold clears (no data loss, one sender).
func TestTelemetryLaneSameAccountAcrossRestartKeepsQueue(t *testing.T) {
	dir, ks := t.TempDir(), &spool.Memory{}
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
	l1, _, stop1 := newTestLaneIn(t, f, dir, ks, nil)
	queueHeldSession(t, f, l1)
	stop1()

	f.set(func(f *laneFleet) { f.approvals = nil })
	_, _, _ = newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatal("queued telemetry of the same account was not delivered after restart")
	}
	assertNothingFrom(t, f, "sub-2")
}

// Sign-out while data is queued purges it; signing back in sends nothing
// collected before the sign-out.
func TestTelemetryLaneSignOutDropsQueue(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
	l, _, _ := newTestLane(t, f)
	queueHeldSession(t, f, l)

	f.set(func(f *laneFleet) { f.signedOut = true })
	if !waitFor(t, func() bool { return !l.cm.State().SignedIn && l.sp.Empty() }) {
		t.Fatal("sign-out did not purge the queue")
	}
	f.set(func(f *laneFleet) { f.signedOut = false; f.approvals = nil })
	if !waitFor(t, func() bool { return l.dec.SignedIn() }) {
		t.Fatal("never signed back in")
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches collected before sign-out were uploaded", n)
	}
}

// Consent revoked in Fleet while data is queued: purged, never uploaded.
func TestTelemetryLaneRevokedConsentDropsQueue(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
	l, _, _ := newTestLane(t, f)
	queueHeldSession(t, f, l)

	f.set(func(f *laneFleet) { f.optedIn = false })
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatal("revocation did not purge the queue")
	}
	f.set(func(f *laneFleet) { f.approvals = nil })
	time.Sleep(200 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches uploaded after consent was revoked", n)
	}
}

// Account switch with no approval hold and no consent refresh due: the
// per-upload session check stops the previous account's spooled batches
// from being posted with the new account's credentials, and a kick (as
// sent by `kenaz login`) adopts the new account at once.
func TestTelemetryLaneAccountSwitchBeforeRefresh(t *testing.T) {
	old := testRefreshEvery
	testRefreshEvery = time.Hour
	t.Cleanup(func() { testRefreshEvery = old })

	f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: &fleetapi.APIError{Status: 503}}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("not signed in")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return !l.sp.Empty() }) {
		t.Fatal("nothing spooled after the transient failure")
	}

	f.set(func(f *laneFleet) { f.user = "sub-2"; f.postErr = nil })
	l.kick() // drain now; no refresh is due for an hour
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatal("previous account's spool not purged")
	}
	time.Sleep(100 * time.Millisecond)
	assertNothingFrom(t, f, "sub-1")
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches posted after the switch", n)
	}

	// The login kick adopts the new account; its own data then flows.
	l.requestRefresh()
	if !waitFor(t, func() bool { s, ok := l.cm.Session(); return ok && s.UserID == "sub-2" && l.dec.SignedIn() }) {
		t.Fatal("kick did not adopt the new account")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatalf("new account's data not sent; status %+v", l.Status())
	}
	assertNothingFrom(t, f, "sub-1")
}
