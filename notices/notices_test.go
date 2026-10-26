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
		AgentTelemetryV3ID:      "af510d9d6bda99de6cfc0e0b9b16ba8319396f46c55e02b2e8c6bde49b1bf6ea",
		AgentTelemetryV3SmallID: "3923162a96e3f54c5251ae3c994b2470e30b3ede132d056e9927e2bc5e1dde1e",
	}
	for _, e := range []Edition{
		AgentTelemetry(false, OrgSizeUnknown),
		{AgentTelemetryV2ID, agentTelemetryV2},
		{AgentTelemetryV2SmallID, agentTelemetryV2Small},
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
	for _, text := range []string{AgentTelemetryV1, agentTelemetryV2, agentTelemetryV2Small, agentTelemetryV3, agentTelemetryV3Small} {
		if strings.Count(text, modelCallsBullet+"\n") != 1 {
			t.Error("edition does not carry the revised Model calls bullet exactly once")
		}
		if strings.Contains(text, "timing and why the response ended.") {
			t.Error("edition still carries the superseded Model calls ending")
		}
	}
}

// TestMemorySyncNoticePinned ties each Memory Sync Notice edition to its
// reviewed wording, and checks the current one states what the lane sends
// and how to delete it. It must stay a separate edition from the Agent
// Telemetry Notice.
func TestMemorySyncNoticePinned(t *testing.T) {
	pins := map[string]string{
		MemorySyncV1ID: "46f552ef5d363a792363aebc5594d1ae0c3bc82b0ee79606a6bdc95e420e1a50",
		MemorySyncV2ID: "fe7ca28271b5420b7cfc4ab142306728d2e9e16ac09695347dad21258cb19daa",
		MemorySyncV3ID: "74d9de28a5bc7791208fb83f55be322d3c4f89102c28a39db7e7ac12ac5fae8f",
	}
	for id, text := range map[string]string{MemorySyncV1ID: MemorySyncV1, MemorySyncV2ID: MemorySyncV2, MemorySyncV3ID: MemorySyncV3} {
		sum := sha256.Sum256([]byte(text))
		if got := hex.EncodeToString(sum[:]); got != pins[id] {
			t.Errorf("%s: text changed (sha256 %s); decide wording-only vs new edition, then re-pin", id, got)
		}
	}
	e := MemorySync()
	if e.ID != "memory-sync/v3" || e.Text != MemorySyncV3 || strings.HasPrefix(e.ID, "agent-telemetry") {
		t.Fatalf("current edition %q", e.ID)
	}
	for _, must := range []string{"text and title", "a short label for the app that saved it", "kenaz memory forget-all", "does not delete what was already synced",
		"including notes there that had not been synced yet", "separate from telemetry", "It is off until you turn it on", "sent to the tool's AI provider"} {
		if !strings.Contains(e.Text, must) {
			t.Errorf("notice lacks %q", must)
		}
	}
}

// v2 and v3 clarify what v1 already described (nothing new is sent), so an
// enablement recorded under v1 or v2 stands: every edition is accepted, the
// current one first.
func TestMemorySyncV1EnablementsStand(t *testing.T) {
	acc := MemorySyncAccepted()
	if len(acc) != 3 || acc[0] != MemorySync().ID || acc[1] != MemorySyncV2ID || acc[2] != MemorySyncV1ID {
		t.Fatalf("accepted editions = %v", acc)
	}
}

// The v3 Memory Sync Notice names no product: the saving app is a label.
func TestMemorySyncV3NamesNoProduct(t *testing.T) {
	for _, name := range []string{"Claude Code", "Claude Desktop", "Codex,", "OpenCode", "Kenaz CLI"} {
		if strings.Contains(MemorySyncV3, name) {
			t.Errorf("memory-sync/v3 names %q", name)
		}
	}
}

// v3 is the generic-wording revision of v2 (2026-10-25 revision):
// the only change is the closing sentence. Opt-ins under v2 stand
// under v3 (and v2-small under v3-small); v1 and the -small editions stay
// distinct because they say different things about who sees the data.
func TestAgentTelemetryV3(t *testing.T) {
	if e := AgentTelemetry(true, OrgSizeStandard); e.ID != AgentTelemetryV3ID {
		t.Fatalf("standard edition = %s", e.ID)
	}
	if e := AgentTelemetry(true, OrgSizeSmall); e.ID != AgentTelemetryV3SmallID {
		t.Fatalf("small edition = %s", e.ID)
	}
	const oldEnd = "Kenaz never changes or blocks what Claude Code or Codex does.\n"
	const newEnd = "Kenaz never changes or blocks what your AI tools do.\n"
	for _, p := range [][2]string{{agentTelemetryV2, agentTelemetryV3}, {agentTelemetryV2Small, agentTelemetryV3Small}} {
		if !strings.HasSuffix(p[0], oldEnd) || strings.TrimSuffix(p[0], oldEnd)+newEnd != p[1] {
			t.Error("v3 must differ from v2 only in the closing sentence")
		}
	}
	for cur, want := range map[string][]string{
		AgentTelemetryV3ID:      {AgentTelemetryV3ID, AgentTelemetryV2ID},
		AgentTelemetryV3SmallID: {AgentTelemetryV3SmallID, AgentTelemetryV2SmallID},
		AgentTelemetryV1ID:      {AgentTelemetryV1ID},
	} {
		if got := AgentTelemetryAccepted(cur); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("AgentTelemetryAccepted(%s) = %v, want %v", cur, got, want)
		}
	}
}
