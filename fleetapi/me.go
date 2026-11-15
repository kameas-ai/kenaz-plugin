package fleetapi

import "context"

// TeamRef is one of the caller's own team memberships, with its place in the
// organization's team tree.
//
// The fields after Name were added with Fleet's stored team tree. A Fleet, or a
// Fleet connection, that predates it leaves them at their zero values, which
// mean "Fleet did not say", never "no": use TreeKnown before reading Depth or
// an unset IsDefault as a fact. Path makes TeamRef not comparable with ==;
// compare IDs.
type TeamRef struct {
	ID   string
	Name string
	// IsDefault marks the organization's all-members team: the root of the
	// tree, which every member is in.
	IsDefault bool
	// Role is the caller's role on this team: TeamRoleMember or TeamRoleLead.
	Role string
	// ParentTeamID is the team's stored parent, or "" for the default team
	// and for a team directly under it (Fleet sends null: the default team's
	// id is never a parent).
	ParentTeamID string
	// Depth is 0 for the default team, 1 for a team directly under it, and
	// so on down.
	Depth int
	// Path is the ids of the team's ancestors, topmost first, without the
	// team itself and without the default team: empty for the default team
	// and for a team directly under it. The caller need not be a member of
	// them.
	Path []string
	// Slug is the team's short name for paths. Fleet does not send it yet:
	// always "" until it does.
	Slug string
	// Membership is how the caller comes to be in the team:
	// TeamMembershipDirect or TeamMembershipInherited. Fleet does not send it
	// yet: always "" until it does.
	Membership string
}

// The values of TeamRef.Role and TeamRef.Membership.
const (
	TeamRoleMember = "member"
	TeamRoleLead   = "lead"

	TeamMembershipDirect    = "direct"
	TeamMembershipInherited = "inherited"
)

// TreeKnown reports whether Fleet sent the team tree fields for these teams. It
// goes by Role, which Fleet sends on every team once it has the tree, and by
// IsDefault.
func TreeKnown(teams []TeamRef) bool {
	for _, t := range teams {
		if t.IsDefault || t.Role != "" {
			return true
		}
	}
	return false
}

// DefaultTeam returns the team Fleet flagged as the organization's all-members
// team. ok is false when no team is flagged: a Fleet that predates the flag,
// or no teams at all. It never guesses from position or name.
func DefaultTeam(teams []TeamRef) (t TeamRef, ok bool) {
	for _, t := range teams {
		if t.IsDefault {
			return t, true
		}
	}
	return TeamRef{}, false
}

// MeInfo is GET /api/v1/me, reduced to what public code uses. Fleet keeps
// serving /me while an org is paused, so OrgPaused is reliable here.
type MeInfo struct {
	UserID string
	OrgID  string
	// OrgName is the organization's display name, used to fill the Agent
	// Telemetry Notice. "" if Fleet does not send it.
	OrgName        string
	Role           string // e.g. org_owner, admin, member
	Tier           string // e.g. pro, team, enterprise
	OrgPaused      bool
	PausedCategory string
	Teams          []TeamRef
	// OrgSizeBand is Fleet's coarse org-size band for the analytics floor:
	// "sole" (1 Authorized User), "small" (2–4), "standard"
	// (5+), or "" when Fleet does not send it. It selects the Agent
	// Telemetry Notice edition; it is never a count.
	OrgSizeBand string
}

// Capabilities is GET /api/v1/me/capabilities.
type Capabilities struct {
	// Keys are tier-gated capability keys, e.g. "context_sync",
	// "catalog_team_share", "emergency_lockdown".
	Keys map[string]bool
	// TelemetrySchema is the Fleet telemetry schema revision. 0 means
	// unknown or older. The relay sends the cost_known body field only
	// when this is at least the revision that introduced it.
	TelemetrySchema int
}

// Has reports whether capability key k is granted.
func (c Capabilities) Has(k string) bool { return c.Keys[k] }

// Me reads the caller's identity and entitlements.
type Me interface {
	// Get returns /me for the active session.
	Get(ctx context.Context) (MeInfo, error)
	// Capabilities returns /me/capabilities. Callers poll about every 5
	// minutes.
	Capabilities(ctx context.Context) (Capabilities, error)
}
