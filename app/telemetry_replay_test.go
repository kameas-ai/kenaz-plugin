package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// ownedRec is a spooled batch stamped for the default account; its body
// is the single byte n, so a test can tell the batches apart.
func ownedRec(n int) []byte {
	b, _ := json.Marshal(ownedBatch{Owner: sub1Owner, Batch: fleetapi.ProjectedBatch{
		Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{byte(n)}, Records: 1,
	}})
	out := make([]byte, 9, 9+len(b))
	out[0] = tagOwnedBatch
	binary.BigEndian.PutUint64(out[1:], uint64(fixtures.Base.UnixNano()))
	return append(out, b...)
}

// seededLane starts a lane over dir and, once it is admitted, queues n
// batches (bodies 0..n-1) in one spool segment. Nothing drains until they
// are all there.
func seededLane(t *testing.T, f *laneFleet, dir string, ks *spool.Memory, n int) (*telemetryLane, func()) {
	t.Helper()
	l, _, stop := newTestLaneIn(t, f, dir, ks, nil)
	l.Hold("seeding")
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	for i := 0; i < n; i++ {
		if err := l.sp.Append(telemetryLaneName, ownedRec(i)); err != nil {
			t.Fatal(err)
		}
	}
	l.Resume()
	return l, stop
}

// postedBodies returns the first body byte of every accepted upload, in order.
func postedBodies(f *laneFleet) []int {
	var out []int
	for _, b := range f.batches() {
		out = append(out, int(b.Body[0]))
	}
	return out
}

func wantBodies(t *testing.T, f *laneFleet, want ...int) {
	t.Helper()
	got := postedBodies(f)
	if len(got) != len(want) {
		t.Fatalf("posted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("posted %v, want %v", got, want)
		}
	}
}

// spoolSidecars lists everything in the spool directory that is not a segment.
func spoolSidecars(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".seg") {
			out = append(out, e.Name())
		}
	}
	return out
}

// A batch that fails part-way through a segment is retried on later
// drains, and the batches Fleet accepted before it are posted exactly
// once in total, however many drains it takes.
func TestTelemetryLaneFailedBatchDoesNotReplayDelivered(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// dropped: the lane gives the batch up (quarantine) instead of retrying.
		dropped bool
	}{
		{"429", &fleetapi.APIError{Status: http.StatusTooManyRequests}, false},
		{"5xx", &fleetapi.APIError{Status: http.StatusServiceUnavailable}, false},
		{"timeout", context.DeadlineExceeded, false},
		{"401", &fleetapi.APIError{Status: http.StatusUnauthorized}, false},
		{"rejected 4xx", &fleetapi.APIError{Status: http.StatusBadRequest}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tries := 0
			failing := true
			f := &laneFleet{Provider: stub.New(), optedIn: true}
			f.postHook = func(b fleetapi.ProjectedBatch) error {
				if b.Body[0] == 3 && failing {
					tries++
					return tc.err
				}
				return nil
			}
			l, _ := seededLane(t, f, t.TempDir(), &spool.Memory{}, 6)
			if tc.dropped {
				if !waitFor(t, func() bool { return l.sp.Empty() }) {
					t.Fatal("spool never drained")
				}
				wantBodies(t, f, 0, 1, 2, 4, 5)
				return
			}
			// Several drains hit the failing batch.
			if !waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return tries >= 4 }) {
				t.Fatal("failing batch was not retried")
			}
			wantBodies(t, f, 0, 1, 2)
			if c := spoolSidecars(t, l.sp.Dir()); len(c) != 1 {
				t.Fatalf("want one cursor beside the kept segment, got %v", c)
			}
			f.set(func(*laneFleet) { failing = false })
			if !waitFor(t, func() bool { return l.sp.Empty() }) {
				t.Fatalf("spool never drained; status %+v", l.Status())
			}
			wantBodies(t, f, 0, 1, 2, 3, 4, 5)
			if c := spoolSidecars(t, l.sp.Dir()); len(c) != 0 {
				t.Fatalf("cursor left after the segment drained: %v", c)
			}
		})
	}
}

// A hold that arrives mid-drain stops the drain; when it clears, the drain
// resumes after the last accepted batch.
func TestTelemetryLaneHoldMidDrainDoesNotReplay(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	var hold func(string)
	f.postHook = func(b fleetapi.ProjectedBatch) error {
		if b.Body[0] == 2 {
			hold("test")
		}
		return nil
	}
	l, _, _ := newTestLaneIn(t, f, t.TempDir(), &spool.Memory{}, nil)
	l.Hold("seeding")
	hold = l.Hold
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	for i := 0; i < 6; i++ {
		if err := l.sp.Append(telemetryLaneName, ownedRec(i)); err != nil {
			t.Fatal(err)
		}
	}
	l.Resume()
	if !waitFor(t, func() bool { return f.postedCount() == 3 }) {
		t.Fatalf("posted %v", postedBodies(f))
	}
	time.Sleep(150 * time.Millisecond) // several drain ticks under the hold
	wantBodies(t, f, 0, 1, 2)
	if l.sp.Empty() {
		t.Fatal("held batches were not kept")
	}
	l.Resume()
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	wantBodies(t, f, 0, 1, 2, 3, 4, 5)
}

