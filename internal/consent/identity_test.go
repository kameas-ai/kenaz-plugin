package consent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Switching the signed-in account (same org or another org)
// purges everything queued before the new identity is used, so nothing
// collected for one account is sent under another.
func TestAccountSwitchPurgesQueue(t *testing.T) {
	for name, switchTo := range map[string]func(f *fakeFleet){
		"other user, same org": func(f *fakeFleet) { f.user = "u2" },
		"other org":            func(f *fakeFleet) { f.user, f.org = "u3", "o3" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFake(allClasses()...)
			lanes := &fakeLanes{}
			m := newTestManager(t, f, lanes)
			optIn(t, m, fleetapi.ClassUsageCounts)
			before := lanes.purged
			f.set(switchTo)
			if err := m.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if lanes.purged <= before || lanes.stopped == 0 {
				t.Fatalf("account switch did not stop and purge: %+v", lanes)
			}
			if s, _ := m.Session(); s.UserID == "u1" {
				t.Fatal("still on the old identity")
			}
			// A refresh on the same account does not purge again.
			again := lanes.purged
			if err := m.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if lanes.purged != again {
				t.Fatalf("same account purged again: %d → %d", again, lanes.purged)
			}
		})
	}
}

// Account switch across a daemon restart: the queue owner is persisted, so a new
// daemon process that finds another account signed in purges the queue
// left by the previous account before sending anything.
func TestAccountSwitchAcrossRestartPurgesQueue(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "consent.json"))
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m1 := NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store})
	if err := m1.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if store.QueueOwner() == "" {
		t.Fatal("queue owner not recorded")
	}
	// Same account after restart: the queue is kept (upgrade/restart).
	lanes2 := &fakeLanes{}
	m2 := NewManager(Options{Provider: f, Lanes: lanes2, ProjectionVersion: 1, Store: store})
	if err := m2.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lanes2.stopped != 0 {
		t.Fatalf("same account after restart was treated as a switch: %+v", lanes2)
	}
	// Another account after restart: purge first.
	f.set(func(f *fakeFleet) { f.user = "u9" })
	lanes3 := &fakeLanes{}
	m3 := NewManager(Options{Provider: f, Lanes: lanes3, ProjectionVersion: 1, Store: store})
	if err := m3.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lanes3.stopped == 0 || lanes3.purged == 0 {
		t.Fatalf("queue of the previous account not purged: %+v", lanes3)
	}
	if store.QueueOwner() != OwnerKey(fleetapi.Session{Env: fleetapi.EnvProd, UserID: "u9", OrgID: "o1"}) {
		t.Fatal("queue owner not moved to the new account")
	}
}

type failingLanes struct{ fakeLanes }

func (l *failingLanes) PurgeAll() error { return errors.New("disk full") }

// A purge that fails on account switch leaves the manager signed out, so
// every record keeps being dropped.
func TestAccountSwitchPurgeFailureDrops(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &failingLanes{}
	store := NewStore(filepath.Join(t.TempDir(), "consent.json"))
	m := NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store})
	_ = m.Refresh(ctx) // first admission purge fails too; state is irrelevant here
	_ = store.SetQueueOwner(OwnerKey(fleetapi.Session{Env: fleetapi.EnvProd, UserID: "u1", OrgID: "o1"}))
	f.set(func(f *fakeFleet) { f.user = "u2" })
	if err := m.Refresh(ctx); err == nil {
		t.Fatal("purge failure not reported")
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop {
		t.Fatalf("must drop while the old queue could not be purged: %+v", d)
	}
}

// Revoking consent (all classes off in Fleet, e.g. from the dashboard)
// while data is queued stops and purges the queue; later records drop.
func TestRevokedConsentPurgesQueued(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts)
	before := lanes.purged
	f.set(func(f *fakeFleet) {
		s := f.optins[fleetapi.ClassUsageCounts]
		s.OptedIn = false
		f.optins[fleetapi.ClassUsageCounts] = s
	})
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lanes.purged <= before {
		t.Fatalf("revocation in Fleet did not purge: %+v", lanes)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop {
		t.Fatalf("must drop after revocation: %+v", d)
	}
}

// Upgrade: consent.json as written by the previous release (no queue
// owner) keeps the member's locally accepted notice and the organization's
// admission moment and uploads continue; the ownerless queue it left is
// purged once (it may hold another account's data).
func TestUpgradePreviousConsentFileKept(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	f.set(func(f *fakeFleet) {
		s := f.optins[fleetapi.ClassUsageCounts]
		s.OptedIn = true // Fleet predating notice_version: NoticeVersion ""
		f.optins[fleetapi.ClassUsageCounts] = s
	})
	probe := NewManager(Options{Provider: f, ProjectionVersion: 1})
	if err := probe.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	admitted := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "consent.json")
	prev := `{
  "accepted_notice_version": {
    "u1": "` + probe.NoticeVersion() + `"
  },
  "admitted_at": {
    "o1": "` + admitted.Format(time.RFC3339) + `"
  }
}`
	if err := os.WriteFile(path, []byte(prev), 0o600); err != nil {
		t.Fatal(err)
	}
	lanes := &fakeLanes{}
	store := NewStore(path)
	m := NewManager(Options{Provider: f, Lanes: lanes, ProjectionVersion: 1, Store: store,
		Now: func() time.Time { return admitted.Add(24 * time.Hour) }})
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("upgrade lost consent: %+v", d)
	}
	// The previous release recorded no queue owner, so what it queued may
	// be another account's: it is purged once, without stopping the lanes.
	if lanes.purged != 1 || lanes.stopped != 0 {
		t.Fatalf("upgrade: want the ownerless queue purged once: %+v", lanes)
	}
	if since, ok := m.Admission(); !ok || !since.Equal(admitted) {
		t.Fatalf("admission moment %v %v", since, ok)
	}
	if store.Accepted("u1") != probe.NoticeVersion() || store.QueueOwner() == "" {
		t.Fatal("consent.json not carried forward")
	}
}

// SessionCurrent is the per-upload check: an account change purges at
// once (no Refresh needed) and stays false until Refresh adopts it.
func TestSessionCurrentPurgesOnSwitch(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	if !m.SessionCurrent(ctx) {
		t.Fatal("same account reported as changed")
	}
	before := lanes.purged
	f.set(func(f *fakeFleet) { f.user = "u2" })
	if m.SessionCurrent(ctx) || lanes.purged <= before {
		t.Fatalf("switch not caught before Refresh: %+v", lanes)
	}
	if m.SessionCurrent(ctx) {
		t.Fatal("must stay false until Refresh adopts the new account")
	}
	if err := m.Refresh(ctx); err != nil || !m.SessionCurrent(ctx) {
		t.Fatalf("after refresh: %v", err)
	}
	f.set(func(f *fakeFleet) { f.signedIn = false })
	if m.SessionCurrent(ctx) || m.State().SignedIn {
		t.Fatal("sign-out not caught")
	}
}
