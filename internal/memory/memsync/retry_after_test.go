package memsync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// absurdSeconds is a Retry-After no server means: 285 years.
const absurdSeconds = 9000000000

// A Retry-After is honoured up to the cycle's longest backoff and no further, and
// one that is zero, negative or too large to be a duration is no answer at all.
func TestRetryAfterIsBounded(t *testing.T) {
	withSeconds := func(s string) error {
		return &fleetapi.APIError{Status: 429, Code: fleetapi.CodeRateLimited, Details: map[string]string{"retry_after_seconds": s}}
	}
	for _, c := range []struct {
		seconds string
		want    time.Duration
		ok      bool
	}{
		{"1", time.Second, true},
		{"30", 30 * time.Second, true},
		{"300", 5 * time.Minute, true},
		{"301", maxRetryAfter, true},
		{"86400", maxRetryAfter, true},
		{"9000000000", maxRetryAfter, true},
		{"0", 0, false},
		{"-30", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		// Past what a duration holds: the product wraps, and a negative is no answer.
		{"9223372036854775807", 0, false},
		{"99999999999999999999999", 0, false},
	} {
		got, ok := retryAfter(withSeconds(c.seconds), maxRetryAfter)
		if c.seconds == "9223372036854775807" {
			// Whatever the wrap gives, it is never past the bound.
			if got > maxRetryAfter || got < 0 {
				t.Errorf("%s: %v", c.seconds, got)
			}
			continue
		}
		if got != c.want || ok != c.ok {
			t.Errorf("Retry-After %q: %v %v, want %v %v", c.seconds, got, ok, c.want, c.ok)
		}
	}
	if got, ok := retryAfter(withSeconds("1800"), time.Minute); got != time.Minute || !ok {
		t.Fatalf("the bound is per use: %v %v", got, ok)
	}
	if _, ok := retryAfter(errors.New("connection lost"), maxRetryAfter); ok {
		t.Fatal("an error with no Retry-After had one")
	}
	if maxRetryAfter != 5*time.Minute || maxEraseWait != 5*time.Minute {
		t.Fatalf("the bounds are %v and %v", maxRetryAfter, maxEraseWait)
	}
}

// The forget-all wait: Fleet's time when it is sane, the bound when it is absurd,
// and the default when it is none.
func TestEraseWaitIsBounded(t *testing.T) {
	d := newDevice(t, fleetapitest.NewMemoryFleet(), alice)
	for seconds, want := range map[int]time.Duration{
		30: 30 * time.Second, 7: 7 * time.Second, absurdSeconds: maxEraseWait, 1800: maxEraseWait, 0: eraseRetryWait, -5: eraseRetryWait,
	} {
		for _, status := range []int{409, 503} {
			if got := d.w.eraseWait(fleetapitest.EraseInProgress(status, seconds)); got != want {
				t.Errorf("erase_in_progress %d, Retry-After %d: waits %v, want %v", status, seconds, got, want)
			}
		}
	}
}

// A rate-limited push with an absurd Retry-After: the cycle's wait and the op's
// own next try are both bounded, so the note goes up once the bound has passed.
func TestPushRetryDelayIsBounded(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	f.Fault = failingOnce("push", false, fleetapitest.RateLimited(absurdSeconds))
	d := newDevice(t, f, alice)
	d.enable()
	id := d.save("x", "rate limited", true)
	err := d.w.RunOnce(context.Background())
	if ra, ok := fleetapi.RetryAfter(err); !ok || ra != absurdSeconds*time.Second {
		t.Fatalf("the provider's answer changed: %v %v", ra, ok)
	}
	if wait := d.w.nextBackoff(err); wait != maxRetryAfter {
		t.Fatalf("the cycle waits %v", wait)
	}
	// Not before the bound...
	d.st.SetClock(func() time.Time { return time.Now().Add(maxRetryAfter - 20*time.Second) })
	d.sync()
	if n, _ := d.get(id); n.SyncState != memory.StatePending {
		t.Fatalf("sent before the bound: %+v", n)
	}
	// ...and just after it, not in 285 years.
	d.st.SetClock(func() time.Time { return time.Now().Add(maxRetryAfter + 20*time.Second) })
	d.sync()
	if n, _ := d.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("still waiting after the bound: %+v", n)
	}

	// A sane Retry-After is honoured as sent: longer than the first backoff.
	f2 := fleetapitest.NewMemoryFleet()
	f2.Fault = failingOnce("push", false, fleetapitest.RateLimited(90))
	d2 := newDevice(t, f2, alice)
	d2.enable()
	id2 := d2.save("y", "rate limited", true)
	err = d2.w.RunOnce(context.Background())
	if wait := d2.w.nextBackoff(err); wait != 90*time.Second {
		t.Fatalf("the cycle waits %v, want Fleet's 90s", wait)
	}
	d2.st.SetClock(func() time.Time { return time.Now().Add(60 * time.Second) })
	d2.sync()
	if n, _ := d2.get(id2); n.SyncState != memory.StatePending {
		t.Fatalf("sent before Fleet's Retry-After: %+v", n)
	}
	d2.st.SetClock(func() time.Time { return time.Now().Add(100 * time.Second) })
	d2.sync()
	if n, _ := d2.get(id2); n.SyncState != memory.StateSynced {
		t.Fatalf("after Fleet's Retry-After: %+v", n)
	}
}

