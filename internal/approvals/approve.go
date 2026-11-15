package approvals

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"
)

func random32() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

// Approve runs a pending request once, for the person who pressed the button or answered
// the command. It rechecks the account, the profile generation, the payload hash and the
// expiry, moves the record to executing by compare-and-swap, and runs the executor with
// the stored payload and key. A second press finds it no longer pending.
func (m *Manager) Approve(ctx context.Context, requestID string) (Status, error) {
	return m.approve(ctx, requestID, "")
}

// approve is Approve for by: "" for the person, or ApprovedByGrant.
func (m *Manager) approve(ctx context.Context, requestID, by string) (Status, error) {
	rec, st, err := m.claim(ctx, requestID, by)
	if rec == nil {
		return st, err
	}
	defer m.releaseRun(requestID)
	return m.run(ctx, rec), nil
}

// claim moves a pending request to executing, with every check an approval makes. A
// non-nil record means this caller now runs it and must releaseRun; otherwise the
// status says where the request stands.
func (m *Manager) claim(ctx context.Context, requestID, by string) (*record, Status, error) {
	if !m.trusted() {
		return nil, "", ErrUnavailable
	}
	cur, gen, err := m.current(ctx)
	if err != nil {
		return nil, "", err
	}
	m.sweep(ctx)
	if !m.claimRun(requestID) {
		return nil, StatusExecuting, nil
	}
	now := m.clk.Now()
	rec, err := m.st.transition(requestID, []Status{StatusPending}, StatusExecuting, now, func(r *record) error {
		switch {
		case r.Identity != cur || r.ProfileGen != gen:
			return ErrIdentityMismatch
		case payloadHash(r.Payload) != r.PayloadSHA256:
			return ErrPayloadChanged
		case !now.Before(r.ExpiresAt):
			return ErrExpired
		}
		r.Attempts++
		r.ApprovedBy = by
		return nil
	})
	if err != nil {
		m.releaseRun(requestID)
		if errors.Is(err, ErrConflict) {
			st, _, serr := m.Status(ctx, requestID)
			if serr != nil {
				return nil, "", serr
			}
			return nil, st, nil
		}
		return nil, "", err
	}
	return rec, StatusExecuting, nil
}

// Deny closes a pending request. Its content is purged and nothing is written.
func (m *Manager) Deny(ctx context.Context, requestID string) error {
	cur, _, err := m.current(ctx)
	if err != nil {
		return err
	}
	_, err = m.st.transition(requestID, []Status{StatusPending}, StatusDenied, m.clk.Now(), func(r *record) error {
		if r.Identity != cur {
			return ErrIdentityMismatch
		}
		return nil
	})
	return err
}

// ApprovedByGrant marks a request that ran under a standing grant the person gave
// earlier, with no approval of its own.
const ApprovedByGrant = "grant"

// AutoApprover decides, before Prepare returns, whether a request just prepared runs
// under a standing grant. It gets the signed-in identity and the stored payload. It
// must answer quickly and within ctx's deadline; anything it can't settle in time is
// "not covered", and the request waits for the person. When it says covered, the relay
// runs the request the way the person's own approval would (the same checks, the same
// executor) and then calls settled with how it ended. It runs in the relay only;
// nothing a tool or the control API sends can register one or give a grant.
type AutoApprover func(ctx context.Context, who Identity, requestID string, payload []byte) (settled func(Status), covered bool)

// RegisterAutoApprover installs the grant check for operation. Call before serving.
func (m *Manager) RegisterAutoApprover(operation string, a AutoApprover) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autos == nil {
		m.autos = map[string]AutoApprover{}
	}
	m.autos[operation] = a
}

// Prepare's time budget when a grant is in play. The control client gives a call
// five seconds, so Prepare answers within prepareBudget of starting: the grant's
// decision gets at most decideWait of that, and the write whatever is left. A write
// still running then carries on, and the caller is told executing, never pending.
var (
	prepareBudget = 4 * time.Second
	decideWait    = 1500 * time.Millisecond
)

// autoApprove offers a new pending request to its operation's AutoApprover. The
// decision is made here, before Prepare answers, so the status returned is true:
// pending means the grant didn't cover it and nothing will run without the person;
// anything else means the grant ran it, or is running it.
func (m *Manager) autoApprove(ctx context.Context, who Identity, id string, r Request, started time.Time) Status {
	m.mu.Lock()
	auto := m.autos[r.Operation]
	m.mu.Unlock()
	deadline := started.Add(prepareBudget)
	if auto == nil || time.Until(deadline) <= 0 {
		return StatusPending
	}
	decideBy := time.Now().Add(decideWait)
	if decideBy.After(deadline) {
		decideBy = deadline
	}
	settled, covered := m.decide(ctx, auto, decideBy, who, id, r.Payload)
	if !covered {
		return StatusPending
	}
	if settled == nil {
		settled = func(Status) {}
	}
	ctx = context.WithoutCancel(ctx)
	rec, st, err := m.claim(ctx, id, ApprovedByGrant)
	if rec == nil {
		// It didn't start: the request is as it was, for the person.
		settled(StatusCanceled)
		if err != nil {
			return StatusPending
		}
		return st
	}
	done := make(chan Status, 1)
	go func() {
		st := StatusReconciling
		defer func() {
			if recover() != nil {
				// The executor may have sent it: the outcome is looked up, never assumed.
				_, _ = m.st.transition(id, []Status{StatusExecuting}, StatusReconciling, m.clk.Now(), nil)
				m.log.Warn("approval executor panicked", "op", r.Operation, "request", id)
				st = StatusReconciling
			}
			m.releaseRun(id)
			settled(st)
			done <- st
		}()
		st = m.run(ctx, rec)
	}()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case st := <-done:
		return st
	case <-t.C:
		return StatusExecuting
	}
}

// decide asks the AutoApprover, bounded by decideBy. A panic is "not covered".
func (m *Manager) decide(ctx context.Context, auto AutoApprover, decideBy time.Time, who Identity, id string, payload []byte) (settled func(Status), covered bool) {
	ctx, cancel := context.WithDeadline(ctx, decideBy)
	defer cancel()
	defer func() {
		if recover() != nil {
			m.log.Warn("grant check panicked", "request", id)
			settled, covered = nil, false
		}
	}()
	return auto(ctx, who, id, payload)
}

// stampApprovedBy adds who approved to result metadata, for a request a grant ran.
func stampApprovedBy(res []byte, by string) []byte {
	if by == "" {
		return res
	}
	out := map[string]json.RawMessage{}
	if len(res) > 0 && json.Unmarshal(res, &out) != nil {
		return res
	}
	out["approved_by"], _ = json.Marshal(by)
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	return b
}
