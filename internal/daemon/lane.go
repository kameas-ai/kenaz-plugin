package daemon

import "context"

// Lane is an upload pipeline hosted by the daemon (telemetry, skills).
// The daemon starts lanes after sign-in and the consent controller holds, resumes and purges them.
type Lane interface {
	Name() string
	// Start begins processing; it must not block. ctx ends at shutdown.
	Start(ctx context.Context) error
	// Stop halts processing and flushes in-memory state to the spool.
	Stop() error
	// Hold stops uploads (spooling instead) with a short enum-like reason,
	// e.g. "org_paused", "lockdown", "approval_pending".
	Hold(reason string)
	// Resume restarts uploads and drains the spool.
	Resume()
	// Purge drops queued data (logout, withdrawal, node removed).
	Purge() error
	// Status reports counters only, never payloads.
	Status() LaneStatus
}

// LaneStatus is one lane's counters for status / fleet_status.
type LaneStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"` // running | held | stopped
	Reason   string `json:"reason,omitempty"`
	Uploaded int64  `json:"uploaded"`
	Held     int64  `json:"held"`
	Dropped  int64  `json:"dropped"`
}
