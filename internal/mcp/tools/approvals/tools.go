// Package approvaltools is the read-only approval_status MCP tool. It
// reports a prepared write's state. There is deliberately no tool that
// approves, enrolls an authenticator or returns a receipt: approval only
// happens on the local approval page with the user's authenticator.
package approvaltools

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// ToolStatus is the tool name.
const ToolStatus = "approval_status"

// Group registers approval_status.
var Group = mcp.GroupFunc(register)

// StatusInput is approval_status's arguments.
type StatusInput struct {
	RequestID string `json:"request_id" jsonschema:"the request_id a write tool returned"`
}

// StatusOutput is approval_status's result. Result is bounded metadata
// (for example the created id), never the written content.
type StatusOutput struct {
	RequestID  string          `json:"request_id"`
	Status     string          `json:"status"`
	ApproveURL string          `json:"approve_url,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Message    string          `json:"message"`
}

func register(s *sdk.Server, d mcp.Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolStatus,
		Title:       "Check a write approval",
		Description: "Report whether a write you prepared (skills_publish, context_share, context_promote, memory writes) is still waiting for the user's approval, was approved and completed, or was canceled, denied or expired. Read-only; it cannot approve anything.",
		Annotations: &sdk.ToolAnnotations{Title: "Check a write approval", ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
		if d.Touch != nil {
			d.Touch()
		}
		out, err := Status(ctx, d.Approvals, in)
		return nil, out, err
	})
}

var messages = map[approvals.Status]string{
	approvals.StatusPending:     "Waiting for the user to approve at approve_url. Nothing has been written.",
	approvals.StatusApproved:    "Approved; the write is starting.",
	approvals.StatusExecuting:   "Approved; the write is in progress.",
	approvals.StatusDone:        "Approved and written.",
	approvals.StatusDenied:      "The user canceled this request. Nothing was written.",
	approvals.StatusExpired:     "The request expired before approval. Nothing was written.",
	approvals.StatusCanceled:    "The request was canceled (sign-out, account change or recovery). Nothing was written.",
	approvals.StatusReconciling: "The outcome is being checked after an interruption; it will not be repeated blindly.",
	approvals.StatusFailed:      "Approved, but the destination refused the write; see result.error.",
}

// Status implements approval_status.
func Status(ctx context.Context, p approvals.Preparer, in StatusInput) (StatusOutput, error) {
	if !approvals.ValidID(in.RequestID) {
		return StatusOutput{}, errors.New("request_id is not a Kenaz approval request id")
	}
	if p == nil {
		return StatusOutput{RequestID: in.RequestID, Status: string(approvals.StatusApprovalUnavailable), Message: approvals.UnavailableMessage}, nil
	}
	st, res, err := p.Status(ctx, in.RequestID)
	if errors.Is(err, approvals.ErrNotFound) {
		return StatusOutput{}, errors.New("no such request (it may have been cleaned up after 24 hours)")
	}
	if err != nil {
		return StatusOutput{}, errors.New("the Kenaz relay could not be reached")
	}
	out := StatusOutput{RequestID: in.RequestID, Status: string(st), Message: messages[st]}
	if len(res) > 0 && json.Valid(res) {
		out.Result = res
	}
	if st == approvals.StatusPending {
		out.ApproveURL, _ = p.ApprovalURL(ctx, in.RequestID)
	}
	return out, nil
}
