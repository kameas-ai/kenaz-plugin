package approvals

import (
	"context"
	"crypto/rand"
	"errors"
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
	if !m.trusted() {
		return "", ErrUnavailable
	}
	cur, gen, err := m.current(ctx)
	if err != nil {
		return "", err
	}
	m.sweep(ctx)
	if !m.claimRun(requestID) {
		return StatusExecuting, nil
	}
	defer m.releaseRun(requestID)
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
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			st, _, serr := m.Status(ctx, requestID)
			if serr != nil {
				return "", serr
			}
			return st, nil
		}
		return "", err
	}
	return m.run(ctx, rec), nil
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
