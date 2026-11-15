package app

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// failWriteBack makes the write-back of a held record's batches fail like
// a full disk while fail returns true for the set, until the returned func
// is called. It returns a count of the failed attempts.
func failWriteBack(t *testing.T, fail func(recs [][]byte) bool) (failed *atomic.Int64, restore func()) {
	t.Helper()
	old := spoolAppendAll
	failed = new(atomic.Int64)
	var off atomic.Bool
	spoolAppendAll = func(sp *spool.Spool, lane, origin string, recs [][]byte) error {
		if !off.Load() && fail(recs) {
			failed.Add(1)
			return errors.New("no space left on device")
		}
		return old(sp, lane, origin, recs)
	}
	t.Cleanup(func() { spoolAppendAll = old })
	return failed, func() { off.Store(true) }
}

// spoolFiles returns the segment names and everything else in the spool.
func spoolFiles(t *testing.T, dir string) (segs, other []string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".seg") {
			segs = append(segs, e.Name())
		} else {
			other = append(other, e.Name())
		}
	}
	return segs, other
}

// wantHeldDeliveredOnce checks that everything the held records became
// was accepted exactly once: want telemetry records in all, no body twice,
// one span batch.
func wantHeldDeliveredOnce(t *testing.T, f *laneFleet, want int64) {
	t.Helper()
	seen := map[string]bool{}
	var records int64
	spans := 0
	for _, b := range f.batches() {
		if seen[string(b.Body)] {
			t.Fatal("a batch was accepted twice")
		}
		seen[string(b.Body)] = true
		records += int64(b.Records)
		if b.Signal == fleetapi.SignalTraces {
			spans++
		}
	}
	if records != want || spans != 1 {
		t.Fatalf("accepted %d records in %d batches (%d span batches), want %d records and 1 span batch", records, len(seen), spans, want)
	}
}

// The write-back of a held record fails (no room on disk). The held record
// must stay in the spool, not be marked done; when writes work again it is
// delivered exactly once.
func TestTelemetryLaneWriteBackFailureKeepsHeldRecord(t *testing.T) {
	f := &laneFleet{Provider: stub.New()}
	failed, restore := failWriteBack(t, func([][]byte) bool { return true })
	l, _ := heldLane(t, f, t.TempDir(), &spool.Memory{})
	heldN, queued := l.held.Load(), l.sp.Stats().Records
	f.set(func(f *laneFleet) { f.approvals = nil })
	// Several drains re-gate the first held record and fail to write it back.
	if !waitFor(t, func() bool { return failed.Load() >= 3 }) {
		t.Fatal("held records were never re-gated (test setup)")
	}
	l.Hold("inspect") // no drain is part-way through a write while the spool is read
	time.Sleep(50 * time.Millisecond)
	segs, other := spoolFiles(t, l.sp.Dir())
	if len(segs) != 1 || len(other) != 0 || l.sp.Stats().Records != queued || l.held.Load() != heldN {
		t.Fatalf("after failed write-backs: segments %v, other files %v, %d queued (want %d), %d held (want %d)",
			segs, other, l.sp.Stats().Records, queued, l.held.Load(), heldN)
	}
	if n := f.attemptCount(); n != 0 {
		t.Fatalf("%d uploads attempted", n)
	}
	restore()
	l.Resume()

	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	wantHeldDeliveredOnce(t, f, heldN)
	if l.dropped.Load() != 0 || l.held.Load() != 0 {
		t.Fatalf("dropped %d, still held %d", l.dropped.Load(), l.held.Load())
	}
}

// The write-back fails part-way through the held records: the first held
// record's batch is written, the next held record becomes two batches and
// that set does not fit. No part of the set is queued and the first is
// not queued again, however many drains fail the same way, so nothing is
// duplicated once the write goes through. (A set whose bytes reach the disk
// only in part is spool.TestAppendAllIsAllOrNothing.)
func TestTelemetryLaneWriteBackPartialFailureDoesNotDuplicate(t *testing.T) {
	f := &laneFleet{Provider: stub.New()}
	// Nothing is uploaded meanwhile, so the spool's content stays comparable.
	failing := true
	f.postHook = func(fleetapi.ProjectedBatch) error {
		if failing {
			return &fleetapi.APIError{Status: 503}
		}
		return nil
	}
	failed, restore := failWriteBack(t, func(recs [][]byte) bool { return len(recs) > 1 })
	l, _ := heldLane(t, f, t.TempDir(), &spool.Memory{})
	heldN := l.held.Load()
	if l.sp.Stats().Records != 3 {
		t.Fatalf("fixture changed: %d held spool records, want 3", l.sp.Stats().Records)
	}

	f.set(func(f *laneFleet) { f.approvals = nil })
	// The first held record is written back as one batch (a new segment),
	// and the second is re-gated again and again, never fitting.
	if !waitFor(t, func() bool { return failed.Load() >= 6 }) {
		t.Fatal("second held record was not retried (test setup)")
	}
	l.Hold("inspect") // no drain is part-way through a write while the spool is read
	// Two held records left, one batch queued; one cursor; nothing partial.
	if !waitFor(t, func() bool {
		segs, other := spoolFiles(t, l.sp.Dir())
		return len(segs) == 2 && len(other) == 1 && strings.HasSuffix(other[0], ".cur") && l.sp.Stats().Records == 3
	}) {
		segs, other := spoolFiles(t, l.sp.Dir())
		t.Fatalf("after failed write-backs: segments %v, other files %v, %d queued (want 3)", segs, other, l.sp.Stats().Records)
	}
	restore()
	l.Resume()
	f.set(func(*laneFleet) { failing = false })

	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	wantHeldDeliveredOnce(t, f, heldN)
	if _, other := spoolFiles(t, l.sp.Dir()); len(other) != 0 {
		t.Fatalf("left behind: %v", other)
	}
}

