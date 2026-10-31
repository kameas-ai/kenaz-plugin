// Package gate applies the gate ordering to projected records. The
// consent decisions themselves (sign-in, holds, tier, opt-ins) belong to
// internal/consent and arrive through the Decider interface; this package owns only
// the order and the drop counters. What a record may carry is fixed earlier
// by the projector's closed allowlist; Kameas Fleet re-checks every record on
// arrival.
package gate

import (
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
)

// Decider answers consent questions. The consent package implements
// it. Implementations must be cheap (cached state, no network calls).
type Decider interface {
	// SignedIn reports whether a member session exists AND the member has
	// personally opted in to at least one class.
	SignedIn() bool
	// HoldReason returns a non-empty reason when uploads must be held:
	// org paused, lockdown, or a blocking legal acceptance / unknown
	// blocking approval (a blocking ml_notice does not hold).
	HoldReason() string
	// TierAllows reports whether the org's tier includes class.
	TierAllows(class fleetschema.Class) bool
	// OptedIn reports whether the member opted in to class.
	OptedIn(class fleetschema.Class) bool
}

// Admission is optionally implemented by a Decider whose organization may
// have a plan that does not include the plugin. While the organization is not
// admitted every record is dropped (never held). Once it is admitted, only
// records generated at or after the admission moment may be sent.
type Admission interface {
	// AdmittedSince returns the moment this device first saw Fleet accept
	// the organization; ok is false while it is not admitted (or not yet
	// known to be).
	AdmittedSince() (since time.Time, ok bool)
}

// Counters are local drop/hold counters surfaced by `kenaz status`.
// No payload is ever logged.
type Counters struct {
	mu            sync.Mutex
	NotSignedIn   int
	Held          int
	DroppedTier   int
	DroppedOptOut int
	// NotAdmitted counts records dropped because the organization is not
	// admitted (its plan does not include the plugin); BeforeAdmission counts records generated before it
	// was admitted.
	NotAdmitted     int
	BeforeAdmission int
	Passed          int
}

// AddNotAdmitted counts n items dropped before gating because the
// organization is not admitted (its plan does not include the plugin).
func (c *Counters) AddNotAdmitted(n int) {
	c.mu.Lock()
	c.NotAdmitted += n
	c.mu.Unlock()
}

// Admitted reports whether d's organization is admitted (true when d does
// not implement Admission).
func Admitted(d Decider) bool {
	a, ok := d.(Admission)
	if !ok {
		return true
	}
	_, ok = a.AdmittedSince()
	return ok
}

// Snapshot returns a copy of the counters.
func (c *Counters) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]int{
		"not_signed_in":            c.NotSignedIn,
		"held":                     c.Held,
		"dropped_tier":             c.DroppedTier,
		"dropped_opt_out":          c.DroppedOptOut,
		"dropped_not_admitted":     c.NotAdmitted,
		"dropped_before_admission": c.BeforeAdmission,
		"passed":                   c.Passed,
	}
}

// Outcome is the result of Apply.
type Outcome struct {
	// Send may be encoded and forwarded now.
	Send []project.Record
	// Hold must be kept (spool) and re-gated later.
	Hold []project.Record
	// HoldReason is set when Hold is non-empty.
	HoldReason string
}

// Apply gates records in this order:
//  1. not signed in (or no personal opt-in) → drop all
//  2. organization not admitted (plan lacks the plugin) → drop all; once admitted,
//     drop records generated before the admission moment
//  3. hold condition → hold all
//  4. tier lacks class → drop
//  5. member not opted in to class → drop
func Apply(d Decider, recs []project.Record, c *Counters) Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(recs) == 0 {
		return Outcome{}
	}
	if d == nil || !d.SignedIn() {
		c.NotSignedIn += len(recs)
		return Outcome{}
	}
	if a, ok := d.(Admission); ok {
		since, admitted := a.AdmittedSince()
		if !admitted {
			c.NotAdmitted += len(recs)
			return Outcome{}
		}
		kept := recs[:0:0]
		for _, r := range recs {
			if GeneratedBefore(r, since) {
				c.BeforeAdmission++
				continue
			}
			kept = append(kept, r)
		}
		if recs = kept; len(recs) == 0 {
			return Outcome{}
		}
	}
	if reason := d.HoldReason(); reason != "" {
		c.Held += len(recs)
		return Outcome{Hold: recs, HoldReason: reason}
	}
	var out Outcome
	for _, r := range recs {
		switch {
		case !d.TierAllows(r.Class):
			c.DroppedTier++
		case !d.OptedIn(r.Class):
			c.DroppedOptOut++
		default:
			c.Passed++
			out.Send = append(out.Send, r)
		}
	}
	return out
}

// GeneratedBefore reports whether any timestamp of r is before t. A record
// without a timestamp counts as before (it cannot be shown to be after).
func GeneratedBefore(r project.Record, t time.Time) bool {
	if r.Time.IsZero() || r.Time.Before(t) {
		return true
	}
	return !r.Start.IsZero() && r.Start.Before(t)
}

// Earliest returns the earliest timestamp across recs (zero if any record
// has none).
func Earliest(recs []project.Record) time.Time {
	var min time.Time
	for _, r := range recs {
		if r.Time.IsZero() {
			return time.Time{}
		}
		t := r.Time
		if !r.Start.IsZero() && r.Start.Before(t) {
			t = r.Start
		}
		if min.IsZero() || t.Before(min) {
			min = t
		}
	}
	return min
}
