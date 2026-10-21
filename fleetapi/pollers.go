package fleetapi

import (
	"context"
	"encoding/json"
	"time"
)

// Approval kinds Fleet serves today. Clients must handle unknown kinds
// generically: a blocking unknown kind holds uploads.
const (
	ApprovalKindLegalAcceptance  = "legal_acceptance"
	ApprovalKindMLNotice         = "ml_notice" // never holds the telemetry lane
	ApprovalKindMLExclusions     = "ml_exclusions_change"
	ApprovalKindMLOptInAvailable = "ml_opt_in_available"
)

// Approval is one GET /api/v1/me/pending-approvals item. Acceptance is
// browser-only: the seam deliberately has no Accept method.
// Public code opens Link or DocumentURL in the Fleet dashboard.
type Approval struct {
	ID              string
	Kind            string
	Title           string
	Summary         string
	BodyText        string // may be long; never put in hook banners
	DocumentURL     string
	DocumentSHA256  string
	Version         string
	Blocking        bool
	Required        bool
	DeferrableUntil *time.Time
	Link            string // dashboard-relative
}

// LockdownState is the org emergency-lockdown state. The
// plugin reacts by holding uploads and telling the user. It never blocks
// the agent.
type LockdownState struct {
	Locked bool
	Reason string
}

// PauseState is the staff-set org pause state.
type PauseState struct {
	Paused   bool
	Category string
}

// ConfigsRequest is GET /api/v1/configs?machine=&checksum=.
type ConfigsRequest struct {
	MachineID string
	Checksum  string // last applied checksum; "" on first pull
}

// ConfigBundle is the part of the Fleet config bundle this plugin consumes:
// mandated catalog items only. Cedar deltas, MCP allowlists and model
// preferences are dropped inside the implementation and never cross the
// seam.
type ConfigBundle struct {
	NotModified bool // HTTP 304: nothing else is set
	ID          string
	Checksum    string
	Mandated    []MandatedItem
}

// MandatedItem is an admin-mandated catalog item.
type MandatedItem struct {
	CatalogID      string
	Kind           string // "agent_skill" or legacy "skill"
	Version        string
	ReviewedSHA256 string          // admin-reviewed content hash
	Payload        json.RawMessage // opaque JSON, interpreted by internal/skills
}

// AckStatus is the per-item outcome reported back to Fleet.
type AckStatus string

const (
	AckApplied AckStatus = "applied"
	AckRefused AckStatus = "refused"
	AckFailed  AckStatus = "failed"
	AckRemoved AckStatus = "removed"
)

// ConfigAck reports one item's outcome. Detail is a short enum-like
// reason, never user content.
type ConfigAck struct {
	CatalogID string
	Version   string
	Status    AckStatus
	Detail    string
}

// Pollers covers the periodic, read-mostly Fleet state the daemon watches.
type Pollers interface {
	// PendingApprovals lists pending approvals for the caller.
	PendingApprovals(ctx context.Context) ([]Approval, error)
	// LockdownStatus returns the current lockdown state.
	LockdownStatus(ctx context.Context) (LockdownState, error)
	// LockdownWait long-polls for a change (server holds about 60 s).
	// changed=false means timeout with no change: call again.
	LockdownWait(ctx context.Context) (state LockdownState, changed bool, err error)
	// OrgPause returns the org pause state (backed by /me).
	OrgPause(ctx context.Context) (PauseState, error)
	// Configs pulls the config bundle (304-aware).
	Configs(ctx context.Context, req ConfigsRequest) (ConfigBundle, error)
	// AckConfigs posts per-item outcomes for bundle id.
	AckConfigs(ctx context.Context, bundleID string, acks []ConfigAck) error
}
