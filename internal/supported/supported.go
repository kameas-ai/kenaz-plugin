// Package supported lists the hosts (AI tools) this build of kenaz works
// with, for the docs drift check.
//
// The docs Supported Tools page (kenaz-docs
// docs/kenaz-plugin/supported-tools.mdx) renders its table from
// docs/kenaz-plugin/supported-tools.json. The Subscription Agreement, the
// Privacy Policy and the Early Access Addendum point at that page as the
// list of Supported Tools, so it must
// name exactly the hosts a shipped build supports, and say truthfully which
// of them report usage telemetry. testdata/supported-tools.json is a copy of
// that JSON; supported_test.go checks it against Hosts, and
// scripts/check-supported-tools.sh runs the same test against the live docs
// JSON at release time.
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

// Hosts returns the hosts this build supports, sorted by ID. Claude Code and
// Codex are always supported. OpenCode and the Claude desktop app's chat
// (the .mcpb extension) are reachable only in builds with their feature
// tags, which shipped builds do not set.
func Hosts() []Host {
	hs := []Host{
		{string(canon.ClaudeCode), true},
		{string(canon.Codex), true},
	}
	if features.OpenCode {
		hs = append(hs, Host{string(canon.OpenCode), true})
	}
	if features.ClaudeDesktopChat {
		// Chat in the Claude desktop app emits no OpenTelemetry; the
		// extension serves MCP tools only.
		hs = append(hs, Host{mcp.HostClaudeDesktop, false})
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	return hs
}
