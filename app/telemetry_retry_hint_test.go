package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/forward"
)

// tryAgain is Fleet's answer to an upload that waits behind the member's opt-out
// or removal, as the official client delivers it: a 503 with the bounded
// Retry-After in Details.
func tryAgain(seconds string) error {
	e := &fleetapi.APIError{Status: 503, Code: "try_again"}
	if seconds != "" {
		e.Details = map[string]string{"retry_after_seconds": seconds}
	}
	return e
}

// admittedLane is a lane with one session's batches refused by postErr and
// spooled.
func refusedUpload(t *testing.T, postErr error) (*laneFleet, *telemetryLane, func()) {
	t.Helper()
	f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: postErr}
	l, rx, stop := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return f.attemptCount() > 0 && !l.sp.Empty() }) {
		t.Fatalf("the refused upload wasn't kept: attempts=%d", f.attemptCount())
	}
	return f, l, stop
}

// heldUntil is when the lane's hold on the spool ends.
func heldUntil(l *telemetryLane) time.Time {
	l.retryMu.Lock()
	defer l.retryMu.Unlock()
	return l.retryAt
}

// settled waits until no more upload attempts are being made and returns how many
// there were.
func settled(f *laneFleet, quiet time.Duration) int {
	for n := f.attemptCount(); ; {
		time.Sleep(quiet)
		if m := f.attemptCount(); m == n {
			return n
		} else {
			n = m
		}
	}
}

// An upload answered "try again" with a time is sent again when that time has
// passed: not at every drain tick before it, and with the bytes it had.
func TestTelemetryUploadWaitsOutFleetsRetryAfter(t *testing.T) {
	f, l, _ := refusedUpload(t, tryAgain("2"))
	first := settled(f, 100*time.Millisecond)
	until := heldUntil(l)
	// Fleet would take it now, and the drain ticks every 20 ms: without the wait it
	// would go at once. Watched for as long as the hold lasts, less a margin.
	f.set(func(f *laneFleet) { f.postErr = nil })
	for time.Until(until) > 300*time.Millisecond {
		time.Sleep(20 * time.Millisecond)
		if n := f.attemptCount(); n != first || f.postedCount() != 0 {
			t.Fatalf("sent again %v before the time Fleet named: %d attempts then, %d now", time.Until(until).Round(time.Millisecond), first, n)
		}
	}
	if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
		t.Fatalf("never sent again; status %+v", l.Status())
	}
	if late := time.Since(until); late < 0 || late > 3*time.Second {
		t.Fatalf("sent again %v after the time Fleet named", late)
	}
	// The same bytes: a retry carries the same batch id.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.posted {
		same := false
		for _, was := range f.failed {
			same = same || bytes.Equal(was, b.Body)
		}
		if !same {
			t.Fatal("a batch was sent again with other bytes")
		}
	}
}

// With a slow drain tick, the time Fleet named is what sends the batch again:
// about a second later, not at the next tick.
func TestTelemetryUploadIsResentAtFleetsTimeNotTheNextTick(t *testing.T) {
	// No drain tick and no consent refresh comes in time to wake the sender: only
	// the wake-up set for Fleet's time can.
	oldRefresh := testRefreshEvery
	testDrainEvery, testRefreshEvery = time.Hour, time.Hour
	t.Cleanup(func() { testDrainEvery, testRefreshEvery = 20*time.Millisecond, oldRefresh })
	f, l, _ := refusedUpload(t, tryAgain("1"))
	settled(f, 100*time.Millisecond)
	until := heldUntil(l)
	f.set(func(f *laneFleet) { f.postErr = nil })
	if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
		t.Fatalf("not sent again without a tick; status %+v", l.Status())
	}
	if late := time.Since(until); late < 0 || late > 3*time.Second {
		t.Fatalf("sent again %v after the time Fleet named", late)
	}
}

