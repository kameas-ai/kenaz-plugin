package project

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

func TestToolName(t *testing.T) {
	cases := []struct {
		raw  string
		mcp  bool
		want string
	}{
		{"Bash", false, "Bash"},
		{"Agent", false, "Task"},
		{"local_shell", false, "shell"},
		{"apply_patch", false, "apply_patch"},
		{"mcp__github__create_issue", true, "mcp"},
		{"Bash", true, "mcp"},
		{"MyCustomTool", false, "other"},
		{"mcp", false, "other"},
		{"", false, "other"},
	}
	for _, c := range cases {
		got := ToolName(canon.ClaudeCode, c.raw, c.mcp)
		if got != c.want || !ToolNames[got] {
			t.Errorf("ToolName(%q,%v) = %q, want %q", c.raw, c.mcp, got, c.want)
		}
	}
}

func TestErrorCategory(t *testing.T) {
	cases := map[int]string{0: "network", 429: "rate_limited", 529: "overloaded", 503: "overloaded",
		401: "auth", 403: "auth", 400: "bad_request", 413: "bad_request", 422: "bad_request",
		500: "server", 502: "server", 504: "timeout", 404: "other"}
	for status, want := range cases {
		got := ErrorCategory(status, false)
		if got != want || !ErrorCategories[got] {
			t.Errorf("%d: %q want %q", status, got, want)
		}
	}
	if ErrorCategory(0, true) != CatTimeout {
		t.Error("timeout flag")
	}
	for cat, want := range map[string]bool{CatRateLimited: true, CatServer: true, CatAuth: false, CatBadRequest: false, CatOther: false} {
		if Recoverable(cat) != want {
			t.Errorf("Recoverable(%s)", cat)
		}
	}
}

func TestToolNameOpenCode(t *testing.T) {
	cases := []struct {
		raw  string
		mcp  bool
		want string
	}{
		{"bash", false, "Bash"},
		{"read", false, "Read"},
		{"webfetch", false, "WebFetch"},
		{"todowrite", false, "TodoWrite"},
		{"apply_patch", false, "apply_patch"},
		{"list", false, "other"},
		{"Bash", false, "other"}, // OpenCode ids are lower case; anything else is not a built-in
		{"github_create_issue", true, "mcp"},
		{"mcp", false, "other"},
	}
	for _, c := range cases {
		got := ToolName(canon.OpenCode, c.raw, c.mcp)
		if got != c.want || !ToolNames[got] {
			t.Errorf("ToolName(opencode, %q,%v) = %q, want %q", c.raw, c.mcp, got, c.want)
		}
	}
}
