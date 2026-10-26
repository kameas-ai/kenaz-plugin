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
	// AgentTelemetryV3ID and AgentTelemetryV3SmallID replace v2 and
	// v2-small (2026-10-25 revision): the closing sentence names
	// "your AI tools" instead of two products. Description only: nothing new
	// is sent and the audience is unchanged, so an opt-in recorded under v2
	// stands under v3, and one under v2-small stands under v3-small
	// (AgentTelemetryAccepted). v2 and v2-small shipped in v0.6.0 and stay
	// embedded, unchanged, for the record.
	AgentTelemetryV3ID      = "agent-telemetry/v3"
	AgentTelemetryV3SmallID = "agent-telemetry/v3-small"
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
	//go:embed agent-telemetry/v3.md
	agentTelemetryV3 string
	//go:embed agent-telemetry/v3-small.md
	agentTelemetryV3Small string
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
		return Edition{AgentTelemetryV3ID, agentTelemetryV3}
	case OrgSizeSmall:
		return Edition{AgentTelemetryV3SmallID, agentTelemetryV3Small}
	default: // unknown, or sole: the member's data is shown to them unsuppressed
		return Edition{AgentTelemetryV1ID, AgentTelemetryV1}
	}
}

// AgentTelemetryAccepted lists the editions whose opt-ins still stand while
// current is the edition shown: current itself, then any earlier edition
// that described the same data to the same audience in other words. An
// opt-in under any of them is not re-confirmed. Editions that differ in what
// they promise (v1 and v2/v3 differ in "Who sees it"; the -small editions
// add the small-organization sentence) are never equivalent, so moving
// between them still re-displays the notice.
func AgentTelemetryAccepted(current string) []string {
	switch current {
	case AgentTelemetryV3ID:
		return []string{AgentTelemetryV3ID, AgentTelemetryV2ID}
	case AgentTelemetryV3SmallID:
		return []string{AgentTelemetryV3SmallID, AgentTelemetryV2SmallID}
	default:
		return []string{current}
	}
}

// Memory Sync Notice. It is its own consent lane, separate from the Agent
// Telemetry Notice: accepting one never accepts the other. The edition ID
// is sent to Fleet as consent_version when the user turns memory sync on.
// A store enabled under an edition that is not in MemorySyncAccepted sends
// nothing new until the user re-confirms.
//
// v2 clarifies shipped behaviour: it names
// Claude Desktop among the saving tools, says that forget-all also erases
// never-synced notes on the account's other devices (project notes are
// kept), and that a note used in a session goes to the AI tool's provider.
// Nothing new is sent, so enablements recorded under v1 stand without
// re-consent; v1 stays embedded for the record.
//
// v3 (2026-10-25 revision) replaces the list of saving tools
// with "a short label for the app that saved it (for example claude-code,
// codex or cli)", so the notice stays true for every Supported Tool. v2
// shipped in v0.6.0, so it is not edited in place. Nothing new is sent:
// enablements under v1 and v2 stand.
const (
	MemorySyncV1ID = "memory-sync/v1"
	MemorySyncV2ID = "memory-sync/v2"
	MemorySyncV3ID = "memory-sync/v3"
)

var (
	//go:embed memory-sync/v1.md
	MemorySyncV1 string
	//go:embed memory-sync/v2.md
	MemorySyncV2 string
	//go:embed memory-sync/v3.md
	MemorySyncV3 string
)

// MemorySync returns the current Memory Sync Notice edition: the one shown
// and recorded when a user turns memory sync on.
func MemorySync() Edition { return Edition{MemorySyncV3ID, MemorySyncV3} }

// MemorySyncAccepted lists the editions whose enablements still stand: the
// current edition and every earlier edition that changed only wording, not
// what is sent. A store enabled under any of them keeps syncing without a
// re-prompt.
func MemorySyncAccepted() []string { return []string{MemorySyncV3ID, MemorySyncV2ID, MemorySyncV1ID} }
