package enroll

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

// State is the enrollment state the daemon reports.
type State string

const (
	StatePending   State = "pending"    // not yet attempted
	StateEnrolled  State = "enrolled"   // last enroll/heartbeat succeeded
	StateSignedOut State = "signed_out" // no session; waiting for login
	StateRemoved   State = "removed"    // an admin removed this node: lanes stop
	StateError     State = "error"      // transient failure; retried
	// StateEarlyAccess: Fleet refused the enroll because the org is not in
	// the early-access program. Retried at the normal interval (or on a
	// kick), so admission is picked up without a new login.
	StateEarlyAccess State = "early_access_required"
)

// Defaults.
const (
	DefaultInterval = 6 * time.Hour
	DefaultJitter   = 0.10
	retryDelay      = 5 * time.Minute
)

// Heartbeat enrolls at start, then re-enrolls every Interval ±Jitter.
type Heartbeat struct {
	Enroller fleetapi.Enroller
	// Body builds a fresh body each beat, so a device rename is picked up
	// on the next heartbeat.
	Body     func() (fleetapi.EnrollBody, error)
	Clock    clock.Clock
	Interval time.Duration
	Jitter   float64
	// Rand returns a float in [0,1); nil uses math/rand.
	Rand func() float64
	// OnState is called on every state change (optional).
	OnState func(State, fleetapi.EnrollResult)
	// Kick, when it receives, triggers an immediate beat (e.g. right after
	// `kenaz login` or a device rename). Optional.
	Kick <-chan struct{}

	mu     sync.Mutex
	state  State
	result fleetapi.EnrollResult
	beats  int
}

// State returns the current state and last result.
func (h *Heartbeat) State() (State, fleetapi.EnrollResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == "" {
		return StatePending, h.result
	}
	return h.state, h.result
}

// Beats returns how many enroll calls succeeded.
func (h *Heartbeat) Beats() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beats
}

func (h *Heartbeat) set(s State, r fleetapi.EnrollResult) {
	h.mu.Lock()
	changed := h.state != s
	h.state = s
	if s == StateEnrolled {
		h.result = r
		h.beats++
	}
	cb := h.OnState
	h.mu.Unlock()
	if changed && cb != nil {
		cb(s, r)
	}
}

// Once performs a single enroll and returns the resulting state.
func (h *Heartbeat) Once(ctx context.Context) (State, error) {
	body, err := h.Body()
	if err != nil {
		h.set(StateError, fleetapi.EnrollResult{})
		return StateError, err
	}
	res, err := h.Enroller.Enroll(ctx, body)
	switch {
	case err == nil:
		h.set(StateEnrolled, res)
		return StateEnrolled, nil
	case errors.Is(err, fleetapi.ErrNotSignedIn):
		h.set(StateSignedOut, fleetapi.EnrollResult{})
		return StateSignedOut, err
	case isRemoved(err):
		h.set(StateRemoved, fleetapi.EnrollResult{})
		return StateRemoved, err
	case fleetapi.EarlyAccessRequired(err):
		h.set(StateEarlyAccess, fleetapi.EnrollResult{})
		return StateEarlyAccess, err
	default:
		h.set(StateError, fleetapi.EnrollResult{})
		return StateError, err
	}
}

func isRemoved(err error) bool {
	var ae *fleetapi.APIError
	return errors.As(err, &ae) && ae.Code == fleetapi.CodeNodeRemoved
}

// next returns the delay until the next beat: Interval ±Jitter.
func (h *Heartbeat) next() time.Duration {
	iv := h.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	j := h.Jitter
	if j <= 0 {
		j = DefaultJitter
	}
	r := rand.Float64
	if h.Rand != nil {
		r = h.Rand
	}
	return time.Duration(float64(iv) * (1 + j*(2*r()-1)))
}

// Run beats until ctx ends or the node is removed. Unofficial builds
// (ErrUnofficialBuild) stop immediately: there is nothing to talk to.
func (h *Heartbeat) Run(ctx context.Context) error {
	clk := h.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	for {
		st, err := h.Once(ctx)
		if errors.Is(err, fleetapi.ErrUnofficialBuild) {
			return err
		}
		if st == StateRemoved {
			return err
		}
		delay := h.next()
		if st == StateError || st == StateSignedOut {
			delay = retryDelay
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(delay):
		case <-h.Kick:
		}
	}
}
