package notices

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// modelCallsBullet is the approved "Model calls" class bullet as revised
// 2026-10-16: the description now names the coded session reference that
// was already sent. No field changed, so the edition IDs stay.
const modelCallsBullet = "- **Model calls:** for each request to an AI model, the model's name, the provider, token counts, estimated cost, timing, why the response ended, and a coded session reference that groups calls from one session without revealing the session itself."

// TestNoticeTextPinned ties each edition's text to the wording Kameas
// approved. Changing the text fails here until the pin is updated together
// with a deliberate choice: a description-only change keeps the edition ID
// (no re-consent); a change to what is sent needs a new edition ID
// (re-consent). The Fleet dashboard vendors these files with the same
// SHA-256 pins. Pins are of the launch (public-repository) wording.
func TestNoticeTextPinned(t *testing.T) {
	pins := map[string]string{
		AgentTelemetryV1ID:      "4800c72ca991fa107e343f6c7a1c67c24da93417976604faf1c2a396e73004d5",
		AgentTelemetryV2ID:      "6f7b284340d31b8c4282769d1fcaa941e850746415b5b097360a443c24e80a5b",
		AgentTelemetryV2SmallID: "2e924111bb2b1d320a626cf90aa01d4ff42a77d6e2799442c4e0ba7132bc17bf",
	}
	for _, e := range []Edition{
		AgentTelemetry(false, OrgSizeUnknown),
		AgentTelemetry(true, OrgSizeStandard),
		AgentTelemetry(true, OrgSizeSmall),
	} {
		sum := sha256.Sum256([]byte(e.Text))
		if got := hex.EncodeToString(sum[:]); got != pins[e.ID] {
			t.Errorf("%s: text changed (sha256 %s); decide wording-only vs new edition, then re-pin", e.ID, got)
		}
	}
}

// The Model calls completion is description only: every edition carries the
// revised bullet once, the old ending is gone, and the edition IDs that are
// stored as notice_version do not move with it.
func TestModelCallsBulletKeepsEditionIDs(t *testing.T) {
	for id, want := range map[string]string{
		AgentTelemetryV1ID:      "agent-telemetry/v1",
		AgentTelemetryV2ID:      "agent-telemetry/v2",
		AgentTelemetryV2SmallID: "agent-telemetry/v2-small",
	} {
		if id != want {
			t.Errorf("edition id %q changed (want %q): a description-only update must not re-consent", id, want)
		}
	}
	for _, text := range []string{AgentTelemetryV1, agentTelemetryV2, agentTelemetryV2Small} {
		if strings.Count(text, modelCallsBullet+"\n") != 1 {
			t.Error("edition does not carry the revised Model calls bullet exactly once")
		}
		if strings.Contains(text, "timing and why the response ended.") {
			t.Error("edition still carries the superseded Model calls ending")
		}
	}
}
