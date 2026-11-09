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
		MemorySyncV4ID: "0e905ab223c2a683fcc52d6e7a6e0c9dec106dd531974f41aab6d6f1d54feb59",
	}
	for id, text := range map[string]string{MemorySyncV1ID: MemorySyncV1, MemorySyncV2ID: MemorySyncV2, MemorySyncV3ID: MemorySyncV3, MemorySyncV4ID: MemorySyncV4} {
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

// The capture notice is the approved wording, with one model line filled in.
func TestMemoryCaptureNotice(t *testing.T) {
	sum := sha256.Sum256([]byte(memoryCaptureV1))
	if got := hex.EncodeToString(sum[:]); got != "ac4f90e24056aa64d30d67f1e77b69fe8235070013e1c3b14aa2477b6fcf4f4c" {
		t.Errorf("memory-capture/v1 changed (sha256 %s): it needs legal's approval and a new edition", got)
	}
	local := MemoryCapture(MemoryCaptureLocal)
	if local.ID != "memory-capture/v1" || strings.Contains(local.Text, "{") || !strings.Contains(local.Text, "a language model summarizes that activity. A model on your device writes the notes, so this step sends nothing to another company.") {
		t.Errorf("local notice: %s", local.Text)
	}
	agent := MemoryCapture(MemoryCaptureSessionAgent)
	if !strings.Contains(agent.Text, "Your AI tool's own session writes the notes. This uses your plan with that tool and may appear in your session, and nothing extra is sent to another company.") {
		t.Errorf("session-agent notice: %s", agent.Text)
	}
	key := MemoryCapture(MemoryCaptureAPIKey("Example"))
	if !strings.Contains(key.Text, "This uses Example with the key you or your organization set up, so that session content goes to Example under your or your organization's arrangement with them.") {
		t.Errorf("provider notice: %s", key.Text)
	}
	host := MemoryCapture(MemoryCaptureHostCLI("Example Tool", "Example"))
	if !strings.Contains(host.Text, "This runs Example Tool under your own Example account on this device, so session content goes to Example under your arrangement with them, and it counts against your plan. Example's terms and limits apply, and Example may restrict this kind of use. Kameas can't promise it will keep working and isn't responsible for action Example takes on your account. An API key is the alternative.") {
		t.Errorf("host CLI notice: %s", host.Text)
	}
	for _, must := range []string{"not end-to-end encrypted", "In a chat app, Kenaz reads only notes not tied to a project unless you change that in the Kenaz extension's settings in the Claude desktop app. If you do, notes from all your projects can go to that app's AI provider.", "We don't use your notes to train any model."} {
		if !strings.Contains(local.Text, must) {
			t.Errorf("notice lacks %q", must)
		}
	}
}

// Until the project-scope release, v3 stays current and its enablements stand.
func TestMemorySyncV3UntilProjectScopeRelease(t *testing.T) {
	if ProjectScopeRelease {
		t.Skip("project-scope build")
	}
	acc := MemorySyncAccepted()
	if len(acc) != 3 || acc[0] != MemorySyncV3ID || acc[1] != MemorySyncV2ID || acc[2] != MemorySyncV1ID {
		t.Fatalf("accepted editions = %v", acc)
	}
}

// memory-sync/v4 makes the changes the legal ruling lists from v3, and earlier
// enablements no longer stand. It appears only with the project-scope release.
func TestMemorySyncV4(t *testing.T) {
	ProjectScopeRelease = true
	defer func() { ProjectScopeRelease = false }()
	if e := MemorySync(); e.ID != "memory-sync/v4" || e.Text != MemorySyncV4 {
		t.Errorf("current edition %s", e.ID)
	}
	if got := MemorySyncAccepted(); len(got) != 1 || got[0] != "memory-sync/v4" {
		t.Errorf("accepted %v: v4 changes what is sent, so existing syncers confirm again", got)
	}
	for _, must := range []string{
		"the project a note belongs to, where it has one, shown by its name (the name of its folder, unless you change it)",
		"It permanently erases every note saved under your account, including project and session notes, in Kameas Fleet, on this device, and on your other devices.",
		// The interim account of other devices, until the fixes that let v4 promise
		// more ship.
		"That includes notes on those devices that never synced and exist nowhere else. Your other plugin devices erase these notes when they next sync, which may be later if sync is off there or your organization's plan is paused or has lapsed. A device running the Kenaz Harness keeps the notes on it for now. A device that hasn't synced before, or has a wrong clock, could send a note back to Kameas Fleet; you can delete it with `kenaz memory forget` or by asking your AI tool to forget it. Notes saved while you were signed out aren't affected.",
		"Notes saved while you were signed out aren't affected.",
		"Kenaz doesn't send a separate record of your file paths, but a note's text can include paths and other details from your work.",
		"It is not end-to-end encrypted: our systems can read it, and people at Kameas with administrator access to those systems could technically read it.",
		"In a chat app, Kenaz reads only notes not tied to a project unless you change that in the Kenaz extension's settings in the Claude desktop app. If you do, notes from all your projects can go to that app's AI provider.",
	} {
		if !strings.Contains(MemorySyncV4, must) {
			t.Errorf("v4 lacks %q", must)
		}
	}
	for _, gone := range []string{"Notes that belong to a single project are kept", "notes that belong to a single project,",
		"A device keeps its copies", "When a device syncs,", "If its clock is wrong", "notes made before it started recording accounts",
		"If more than one account used that device", "your notes aren't available"} {
		if strings.Contains(MemorySyncV4, gone) {
			t.Errorf("v4 still says %q", gone)
		}
	}
}
