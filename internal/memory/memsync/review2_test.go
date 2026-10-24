package memsync

import (
	"context"
	"errors"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// A reset whose pages come back "sync off" (enabled:false, no records)
// is never committed as an empty snapshot: synced notes stay, sync turns
// off locally.
func TestDisabledPullNeverCommitsEmptySnapshot(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	x := a.save("x", "kept note", true)
	a.sync()
	b.sync()
	gone := a.save("gone", "deleted", true)
	a.sync()
	a.st.ForgetNote(ctx, gone, memory.MutateOptions{SyncApproved: true})
	a.sync()
	f.SweepTombstones(alice.OrgID, alice.UserID) // B's next pull resets
	pulls := 0
	f.Fault = func(c string, after bool) error {
		if c == "pull" && !after {
			if pulls++; pulls == 2 { // the reset answer, then the first snapshot page fails
				return errNet
			}
		}
		return nil
	}
	_ = b.w.RunOnce(ctx)
	if _, pending, _ := b.st.PendingReset(ctx); !pending {
		t.Fatal("setup: no interrupted reset")
	}
	f.Fault = nil
	off := false
	if _, err := f.Client(alice.OrgID, alice.UserID).PutSettings(f.ContextFor(ctx, alice.OrgID, alice.UserID), fleetapi.MemorySettingsUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	b.restart()
	b.sync()
	if _, ok := b.get(x); !ok {
		t.Fatal("a disabled pull was committed as an empty snapshot")
	}
	if s := b.status(); s.Enabled || s.StatusCode != CodeDisabledOnFleet {
		t.Fatalf("status = %+v", s)
	}
}

// Another device's title edit reaches the hidden old copy of an edit in
// progress, so reverting the edit shows the latest title.
func TestReplacedRowTakesRemoteFieldUpdates(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	x := a.save("old title", "original", true)
	a.sync()
	b.sync()
	sc := []string{"long_term"}
	f.Client(alice.OrgID, alice.UserID).PutSettings(f.ContextFor(ctx, alice.OrgID, alice.UserID), fleetapi.MemorySettingsUpdate{Scopes: &sc})
	nb := "edited"
	r, err := a.st.UpdateNote(ctx, x, memory.Update{Body: &nb}, memory.MutateOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	a.sync() // the new copy is refused: the edit is blocked
	nt := "title from B"
	if _, err := b.st.UpdateNote(ctx, x, memory.Update{Title: &nt}, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	if err := a.st.Revert(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if n, ok := a.get(x); !ok || n.Title != nt {
		t.Fatalf("reverted note = %+v %v", n, ok)
	}
}

// switchingMemory signs another account in around a call: before (the
// switch lands after the worker's identity check, before the client picks
// its token) or after (while the answer is on its way back).
type switchingMemory struct {
	fleetapi.Memory
	d      *device
	when   string
	before bool
}

func (s switchingMemory) flip(call string) func() {
	if s.when != call {
		return func() {}
	}
	if s.before {
		s.d.signIn(bob)
		return func() {}
	}
	return func() { s.d.signIn(bob) }
}

func (s switchingMemory) Push(ctx context.Context, r fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	defer s.flip("push")()
	return s.Memory.Push(ctx, r)
}

func (s switchingMemory) Pull(ctx context.Context, r fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	defer s.flip("pull")()
	return s.Memory.Pull(ctx, r)
}

func (s switchingMemory) ForgetAll(ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	defer s.flip("forget_all")()
	return s.Memory.ForgetAll(ctx, r)
}

func (s switchingMemory) Forget(ctx context.Context, r fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	defer s.flip("forget")()
	return s.Memory.Forget(ctx, r)
}

// A sign-in change around a request never lets one account's work reach
// another account. Switched after the worker's check but before the send,
// the client (which, like the official adapter, sends with whichever
// session is active) refuses the request with identity_mismatch: nothing
// reaches Fleet for Bob. Switched while the answer is coming back, the
// answer is discarded: nothing is marked synced or applied on the
// strength of a reply received after the account changed.
func TestIdentityChangeDuringCall(t *testing.T) {
	ctx := context.Background()
	for _, before := range []bool{true, false} {
		for _, when := range []string{"push", "pull"} {
			f := fleetapitest.NewMemoryFleet()
			optIn(t, f, bob)
			a := newDevice(t, f, alice)
			a.enable()
			id := a.save("t", "note "+when, true)
			a.w.o.Memory = switchingMemory{Memory: f.SessionClient(a.sessionFn), d: a, when: when, before: before}
			if err := a.w.RunOnce(ctx); !errors.Is(err, ErrAccountMismatch) {
				t.Fatalf("%s before=%v: err = %v", when, before, err)
			}
			if n, _ := a.get(id); n.SyncState != memory.StatePending {
				t.Fatalf("%s before=%v: answer applied after the switch: %+v", when, before, n)
			}
			if s := a.status(); s.Queue["inflight"] != 0 {
				t.Fatalf("%s before=%v: ops left in flight: %+v", when, before, s.Queue)
			}
			assertNothingFor(t, f, bob)
			if before {
				if f.Refused() == 0 {
					t.Fatalf("%s: the client did not refuse the switched request", when)
				}
				if when == "push" && f.Calls("push") != 0 {
					t.Fatalf("push reached Fleet after the switch")
				}
			}
		}
	}
}

// The account switches between the worker's identity check and the send
// on every route that carries a user's notes or erases them: Fleet gets
// nothing for Alice under Bob's token, and Bob's data is untouched.
func TestAccountSwitchBetweenCheckAndSend(t *testing.T) {
	ctx := context.Background()

	t.Run("push", func(t *testing.T) {
		f := fleetapitest.NewMemoryFleet()
		optIn(t, f, bob) // Bob syncs too, so a misdirected push would land
		a := newDevice(t, f, alice)
		a.enable()
		a.save("t", "alice private note", true)
		// Pull (the first request) goes through as Alice; the switch lands
		// right before the push is sent.
		a.w.o.Memory = switchingMemory{Memory: f.SessionClient(a.sessionFn), d: a, when: "push", before: true}
		if err := a.w.RunOnce(ctx); !errors.Is(err, ErrAccountMismatch) {
			t.Fatalf("err = %v", err)
		}
		assertNothingFor(t, f, bob)
		a.signIn(alice)
		a.w.o.Memory = f.SessionClient(a.sessionFn)
		a.sync()
		if got := pulledBodies(t, f, alice); !got["alice private note"] {
			t.Fatalf("the note never reached Alice after the switch back: %v", got)
		}
	})

	t.Run("forget_all", func(t *testing.T) {
		f := fleetapitest.NewMemoryFleet()
		b := newDevice(t, f, bob)
		b.enable()
		b.save("t", "bob note", true)
		b.sync()
		a := newDevice(t, f, alice)
		a.enable()
		a.save("t", "alice note", true)
		a.sync()
		if _, err := a.st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true}); err != nil {
			t.Fatal(err)
		}
		a.w.o.Memory = switchingMemory{Memory: f.SessionClient(a.sessionFn), d: a, when: "forget_all", before: true}
		if err := a.w.RunOnce(ctx); !errors.Is(err, ErrAccountMismatch) {
			t.Fatalf("err = %v", err)
		}
		if got := pulledBodies(t, f, bob); !got["bob note"] {
			t.Fatalf("Bob's notes were erased by Alice's forget-all: %v", got)
		}
		if set, _ := a.st.SyncSettings(ctx); !set.ErasePending {
			t.Fatal("the erase barrier came down without Fleet's answer")
		}
	})

	t.Run("enable", func(t *testing.T) {
		f := fleetapitest.NewMemoryFleet()
		a := newDevice(t, f, alice)
		r := Remote{Memory: switchingPut{Memory: f.SessionClient(a.sessionFn), d: a}, Session: a.sessionFn}
		if err := r.EnableSync(ctx, alice, consent); !errors.Is(err, ErrAccountMismatch) {
			t.Fatalf("err = %v", err)
		}
		s, err := f.Client(bob.OrgID, bob.UserID).Settings(f.ContextFor(ctx, bob.OrgID, bob.UserID))
		if err != nil {
			t.Fatal(err)
		}
		if s.Enabled {
			t.Fatal("Alice's opt-in turned sync on for Bob")
		}
	})
}

type switchingPut struct {
	fleetapi.Memory
	d *device
}

func (s switchingPut) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	s.d.signIn(bob)
	return s.Memory.PutSettings(ctx, u)
}

func pulledBodies(t *testing.T, f *fleetapitest.MemoryFleet, p memory.Profile) map[string]bool {
	t.Helper()
	resp, err := f.Client(p.OrgID, p.UserID).Pull(f.ContextFor(context.Background(), p.OrgID, p.UserID), fleetapi.MemoryPullRequest{Limit: fleetapi.MemoryMaxPullLimit})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range resp.Records {
		if r.State == "live" {
			out[r.Content] = true
		}
	}
	return out
}

func optIn(t *testing.T, f *fleetapitest.MemoryFleet, p memory.Profile) {
	t.Helper()
	on := true
	if _, err := f.Client(p.OrgID, p.UserID).PutSettings(f.ContextFor(context.Background(), p.OrgID, p.UserID),
		fleetapi.MemorySettingsUpdate{Enabled: &on, ConsentVersion: consent}); err != nil {
		t.Fatal(err)
	}
}

func assertNothingFor(t *testing.T, f *fleetapitest.MemoryFleet, p memory.Profile) {
	t.Helper()
	if got := pulledBodies(t, f, p); len(got) != 0 {
		t.Fatalf("Fleet holds notes for %s after the switch: %d", p.UserID, len(got))
	}
}

// A per-item tombstone_quota reject is final for that record: it is not
// retried in a loop (even if Fleet's report says retry) and the record
// shows as rejected with the code.
func TestTombstoneQuotaRejectNotRetried(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "quota note", true)
	q := &quotaMemory{Memory: f.SessionClient(a.sessionFn)}
	a.w.o.Memory = q
	for i := 0; i < 3; i++ {
		if err := a.w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if q.pushes != 1 {
		t.Fatalf("pushed %d times, want 1", q.pushes)
	}
	if n, _ := a.get(id); n.SyncState != memory.StateRejected || n.SyncCode != fleetapi.CodeTombstoneQuota {
		t.Fatalf("note = %+v", n)
	}
}

type quotaMemory struct {
	fleetapi.Memory
	pushes int
}

func (q *quotaMemory) Push(ctx context.Context, r fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	q.pushes++
	retry := true
	resp := fleetapi.MemoryPushResponse{}
	for _, it := range r.Items {
		resp.Results = append(resp.Results, fleetapi.MemoryPushResult{ID: it.ID, Status: fleetapi.MemoryRejected, Code: fleetapi.CodeTombstoneQuota, Retry: &retry})
	}
	return resp, nil
}

// Forget-all on a signed-in account always erases in Fleet, even when
// this device never turned sync on (another device may have), so the
// approval's "and in Kameas Fleet" is true.
func TestForgetAllErasesFleetWithoutLocalSync(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	other := newDevice(t, f, alice)
	other.enable()
	other.save("t", "from another device", true)
	other.sync()
	a := newDevice(t, f, alice) // never enabled here
	res, err := a.st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true})
	if err != nil || !res.FleetErasePending {
		t.Fatalf("forget-all = %+v %v", res, err)
	}
	if work, _ := a.st.HasSyncWork(ctx); !work {
		t.Fatal("the erase would never be delivered")
	}
	a.sync()
	if got := pulledBodies(t, f, alice); len(got) != 0 {
		t.Fatalf("Fleet still holds %d notes", len(got))
	}
}
