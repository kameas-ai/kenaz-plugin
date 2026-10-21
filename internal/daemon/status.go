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
	Notes            []string     `json:"notes,omitempty"`
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
	return s
}
