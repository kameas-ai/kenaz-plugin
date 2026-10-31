package consent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

func planRefusalErr() error {
	return &fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text", Details: map[string]string{"capability": fleetapi.PluginCapability}}
}

// legacyRefusalErr is the refusal older Fleet servers send for the same
// case.
func legacyRefusalErr() error {
	return &fleetapi.APIError{Status: 403, Code: fleetapi.CodeLegacyEarlyAccessRequired, Message: "fleet text"}
}

func lanePurges(l *fakeLanes) int { l.mu.Lock(); defer l.mu.Unlock(); return l.purged }

// Fleet refuses every plugin request but /me, capabilities and the opt-ins
// for an org whose plan does not include the plugin (e.g. after a plan change). Nothing is
// collected or kept: records drop (never hold) and anything queued is
// purged. A member can still withdraw, and the local withdrawal (stop +
// purge) still runs; the opt-in record itself stays with Fleet.
func TestWithdrawalWhilePlanRequired(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts, fleetapi.ClassToolCalls)

	f.set(func(f *fakeFleet) { f.approvalsErr = planRefusalErr() })
	before := lanePurges(lanes)
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("refresh while plan required: %v", err)
	}
	if lanePurges(lanes) <= before {
		t.Fatal("a refusal must purge anything queued on the device")
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop || d.Reason != ReasonPlanRequired {
		t.Fatalf("decide while plan required: %+v (must drop, never hold)", d)
	}
	st := m.State()
	if !st.PlanRequired || len(st.Holds) != 0 || !st.AdmittedAt.IsZero() {
		t.Fatalf("state while plan required: %+v", st)
	}
	if st.Classes[string(fleetapi.ClassUsageCounts)] != ClassOn {
		t.Fatal("the member's opt-in record stays")
	}

	// Opting in to more is refused with the plan-required error.
	err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassErrors: true})
	if !fleetapi.PlanRequired(err) {
		t.Fatalf("opt-in while plan required: err=%v", err)
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
	if st := m.State(); st.PlanRequired || st.AdmittedAt.IsZero() {
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
	// One purge claims the (ownerless) queue on first run, one is the
	// first admitted response's.
	if lanePurges(lanes) != 2 {
		t.Fatalf("first admitted response must purge earlier data: %d", lanePurges(lanes))
	}
	if got := m.State().AdmittedAt; !got.Equal(t0) {
		t.Fatalf("admitted at %v, want %v", got, t0)
	}
	now = t0.Add(time.Hour)
	if err := m.Refresh(ctx); err != nil || lanePurges(lanes) != 2 || !m.State().AdmittedAt.Equal(t0) {
		t.Fatalf("later admitted responses must not move the moment or purge: %v %d", err, lanePurges(lanes))
	}
	// Restart: the recorded moment stands; nothing is purged.
	m2 := newM()
	if err := m2.Refresh(ctx); err != nil || lanePurges(lanes) != 2 || !m2.State().AdmittedAt.Equal(t0) {
		t.Fatalf("restart: %v purges=%d at=%v", err, lanePurges(lanes), m2.State().AdmittedAt)
	}
	// An upload refusal clears it; re-admission is a new moment.
	m2.ObserveUploadError(planRefusalErr())
	if !store.AdmittedAt(m2.Me().OrgID).IsZero() {
		t.Fatal("a refusal must clear the recorded admission")
	}
	if st := m2.State(); !st.PlanRequired || !st.AdmittedAt.IsZero() {
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
	m.ObserveUploadError(planRefusalErr())
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
	// The first-run claim of the ownerless queue purges too.
	if got := m.State().AdmittedAt; !got.Equal(now) || lanePurges(lanes) != 2 {
		t.Fatalf("admitted at %v (purges %d), want %v", got, lanePurges(lanes), now)
	}
}

// A 403 that is not the plan-required refusal neither ends the admission
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
	if _, ok := m.Admission(); !ok || lanePurges(lanes) != before || m.State().PlanRequired {
		t.Fatalf("other 403: admitted=%v purges %d->%d", ok, before, lanePurges(lanes))
	}
}

// Older Fleet servers' refusal code is the same plan-required state: every
// record drops, anything queued is purged, and the rendered status shows
// only the plan-required text.
func TestLegacyRefusalIsPlanRequired(t *testing.T) {
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts)

	f.set(func(f *fakeFleet) { f.approvalsErr = legacyRefusalErr() })
	before := lanePurges(lanes)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if lanePurges(lanes) <= before {
		t.Fatal("a legacy refusal must purge anything queued on the device")
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop || d.Reason != ReasonPlanRequired {
		t.Fatalf("decide: %+v (must drop, never hold)", d)
	}
	st := m.State()
	if !st.PlanRequired {
		t.Fatalf("state: %+v", st)
	}
	var b strings.Builder
	WriteText(&b, st)
	out := b.String() + SessionStartContext(st)
	if !strings.Contains(out, fleetapi.PlanRequiredMessage) || !strings.Contains(out, PlanRequiredNothingKept) {
		t.Fatalf("render = %s", out)
	}
	if strings.Contains(strings.ToLower(out), "early access") || strings.Contains(out, "fleet text") {
		t.Fatalf("render shows early-access or Fleet text: %s", out)
	}
	// Opting in is refused with the current code.
	err := m.SetOptIns(context.Background(), map[fleetapi.TelemetryClass]bool{fleetapi.ClassErrors: true})
	var ae *fleetapi.APIError
	if !errors.As(err, &ae) || ae.Code != fleetapi.CodeCapabilityNotInTier {
		t.Fatalf("opt-in: err=%v", err)
	}
}

func featureLimitErr(capability string) error {
	e := &fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text"}
	if capability != "" {
		e.Details = map[string]string{"capability": capability}
	}
	return e
}

// A per-feature capability_not_in_tier (another capability, or none named)
// is not the plan-required refusal: the admission is unchanged, nothing is
// purged, and records are not dropped as plan-required.
func TestPerFeatureTierLimitKeepsAdmission(t *testing.T) {
	for name, err := range map[string]error{
		"org_graph_sharing": featureLimitErr("org_graph_sharing"),
		"no capability":     featureLimitErr(""),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(allClasses()...)
			lanes := &fakeLanes{}
			m := newTestManager(t, f, lanes)
			optIn(t, m, fleetapi.ClassUsageCounts)
			since, ok := m.Admission()
			if !ok {
				t.Fatal("not admitted before the refusal")
			}
			before := lanePurges(lanes)

			m.ObserveUploadError(err)
			// Pending-approvals answering a per-feature limit fails the
			// refresh but is not a plan-required answer either.
			f.set(func(f *fakeFleet) { f.approvalsErr = err })
			_ = m.Refresh(context.Background())

			got, ok := m.Admission()
			if !ok || !got.Equal(since) || lanePurges(lanes) != before || m.State().PlanRequired {
				t.Fatalf("admitted=%v since %v->%v purges %d->%d planRequired=%v", ok, since, got, before, lanePurges(lanes), m.State().PlanRequired)
			}
			if d := m.Decide(fleetapi.ClassUsageCounts); d.Reason == ReasonPlanRequired {
				t.Fatalf("decide: %+v", d)
			}
		})
	}
}

