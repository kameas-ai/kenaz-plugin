package consent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

func earlyAccessErr() error {
	return &fleetapi.APIError{Status: 403, Code: fleetapi.CodeEarlyAccessRequired, Message: "fleet text"}
}

func lanePurges(l *fakeLanes) int { l.mu.Lock(); defer l.mu.Unlock(); return l.purged }

// Fleet refuses every plugin request but /me, capabilities and the opt-ins
// for an org outside early access (e.g. after a staff revoke). Nothing is
// collected or kept: records drop (never hold) and anything queued is
// purged. A member can still withdraw, and the local withdrawal (stop +
// purge) still runs; the opt-in record itself stays with Fleet.
func TestWithdrawalOutsideEarlyAccess(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts, fleetapi.ClassToolCalls)

	f.set(func(f *fakeFleet) { f.approvalsErr = earlyAccessErr() })
	before := lanePurges(lanes)
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("refresh outside early access: %v", err)
	}
	if lanePurges(lanes) <= before {
		t.Fatal("a refusal must purge anything queued on the device")
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop || d.Reason != ReasonEarlyAccess {
		t.Fatalf("decide outside early access: %+v (must drop, never hold)", d)
	}
	st := m.State()
	if !st.EarlyAccess || len(st.Holds) != 0 || !st.AdmittedAt.IsZero() {
		t.Fatalf("state outside early access: %+v", st)
	}
	if st.Classes[string(fleetapi.ClassUsageCounts)] != ClassOn {
		t.Fatal("the member's opt-in record stays")
	}

	// Opting in to more is refused with the early-access error.
	err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassErrors: true})
	if !fleetapi.EarlyAccessRequired(err) {
		t.Fatalf("opt-in outside early access: err=%v", err)
	}

	// Per-class opt-out works and purges.
	if err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassToolCalls: false}); err != nil {
		t.Fatalf("per-class opt-out: %v", err)
	}
	if len(lanes.classes) != 1 || lanes.classes[0] != fleetapi.ClassToolCalls {
		t.Fatalf("per-class purge: %+v", lanes)
	}
	// All-off is withdrawal: stop and purge.
	before = lanePurges(lanes)
	if err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassUsageCounts: false}); err != nil {
		t.Fatalf("withdrawal: %v", err)
	}
	if lanes.stopped != 1 || lanePurges(lanes) <= before {
		t.Fatalf("withdrawal: %+v", lanes)
	}

	// Admission: purge first, then data generated from now on is sendable.
	f.set(func(f *fakeFleet) { f.approvalsErr = nil })
	before = lanePurges(lanes)
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lanePurges(lanes) <= before {
		t.Fatal("admission must purge anything held from before")
	}
	if st := m.State(); st.EarlyAccess || st.AdmittedAt.IsZero() {
		t.Fatalf("admission not recorded: %+v", st)
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("decide after admission: %+v", d)
	}
}

// The admission moment is the client's observation time of the first
// admitted response. It is kept across restarts (so data spooled after it
// for a transient failure survives) and cleared by a refusal. A device with
// no recorded admission (e.g. data held by an earlier build) purges first.
func TestAdmissionMomentRecordedAndPurges(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "consent.json"))
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	now := t0
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	newM := func() *Manager {
		return NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store, Now: func() time.Time { return now }})
	}
	m := newM()
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lanePurges(lanes) != 1 {
		t.Fatalf("first admitted response must purge earlier data: %d", lanePurges(lanes))
	}
	if got := m.State().AdmittedAt; !got.Equal(t0) {
		t.Fatalf("admitted at %v, want %v", got, t0)
	}
	now = t0.Add(time.Hour)
	if err := m.Refresh(ctx); err != nil || lanePurges(lanes) != 1 || !m.State().AdmittedAt.Equal(t0) {
		t.Fatalf("later admitted responses must not move the moment or purge: %v %d", err, lanePurges(lanes))
	}
	// Restart: the recorded moment stands; nothing is purged.
	m2 := newM()
	if err := m2.Refresh(ctx); err != nil || lanePurges(lanes) != 1 || !m2.State().AdmittedAt.Equal(t0) {
		t.Fatalf("restart: %v purges=%d at=%v", err, lanePurges(lanes), m2.State().AdmittedAt)
	}
	// An upload refusal clears it; re-admission is a new moment.
	m2.ObserveUploadError(earlyAccessErr())
	if !store.AdmittedAt(m2.Me().OrgID).IsZero() {
		t.Fatal("a refusal must clear the recorded admission")
	}
	if st := m2.State(); !st.EarlyAccess || !st.AdmittedAt.IsZero() {
		t.Fatalf("after refusal: %+v", st)
	}
	now = t0.Add(2 * time.Hour)
	m2.ObserveEnrolled()
	if got := m2.State().AdmittedAt; !got.Equal(now) {
		t.Fatalf("re-admitted at %v, want %v", got, now)
	}
}

// Any other approvals failure still fails the refresh (no silent success).
func TestRefreshOtherApprovalsErrorStillFails(t *testing.T) {
	f := newFake(allClasses()...)
	m := newTestManager(t, f, &fakeLanes{})
	boom := errors.New("boom")
	f.set(func(f *fakeFleet) { f.approvalsErr = boom })
	if err := m.Refresh(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func hasHold(s OrgState, reason string) bool {
	for _, h := range s.Holds {
		if h.Reason == reason {
			return true
		}
	}
	return false
}

// A refusal seen by this process ends the admission even when clearing the
// stored moment failed: re-admission purges again and starts a new moment.
func TestReadmissionAfterRefusalPurgesEvenIfStoreKeptMoment(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "consent.json"))
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	now := t0
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store, Now: func() time.Time { return now }})
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	org := m.Me().OrgID
	m.ObserveUploadError(earlyAccessErr())
	// Simulate the clear failing: the old moment is still on disk.
	if err := store.SetAdmitted(org, t0); err != nil {
		t.Fatal(err)
	}
	before := lanePurges(lanes)
	now = t0.Add(time.Hour)
	m.ObserveEnrolled()
	if got := m.State().AdmittedAt; !got.Equal(now) {
		t.Fatalf("re-admitted at %v, want %v (old moment reused)", got, now)
	}
	if lanePurges(lanes) != before+1 {
		t.Fatalf("re-admission did not purge: %d -> %d", before, lanePurges(lanes))
	}
}

// A stored moment in the future (clock was ahead) is replaced at the next
// admitted response rather than holding back everything until then.
func TestFutureAdmissionMomentReplaced(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "consent.json"))
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store, Now: func() time.Time { return now }})
	if err := store.SetAdmitted(f.me.OrgID, now.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.State().AdmittedAt; !got.Equal(now) || lanePurges(lanes) != 1 {
		t.Fatalf("admitted at %v (purges %d), want %v", got, lanePurges(lanes), now)
	}
}

// A 403 that is not the early-access refusal neither ends the admission
// nor purges anything.
func TestOther403KeepsAdmission(t *testing.T) {
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := lanePurges(lanes)
	m.ObserveUploadError(&fleetapi.APIError{Status: 403, Code: "forbidden"})
	if _, ok := m.Admission(); !ok || lanePurges(lanes) != before || m.State().EarlyAccess {
		t.Fatalf("other 403: admitted=%v purges %d->%d", ok, before, lanePurges(lanes))
	}
}