// A 503 with no time, or a time of zero or less, or one that can't be read, holds
// nothing: the next tick retries, as it always has. An absurd time is bounded.
func TestTelemetryUploadRetryHintIsBounded(t *testing.T) {
	for _, seconds := range []string{"", "0", "-5", "soon"} {
		f, l, stop := refusedUpload(t, tryAgain(seconds))
		if l.retryWaiting() {
			t.Fatalf("Retry-After %q held the spool", seconds)
		}
		f.set(func(f *laneFleet) { f.postErr = nil })
		if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
			t.Fatalf("Retry-After %q: never sent again", seconds)
		}
		stop() // one lane at a time: the harness's timers are package variables
	}
	for _, seconds := range []string{"9000000000", "3600", "61"} {
		_, l, stop := refusedUpload(t, tryAgain(seconds))
		l.retryMu.Lock()
		ahead := time.Until(l.retryAt)
		l.retryMu.Unlock()
		stop()
		if ahead > time.Minute || ahead < time.Minute-10*time.Second {
			t.Fatalf("a Retry-After of %s put the retry %v ahead, want the minute a hint is bounded at", seconds, ahead)
		}
	}
	if forward.MaxRetryAfter != time.Minute || maxUploadRetryWait != forward.MaxRetryAfter || forward.MaxBackoff != time.Hour {
		t.Fatalf("the bounds are %v and %v", forward.MaxRetryAfter, maxUploadRetryWait)
	}
	// What is dropped or quarantined is as it was: a 503 is a retry, whatever it carries.
	fw := &forward.Forwarder{Poster: lanePoster{&laneFleet{Provider: stub.New(), postErr: tryAgain("5")}}}
	res := fw.Send(t.Context(), sub1Owner, fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs})
	if res.Disposition != forward.Retry || res.RetryAfter != 5*time.Second {
		t.Fatalf("%+v", res)
	}
	fw.Poster = lanePoster{&laneFleet{Provider: stub.New(), postErr: &fleetapi.APIError{Status: 400, Code: "bad", Details: map[string]string{"retry_after_seconds": "5"}}}}
	if res := fw.Send(t.Context(), sub1Owner, fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs}); res.Disposition != forward.Quarantine || res.RetryAfter != 0 {
		t.Fatalf("%+v", res)
	}
}

// Stopping the lane during the wait returns at once, and the batch is still in
// the spool for the next relay.
func TestTelemetryUploadRetryWaitDoesNotHoldShutdown(t *testing.T) {
	f, l, stop := refusedUpload(t, tryAgain("3000"))
	if !l.retryWaiting() {
		t.Fatal("no wait was set")
	}
	n := settled(f, 100*time.Millisecond)
	start := time.Now()
	stop()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("stopping took %v", took)
	}
	l.retryMu.Lock()
	timer, waiting := l.retryTimer, time.Now().Before(l.retryAt)
	l.retryMu.Unlock()
	if timer != nil || waiting {
		t.Fatal("the hold or its wake-up outlived the lane")
	}
	// A send that was in flight when the lane stopped comes back with a time to
	// wait: it arms nothing.
	l.noteRetryHint(forward.Result{Disposition: forward.Retry, RetryAfter: 5 * time.Second})
	l.retryMu.Lock()
	timer, waiting = l.retryTimer, time.Now().Before(l.retryAt)
	l.retryMu.Unlock()
	if timer != nil || waiting {
		t.Fatal("a stopped lane armed a hold")
	}
	time.Sleep(100 * time.Millisecond)
	if f.attemptCount() != n {
		t.Fatal("an upload was tried after the lane stopped")
	}
}

// One hostile answer can't hold the backlog: a Retry-After of an hour is cut to
// the bound, and the backlog goes again when that has passed. (The bound is a
// minute; the test shortens it.)
func TestTelemetryUploadHostileRetryAfterIsCutToTheBound(t *testing.T) {
	old := maxUploadRetryWait
	maxUploadRetryWait = 2 * time.Second
	t.Cleanup(func() { maxUploadRetryWait = old })
	f, l, _ := refusedUpload(t, tryAgain("3600"))
	first := settled(f, 100*time.Millisecond)
	until := heldUntil(l)
	if ahead := time.Until(until); ahead > maxUploadRetryWait {
		t.Fatalf("held %v ahead, past the bound of %v", ahead, maxUploadRetryWait)
	}
	f.set(func(f *laneFleet) { f.postErr = nil })
	// Held for the bound: ticks and kicks don't get through before it.
	for time.Until(until) > 300*time.Millisecond {
		l.kick()
		time.Sleep(20 * time.Millisecond)
		if n := f.attemptCount(); n != first {
			t.Fatalf("%d attempts inside the hold, %d before it", n, first)
		}
	}
	if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
		t.Fatalf("the backlog was never sent; status %+v", l.Status())
	}
	if late := time.Since(until); late < 0 || late > 3*time.Second {
		t.Fatalf("the backlog went %v after the bound, on a Retry-After of an hour", late)
	}
}

