package fleetapi

import "context"

// TeamRef is one of the caller's own team memberships.
type TeamRef struct {
	ID   string
	Name string
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
