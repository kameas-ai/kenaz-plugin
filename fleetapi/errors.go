package fleetapi

import (
	"errors"
	"fmt"
)

// ErrUnofficialBuild is returned by every method of the stub provider.
// The CLI maps it to exit code 2 and prints its message verbatim.
var ErrUnofficialBuild = errors.New("this is an unofficial build and cannot connect to Kameas Fleet; install from the official channels")

// ErrNotSignedIn means no usable session exists. Run `kenaz login`.
var ErrNotSignedIn = errors.New("not signed in to Kameas Fleet")

// Error codes that Fleet returns in APIError.Code and that public code
// branches on. Unknown codes must be handled generically.
const (
	// CodeOrgPaused is transient: hold uploads and retry later.
	// Details["paused_category"] names the paused category.
	CodeOrgPaused = "org_paused"
	// CodeClientNotAllowed means this client id is not on Fleet's
	// FLEET_OIDC_ALLOWED_CLIENT_IDS list.
	CodeClientNotAllowed = "client_not_allowed"
	// CodeNodeRemoved means an admin removed this node. Stop heartbeating
	// and require a fresh login.
	CodeNodeRemoved = "node_removed"
	// CodeEarlyAccessRequired (HTTP 403 on enroll and heartbeat) means the
	// member's organization is not in the Kenaz for Claude Code and
	// Codex early-access program. Show EarlyAccessMessage; keep heartbeating
	// at the normal interval so admission is picked up.
	CodeEarlyAccessRequired = "fleet_plugin_early_access_required"
)

// EarlyAccessMessage is the text shown for CodeEarlyAccessRequired. It is
// the approved wording. The plugin owns it: never show
// Fleet's response message instead.
const EarlyAccessMessage = "Kenaz for Claude Code and Codex is in early access. Contact sales@kameas.ai to join."

// EarlyAccessRequired reports whether err is Fleet's early-access refusal.
func EarlyAccessRequired(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == CodeEarlyAccessRequired
}

// APIError is a non-2xx Fleet response, normalized by the implementation.
// It carries no request or response bodies. Message must be safe to log.
type APIError struct {
	Status  int               // HTTP status
	Code    string            // Fleet error code, e.g. CodeOrgPaused
	Message string            // human-readable, no user content
	Details map[string]string // flat, string-only details
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("kameas fleet: http %d", e.Status)
	}
	return fmt.Sprintf("kameas fleet: http %d %s", e.Status, e.Code)
}

// OrgPaused reports whether err is a Fleet org-pause refusal and, if so,
// the paused category.
func OrgPaused(err error) (category string, ok bool) {
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == CodeOrgPaused {
		return ae.Details["paused_category"], true
	}
	return "", false
}
