package mcp

import (
	"strings"
	"testing"
)

// Chat is told about context files only by a server that serves their tools.
func TestDesktopInstructionsMentionFilesOnlyWithTheFeature(t *testing.T) {
	getenv := func(k string) string {
		if k == HostEnvVar {
			return HostClaudeDesktop
		}
		return ""
	}
	old := ContextFilesServed
	t.Cleanup(func() { ContextFilesServed = old })
	ContextFilesServed = false
	if got := serverOptions(getenv).Instructions; strings.Contains(got, "context_list") {
		t.Fatalf("off: %s", got)
	}
	ContextFilesServed = true
	if got := serverOptions(getenv).Instructions; !strings.Contains(got, "context_list") || !strings.Contains(got, "context_search") {
		t.Fatalf("on: %s", got)
	}
}
