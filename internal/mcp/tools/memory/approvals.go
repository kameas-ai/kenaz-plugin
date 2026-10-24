package memorytools

import (
	"context"

	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Submitter adapts the relay's approval client (a Preparer: it can
// prepare and read status, never approve) to memory.Submitter.
func Submitter(p approvals.Preparer) memory.Submitter {
	if p == nil {
		return nil
	}
	return submitter{p}
}

type submitter struct{ p approvals.Preparer }

func (s submitter) Submit(ctx context.Context, r memory.ApprovalRequest) (memory.Pending, error) {
	pd, err := approvals.Submit(ctx, s.p, approvals.Request{Operation: r.Operation, Env: r.Env, OrgID: r.OrgID, UserID: r.UserID,
		TargetIDs: r.TargetIDs, Payload: r.Payload, IdempotencyKey: r.IdempotencyKey, Summary: r.Summary, Impact: r.Impact})
	if err != nil {
		return memory.Pending{}, err
	}
	return memory.Pending{RequestID: pd.RequestID, Status: memory.ApprovalStatus(pd.Status), ApproveURL: pd.ApproveURL, Message: pd.Message}, nil
}

func (s submitter) Status(ctx context.Context, id string) (memory.ApprovalStatus, []byte, error) {
	st, b, err := s.p.Status(ctx, id)
	return memory.ApprovalStatus(st), b, err
}
