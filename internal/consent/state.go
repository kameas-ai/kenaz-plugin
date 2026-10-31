package consent

import (
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Classes is the fixed set the relay produces, in display
// order. Labels are the notice's class bullets.
var Classes = []ClassInfo{
	{fleetapi.ClassUsageCounts, "Usage counts"},
	{fleetapi.ClassToolCalls, "Tool calls"},
	{fleetapi.ClassErrors, "Errors"},
	{fleetapi.ClassModelCallTelemetry, "Model calls"},
}

// ClassInfo names one class.
type ClassInfo struct {
	Class fleetapi.TelemetryClass
	Label string
}

// ClassStatus values in OrgState.Classes.
const (
	ClassOn          = "on"
	ClassOff         = "off"
	ClassUnavailable = "unavailable"
)

// HoldReason values in OrgState.Holds.
const (
	HoldApproval = "approval"
	HoldPaused   = "paused"
	HoldLockdown = "lockdown"
)

// PlanRequiredNothingKept is shown with fleetapi.PlanRequiredMessage while
// Fleet refuses the organization because its plan does not include the
// plugin.
const PlanRequiredNothingKept = "Nothing is being collected or held on this device until your organization's plan includes Kenaz for Claude Code and Codex."

// OrgState is the single status model rendered by `status`, the
// fleet_status MCP tool and the SessionStart banner.
type OrgState struct {
	SignedIn   bool              `json:"signed_in"`
	Env        string            `json:"env,omitempty"`
	OrgDisplay string            `json:"org_display,omitempty"`
	Tier       string            `json:"tier,omitempty"`
	Classes    map[string]string `json:"classes"`
	// AdminSuggested lists classes an admin pre-selected. They are never
	// enabled by that.
	AdminSuggested []string `json:"admin_suggested,omitempty"`
	// NoticeReconfirm is true when an opted-in class was accepted under an
	// older notice version. Uploads drop until the member re-confirms.
	NoticeReconfirm bool `json:"notice_reconfirm,omitempty"`
	// PlanRequired is true while Fleet refuses this organization because
	// its plan does not include the plugin. Nothing is collected or held on
	// the device; opting out still works.
	PlanRequired bool `json:"plan_required,omitempty"`
	// AdmittedAt is when this device first saw Fleet admit the
	// organization (zero while not admitted). Only data generated from
	// then on is sent.
	AdmittedAt time.Time `json:"-"`
	// DashboardURL is the Fleet dashboard of the selected environment.
	DashboardURL string     `json:"dashboard_url,omitempty"`
	Holds        []Hold     `json:"holds"`
	Pending      []Pending  `json:"pending"`
	Spool        SpoolStats `json:"spool"`
	Drops        Drops      `json:"drops"`
}

// Hold is one active upload hold.
type Hold struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Pending is one pending approval, without its body text.
type Pending struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Blocking bool   `json:"blocking"`
	// URL opens the item in the selected environment's Fleet dashboard.
	URL string `json:"url,omitempty"`
}

// SpoolStats is filled from the spool.
type SpoolStats struct {
	Bytes        int64 `json:"bytes"`
	OldestAgeSec int64 `json:"oldest_age_s"`
}

// Drops counts records discarded on the device.
type Drops struct {
	OptOut       int64 `json:"optout"`
	PII          int64 `json:"pii"`
	TTL          int64 `json:"ttl"`
	Invalid      int64 `json:"invalid"`
	PlanRequired int64 `json:"plan_required"`
}

// AnyOn reports whether at least one class is on.
func (s OrgState) AnyOn() bool {
	for _, v := range s.Classes {
		if v == ClassOn {
			return true
		}
	}
	return false
}
