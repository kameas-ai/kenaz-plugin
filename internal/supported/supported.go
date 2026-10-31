// Package supported lists the hosts (AI tools) this build of kenaz works
// with, for the docs drift check.
//
// The docs Supported Tools page (kenaz-docs
// docs/kenaz-plugin/supported-tools.mdx) renders its table from
// docs/kenaz-plugin/supported-tools.json. That page is the list of
// Supported Tools the Subscription Agreement points to (the Privacy Policy
// links it too; the Early Access Addendum that once did is retired), so it
// must name exactly the hosts a shipped build supports, and say truthfully
// which of them report usage telemetry. testdata/supported-tools.json is a
// copy of that JSON; supported_test.go checks it against Hosts and
// GatedHosts, and scripts/check-supported-tools.sh runs the same test
// against the live docs JSON at release time.
package supported

import (
	"sort"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// Host is one AI tool this build connects to, by its kenaz identifier: the
// telemetry client kind (canon.ClientKind) for harnesses that report usage,
// or the MCP host label for one that does not.
type Host struct {
	ID string
	// UsageTelemetry reports whether kenaz relays usage telemetry for it.
	UsageTelemetry bool
}

// HostCowork is the Supported Tools host for Cowork sessions that run on the
// user's computer, which load the Kenaz plugin. Cowork has no telemetry
// client kind: its OpenTelemetry export is configured only by the Claude
// organization's admin settings, runs inside the Cowork VM, and carries
// service.name "cowork", which the relay does not decode. So kenaz relays no
// usage telemetry for it.
const HostCowork = "cowork"

// Hosts returns the hosts this build supports, sorted by ID. Claude Code and
// Codex are always supported. OpenCode is reachable only with its feature
// tag. The Claude desktop app's chat (the .mcpb extension) is listed in
// builds with features.ClaudeDesktopChat: the official release sets it
// exactly when it ships the extension. Cowork is listed only in builds with
// features.Cowork (-tags kenaz_cowork), off in shipped builds until the
// Claude directory listing is live.
func Hosts() []Host {
	hs := []Host{
		{string(canon.ClaudeCode), true},
		{string(canon.Codex), true},
	}
	if features.OpenCode {
		hs = append(hs, Host{string(canon.OpenCode), true})
	}
	if features.ClaudeDesktopChat {
		hs = append(hs, claudeDesktopChatHost())
	}
	if features.Cowork {
		hs = append(hs, coworkHost())
	}
	sortHosts(hs)
	return hs
}

// claudeDesktopChatHost is the host features.ClaudeDesktopChat turns on.
// Chat in the Claude desktop app emits no OpenTelemetry (the extension
// serves MCP tools only).
func claudeDesktopChatHost() Host { return Host{mcp.HostClaudeDesktop, false} }

// coworkHost is the host features.Cowork turns on; kenaz relays no usage
// telemetry for it (see HostCowork).
func coworkHost() Host { return Host{HostCowork, false} }

// GatedHosts returns the hosts that only builds with a release feature
// support, as those builds report them, whether or not this build has the
// feature: the Claude desktop app's chat (the official release with
// -tags kenaz_claude_desktop_chat) and Cowork (-tags kenaz_cowork, not yet
// in any release). The one Supported Tools list
// serves every build, so the drift check accepts a row for a gated host
// this build leaves off, and still rejects a host no build knows. OpenCode
// is not listed: it is not part of any release, so the docs must not name
// it.
func GatedHosts() []Host {
	hs := []Host{claudeDesktopChatHost(), coworkHost()}
	sortHosts(hs)
	return hs
}

func sortHosts(hs []Host) {
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
}
