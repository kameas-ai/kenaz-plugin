package daemon

import (
	"context"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// StatusSchemaVersion versions the Status JSON (status --json, fleet_status).
const StatusSchemaVersion = 1

// Session states.
const (
	SessionSignedIn   = "signed_in"
	SessionSignedOut  = "signed_out"
	SessionRemoved    = "removed"
	SessionUnofficial = "unofficial_build"
)

// Status is what `kenaz status --json` and the fleet_status MCP tool
// return. It never contains tokens, email, user ids or hostnames.
type Status struct {
	SchemaVersion    int          `json:"schema_version"`
	Flavor           string       `json:"flavor"`
	Env              string       `json:"env"`
	Session          string       `json:"session"`
	Tier             string       `json:"tier,omitempty"`
	OrgName          string       `json:"org_name,omitempty"`
	OrgPaused        bool         `json:"org_paused"`
	PausedCategory   string       `json:"paused_category,omitempty"`
	Lockdown         bool         `json:"lockdown"`
	DeviceName       string       `json:"device_name,omitempty"`
	Enroll           string       `json:"enroll"`
	LastHeartbeat    *time.Time   `json:"last_heartbeat,omitempty"`
	Capabilities     []string     `json:"capabilities,omitempty"`
	Daemon           DaemonStatus `json:"daemon"`
	Lanes            []LaneStatus `json:"lanes"`
	Spool            *spool.Stats `json:"spool,omitempty"`
	PendingApprovals []string     `json:"pending_approvals"`
	// DashboardURL is the selected environment's Fleet dashboard, where
	// pending approvals are reviewed.
	DashboardURL string `json:"dashboard_url,omitempty"`
	// Reporting says whether telemetry reporting to Fleet is working.
	Reporting *Reporting `json:"reporting,omitempty"`
	// Hosts is the per-host setup state (`kenaz setup`).
	Hosts []HostSetup `json:"hosts,omitempty"`
	Notes []string    `json:"notes,omitempty"`
}

// Reporting states, most blocking first.
const (
	ReportingRelayStopped    = "relay_not_running"
	ReportingNotSignedIn     = "not_signed_in"
	ReportingConsentUnknown  = "consent_unknown" // signed in; consent not read from Fleet yet
	ReportingNotAdmitted     = "not_admitted"
	ReportingNoConsent       = "no_consent"
	ReportingReconfirm       = "consent_reconfirm_needed"
	ReportingHeld            = "held"
	ReportingFailing         = "failing"
	ReportingWaitingForFirst = "ready_no_upload_yet"
	ReportingWorking         = "working"
)

// Consent values in Reporting.Consent.
const (
	ConsentGiven     = "given"
	ConsentNone      = "none"
	ConsentReconfirm = "reconfirm"
	ConsentUnknown   = "unknown"
)

// Upload error categories in Reporting.LastError. Categories only: never a
// server message, payload or identifier.
const (
	ErrCatNetwork     = "network"
	ErrCatAuth        = "auth"
	ErrCatRateLimited = "rate_limited"
	ErrCatServer      = "server"
	ErrCatRejected    = "rejected"
	ErrCatOrgPaused   = "org_paused"
	ErrCatEarlyAccess = "early_access"
)

// Reporting summarizes telemetry delivery. Counts, times and categories
// only; it never holds content, tokens or identifiers.
type Reporting struct {
	State    string `json:"state"`
	SignedIn bool   `json:"signed_in"`
	Consent  string `json:"consent"`
	// HoldReason is set while State is held (approval, paused, lockdown).
	HoldReason   string     `json:"hold_reason,omitempty"`
	LastUploadAt *time.Time `json:"last_upload_at,omitempty"`
	// Queued counts records waiting on this device (spool and memory).
	Queued      int64      `json:"queued"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// ReceiverPort is the loopback OTLP receiver port (0: not running).
	ReceiverPort int `json:"receiver_port,omitempty"`
}

// HostSetup is one host's `kenaz setup` state.
type HostSetup struct {
	Host  string `json:"host"`
	State string `json:"state"` // configured | modified | not_configured | unknown
}

// DaemonStatus describes the daemon process.
type DaemonStatus struct {
	Running   bool       `json:"running"`
	PID       int        `json:"pid,omitempty"`
	Port      int        `json:"port,omitempty"`
	Version   string     `json:"version,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// StatusContributor lets other packages (consent, skills) add to
// Status without the daemon knowing about them.
type StatusContributor interface {
	ContributeStatus(*Status)
}

// StatusContributorFunc adapts a func.
type StatusContributorFunc func(*Status)

func (f StatusContributorFunc) ContributeStatus(s *Status) { f(s) }

// LocalStatus is the status shown when no daemon is reachable (fleet_status
// fallback, `status` before the daemon starts). It does no network I/O.
func LocalStatus(ctx context.Context, p fleetapi.Provider, env fleetapi.Env, note string) Status {
	s := Status{
		SchemaVersion:    StatusSchemaVersion,
		Flavor:           string(p.Flavor()),
		Env:              string(env),
		Session:          SessionSignedOut,
		Enroll:           "unknown",
		Lanes:            []LaneStatus{},
		PendingApprovals: []string{},
	}
	if p.Flavor() == fleetapi.FlavorUnofficial {
		s.Session = SessionUnofficial
	} else if _, err := p.Auth().Session(ctx); err == nil {
		s.Session = SessionSignedIn
	}
	if note != "" {
		s.Notes = append(s.Notes, note)
	}
	// Without a relay nothing is received or uploaded, whatever the
	// sign-in and consent state.
	s.Reporting = &Reporting{State: ReportingRelayStopped, SignedIn: s.Session == SessionSignedIn, Consent: ConsentUnknown}
	return s
}