// While the worker waits out a Retry-After, a kick starts the next cycle at once
// and shutdown returns at once, however long Fleet said to wait.
func TestRetryAfterWaitIsInterruptible(t *testing.T) {
	for name, fault := range map[string]func() (string, error){
		"erase in progress": func() (string, error) { return "forget_all", fleetapitest.EraseInProgress(409, absurdSeconds) },
		"rate limited":      func() (string, error) { return "push", fleetapitest.RateLimited(absurdSeconds) },
	} {
		t.Run(name, func(t *testing.T) {
			call, ferr := fault()
			f := fleetapitest.NewMemoryFleet()
			d := newDevice(t, f, alice)
			d.enable()
			if call == "forget_all" {
				forgetAllOn(t, d)
			} else {
				d.save("x", "rate limited", true)
			}
			f.Fault = func(c string, after bool) error {
				if c == call && !after {
					return ferr
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- d.w.Run(ctx) }()
			cycles := func(n int64) {
				t.Helper()
				for end := time.Now().Add(10 * time.Second); d.w.cycles.Load() < n; time.Sleep(5 * time.Millisecond) {
					if time.Now().After(end) {
						t.Fatalf("%d cycles, want %d", d.w.cycles.Load(), n)
					}
				}
			}
			cycles(1)
			time.Sleep(50 * time.Millisecond) // into the wait
			d.w.Kick()
			cycles(2)
			time.Sleep(50 * time.Millisecond)
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
					t.Fatalf("%v after %v", err, time.Since(start))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown waited out the Retry-After")
			}
		})
	}
}

// Fleet's 503 busy carries a Retry-After (5 s when a member's state is held, 30 s
// when the server is running its quota of erases): it is honoured, bounded like
// any other.
func TestBusyRetryAfterIsHonoured(t *testing.T) {
	busy := func(seconds string) error {
		return &fleetapi.APIError{Status: 503, Code: "busy", Details: map[string]string{"retry_after_seconds": seconds}}
	}
	d := newDevice(t, fleetapitest.NewMemoryFleet(), alice)
	if wait := d.w.nextBackoff(busy("30")); wait != 30*time.Second {
		t.Fatalf("waits %v, want Fleet's 30s", wait)
	}
	d.w.backoff = 0
	if wait := d.w.nextBackoff(busy("9000000000")); wait != maxRetryAfter {
		t.Fatalf("waits %v", wait)
	}
	d.w.backoff = 0
	// A 503 with no time is the cycle's own backoff, as any failure is.
	if wait := d.w.nextBackoff(&fleetapi.APIError{Status: 503, Code: "busy"}); wait < minBackoff/2 || wait > minBackoff {
		t.Fatalf("waits %v", wait)
	}
	// A busy push: the op waits Fleet's time, not only its own first backoff.
	f := fleetapitest.NewMemoryFleet()
	f.Fault = failingOnce("push", false, busy("90"))
	p := newDevice(t, f, alice)
	p.enable()
	id := p.save("x", "busy", true)
	_ = p.w.RunOnce(context.Background())
	p.st.SetClock(func() time.Time { return time.Now().Add(60 * time.Second) })
	p.sync()
	if n, _ := p.get(id); n.SyncState != memory.StatePending {
		t.Fatalf("sent before Fleet's Retry-After: %+v", n)
	}
	p.st.SetClock(func() time.Time { return time.Now().Add(100 * time.Second) })
	p.sync()
	if n, _ := p.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("after Fleet's Retry-After: %+v", n)
	}
}

