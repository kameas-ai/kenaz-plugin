package memsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// The storage-sync contract's crash/failure matrix. Each case injects a
// failure, "restarts" (reopens the store with a new worker) and asserts:
// acknowledged local data persists; no duplicate logical writes; the
// cursor never skips applied data; deleted notes stay hidden; accepted
// edits converge; a permanent rejection is visible and does not spin.

// Before commit: a save that fails stores nothing and leaves no queue.
func TestCrashBeforeLocalCommit(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.st.SaveNote(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "b", Source: memory.SourceCLI}, memory.SaveOptions{SyncApproved: true}); err == nil {
		t.Fatal("save with a dead context succeeded")
	}
	a.open()
	if s := a.status(); len(s.Queue) != 0 || len(s.Notes) != 0 {
		t.Fatalf("half-written save: %+v", s)
	}
}

// After local commit, before any send: the note and its queued upsert
// survive a restart and sync once.
func TestCrashAfterLocalCommit(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "durable", true)
	a.open()
	a.sync()
	if n, ok := a.get(id); !ok || n.SyncState != memory.StateSynced {
		t.Fatalf("after restart: %+v %v", n, ok)
	}
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 1 {
		t.Fatal("not exactly one record on Fleet")
	}
}

// Before HTTP: ops were leased (marked possibly sent) when the process
// died. The next worker takes the lease over and resends them once.
func TestCrashBeforeHTTP(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "leased then crashed", true)
	l, err := a.st.AcquireLease(ctx, "dead-worker", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := a.st.LeasePushBatch(ctx, l); err != nil || len(b.Ops) != 1 {
		t.Fatalf("lease = %+v %v", b, err)
	}
	a.open()
	a.sync()
	if n, _ := a.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("not resent: %+v", n)
	}
	// It was possibly sent, so deleting it now must send a forget.
	r, _ := a.st.ForgetNote(ctx, id, memory.MutateOptions{SyncApproved: true})
	if r.Remote != "pending" {
		t.Fatalf("forget after possible send = %+v", r)
	}
}

// After server commit, before local acknowledgement: the reply is lost.
// Replaying the same payload is idempotent (unchanged) and marks synced.
func TestCrashAfterServerCommit(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	f.Fault = failingOnce("push", true, errNet)
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "server has it", true)
	if err := a.w.RunOnce(context.Background()); !errors.Is(err, errNet) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := a.get(id); n.SyncState != memory.StatePending {
		t.Fatalf("marked synced without a result: %+v", n)
	}
	a.open()
	a.st.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	a.sync()
	if n, _ := a.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("replay did not converge: %+v", n)
	}
	if live := f.LiveContents(alice.OrgID, alice.UserID); len(live) != 1 {
		t.Fatalf("duplicate logical write: %v", live)
	}
}

// Between replacement phases: the new copy reached Fleet but the reply
// was lost; after restart the job completes (old copy forgotten) and
// nothing is duplicated.
func TestCrashBetweenReplacementPhases(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "v1", true)
	a.sync()
	nb := "v2"
	r, err := a.st.UpdateNote(ctx, id, memory.Update{Body: &nb}, memory.MutateOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	f.Fault = failingOnce("push", true, errNet)
	_ = a.w.RunOnce(ctx)
	// Fleet holds both now; locally the job waits for the new copy's ack.
	if live := f.LiveContents(alice.OrgID, alice.UserID); len(live) != 2 {
		t.Fatalf("fleet = %v", live)
	}
	if s := a.status(); s.EditsInProgress != 1 {
		t.Fatalf("status = %+v", s)
	}
	a.open()
	a.st.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	a.sync()
	a.sync()
	live := f.LiveContents(alice.OrgID, alice.UserID)
	if len(live) != 1 || live[r.ID] != "v2" {
		t.Fatalf("fleet after recovery = %v", live)
	}
	if s := a.status(); s.EditsInProgress != 0 {
		t.Fatalf("job not complete: %+v", s)
	}
}

// During pull page apply: a failure between pages keeps the applied
// pages and the cursor; nothing is skipped on resume. An invalid page is
// not applied and the cursor stays.
func TestCrashDuringPull(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	for i := 0; i < 5; i++ {
		a.save("n", string(rune('a'+i))+" note", true)
	}
	a.sync()
	calls := 0
	f.Fault = func(c string, after bool) error {
		if c == "pull" && !after {
			calls++
			if calls == 2 {
				return errNet
			}
		}
		return nil
	}
	_ = b.w.RunOnce(ctx) // page 1 (2 records) applied, page 2 fails
	if n := len(b.bodies()); n != 2 {
		t.Fatalf("after first page: %d", n)
	}
	cur, _ := b.st.Cursor(ctx)
	if cur == "" {
		t.Fatal("cursor not persisted with the applied page")
	}
	b.restart()
	b.sync()
	if n := len(b.bodies()); n != 5 {
		t.Fatalf("after resume: %d", n)
	}

	// An invalid page: nothing applied, cursor unchanged.
	l, _ := b.st.AcquireLease(ctx, "probe", 0)
	before, _ := b.st.Cursor(ctx)
	_, err := b.st.ApplyPullPage(ctx, l, memory.PullPage{Cursor: "999", Records: []memory.PulledRecord{
		{ID: "mem-ok", State: "live", Content: "x", ContentHash: memory.ContentHash("x"), ScopeKind: "global"},
		{ID: "mem-bad", State: "live", Content: "y", ContentHash: memory.ContentHash("not y"), ScopeKind: "global"},
	}})
	if !errors.Is(err, memory.ErrInvalidPage) {
		t.Fatalf("invalid page = %v", err)
	}
	if after, _ := b.st.Cursor(ctx); after != before {
		t.Fatal("cursor advanced past a refused page")
	}
	if _, ok := b.get("mem-ok"); ok {
		t.Fatal("part of a refused page was applied")
	}
}

