package contexttools

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// Error codes returned in ToolError.Code.
const (
	CodeNotSignedIn        = "not_signed_in"
	CodeUnofficialBuild    = "unofficial_build"
	CodeTierRequired       = "tier_required"
	CodeRoleRequired       = "role_required"
	CodeOrgPaused          = "org_paused"
	CodeLockdown           = "lockdown"
	CodeBlockingApproval   = "blocking_approval"
	CodeTeamRequired       = "team_required"
	CodeInvalidInput       = "invalid_input"
	CodeCredentialDetected = "credential_detected"
	CodeNotConfirmed       = "not_confirmed"
	CodeFleetError         = "fleet_error"
	// CodeFleetRejected is a 400/422 refusal; Message names only Fleet's
	// machine reason code.
	CodeFleetRejected = "fleet_rejected"
)

// Capability keys from Fleet's per-tier capability table.
const (
	CapSharedTeamGraph  = "shared_team_graph"  // Team+
	CapTeamGraphSharing = "team_graph_sharing" // Team+: push/store team_shared
	CapOrgGraphSharing  = "org_graph_sharing"  // Enterprise: org_shared
)

// promoteRoles hold Fleet's context:promote permission
// (service/rbac/permissions.go orgRolePermissions).
var promoteRoles = map[string]bool{"org_owner": true, "org_admin": true}

