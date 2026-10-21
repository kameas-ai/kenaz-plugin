// Package notices ships the notice texts the CLI renders verbatim.
//
// Each file is a versioned asset copied from Kameas's approved notice text,
// with its bracketed build-state markers resolved for the Fleet state it
// describes. Never edit wording here without Kameas's approval. A change to
// what is sent needs a new edition ID (re-consent); a description-only
// change keeps the ID. Either way the SHA-256 pins in notices_test.go must
// be updated.
package notices

import _ "embed"

// Edition is one resolved revision of the Agent Telemetry Notice. ID is
// stored with an opt-in, so a change of edition forces re-display and
// re-confirmation.
type Edition struct {
	ID   string
	Text string
}

// Org size bands as Fleet reports them on /me.
const (
	OrgSizeUnknown  = ""
	OrgSizeSole     = "sole"     // exactly 1 Authorized User: sees every view
	OrgSizeSmall    = "small"    // 2–4: administrators see org-wide totals only
	OrgSizeStandard = "standard" // 5+: slices under 5 reporting members suppressed
)

// Agent Telemetry Notice editions. Markers resolved in every edition: device display name
// built; per-plan retention periods not built (retention for the
// subscription term). They differ only in the "Who sees it" sentence.
// The notice's "Controls" line is rendered by the UI, not part of the text.
const (
	// AgentTelemetryV1ID: analytics floor not built (or org size unknown, or a
	// sole-member org): "In a small team, totals may effectively show your
	// own activity."
	AgentTelemetryV1ID = "agent-telemetry/v1"
	// AgentTelemetryV2ID: analytics floor built, org of 5+ members.
	AgentTelemetryV2ID = "agent-telemetry/v2"
	// AgentTelemetryV2SmallID: analytics floor built, org of 2–4 members (adds
	// the small-organization sentence).
	AgentTelemetryV2SmallID = "agent-telemetry/v2-small"
)

// Placeholders in braces are filled by internal/consent:
// {Organization name}, {harnesses}, {device display name},
// {harness versions}.
var (
	//go:embed agent-telemetry/v1.md
	AgentTelemetryV1 string
	//go:embed agent-telemetry/v2.md
	agentTelemetryV2 string
	//go:embed agent-telemetry/v2-small.md
	agentTelemetryV2Small string
)

// AgentTelemetry picks the edition that is true for the Fleet the member is
// connected to. floorBuilt is Fleet's advertisement that the analytics
// floor is enforced (capability key "analytics_member_floor"); orgSize is
// the /me size band. Anything uncertain falls back to v1, whose wording is
// accurate whether or not the floor applies: the notice never claims a
// protection Fleet has not shipped.
func AgentTelemetry(floorBuilt bool, orgSize string) Edition {
	if !floorBuilt {
		return Edition{AgentTelemetryV1ID, AgentTelemetryV1}
	}
	switch orgSize {
	case OrgSizeStandard:
		return Edition{AgentTelemetryV2ID, agentTelemetryV2}
	case OrgSizeSmall:
		return Edition{AgentTelemetryV2SmallID, agentTelemetryV2Small}
	default: // unknown, or sole: the member's data is shown to them unsuppressed
		return Edition{AgentTelemetryV1ID, AgentTelemetryV1}
	}
}
