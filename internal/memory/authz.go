package memory

import (
	"context"
	"errors"
)

// Human authorization.
//
// Memory writes that reach Kameas Fleet, or that are irreversible for an
// account, are authorized only through the relay's approval service: the
// user approves the exact stored payload on the local approval page with a
// WebAuthn authenticator. No CLI prompt, flag or MCP argument approves
// anything. This package stays free of the approvals package (and its
// dependencies) and codes against two narrow sides:
//
//   - Submitter (MCP process, CLI): prepares a request and reads its
//     status. It cannot approve. internal/mcp/tools/memory adapts
//     approvals.Submit / approvals.Preparer to it.
//   - Registrar (relay daemon only): RegisterExecutors installs one
//     Executor (and a Reconciler) per operation. The approval service runs
//     an executor only after verifying the assertion, with exactly the
//     stored payload and idempotency key. Executors open the payload's
//     profile store in the daemon and apply it idempotently (the key is
//     the store request key, so a replay returns the first result).
//
// What is gated when (Service.gate):
//
//   - The signed-out device notebook has no Fleet effect and no signed-in
//     identity to approve with: save, update and forget run directly, as
//     before sync existed. forget-all, sync-enable, sync-approve and adopt
//     are refused there (the notebook's own delete-all is `kenaz memory
//     purge`).
//   - An account profile: forget-all, sync-enable, sync-approve and adopt
//     are always submitted for approval. A save, update or forget is
//     submitted only when it reaches Fleet: sync is on for the account,
//     or (update, forget) Fleet may hold the note. Otherwise it is device
//     data and runs directly, like the notebook. The approval's
//     idempotency key is derived from the operation, the payload and the
//     store's clock, so an agent's retry returns the same request.
//   - If approval is unavailable (no authenticator enrolled, relay not
//     running): forget-all, sync-enable, sync-approve and adopt are
//     refused (approval_unavailable). Save, update and forget apply on the
//     device only, and whatever they would change on Fleet (an upload, a
//     title write, a forget of a synced note) is queued "unapproved" and
//     is not sent until the user approves it with memory.sync_approve.
//   - Turning sync off, reverting a refused edit, reading, searching and
//     exporting are never gated: they send nothing new.

// Operations submitted to the approval service.
const (
	OpSave        = "memory.save"
	OpUpdate      = "memory.update"
	OpForgetOne   = "memory.forget"
	OpForgetAll   = "memory.forget_all"
	OpSyncEnable  = "memory.sync_enable"
	OpSyncApprove = "memory.sync_approve"
	OpAdopt       = "memory.adopt"
)

// Operations lists every gated operation (RegisterExecutors covers them).
var Operations = []string{OpSave, OpUpdate, OpForgetOne, OpForgetAll, OpSyncEnable, OpSyncApprove, OpAdopt}

// ApprovalRequest mirrors approvals.Request.
type ApprovalRequest struct {
	Operation, Env, OrgID, UserID string
	TargetIDs                     []string
	Payload                       []byte
	IdempotencyKey, Summary       string
	Impact                        string
}

// ApprovalStatus mirrors approvals.Status.
type ApprovalStatus string

// Approval statuses.
const (
	ApprovalPending     ApprovalStatus = "pending"
	ApprovalApproved    ApprovalStatus = "approved"
	ApprovalExecuting   ApprovalStatus = "executing"
	ApprovalDone        ApprovalStatus = "done"
	ApprovalDenied      ApprovalStatus = "denied"
	ApprovalExpired     ApprovalStatus = "expired"
	ApprovalCanceled    ApprovalStatus = "canceled"
	ApprovalReconciling ApprovalStatus = "reconciling"
	ApprovalFailed      ApprovalStatus = "failed"
	ApprovalUnavailable ApprovalStatus = "approval_unavailable"
)

// Pending mirrors approvals.Pending: what a prepared request returns.
type Pending struct {
	RequestID  string
	Status     ApprovalStatus
	ApproveURL string
	Message    string
}

// Submitter prepares requests (tool processes). It has no approve method.
type Submitter interface {
	Submit(ctx context.Context, r ApprovalRequest) (Pending, error)
	Status(ctx context.Context, requestID string) (ApprovalStatus, []byte, error)
}

// Executor runs an approved operation (approvals.Executor).
type Executor func(ctx context.Context, payload []byte, idempotencyKey string) ([]byte, error)

// Reconciler reports whether an operation whose outcome is unknown was
// applied (approvals.Reconciler). It must not write.
type Reconciler func(ctx context.Context, payload []byte, idempotencyKey string) (result []byte, applied bool, err error)

// Registrar is the daemon's approval service, as executors see it.
type Registrar interface {
	Register(operation string, exec Executor)
	RegisterReconciler(operation string, rec Reconciler)
}

// ErrOutcomeUnknown marks an executor error whose Fleet effect may or may
// not have happened (sync-enable's settings call failed in transit).
var ErrOutcomeUnknown = errors.New("memory write outcome unknown")

// alwaysHuman reports operations that never run without an approval.
func alwaysHuman(op string) bool {
	switch op {
	case OpForgetAll, OpSyncEnable, OpSyncApprove, OpAdopt:
		return true
	}
	return false
}

// ExecOptions configure the daemon-side executors.
type ExecOptions struct {
	StateRoot      string
	Active         func(ctx context.Context) Profile
	Remote         Remote
	ConsentVersion string
}

// RegisterExecutors installs the memory executors and reconcilers in the
// relay's approval service. Every executor is idempotent per key (the
// store records each applied key with its result), so the reconciler
// reports applied=true with the stored result when the key was applied,
// and applied=false otherwise so the relay retries the executor once.
func RegisterExecutors(r Registrar, o ExecOptions) {
	s := NewService(ServiceOptions{StateRoot: o.StateRoot, Active: o.Active, Remote: o.Remote, ConsentVersion: o.ConsentVersion})
	for _, op := range Operations {
		op := op
		r.Register(op, func(ctx context.Context, payload []byte, key string) ([]byte, error) {
			return s.execute(ctx, op, payload, key, true, false)
		})
		r.RegisterReconciler(op, func(ctx context.Context, payload []byte, key string) ([]byte, bool, error) {
			return s.applied(ctx, payload, key)
		})
	}
}

// ErrorCode is a short machine code for a definite executor refusal, or ""
// when err is not one of this package's definite errors.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrAccountChanged):
		return "account_changed"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrLooksSecret):
		return "looks_like_secret"
	case errors.Is(err, ErrFull):
		return "memory_full"
	case errors.Is(err, ErrInvalidInput):
		return "invalid_memory"
	case errors.Is(err, ErrLocalProfile):
		return "not_signed_in"
	case errors.Is(err, ErrSyncUnavailable):
		return "sync_unavailable"
	case errors.Is(err, ErrProfileMismatch):
		return "profile_mismatch"
	case errors.Is(err, ErrNewerSchema):
		return "newer_schema"
	}
	return ""
}