// A forget-all refused for its allowance is asked for again every five minutes,
// not at the half hour Fleet names: Fleet's allowance refills on its own clock, a
// refused request costs nothing, and an erase mustn't land half an hour after
// Fleet would have taken it. Nothing sleeps meanwhile; a kick or a restart
// doesn't send it sooner; and the status names no time.
func TestForgetAllAllowanceRefusalIsAskedAgainEveryFiveMinutes(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d.w.o.Now = clock
	d.enable()
	forgetAllOn(t, d)
	// asked counts every forget-all that reaches Fleet, refused or not.
	asked, refuse := 0, true
	f.Fault = func(call string, after bool) error {
		if call == "forget_all" && !after {
			asked++
			if refuse {
				return fleetapitest.RateLimited(1800)
			}
		}
		return nil
	}
	err := d.w.RunOnce(ctx)
	if !errors.Is(err, errEraseWaiting) || errors.Is(err, errEraseRunning) || fleetapi.APICode(err) != fleetapi.CodeRateLimited {
		t.Fatalf("%v", err)
	}
	if at, _ := d.st.EraseRetryAt(ctx); !at.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("the next try is at %v, want five minutes from %v, not Fleet's half hour", at, now)
	}
	st, serr := d.st.Status(ctx)
	if serr != nil || !st.ErasePending || st.StatusCode != CodeRateLimited {
		t.Fatalf("%+v %v", st, serr)
	}
	// The cycle sleeps until the erase is due, and that isn't counted as a failure.
	d.w.backoff = time.Minute
	if wait := d.w.allowanceWait(ctx); wait != 5*time.Minute || d.w.backoff != 0 {
		t.Fatalf("the cycle sleeps %v (backoff %v)", wait, d.w.backoff)
	}
	// Before then, however often it runs, at a kick or after a restart, Fleet
	// isn't asked.
	for i := 0; i < 4; i++ {
		now = now.Add(time.Minute)
		if err := d.w.RunOnce(ctx); !errors.Is(err, errEraseWaiting) {
			t.Fatalf("%v", err)
		}
	}
	d.restart()
	d.w.o.Now = clock
	if err := d.w.RunOnce(ctx); !errors.Is(err, errEraseWaiting) {
		t.Fatalf("after a restart: %v", err)
	}
	if asked != 1 {
		t.Fatalf("Fleet was asked %d more times within five minutes", asked-1)
	}
	if wait := d.w.allowanceWait(ctx); wait != time.Minute {
		t.Fatalf("four minutes in, the cycle sleeps %v", wait)
	}
	// At five minutes it asks again; still refused, it asks five minutes after that:
	// six tries in the half hour Fleet named, where waiting for it would have made one.
	for i := 2; i <= 6; i++ {
		now = now.Add(time.Minute)
		if i > 2 {
			now = now.Add(4 * time.Minute)
		}
		if err := d.w.RunOnce(ctx); !errors.Is(err, errEraseWaiting) || asked != i {
			t.Fatalf("try %d: %v, asked %d times", i, err, asked)
		}
	}
	// Fleet has room again: the next try is taken, and the erase completes.
	now = now.Add(5 * time.Minute)
	refuse = false
	if err := d.w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if asked != 7 || now.Sub(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)) != 30*time.Minute {
		t.Fatalf("asked %d times by %v", asked, now)
	}
	if pending, _ := d.st.ErasePending(ctx); pending {
		t.Fatal("the erase stayed pending")
	}
	if at, _ := d.st.EraseRetryAt(ctx); !at.IsZero() {
		t.Fatalf("the retry time outlived the erase: %v", at)
	}
}

