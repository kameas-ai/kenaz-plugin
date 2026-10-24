package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Doer performs an authenticated JSON call on the daemon's control API
// (daemon.Client implements it).
type Doer interface {
	DoJSON(ctx context.Context, method, path string, in, out any) error
}

// HTTPStatusError is a non-2xx control-API reply with its wire error code.
// A Doer may return any error with an APICode() string method instead.
type HTTPStatusError struct {
	Status int
	Code   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("approvals: http %d %s", e.Status, e.Code)
}

// RemoteClient is a Preparer for processes other than the daemon (the MCP
// server, the CLI). It reaches the daemon's nonce-authenticated control
// API; it has no way to approve.
type RemoteClient struct {
	// Conn returns a connected daemon client, or an error when the relay
	// cannot be reached.
	Conn func(ctx context.Context) (Doer, error)
}

func (c RemoteClient) do(ctx context.Context, method, path string, in, out any) error {
	if c.Conn == nil {
		return ErrUnavailable
	}
	d, err := c.Conn(ctx)
	if err != nil || d == nil {
		return fmt.Errorf("%w: the Kenaz relay is not running", ErrRelayUnavailable)
	}
	err = d.DoJSON(ctx, method, path, in, out)
	var ce interface{ APICode() string }
	if errors.As(err, &ce) {
		if e, ok := codeErrors[ce.APICode()]; ok {
			return e
		}
	}
	return err
}

// ErrRelayUnavailable means the daemon that serves approvals could not be
// reached; nothing was prepared.
var ErrRelayUnavailable = errors.New("approval relay unavailable")

// Prepare implements Preparer.
func (c RemoteClient) Prepare(ctx context.Context, r Request) (string, Status, error) {
	var out WireStatus
	err := c.do(ctx, http.MethodPost, ControlPrepare, WireRequest{
		Operation: r.Operation, Env: r.Env, OrgID: r.OrgID, UserID: r.UserID, TargetIDs: r.TargetIDs,
		Payload: json.RawMessage(r.Payload), IdempotencyKey: r.IdempotencyKey, Summary: r.Summary, Impact: r.Impact,
	}, &out)
	if err != nil {
		return "", "", err
	}
	return out.RequestID, out.Status, nil
}

// Status implements Preparer.
func (c RemoteClient) Status(ctx context.Context, requestID string) (Status, []byte, error) {
	if !ValidID(requestID) {
		return "", nil, ErrNotFound
	}
	var out WireStatus
	if err := c.do(ctx, http.MethodGet, ControlStatus+requestID, nil, &out); err != nil {
		return "", nil, err
	}
	return out.Status, out.Result, nil
}

// ApprovalURL implements Preparer.
func (c RemoteClient) ApprovalURL(ctx context.Context, requestID string) (string, error) {
	l, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	if !ValidID(requestID) {
		return "", ErrNotFound
	}
	return l.PageURL + "r/" + requestID, nil
}

// List returns the daemon's request listing and page URL.
func (c RemoteClient) List(ctx context.Context) (WireList, error) {
	var out WireList
	err := c.do(ctx, http.MethodGet, ControlList, nil, &out)
	return out, err
}

// Pending is the tool-facing reply for a prepared write. It never contains
// an approval token: request_id is an opaque handle, and approval needs the
// user's authenticator on the page at approve_url.
type Pending struct {
	RequestID  string `json:"request_id,omitempty"`
	Status     Status `json:"status"`
	ApproveURL string `json:"approve_url,omitempty"`
	Message    string `json:"message"`
}

// Submit prepares r with p and returns the reply a tool shows the model
// and user. It never writes.
func Submit(ctx context.Context, p Preparer, r Request) (Pending, error) {
	if p == nil {
		return Pending{Status: StatusApprovalUnavailable, Message: UnavailableMessage}, nil
	}
	id, st, err := p.Prepare(ctx, r)
	if errors.Is(err, ErrRelayUnavailable) || errors.Is(err, ErrUnavailable) {
		return Pending{Status: StatusApprovalUnavailable, Message: UnavailableMessage}, nil
	}
	if err != nil {
		return Pending{}, err
	}
	out := Pending{RequestID: id, Status: st}
	switch st {
	case StatusApprovalUnavailable:
		out.Message = UnavailableMessage
	case StatusPending:
		out.ApproveURL, _ = p.ApprovalURL(ctx, id)
		out.Message = "Nothing has been written yet. Ask the user to open approve_url in their browser, review the exact content, and approve it with their security key or device unlock within 5 minutes. Run `kenaz approvals` to see its status."
	case StatusDone:
		out.Message = "This exact request was already approved and completed."
	default:
		out.Message = "This exact request was already handled (" + string(st) + "). Prepare it again with a new request if it is still wanted."
	}
	return out, nil
}

// UnavailableMessage explains approval_unavailable to the user.
const UnavailableMessage = "Nothing was written. Writes need approval with a security key or this device's unlock (WebAuthn), and none is available here. Run `kenaz approvals enroll` on a machine with a browser and an authenticator."