// ToolError is a structured tool refusal with a one-line remedy.
type ToolError struct {
	Code    string   `json:"code"`
	Need    string   `json:"need,omitempty"`
	Message string   `json:"message"`
	Remedy  string   `json:"remedy,omitempty"`
	Choices []string `json:"choices,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// Op is a context tool operation, used to pick the entitlement it needs.
type Op int

const (
	OpSearchTeam Op = iota
	OpSearchOrg
	OpPull
	OpShare
	OpPromote
	OpTeamMembers
)

func (op Op) write() bool { return op == OpShare || op == OpPromote }

// StatusSource is the daemon status (lockdown, approvals). Optional.
type StatusSource interface {
	Status(ctx context.Context) (daemon.Status, error)
}

// Session is what a tool call learned while checking entitlements.
type Session struct {
	Me   fleetapi.MeInfo
	Caps fleetapi.Capabilities
}

// Check reports whether op can be attempted. It mirrors Fleet's own gates
// so the user gets a clear remedy instead of a bare 403; Fleet stays the
// enforcer. Write operations also refuse locally while uploads are held for
// lockdown, org pause or a blocking approval.
func Check(ctx context.Context, p fleetapi.Provider, st StatusSource, op Op) (Session, *ToolError) {
	me, err := p.Me().Get(ctx)
	if err != nil {
		return Session{}, MapError(err)
	}
	caps, err := p.Me().Capabilities(ctx)
	if err != nil {
		return Session{}, MapError(err)
	}
	s := Session{Me: me, Caps: caps}

	if op.write() {
		if me.OrgPaused {
			return s, orgPaused(me.PausedCategory)
		}
		if st != nil {
			if ds, err := st.Status(ctx); err == nil {
				if ds.OrgPaused {
					return s, orgPaused(ds.PausedCategory)
				}
				if ds.Lockdown {
					return s, &ToolError{Code: CodeLockdown, Message: "Your organization has paused sharing from Kameas Fleet (emergency lockdown).", Remedy: "Ask your Kameas Fleet administrator; reading shared context still works."}
				}
				if heldForApproval(ds) {
					remedy := "Open the Kameas Fleet dashboard to review it (run /kenaz:status for details)."
					if ds.DashboardURL != "" {
						remedy = "Open " + ds.DashboardURL + " to review it (run /kenaz:status for details)."
					}
					return s, &ToolError{Code: CodeBlockingApproval, Message: "A required approval is pending, so Kameas Fleet is not uploading anything.", Remedy: remedy}
				}
			}
		}
	}

	switch op {
	case OpSearchTeam, OpPull, OpTeamMembers:
		if !caps.Has(CapSharedTeamGraph) && !caps.Has(CapTeamGraphSharing) {
			return s, tierRequired("team")
		}
	case OpShare:
		if !caps.Has(CapTeamGraphSharing) {
			return s, tierRequired("team")
		}
	case OpSearchOrg:
		if !caps.Has(CapOrgGraphSharing) {
			return s, tierRequired("enterprise")
		}
	case OpPromote:
		if !caps.Has(CapOrgGraphSharing) {
			return s, tierRequired("enterprise")
		}
		if !promoteRoles[me.Role] {
			return s, &ToolError{Code: CodeRoleRequired, Need: "admin", Message: "Only organization owners and admins can share context with the whole organization.", Remedy: "Ask an organization admin to promote this note."}
		}
	}
	return s, nil
}

func heldForApproval(ds daemon.Status) bool {
	for _, l := range ds.Lanes {
		if l.State == "held" && l.Reason == "approval_pending" {
			return true
		}
	}
	return false
}

func orgPaused(category string) *ToolError {
	e := &ToolError{Code: CodeOrgPaused, Message: "Kameas Fleet has paused this organization, so nothing is being shared right now.", Remedy: "This is usually temporary; try again later or ask your Kameas Fleet administrator."}
	if category != "" {
		e.Need = category
	}
	return e
}

func tierRequired(need string) *ToolError {
	plan := map[string]string{"team": "Team", "enterprise": "Enterprise"}[need]
	return &ToolError{Code: CodeTierRequired, Need: need, Message: "This needs the Kameas Fleet " + plan + " plan.", Remedy: "Ask your Kameas Fleet administrator about upgrading."}
}

// MapError turns a fleetapi error into a ToolError. It never includes
// request or response content: Fleet's free-text message and details are
// never shown, only its machine reason code.
func MapError(err error) *ToolError {
	if err == nil {
		return nil
	}
	if errors.Is(err, fleetapi.ErrUnofficialBuild) {
		return &ToolError{Code: CodeUnofficialBuild, Message: fleetapi.ErrUnofficialBuild.Error()}
	}
	if errors.Is(err, fleetapi.ErrNotSignedIn) {
		return &ToolError{Code: CodeNotSignedIn, Message: "You are not signed in to Kameas Fleet.", Remedy: mcp.SignInRemedy()}
	}
	if cat, ok := fleetapi.OrgPaused(err); ok {
		return orgPaused(cat)
	}
	var ae *fleetapi.APIError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "org_scope_requires_enterprise":
			return tierRequired("enterprise")
		case "user_not_provisioned":
			return &ToolError{Code: CodeNotSignedIn, Message: "Your account is not provisioned in this Kameas Fleet organization yet.", Remedy: "Ask your administrator to add you, then sign in again. " + mcp.SignInRemedy()}
		case "lockdown", "emergency_lockdown":
			return &ToolError{Code: CodeLockdown, Message: "Your organization has paused sharing from Kameas Fleet (emergency lockdown)."}
		case "tier_required", "capability_required":
			return tierRequired("team")
		}
		switch ae.Status {
		case http.StatusUnauthorized:
			return &ToolError{Code: CodeNotSignedIn, Message: "Your Kameas Fleet session has expired.", Remedy: mcp.SignInRemedy()}
		case http.StatusForbidden:
			return &ToolError{Code: CodeRoleRequired, Need: "admin", Message: "Kameas Fleet refused this for your role.", Remedy: "Ask an organization admin."}
		}
		if ae.Status == http.StatusUnprocessableEntity && ae.Code == "lint_blocked" {
			return &ToolError{Code: CodeCredentialDetected, Need: safeCode(ae.Code), Message: "Nothing was shared: Kameas Fleet blocked this because it looks like it contains a secret or other sensitive content.", Remedy: "Remove it and try again."}
		}
		if credentialRejection(ae.Code) {
			return &ToolError{Code: CodeCredentialDetected, Need: safeCode(ae.Code), Message: "Nothing was shared: Kameas Fleet rejected this because it looks like it contains a credential.", Remedy: "Remove the secret and try again."}
		}
		if ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity {
			msg := "Kameas Fleet rejected this request."
			if c := safeCode(ae.Code); c != "" {
				msg = "Kameas Fleet rejected this request (reason: " + c + ")."
			}
			return &ToolError{Code: CodeFleetRejected, Need: safeCode(ae.Code), Message: msg}
		}
		return &ToolError{Code: CodeFleetError, Need: safeCode(ae.Code), Message: "Kameas Fleet returned an error (HTTP " + http.StatusText(ae.Status) + ")."}
	}
	return &ToolError{Code: CodeFleetError, Message: "Kameas Fleet could not be reached."}
}

// credentialRejection reports whether a Fleet error code names a
// credential/secret refusal. Fleet checks shared content on arrival; the
// plugin only recognizes the refusal so it can explain it plainly.
func credentialRejection(code string) bool {
	c := strings.ToLower(code)
	return strings.Contains(c, "credential") || strings.Contains(c, "secret")
}

// reasonCodeRe accepts only short machine reason codes, so nothing from a
// note can reach the message even if Fleet ever echoed it in Code.
var reasonCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// safeCode returns code if it is a short machine reason code, else "".
func safeCode(code string) string {
	if reasonCodeRe.MatchString(code) {
		return code
	}
	return ""
}