// Stop mid-drain: the next relay (a new process over the same state
// directory) sends only what the last one had not.
func TestTelemetryLaneStopMidDrainDoesNotReplay(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	dir, ks := t.TempDir(), &spool.Memory{}
	var cancelLane func()
	f.postHook = func(b fleetapi.ProjectedBatch) error {
		if b.Body[0] == 2 {
			cancelLane() // the daemon is stopping; this upload still succeeds
		}
		return nil
	}
	l, _, stop := newTestLaneIn(t, f, dir, ks, nil)
	l.Hold("seeding")
	l.mu.Lock()
	cancelLane = l.cancel
	l.mu.Unlock()
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	for i := 0; i < 6; i++ {
		if err := l.sp.Append(telemetryLaneName, ownedRec(i)); err != nil {
			t.Fatal(err)
		}
	}
	l.Resume()
	if !waitFor(t, func() bool { return f.postedCount() == 3 }) {
		t.Fatalf("posted %v", postedBodies(f))
	}
	stop()
	l.sp.Close()
	wantBodies(t, f, 0, 1, 2)
	f.set(func(f *laneFleet) { f.postHook = nil })

	next, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return next.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", next.Status())
	}
	wantBodies(t, f, 0, 1, 2, 3, 4, 5)
	if c := spoolSidecars(t, next.sp.Dir()); len(c) != 0 {
		t.Fatalf("cursor left: %v", c)
	}
}

// Logout and an account change purge the lane: no cursor is left for a
// later spool to pick up.
func TestTelemetryLanePurgeLeavesNoCursor(t *testing.T) {
	partly := func(t *testing.T) (*laneFleet, *telemetryLane, func()) {
		f := &laneFleet{Provider: stub.New(), optedIn: true}
		f.postHook = func(b fleetapi.ProjectedBatch) error {
			if b.Body[0] >= 3 {
				return &fleetapi.APIError{Status: http.StatusServiceUnavailable}
			}
			return nil
		}
		l, stop := seededLane(t, f, t.TempDir(), &spool.Memory{}, 6)
		if !waitFor(t, func() bool { return f.postedCount() == 3 && len(spoolSidecars(t, l.sp.Dir())) == 1 }) {
			t.Fatalf("no partly delivered segment: posted %v", postedBodies(f))
		}
		return f, l, stop
	}
	empty := func(t *testing.T, dir string) bool {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(ents) == 0
	}
	t.Run("logout", func(t *testing.T) {
		_, l, stop := partly(t)
		stop()
		if err := l.Purge(); err != nil {
			t.Fatal(err)
		}
		if !empty(t, l.sp.Dir()) {
			t.Fatalf("left after purge: %v", spoolSidecars(t, l.sp.Dir()))
		}
	})
	t.Run("logout from another process", func(t *testing.T) {
		_, l, stop := partly(t)
		stop()
		l.sp.Close()
		if err := spool.PurgeDir(l.sp.Dir()); err != nil {
			t.Fatal(err)
		}
		if !empty(t, l.sp.Dir()) {
			t.Fatalf("left after PurgeDir: %v", spoolSidecars(t, l.sp.Dir()))
		}
	})
	t.Run("account change", func(t *testing.T) {
		f, l, _ := partly(t)
		f.set(func(f *laneFleet) { f.user = "sub-2" })
		if !waitFor(t, func() bool { return empty(t, l.sp.Dir()) }) {
			t.Fatalf("left after account change: %v", spoolSidecars(t, l.sp.Dir()))
		}
		wantBodies(t, f, 0, 1, 2)
	})
}

// postClaudeTraces sends the Claude Code trace fixture (spans get fresh
// random ids every time they are encoded).
func postClaudeTraces(t *testing.T, rx http.Handler) {
	t.Helper()
	body, err := proto.Marshal(fixtures.ClaudeTraces())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	rx.ServeHTTP(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("receiver status %d: %s", w.Code, w.Body.String())
	}
}

// heldLane returns a lane held by a blocking approval with held records
// (logs and spans) in its spool.
func heldLane(t *testing.T, f *laneFleet, dir string, ks *spool.Memory) (*telemetryLane, func()) {
	t.Helper()
	f.set(func(f *laneFleet) {
		f.optedIn = true
		f.approvals = []fleetapi.Approval{{ID: "a1", Kind: "legal_acceptance", Title: "Terms", Blocking: true, Required: true}}
	})
	l, rx, stop := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return l.dec.HoldReason() != "" && admitted(l) }) {
		t.Fatal("blocking approval did not hold the lane")
	}
	postClaudeTraces(t, rx)
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return l.held.Load() > 0 && !l.sp.Empty() }) {
		t.Fatal("nothing was held")
	}
	return l, stop
}

