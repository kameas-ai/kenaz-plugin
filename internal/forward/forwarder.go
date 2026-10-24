// Package forward posts projected batches to Kameas Fleet through
// fleetapi.OTLPPoster: a token bucket keeps
// under Fleet's per-(org, machine) rate, and each failure is classified as
// hold (org paused), retry (transient), refused (org not in early access)
// or quarantine (repeated 4xx).
// Payloads are never logged.
package forward

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// DefaultPerMinute keeps headroom under Fleet's 100 batches/min.
const DefaultPerMinute = 90

// Disposition tells the caller what to do with a batch after Send.
type Disposition int

const (
	// Sent: accepted by Fleet (individual records may still be dropped by
	// Fleet; see Result).
	Sent Disposition = iota + 1
	// Hold: org paused; keep the batch in the spool and retry after the
	// pause clears.
	Hold
	// Retry: transient failure; keep the batch and retry with backoff.
	Retry
	// Quarantine: Fleet rejected the batch as invalid; drop it and count.
	Quarantine
	// Refused: Fleet refused the batch because the organization is not in
	// the early-access program. Drop it at once and keep nothing: nothing
	// is collected or kept on the device until the organization is admitted.
	Refused
	// Stale: the batch's owning account is no longer the signed-in one
	// (re-checked after the rate-limit wait, and enforced by the poster
	// with identity_mismatch). Nothing was sent; drop it.
	Stale
)

// Result is the outcome of one Send.
type Result struct {
	Disposition Disposition
	Export      fleetapi.ExportResult
	Err         error
}

// Forwarder is safe for concurrent use.
type Forwarder struct {
	Poster    fleetapi.OTLPPoster
	PerMinute int
	// Current, when set, reports whether owner is still the signed-in
	// account. Send calls it after the rate-limit wait, immediately before
	// posting, because the wait can be long enough for an account switch.
	Current func(ctx context.Context, owner fleetapi.ExpectedSession) bool
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (f *Forwarder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Forwarder) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wait takes one token, sleeping until one is available.
func (f *Forwarder) wait(ctx context.Context) error {
	per := f.PerMinute
	if per <= 0 {
		per = DefaultPerMinute
	}
	rate := float64(per) / 60.0 // tokens per second
	for {
		f.mu.Lock()
		now := f.now()
		if f.last.IsZero() {
			f.tokens, f.last = float64(per), now
		}
		f.tokens += now.Sub(f.last).Seconds() * rate
		if f.tokens > float64(per) {
			f.tokens = float64(per)
		}
		f.last = now
		if f.tokens >= 1 {
			f.tokens--
			f.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - f.tokens) / rate * float64(time.Second))
		f.mu.Unlock()
		if err := f.sleep(ctx, need); err != nil {
			return err
		}
	}
}

// Send posts one batch owned by owner (the account whose session stamped
// it) and classifies the outcome. The owner is re-checked after the
// rate-limit wait and attached to the Post with WithExpectedSession, so
// the poster refuses it if another account's credentials would be used.
func (f *Forwarder) Send(ctx context.Context, owner fleetapi.ExpectedSession, b fleetapi.ProjectedBatch) Result {
	if err := f.wait(ctx); err != nil {
		return Result{Disposition: Retry, Err: err}
	}
	if f.Current != nil && !f.Current(ctx, owner) {
		return Result{Disposition: Stale}
	}
	res, err := f.Poster.Post(fleetapi.WithExpectedSession(ctx, owner), b)
	if err == nil {
		return Result{Disposition: Sent, Export: res}
	}
	if fleetapi.APICode(err) == fleetapi.CodeIdentityMismatch {
		return Result{Disposition: Stale, Err: err}
	}
	if _, paused := fleetapi.OrgPaused(err); paused {
		return Result{Disposition: Hold, Err: err}
	}
	if fleetapi.EarlyAccessRequired(err) {
		return Result{Disposition: Refused, Err: err}
	}
	var ae *fleetapi.APIError
	if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 401 && ae.Status != 408 && ae.Status != 429 {
		return Result{Disposition: Quarantine, Err: err}
	}
	return Result{Disposition: Retry, Err: err}
}

// Backoff returns the retry delay for attempt n (0-based): exponential from
// 5s, capped at 1h.
func Backoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 0; i < n && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}
