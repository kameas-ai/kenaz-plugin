package memsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// two devices of one user converge on saves, title edits, body
// edits (replacement + forget) and deletes.
func TestTwoDevicesConverge(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()

	id := a.save("Retry policy", "Use exponential backoff capped at 30 s.", true)
	proj := a.save("Project only", "this stays on device A", false)
	if s := a.status(); s.Notes[memory.StatePending] != 1 || s.Notes[memory.StateLocal] != 1 {
		t.Fatalf("before sync: %+v", s.Notes)
	}
	a.sync()
	if n, _ := a.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("a after push: %+v", n)
	}
	if n, _ := a.get(proj); n.SyncState != memory.StateLocal || n.SyncCode != "scope_not_supported_yet" {
		t.Fatalf("project note: %+v", n)
	}
	b.sync()
	got, ok := b.get(id)
	if !ok || got.Body != "Use exponential backoff capped at 30 s." || got.Origin != "fleet" || got.SyncState != memory.StateSynced {
		t.Fatalf("b did not receive the note: %+v %v", got, ok)
	}
	if _, ok := b.get(proj); ok {
		t.Fatal("a project-scope note reached another device")
	}

	// Title edit on B (LWW register) reaches A with the same id.
	nt := "Retry policy (net)"
	if _, err := b.st.UpdateNote(ctx, id, memory.Update{Title: &nt}, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	if n, _ := a.get(id); n.Title != nt {
		t.Fatalf("title did not converge: %q", n.Title)
	}

	// Body edit on A: a replacement. B ends with the new text only.
	nb := "Use exponential backoff capped at 60 s."
	r, err := a.st.UpdateNote(ctx, id, memory.Update{Body: &nb}, memory.MutateOptions{SyncApproved: true})
	if err != nil || r.Replaced != id || r.ID == id {
		t.Fatalf("replace = %+v %v", r, err)
	}
	if _, ok := a.get(id); ok {
		t.Fatal("old copy still visible after the edit")
	}
	a.sync() // new upsert accepted, old forget released
	a.sync() // old forget sent
	if s := a.status(); s.EditsInProgress != 0 || s.ForgetsPending != 0 {
		t.Fatalf("replacement did not complete: %+v", s)
	}
	b.sync()
	bodies := b.bodies()
	if _, ok := bodies[nb]; !ok || len(bodies) != 1 {
		t.Fatalf("b after edit: %v", keys(bodies))
	}
	if live := f.LiveContents(alice.OrgID, alice.UserID); len(live) != 1 || live[r.ID] != nb {
		t.Fatalf("fleet after edit: %v", live)
	}

	// Delete on B reaches A.
	if _, err := b.st.ForgetNote(ctx, r.ID, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	if _, ok := a.get(r.ID); ok {
		t.Fatal("delete did not converge")
	}
	if live := f.LiveContents(alice.OrgID, alice.UserID); len(live) != 0 {
		t.Fatalf("fleet still holds %v", live)
	}
}

func keys(m map[string]memory.Note) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// another user, staff, and another org see nothing.
func TestOtherIdentitiesDenied(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	a.save("Alice only", "ALICE-SECRET-FREE-CANARY note", true)
	a.sync()

	c := newDevice(t, f, bob)
	c.enable()
	c.sync()
	if len(c.bodies()) != 0 {
		t.Fatal("bob pulled alice's memory")
	}
	if live := f.LiveContents(bob.OrgID, bob.UserID); len(live) != 0 {
		t.Fatal("bob's server store has content")
	}
	// Same user id in another org is another account.
	other := memory.Profile{Env: "dev", OrgID: "org-2", UserID: "alice"}
	o := newDevice(t, f, other)
	o.enable()
	o.sync()
	if len(o.bodies()) != 0 {
		t.Fatal("same user id in another org saw the memory")
	}
	// Staff have no read path at all.
	if _, err := f.StaffClient().Pull(ctx, fleetapi.MemoryPullRequest{}); fleetapi.APICode(err) != fleetapi.CodeStaffNotPermitted {
		t.Fatalf("staff pull = %v", err)
	}
	if _, err := f.StaffClient().Export(ctx, func(fleetapi.MemoryRecord) error { return nil }); fleetapi.APICode(err) != fleetapi.CodeStaffNotPermitted {
		t.Fatalf("staff export = %v", err)
	}
	// Bob cannot forget alice's note by id either.
	ids := []string{}
	for id := range f.LiveContents(alice.OrgID, alice.UserID) {
		ids = append(ids, id)
	}
	if _, err := f.Client(bob.OrgID, bob.UserID).Forget(f.ContextFor(ctx, bob.OrgID, bob.UserID), fleetapi.MemoryForgetRequest{IDs: ids}); err != nil {
		t.Fatal(err)
	}
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 1 {
		t.Fatal("another user's forget reached alice's record")
	}
}

// an offline device's stale title edit cannot resurrect a note
// deleted elsewhere; its own unsynced note survives.
func TestOfflineEditVsDeleteConverges(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	x := a.save("X", "note x", true)
	a.sync()
	b.sync()

	// A deletes X while B (offline) retitles it and saves a new note.
	if _, err := a.st.ForgetNote(ctx, x, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	a.sync()
	tt := "X retitled offline"
	if _, err := b.st.UpdateNote(ctx, x, memory.Update{Title: &tt}, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	y := b.save("Y", "made offline", true)
	b.sync() // pull first: the tombstone wins, then push Y
	if _, ok := b.get(x); ok {
		t.Fatal("deleted note resurrected on the stale device")
	}
	a.sync()
	if _, ok := a.get(x); ok {
		t.Fatal("deleted note came back to A")
	}
	if _, ok := a.get(y); !ok {
		t.Fatal("offline save did not converge")
	}
	if live := f.LiveContents(alice.OrgID, alice.UserID); len(live) != 1 {
		t.Fatalf("fleet = %v", live)
	}
}

// a cursor older than Fleet's floor resets; synced notes Fleet no
// longer has go, unsynced ones stay and are pushed after the reset.
func TestCursorExpiredReset(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	keep := a.save("keep", "kept note", true)
	gone := a.save("gone", "deleted note", true)
	a.sync()
	b.sync()
	if _, err := a.st.ForgetNote(ctx, gone, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	a.sync()
	// More changes, then the 180-day sweep raises the floor past B's cursor.
	a.save("later", "later note", true)
	a.sync()
	f.SweepTombstones(alice.OrgID, alice.UserID)
	offline := b.save("offline", "unsynced on b", true)
	b.sync()
	for _, id := range []string{keep, offline} {
		if _, ok := b.get(id); !ok {
			t.Fatalf("%s missing after reset", id)
		}
	}
	if _, ok := b.get(gone); ok {
		t.Fatal("a note deleted on Fleet survived the reset")
	}
	if n := len(b.bodies()); n != 3 {
		t.Fatalf("b has %d notes: %v", n, keys(b.bodies()))
	}
	if _, ok := f.LiveContents(alice.OrgID, alice.UserID)[offline]; !ok {
		t.Fatal("the unsynced note was not pushed after the reset")
	}
}

// Merged: the same text saved on two devices is one record on Fleet; the
// second device re-keys to the canonical id.
func TestDuplicateContentMerges(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a, b := newDevice(t, f, alice), newDevice(t, f, alice)
	a.enable()
	b.enable()
	ida := a.save("same", "identical body", true)
	idb := b.save("same", "identical body", true)
	a.sync()
	b.sync()
	if n, ok := b.get(ida); !ok || n.ID != ida || n.SyncState != memory.StateSynced {
		t.Fatalf("b not re-keyed to canonical: %+v %v", n, ok)
	}
	// The alias still resolves.
	if n, ok := b.get(idb); !ok || n.ID != ida {
		t.Fatalf("alias does not resolve: %+v", n)
	}
	if len(b.bodies()) != 1 || len(f.LiveContents(alice.OrgID, alice.UserID)) != 1 {
		t.Fatal("duplicate content stored twice")
	}
	a.sync()
	if len(a.bodies()) != 1 {
		t.Fatal("a sees a duplicate")
	}
}

// Partial rejection: one item refused for good is visible and not
// retried; the rest of the batch syncs.
func TestPartialBatchRejection(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	ok1 := a.save("fine", "fine one", true)
	// A device clock an hour ahead stamps HLCs Fleet refuses.
	a.st.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	bad := a.save("future", "future clock", true)
	a.st.SetClock(time.Now)
	ok2 := a.save("fine2", "fine two", true)
	a.sync()
	for _, id := range []string{ok1, ok2} {
		if n, _ := a.get(id); n.SyncState != memory.StateSynced {
			t.Fatalf("%s = %+v", id, n)
		}
	}
	n, _ := a.get(bad)
	if n.SyncState != memory.StateRejected || n.SyncCode != "clock_in_future" {
		t.Fatalf("rejected item = %+v", n)
	}
	before := f.Calls("push")
	a.sync()
	a.sync()
	if f.Calls("push") != before {
		t.Fatal("a permanently rejected item is being resent")
	}
	if s := a.status(); s.Queue["blocked"] != 1 || len(s.BlockedCodes) != 1 {
		t.Fatalf("status = %+v", s)
	}
}

// Quota (retry:true) backs off and stays pending; 429 honours
// Retry-After.
func TestQuotaAndRateLimitBackoff(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	f.MaxRecords = 1
	a := newDevice(t, f, alice)
	a.enable()
	a.save("one", "first", true)
	two := a.save("two", "second", true)
	a.sync()
	n, _ := a.get(two)
	if n.SyncState != memory.StatePending {
		t.Fatalf("over-quota note = %+v", n)
	}
	before := f.Calls("push")
	a.sync()
	if f.Calls("push") != before {
		t.Fatal("over-quota item retried without backoff")
	}

	f2 := fleetapitest.NewMemoryFleet()
	f2.Fault = failingOnce("push", false, fleetapitest.RateLimited(120))
	b := newDevice(t, f2, alice)
	b.enable()
	id := b.save("x", "rate limited", true)
	err := b.w.RunOnce(context.Background())
	if ra, ok := fleetapi.RetryAfter(err); !ok || ra != 120*time.Second {
		t.Fatalf("429 not surfaced: %v", err)
	}
	if d := b.w.nextBackoff(err); d < 120*time.Second {
		t.Fatalf("backoff %v ignores Retry-After", d)
	}
	b.sync() // op is delayed: nothing sent yet
	if n, _ := b.get(id); n.SyncState != memory.StatePending {
		t.Fatalf("after 429: %+v", n)
	}
	b.st.SetClock(func() time.Time { return time.Now().Add(3 * time.Minute) })
	b.sync()
	if n, _ := b.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("after Retry-After: %+v", n)
	}
}

// Delete coalescing: a never-sent create plus its delete send nothing; a
// delete after a push sends a forget.
func TestDeleteCoalescing(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("tmp", "never sent", true)
	r, err := a.st.ForgetNote(ctx, id, memory.MutateOptions{SyncApproved: true})
	if err != nil || r.Remote != "none" {
		t.Fatalf("forget = %+v %v", r, err)
	}
	a.sync()
	if f.Calls("push") != 0 {
		t.Fatal("a coalesced create+delete reached Fleet")
	}
	id2 := a.save("kept", "sent then deleted", true)
	a.sync()
	r, _ = a.st.ForgetNote(ctx, id2, memory.MutateOptions{SyncApproved: true})
	if r.Remote != "pending" {
		t.Fatalf("forget after push = %+v", r)
	}
	a.sync()
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 0 || a.status().ForgetsPending != 0 {
		t.Fatal("forget not delivered")
	}
}

// A replacement whose new text Fleet refuses for good keeps the old
// synchronized copy (hidden), never forgets it, and can be reverted.
func TestBlockedReplacementKeepsOldCopy(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("t", "original text", true)
	a.sync()
	// Fleet now syncs only long_term, so a new global record is refused.
	sc := []string{"long_term"}
	if _, err := f.Client(alice.OrgID, alice.UserID).PutSettings(f.ContextFor(ctx, alice.OrgID, alice.UserID), fleetapi.MemorySettingsUpdate{Scopes: &sc}); err != nil {
		t.Fatal(err)
	}
	nb := "edited text"
	r, err := a.st.UpdateNote(ctx, id, memory.Update{Body: &nb}, memory.MutateOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	a.sync()
	a.sync()
	if s := a.status(); s.EditsBlocked != 1 {
		t.Fatalf("status = %+v", s)
	}
	if live := f.LiveContents(alice.OrgID, alice.UserID); live[id] != "original text" {
		t.Fatalf("the only synced copy was deleted: %v", live)
	}
	if err := a.st.Revert(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if n, ok := a.get(id); !ok || n.Body != "original text" {
		t.Fatalf("revert = %+v %v", n, ok)
	}
}

// The on-device credential scan runs before anything is queued.
func TestCredentialScanBeforeSync(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("db", "connect with password=hunter2hunter2 on staging", true)
	n, _ := a.get(id)
	if n.SyncState != memory.StateRejected || n.SyncCode != "secret_detected" {
		t.Fatalf("note = %+v", n)
	}
	a.sync()
	for _, c := range f.AllContents() {
		if strings.Contains(c, "hunter2") {
			t.Fatal("a credential-like note reached Fleet")
		}
	}
}

// Sync off: no new notes leave; already-synced notes can still be deleted
// (data-rights forget route), and a lapsed plan behaves the same.
func TestDisabledAndLapsedOnlyForget(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	old := a.save("old", "synced before", true)
	a.sync()
	if err := a.st.DisableSync(ctx, "disabled_by_user"); err != nil {
		t.Fatal(err)
	}
	newer := a.save("new", "saved while off", true)
	if n, _ := a.get(newer); n.SyncState != memory.StateLocal {
		t.Fatalf("saved while off = %+v", n)
	}
	if _, err := a.st.ForgetNote(ctx, old, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	f.Entitled = func(string) bool { return false }
	a.sync()
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 0 {
		t.Fatal("forget not delivered while sync was off and the plan lapsed")
	}
	if f.Calls("push") != 1 || f.Calls("forget") != 1 {
		t.Fatalf("push=%d forget=%d", f.Calls("push"), f.Calls("forget"))
	}
}

// Consent: a store enabled under an older notice edition sends nothing new.
func TestConsentReconfirm(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	a.save("x", "y", true)
	a.w.o.ConsentVersion = "memory-sync/v2"
	a.sync()
	if f.Calls("push") != 0 {
		t.Fatal("sent under a superseded consent")
	}
	if s := a.status(); s.StatusCode != CodeConsentReconfirm {
		t.Fatalf("status = %+v", s)
	}
}

// Capability check: no memory_sync → not in tier, nothing sent.
func TestNotInTier(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	a.save("x", "y", true)
	f.Entitled = func(string) bool { return false }
	err := a.w.RunOnce(context.Background())
	if fleetapi.APICode(err) != fleetapi.CodeCapabilityNotInTier {
		t.Fatalf("err = %v", err)
	}
	r := Remote{Memory: f.Client(alice.OrgID, alice.UserID), Session: a.sessionFn,
		Capabilities: func(context.Context) (fleetapi.Capabilities, error) { return fleetapi.Capabilities{}, nil }}
	if err := r.EnableSync(context.Background(), alice, consent); !errors.Is(err, ErrNotInTier) {
		t.Fatalf("enable without capability = %v", err)
	}
}

// Disabled on Fleet (another device turned it off): this device stops too.
func TestDisabledElsewhere(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	off := false
	if _, err := f.Client(alice.OrgID, alice.UserID).PutSettings(f.ContextFor(ctx, alice.OrgID, alice.UserID), fleetapi.MemorySettingsUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	a.save("x", "y", true)
	a.sync()
	if s := a.status(); s.Enabled || s.StatusCode != CodeDisabledOnFleet {
		t.Fatalf("status = %+v", s)
	}
}

// A paused organization refuses upserts; deletions still go through the
// forget route and the upserts wait.
func TestOrgPausedStillDeletes(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	old := a.save("old", "synced", true)
	a.sync()
	a.save("new", "waits", true)
	a.st.ForgetNote(ctx, old, memory.MutateOptions{SyncApproved: true})
	f.Fault = func(c string, after bool) error {
		if c == "push" && !after {
			return &fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused, Details: map[string]string{"paused_category": "memory"}}
		}
		return nil
	}
	if _, paused := fleetapi.OrgPaused(a.w.RunOnce(ctx)); !paused {
		t.Fatal("pause not surfaced")
	}
	if len(f.LiveContents(alice.OrgID, alice.UserID)) != 0 {
		t.Fatal("deletion held back by the pause")
	}
	if s := a.status(); s.Notes[memory.StatePending] != 1 || s.StatusCode != fleetapi.CodeOrgPaused {
		t.Fatalf("status = %+v", s)
	}
}