// During a staged reset: an interrupted snapshot is never published; the
// next start restarts it from the empty cursor and commits it whole.
func TestCrashDuringReset(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	for i := 0; i < 4; i++ {
		a.save("n", string(rune('a'+i))+" note", true)
	}
	gone := a.save("gone", "to be deleted", true)
	a.sync()
	b.sync()
	if _, err := a.st.ForgetNote(ctx, gone, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	a.sync()
	f.SweepTombstones(alice.OrgID, alice.UserID)
	pulls := 0
	f.Fault = func(c string, after bool) error {
		if c == "pull" && !after {
			pulls++
			if pulls == 3 { // reset answer, snapshot page 1, then fail
				return errNet
			}
		}
		return nil
	}
	_ = b.w.RunOnce(ctx)
	if _, pending, _ := b.st.PendingReset(ctx); !pending {
		t.Fatal("interrupted reset not recorded")
	}
	// The partial snapshot is not published: B still shows what it had.
	if n := len(b.bodies()); n != 5 {
		t.Fatalf("partial snapshot exposed: %d notes", n)
	}
	f.Fault = nil
	b.restart()
	b.sync()
	if _, pending, _ := b.st.PendingReset(ctx); pending {
		t.Fatal("reset not completed")
	}
	if n := len(b.bodies()); n != 4 {
		t.Fatalf("after reset: %d notes", n)
	}
	if _, ok := b.get(gone); ok {
		t.Fatal("a deleted note survived the reset")
	}
}

// During forget-all: until Fleet confirms, the erase stays pending and
// nothing new is sent; afterwards every device clears, notes saved after
// the barrier survive, project notes are untouched by the remote erase.
func TestForgetAllBarrier(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	a.save("one", "erase me", true)
	a.sync()
	b.sync()
	bProject := b.save("p", "b project note", false)
	bPending := b.save("bp", "b unsynced before erase", true)

	res, err := a.st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true})
	if err != nil || !res.FleetErasePending {
		t.Fatalf("forget-all = %+v %v", res, err)
	}
	after := a.save("after", "saved after the barrier", true)
	f.Fault = failingOnce("forget_all", false, errNet)
	pushes := f.Calls("push")
	if err := a.w.RunOnce(ctx); !errors.Is(err, errNet) {
		t.Fatalf("err = %v", err)
	}
	if pending, _ := a.st.ErasePending(ctx); !pending {
		t.Fatal("a failed erase counted as done")
	}
	if f.Calls("push") != pushes {
		t.Fatal("sent new notes before the erase was confirmed")
	}
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 1 {
		t.Fatal("fleet erased without a successful call")
	}
	a.sync()
	if pending, _ := a.st.ErasePending(ctx); pending {
		t.Fatal("erase still pending")
	}
	live := f.LiveContents(alice.OrgID, alice.UserID)
	if len(live) != 1 || live[after] == "" {
		t.Fatalf("fleet after erase = %v", live)
	}
	if n, ok := a.get(after); !ok || n.SyncState != memory.StateSynced {
		t.Fatalf("note saved after the barrier: %+v %v", n, ok)
	}
	b.sync()
	bodies := b.bodies()
	if _, ok := bodies["erase me"]; ok {
		t.Fatal("erased note still on the other device")
	}
	if _, ok := b.get(bProject); !ok {
		t.Fatal("remote erase touched a project (local-only) note")
	}
	if _, ok := b.get(bPending); ok {
		t.Fatal("an unsynced note created before the erase survived it")
	}
	if _, ok := bodies["saved after the barrier"]; !ok {
		t.Fatal("post-barrier note did not reach the other device")
	}
}

// During account switch: a worker never sends one account's queue with
// another account's session, and a stale worker cannot apply results.
func TestAccountSwitch(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "alice's note", true)
	a.signIn(bob)
	if err := a.w.RunOnce(ctx); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("err = %v", err)
	}
	a.signIn(memory.Profile{})
	if err := a.w.RunOnce(ctx); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("signed out err = %v", err)
	}
	if f.Calls("push")+f.Calls("pull") != 0 || len(f.LiveContents(bob.OrgID, bob.UserID)) != 0 {
		t.Fatal("a request went out under the wrong identity")
	}
	if n, _ := a.get(id); n.SyncState != memory.StatePending {
		t.Fatalf("queued note changed: %+v", n)
	}
	// A lease taken before the switch is dead after InvalidateLeases.
	l, err := a.st.AcquireLease(ctx, "old", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := a.st.LeasePushBatch(ctx, l)
	if err := a.st.InvalidateLeases(ctx); err != nil {
		t.Fatal(err)
	}
	ok := true
	if _, err := a.st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: id, Status: fleetapi.MemoryAccepted, Retry: &ok}}); !errors.Is(err, memory.ErrLeaseLost) {
		t.Fatalf("stale worker applied results: %v", err)
	}
	// Signing back in as alice resumes.
	a.signIn(alice)
	a.open()
	a.sync()
	if n, _ := a.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("after re-sign-in: %+v", n)
	}
}

// Two workers on one profile: only the lease holder sends.
func TestSingleSender(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	a.save("t", "x", true)
	if _, err := a.st.AcquireLease(ctx, "other-live-worker", leaseTTL); err != nil {
		t.Fatal(err)
	}
	if err := a.w.RunOnce(ctx); !errors.Is(err, memory.ErrLeaseHeld) {
		t.Fatalf("second sender ran: %v", err)
	}
	if f.Calls("push") != 0 {
		t.Fatal("pushed without the lease")
	}
}