// The wait is Fleet's time or five minutes, whichever is shorter, and a minute at
// least, so that neither a short Retry-After nor a 429 with none lets kicks send
// one request after another. A stored time no worker could have set holds nothing up.
func TestForgetAllAllowanceWaitIsBounded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for seconds, want := range map[int]time.Duration{
		absurdSeconds: 5 * time.Minute, 7200: 5 * time.Minute, 1800: 5 * time.Minute, 301: 5 * time.Minute,
		300: 5 * time.Minute, 120: 2 * time.Minute, 60: time.Minute, 5: time.Minute, 1: time.Minute,
		0: 5 * time.Minute, -5: 5 * time.Minute,
	} {
		f := fleetapitest.NewMemoryFleet()
		d := newDevice(t, f, alice)
		d.w.o.Now = func() time.Time { return now }
		d.enable()
		forgetAllOn(t, d)
		asked := 0
		f.Fault = func(call string, after bool) error {
			if call == "forget_all" && !after {
				asked++
				return fleetapitest.RateLimited(seconds)
			}
			return nil
		}
		err := d.w.RunOnce(ctx)
		at, _ := d.st.EraseRetryAt(ctx)
		if !errors.Is(err, errEraseWaiting) || !at.Equal(now.Add(want)) {
			t.Fatalf("Retry-After %d: %v, next try %v ahead, want %v", seconds, err, at.Sub(now), want)
		}
		// A storm of kicks inside the wait sends nothing.
		for i := 0; i < 20; i++ {
			if err := d.w.RunOnce(ctx); !errors.Is(err, errEraseWaiting) {
				t.Fatalf("Retry-After %d: %v", seconds, err)
			}
		}
		if asked != 1 {
			t.Fatalf("Retry-After %d: Fleet was asked %d times", seconds, asked)
		}
	}
	// A stored time further ahead than any worker sets (an earlier version's half
	// hour, a moved clock) doesn't hold the erase.
	for _, ahead := range []time.Duration{5*time.Minute + time.Second, 30 * time.Minute, 48 * time.Hour} {
		f := fleetapitest.NewMemoryFleet()
		d := newDevice(t, f, alice)
		d.w.o.Now = func() time.Time { return now }
		d.enable()
		forgetAllOn(t, d)
		if err := d.st.SetEraseRetryAt(ctx, now.Add(ahead)); err != nil {
			t.Fatal(err)
		}
		if err := d.w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if pending, _ := d.st.ErasePending(ctx); pending {
			t.Fatalf("a stored time %v ahead held the erase back", ahead)
		}
	}
}

// While the erase waits for Fleet's time the worker's loop is awake: a kick runs
// a cycle at once without asking Fleet again, and shutdown returns at once.
func TestForgetAllAllowanceWaitIsInterruptible(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	d := newDevice(t, f, alice)
	d.enable()
	forgetAllOn(t, d)
	var asked atomic.Int64
	f.Fault = func(call string, after bool) error {
		if call == "forget_all" && !after {
			asked.Add(1)
			return fleetapitest.RateLimited(1800)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.w.Run(ctx) }()
	cycles := func(n int64) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); d.w.cycles.Load() < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%d cycles, want %d", d.w.cycles.Load(), n)
			}
		}
	}
	cycles(1)
	for i := int64(2); i <= 4; i++ {
		time.Sleep(30 * time.Millisecond)
		d.w.Kick()
		cycles(i)
	}
	time.Sleep(30 * time.Millisecond)
	if n := asked.Load(); n != 1 {
		t.Fatalf("Fleet was asked %d times; kicks must not ask before its time", n)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
			t.Fatalf("%v after %v", err, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for Fleet's time")
	}
}