// A later answer replaces the hold; holds don't add up past the bound.
func TestTelemetryUploadLaterHintReplacesTheHold(t *testing.T) {
	_, l, stop := refusedUpload(t, tryAgain("50"))
	defer stop()
	for i := 0; i < 20; i++ {
		l.noteRetryHint(forward.Result{Disposition: forward.Retry, RetryAfter: 50 * time.Second})
	}
	l.noteRetryHint(forward.Result{Disposition: forward.Retry, RetryAfter: 2 * time.Second})
	l.retryMu.Lock()
	ahead := time.Until(l.retryAt)
	l.retryMu.Unlock()
	if ahead > 2*time.Second || ahead < time.Second {
		t.Fatalf("after twenty answers of 50 s and one of 2 s, the hold is %v", ahead)
	}
}

// A purge (sign-out, or an opt-out) ends the hold with the backlog it was for:
// the next account, or the next opt-in, doesn't wait out the rest of it.
func TestTelemetryUploadPurgeEndsTheHold(t *testing.T) {
	f, l, _ := refusedUpload(t, tryAgain("60"))
	if !l.retryWaiting() {
		t.Fatal("no hold was set")
	}
	settled(f, 100*time.Millisecond)
	if err := l.Purge(); err != nil {
		t.Fatal(err)
	}
	l.retryMu.Lock()
	timer, waiting := l.retryTimer, time.Now().Before(l.retryAt)
	l.retryMu.Unlock()
	if timer != nil || waiting {
		t.Fatal("the hold outlived the purge")
	}
	// What is queued after it goes at the next tick, with Fleet taking uploads.
	f.set(func(f *laneFleet) { f.postErr = nil })
	l.spoolBatch(queued{b: fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, Body: []byte("after the purge"), Records: 1}, earliest: laneNow().Add(time.Hour), owner: sub1Owner})
	start := time.Now()
	if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
		t.Fatalf("a batch queued after the purge was held; status %+v", l.Status())
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("held %v after a purge", took)
	}
}

// An upload Fleet accepts ends the hold at once: a new session's batch going up
// shows Fleet is taking uploads, and the backlog goes with it.
func TestTelemetryUploadSuccessEndsTheHold(t *testing.T) {
	// No drain tick and no consent refresh comes in time: the backlog goes because
	// the accepted upload wakes the sender.
	oldRefresh := testRefreshEvery
	testDrainEvery, testRefreshEvery = time.Hour, time.Hour
	t.Cleanup(func() { testDrainEvery, testRefreshEvery = 20*time.Millisecond, oldRefresh })
	f, l, _ := refusedUpload(t, tryAgain("60"))
	if !l.retryWaiting() {
		t.Fatal("no hold was set")
	}
	first := settled(f, 100*time.Millisecond)
	f.set(func(f *laneFleet) { f.postErr = nil })
	time.Sleep(200 * time.Millisecond)
	if f.attemptCount() != first || l.sp.Empty() {
		t.Fatal("the backlog went while it was held")
	}
	// A fresh batch, sent as it is produced, is accepted.
	start := time.Now()
	l.send(t.Context(), queued{b: fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, Body: []byte("fresh"), Records: 1}, earliest: laneNow().Add(time.Hour), owner: sub1Owner})
	if l.retryWaiting() {
		t.Fatal("the hold outlived an accepted upload")
	}
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatalf("the backlog stayed held after Fleet took an upload; status %+v", l.Status())
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the backlog went %v after Fleet took an upload", took)
	}
}

// A lane that starts again starts with no hold.
func TestTelemetryUploadRestartEndsTheHold(t *testing.T) {
	_, l, stop := refusedUpload(t, tryAgain("60"))
	stop()
	l.retryMu.Lock()
	l.retryAt = time.Now().Add(time.Minute) // as if one had been left
	l.retryMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if l.retryWaiting() {
		t.Fatal("a restarted lane kept a hold")
	}
	l.noteRetryHint(forward.Result{Disposition: forward.Retry, RetryAfter: 5 * time.Second})
	if !l.retryWaiting() {
		t.Fatal("a restarted lane arms nothing")
	}
}