// The whole-plugin refusal (capability_not_in_tier naming the plugin
// capability) still ends the admission and purges.
func TestPluginCapabilityRefusalPurges(t *testing.T) {
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts)
	before := lanePurges(lanes)
	m.ObserveUploadError(planRefusalErr())
	if _, ok := m.Admission(); ok || lanePurges(lanes) <= before || !m.State().PlanRequired {
		t.Fatalf("admitted=%v purges %d->%d", ok, before, lanePurges(lanes))
	}
}

// A per-feature tier limit from the lockdown long-poll is not observed as
// an upload error: no purge, the admission unchanged, and the watch keeps
// polling at the maximum backoff. The whole-plugin refusal still purges.
func TestLockdownWaitFailed(t *testing.T) {
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts)
	since, _ := m.Admission()
	before := lanePurges(lanes)

	for _, err := range []error{featureLimitErr("emergency_lockdown"), featureLimitErr("")} {
		wait, next := m.lockdownWaitFailed(err, time.Second)
		if wait != lockdownWaitMaxBackoff || next != lockdownWaitMaxBackoff {
			t.Fatalf("tier limit backoff = %v, %v", wait, next)
		}
	}
	if got, ok := m.Admission(); !ok || !got.Equal(since) || lanePurges(lanes) != before || m.State().PlanRequired {
		t.Fatalf("per-feature limit: admitted=%v purges %d->%d", ok, before, lanePurges(lanes))
	}

	// Other errors keep the existing doubling backoff.
	if wait, next := m.lockdownWaitFailed(errors.New("dial"), 2*time.Second); wait != 2*time.Second || next != 4*time.Second {
		t.Fatalf("other error backoff = %v, %v", wait, next)
	}

	if _, _ = m.lockdownWaitFailed(planRefusalErr(), time.Second); lanePurges(lanes) <= before || !m.State().PlanRequired {
		t.Fatal("the whole-plugin refusal must still purge")
	}
}

// End to end: the watch with a per-feature tier error does not purge.
func TestLockdownWatchPerFeatureTierNoPurge(t *testing.T) {
	f := newFake(allClasses()...)
	f.caps.Keys[CapEmergencyLockdown] = true
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts)
	since, _ := m.Admission()
	before := lanePurges(lanes)
	f.set(func(f *fakeFleet) { f.lockWaitErr = featureLimitErr("emergency_lockdown") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.RunLockdownWatch(ctx); close(done) }()
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.lockWaitCalls >= 1 })
	time.Sleep(50 * time.Millisecond) // let the error be handled
	f.mu.Lock()
	calls := f.lockWaitCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("polled %d times; the watch must wait the maximum backoff", calls)
	}
	if got, ok := m.Admission(); !ok || !got.Equal(since) || lanePurges(lanes) != before || m.State().PlanRequired {
		t.Fatalf("admitted=%v purges %d->%d", ok, before, lanePurges(lanes))
	}
	cancel()
	<-done
}