// The relay dies after a held record's batches were queued and before the
// record was marked done. The record is re-gated (and so encoded again,
// with new batch ids) by the next drain, but only the bytes queued the
// first time are sent: each event is accepted once.
func TestTelemetryLaneCrashAfterWriteBackDoesNotReencode(t *testing.T) {
	f := &laneFleet{Provider: stub.New()}
	old := spoolAppendAll
	t.Cleanup(func() { spoolAppendAll = old })
	var mu sync.Mutex
	first := map[string][][]byte{} // per held record: the set queued first
	calls := map[string]int{}
	spoolAppendAll = func(sp *spool.Spool, lane, origin string, recs [][]byte) error {
		mu.Lock()
		calls[origin]++
		n := calls[origin]
		if n == 1 {
			first[origin] = recs
		}
		mu.Unlock()
		if err := old(sp, lane, origin, recs); err != nil {
			return err
		}
		if n <= 2 {
			// Queued, then the "crash": the drain stops before the cursor moves.
			return errors.New("killed after write-back")
		}
		return nil
	}
	l, _ := heldLane(t, f, t.TempDir(), &spool.Memory{})
	heldN := l.held.Load()
	f.set(func(f *laneFleet) { f.approvals = nil })
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	wantHeldDeliveredOnce(t, f, heldN)

	// What was accepted is exactly what was queued the first time.
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{}
	for origin, recs := range first {
		if origin == "" || calls[origin] != 3 {
			t.Fatalf("held record %q re-gated %d times, want 3", origin, calls[origin])
		}
		for _, rec := range recs {
			var ob ownedBatch
			if err := json.Unmarshal(rec[9:], &ob); err != nil {
				t.Fatal(err)
			}
			want[string(ob.Batch.Body)] = true
		}
	}
	posted := f.batches()
	if len(posted) != len(want) {
		t.Fatalf("accepted %d batches, want the %d first queued", len(posted), len(want))
	}
	ids := map[string]bool{}
	for _, b := range posted {
		if !want[string(b.Body)] {
			t.Fatal("a batch from a later encoding was sent")
		}
		ids[laneBatchID(t, b.Signal, b.Body)] = true
	}
	if len(ids) != len(posted) {
		t.Fatalf("%d batch ids over %d batches", len(ids), len(posted))
	}
}

// A hold arrives between the drain's own check and a held record's
// re-gate (here: a lockdown lands while the batch before it is uploaded).
// The gate holds the record again. It must stay in the spool where it is,
// not be marked done, and be delivered once when the hold clears.
func TestTelemetryLaneHeldAgainStaysInSpool(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: []fleetapi.Approval{{
		ID: "a1", Kind: "legal_acceptance", Title: "Terms", Blocking: true, Required: true,
	}}}
	var lock func()
	f.postHook = func(b fleetapi.ProjectedBatch) error {
		if len(b.Body) == 1 && lock != nil {
			lock() // while the batch ahead of the held records is uploaded
			lock = nil
		}
		return nil
	}
	l, rx, _ := newTestLaneIn(t, f, t.TempDir(), &spool.Memory{}, nil)
	if !waitFor(t, func() bool { return l.dec.HoldReason() != "" && admitted(l) }) {
		t.Fatal("blocking approval did not hold the lane")
	}
	// One segment: a batch, then the held records.
	if err := l.sp.Append(telemetryLaneName, ownedRec(0)); err != nil {
		t.Fatal(err)
	}
	postClaudeTraces(t, rx)
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return l.held.Load() > 0 }) {
		t.Fatal("nothing was held")
	}
	heldN, queued := l.held.Load(), l.sp.Stats().Records
	f.set(func(*laneFleet) {
		lock = func() {
			l.cm.SetLockdown(fleetapi.LockdownState{Locked: true, Reason: "incident"})
			l.dec.invalidate()
		}
	})

	f.set(func(f *laneFleet) { f.approvals = nil }) // the first hold clears
	if !waitFor(t, func() bool { return f.postedCount() == 1 }) {
		t.Fatal("the batch ahead of the held records was not uploaded")
	}
	time.Sleep(200 * time.Millisecond) // drain ticks under the lockdown
	if n := f.postedCount(); n != 1 {
		t.Fatalf("%d batches posted during the lockdown", n)
	}
	if segs, _ := spoolFiles(t, l.sp.Dir()); len(segs) != 1 || l.sp.Stats().Records != queued-1 || l.held.Load() != heldN {
		t.Fatalf("held-again records were not left in place: segments %v, %d queued (want %d), %d held (want %d)",
			segs, l.sp.Stats().Records, queued-1, l.held.Load(), heldN)
	}

	l.cm.SetLockdown(fleetapi.LockdownState{})
	l.dec.invalidate()
	l.kick()
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	// Everything but the one-byte batch came from the held records.
	seen := map[string]bool{}
	var records int64
	for _, b := range f.batches() {
		if seen[string(b.Body)] {
			t.Fatal("a batch was accepted twice")
		}
		seen[string(b.Body)] = true
		if len(b.Body) > 1 {
			records += int64(b.Records)
		}
	}
	if records != heldN || l.dropped.Load() != 0 {
		t.Fatalf("accepted %d held records (want %d), dropped %d", records, heldN, l.dropped.Load())
	}
}
