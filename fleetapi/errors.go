package fleetapi

import (
	"errors"
	"fmt"
	"regexp"
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
	// CodeLegacyEarlyAccessRequired is the refusal older Fleet servers
	// send (HTTP 403 on enroll and heartbeat) for an organization whose
	// plan does not include Kenaz for Claude Code and Codex. Current
	// servers send CodeCapabilityNotInTier instead. Both are handled the
	// same way: see PlanRequired.
	CodeLegacyEarlyAccessRequired = "fleet_plugin_early_access_required"

	// CodeEarlyAccessRequired is the old name of
	// CodeLegacyEarlyAccessRequired.
	//
	// Deprecated: use CodeLegacyEarlyAccessRequired, and PlanRequired to
	// recognize the refusal.
	CodeEarlyAccessRequired = CodeLegacyEarlyAccessRequired
)

// PluginCapability is the capability Fleet names in Details["capability"]
// of a CodeCapabilityNotInTier refusal when the organization's plan does
// not include the plugin at all. It must match the value Fleet sends for
// that whole-plugin refusal. Every other CodeCapabilityNotInTier refusal is
// a per-feature limit naming its own capability: see TierLimit.
const PluginCapability = "fleet_plugin"

// PlanRequiredMessage is the text shown when Fleet refuses the plugin
// because the organization's plan does not include it. The plugin owns
// it: never show Fleet's response message instead.
const PlanRequiredMessage = "Kenaz for Claude Code and Codex needs a Kameas Fleet plan that includes it. Ask your organization's Kameas Fleet administrator."

// EarlyAccessMessage is the old name of PlanRequiredMessage.
//
// Deprecated: use PlanRequiredMessage.
const EarlyAccessMessage = PlanRequiredMessage

// TierLimitMessage is the text shown when Fleet refuses one feature
// because the organization's plan does not include it. The plugin owns it.
const TierLimitMessage = "Your organization's Kameas Fleet plan doesn't include this feature."

// PlanRequired reports whether err is Fleet's refusal because the
// organization's plan does not include the plugin at all: either
// capability_not_in_tier naming PluginCapability, or the code older
// servers send for the same refusal. A capability_not_in_tier for any
// other capability, or naming none, is a per-feature limit (TierLimit),
// not this. While refused, nothing is collected, held or sent; heartbeats
// continue at the normal interval so a plan change is picked up.
func PlanRequired(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case CodeLegacyEarlyAccessRequired:
		return true
	case CodeCapabilityNotInTier:
		return ae.Details["capability"] == PluginCapability
	}
	return false
}

// EarlyAccessRequired is the old name of PlanRequired.
//
// Deprecated: use PlanRequired.
func EarlyAccessRequired(err error) bool { return PlanRequired(err) }

// TierLimit reports whether err is a per-feature capability_not_in_tier
// refusal: the organization's plan includes the plugin but not this one
// feature. capability is Fleet's Details["capability"] as sent (possibly
// empty); show it only through TierLimitText. The whole-plugin refusal is
// PlanRequired, never TierLimit.
func TierLimit(err error) (capability string, ok bool) {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != CodeCapabilityNotInTier || PlanRequired(err) {
		return "", false
	}
	return ae.Details["capability"], true
}

var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// SafeCapability returns capability if it is a short machine name that is
// safe to show, else "".
func SafeCapability(capability string) string {
	if capabilityName.MatchString(capability) {
		return capability
	}
	return ""
}

// TierLimitText is TierLimitMessage, naming the capability only when it is
// a short machine name (see SafeCapability).
func TierLimitText(capability string) string {
	if c := SafeCapability(capability); c != "" {
		return TierLimitMessage + " (feature: " + c + ")"
	}
	return TierLimitMessage
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
