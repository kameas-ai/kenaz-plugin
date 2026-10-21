package fleetapi

import "context"

// TelemetryClass is a Fleet telemetry opt-in class
// (Fleet's telemetry schema v1). Every class defaults
// to OFF.
type TelemetryClass string

// Classes the projector can emit.
const (
	ClassUsageCounts        TelemetryClass = "harness.usage_counts"
	ClassToolCalls          TelemetryClass = "harness.tool_calls"
	ClassErrors             TelemetryClass = "harness.errors"
	ClassModelCallTelemetry TelemetryClass = "model_call_telemetry"
)

// OptInState is one class's current state for the caller.
type OptInState struct {
	Class   TelemetryClass
	OptedIn bool
	// NoticeVersion is the Agent Telemetry Notice version the member saw
	// when opting in ("" if Fleet predates notice_version).
	NoticeVersion string
	// Available is false when the org tier does not allow the class.
	Available bool
	// AdminSuggested marks an admin pre-selection. It is display only:
	// admins may pre-select, never pre-enable.
	AdminSuggested bool
}

// OptInPut is PUT /api/v1/me/telemetry-opt-ins.
type OptInPut struct {
	Updates []OptInUpdate
	// NoticeVersion is the version of the notice text rendered to the
	// member in the same flow. Required when any update opts
	// in.
	NoticeVersion string
}

// OptInUpdate sets one class.
type OptInUpdate struct {
	Class   TelemetryClass
	OptedIn bool
}

// OptIns reads and writes the member's own opt-ins. Only the member's
// own action may call Put with OptedIn=true. Managed configuration never
// does.
type OptIns interface {
	Get(ctx context.Context) ([]OptInState, error)
	Put(ctx context.Context, req OptInPut) error
}