// A held record is re-gated and encoded once. While a batch after it in
// the same segment keeps failing, the held record is not re-gated again
// on every drain; and when the upload of what it became fails, the retry
// sends the same bytes (spans would get fresh ids if encoded again).
func TestTelemetryLaneHeldRecordEncodedOnce(t *testing.T) {
	f := &laneFleet{Provider: stub.New()}
	marker := []byte{0xEE}
	var spanTries [][]byte // every body a span upload was attempted with
	markerTries := 0
	failSpans, failMarker := true, true
	f.postHook = func(b fleetapi.ProjectedBatch) error {
		fail := &fleetapi.APIError{Status: http.StatusServiceUnavailable}
		switch {
		case bytes.Equal(b.Body, marker):
			markerTries++
			if failMarker {
				return fail
			}
		case b.Signal == fleetapi.SignalTraces:
			spanTries = append(spanTries, b.Body)
			if failSpans {
				return fail
			}
		}
		return nil
	}
	l, _ := heldLane(t, f, t.TempDir(), &spool.Memory{})
	// A batch behind the held records, in the same segment.
	rec := ownedRec(0)
	b, _ := json.Marshal(ownedBatch{Owner: sub1Owner, Batch: fleetapi.ProjectedBatch{
		Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: marker, Records: 1,
	}})
	if err := l.sp.Append(telemetryLaneName, append(rec[:9:9], b...)); err != nil {
		t.Fatal(err)
	}

	// The hold clears; the batch behind the held records keeps failing, so
	// their segment is kept across many drains.
	f.set(func(f *laneFleet) { f.approvals = nil })
	if !waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return markerTries >= 3 }) {
		t.Fatal("the batch behind the held records was not retried")
	}
	queued := l.sp.Stats().Records // the failing batch and what the held records became
	if !waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return markerTries >= 8 }) {
		t.Fatal("the batch behind the held records was not retried")
	}
	if n := f.postedCount(); n != 0 {
		t.Fatalf("%d batches posted from held records replayed with their segment", n)
	}
	if got := l.sp.Stats().Records; got != queued || queued < 2 {
		t.Fatalf("held records were re-gated again: %d queued, then %d", queued, got)
	}

	// That batch goes through; now the span batch fails a few times.
	f.set(func(*laneFleet) { failMarker = false })
	tries := func() (n int, distinct map[string]bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		distinct = map[string]bool{}
		for _, b := range spanTries {
			distinct[string(b)] = true
		}
		return len(spanTries), distinct
	}
	if !waitFor(t, func() bool { n, _ := tries(); return n >= 4 }) {
		t.Fatal("the span batch was not retried (did the fixture produce one?)")
	}
	f.set(func(*laneFleet) { failSpans = false })
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	n, distinct := tries()
	if len(distinct) != 1 {
		t.Fatalf("%d attempts sent %d different encodings of the same spans", n, len(distinct))
	}
	posted := map[string]int{}
	for _, b := range f.batches() {
		posted[string(b.Body)]++
	}
	if f.postedCount() != queued || len(posted) != queued {
		t.Fatalf("posted %d batches (%d distinct), want each of the %d queued once", f.postedCount(), len(posted), queued)
	}
	for body := range distinct {
		if posted[body] != 1 {
			t.Fatalf("the retried span batch was accepted %d times", posted[body])
		}
	}
}

// A held record whose consent no longer allows it (the member opted out
// while the relay was down) is dropped when the hold clears, never sent.
func TestTelemetryLaneHeldRecordNoLongerPermittedDropped(t *testing.T) {
	f := &laneFleet{Provider: stub.New()}
	dir, ks := t.TempDir(), &spool.Memory{}
	l, stop := heldLane(t, f, dir, ks)
	stop()
	l.sp.Close()
	f.set(func(f *laneFleet) { f.optedIn, f.approvals = false, nil })

	next, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return next.sp.Empty() }) {
		t.Fatalf("held records neither sent nor dropped; status %+v", next.Status())
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.attemptCount(); n != 0 {
		t.Fatalf("%d uploads attempted after the opt-out", n)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "spool")); len(ents) != 0 {
		t.Fatalf("left in spool: %d files", len(ents))
	}
}

// Telemetry queued for more than 7 days is dropped when a drain starts,
// never sent, and the drop shows in the lane's dropped counter.
func TestTelemetryLaneDropsExpiredSegmentAtDrain(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	dir, ks := t.TempDir(), &spool.Memory{}
	l, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	l.Hold("seeding")
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	// Two batches queued eight days ago (a second handle on the same spool
	// with an old clock), then one queued now.
	key, err := spool.GetOrCreateKey(ks, "dev")
	if err != nil {
		t.Fatal(err)
	}
	old, err := spool.Open(l.sp.Dir(), key, spool.Options{Clock: clock.NewFake(time.Now().Add(-8 * 24 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := old.Append(telemetryLaneName, ownedRec(i)); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	if err := l.sp.Append(telemetryLaneName, ownedRec(7)); err != nil {
		t.Fatal(err)
	}
	l.Resume()
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("spool never drained; status %+v", l.Status())
	}
	wantBodies(t, f, 7)
	if got := l.Status().Dropped; got != 2 {
		t.Fatalf("dropped = %d, want the 2 expired batches", got)
	}
}
