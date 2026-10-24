package project

import "github.com/kameas-ai/kenaz-plugin/internal/canon"

// ToolNames is the closed tool_name enum.
var ToolNames = map[string]bool{
	"Bash": true, "Read": true, "Edit": true, "Write": true, "Glob": true, "Grep": true,
	"WebFetch": true, "WebSearch": true, "Task": true, "TodoWrite": true, "NotebookEdit": true,
	"Skill": true, "apply_patch": true, "exec_command": true, "shell": true, "view_image": true,
	"update_plan": true, ToolMCP: true, ToolOther: true,
}

const (
	ToolMCP   = "mcp"
	ToolOther = "other"
)

// toolAliases maps documented harness names onto the enum.
var toolAliases = map[string]string{
	"Agent":       "Task",  // Claude Code's current name for Task
	"local_shell": "shell", // Codex
}

// openCodeAliases maps OpenCode's lower-case built-in tool ids onto the
// enum. Only OpenCode records use it. Unlisted ids (list, patch, lsp, …)
// are reported as "other".
var openCodeAliases = map[string]string{
	"bash": "Bash", "read": "Read", "edit": "Edit", "write": "Write", "glob": "Glob",
	"grep": "Grep", "webfetch": "WebFetch", "websearch": "WebSearch", "task": "Task",
	"todowrite": "TodoWrite", "skill": "Skill", "apply_patch": "apply_patch",
}

// ToolName maps a raw harness tool name to the tool-name enum. MCP server and
// tool names never leave: any MCP tool is "mcp".
func ToolName(client canon.ClientKind, raw string, isMCP bool) string {
	if isMCP {
		return ToolMCP
	}
	if client == canon.OpenCode {
		a, ok := openCodeAliases[raw]
		if !ok {
			return ToolOther
		}
		return a
	}
	if a, ok := toolAliases[raw]; ok {
		raw = a
	}
	if raw == ToolMCP || raw == ToolOther {
		return ToolOther
	}
	if ToolNames[raw] {
		return raw
	}
	return ToolOther
}

// Error categories.
const (
	CatRateLimited = "rate_limited"
	CatOverloaded  = "overloaded"
	CatAuth        = "auth"
	CatBadRequest  = "bad_request"
	CatServer      = "server"
	CatNetwork     = "network"
	CatTimeout     = "timeout"
	CatToolFailure = "tool_failure"
	CatOther       = "other"
)

// ErrorCategories is the closed category enum.
var ErrorCategories = map[string]bool{
	CatRateLimited: true, CatOverloaded: true, CatAuth: true, CatBadRequest: true,
	CatServer: true, CatNetwork: true, CatTimeout: true, CatToolFailure: true, CatOther: true,
}

// ErrorCategory maps an HTTP status (0 = none) to a category. Error
// messages are never read.
func ErrorCategory(status int, timeout bool) string {
	switch {
	case timeout:
		return CatTimeout
	case status == 0:
		return CatNetwork
	case status == 429:
		return CatRateLimited
	case status == 529 || status == 503:
		return CatOverloaded
	case status == 401 || status == 403:
		return CatAuth
	case status == 400 || status == 413 || status == 422:
		return CatBadRequest
	case status == 408 || status == 504:
		return CatTimeout
	case status >= 500 && status <= 599:
		return CatServer
	default:
		return CatOther
	}
}

// Recoverable reports whether a category is transient.
func Recoverable(cat string) bool {
	switch cat {
	case CatRateLimited, CatOverloaded, CatServer, CatNetwork, CatTimeout, CatToolFailure:
		return true
	}
	return false
}
