package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Event kinds in the queue.
const (
	eventPrompt = "prompt"
	eventTool   = "tool"
	eventStop   = "stop"
)

// Truncation limits for one event. They bound the queue and the model bill.
const (
	maxPromptBytes = 4 << 10
	maxInputBytes  = 16 << 10
	maxOutputBytes = 32 << 10
	maxStopBytes   = 4 << 10
)

// Event is one queued observation of a session. Input and Output are always
// scrubbed and truncated before an Event is created: raw text never reaches
// the disk.
type Event struct {
	V          int    `json:"v"`
	At         int64  `json:"at"`
	Session    string `json:"session"`
	Host       string `json:"host"`
	ProjectKey string `json:"project_key"`
	Root       string `json:"root"`
	Kind       string `json:"kind"`
	Tool       string `json:"tool,omitempty"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	Redactions int    `json:"redactions,omitempty"`
}

// hookPayload is what the hook events carry on stdin.
type hookPayload struct {
	SessionID    string          `json:"session_id"`
	Cwd          string          `json:"cwd"`
	Prompt       string          `json:"prompt"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	// LastAssistantMessage is on the Stop event.
	LastAssistantMessage string `json:"last_assistant_message"`
}

// sessionHash keeps the host's session id out of the queue and the store.
func sessionHash(id string) string {
	sum := sha256.Sum256([]byte("kenaz-capture-session\x00" + id))
	return hex.EncodeToString(sum[:])[:16]
}

// skipTools add nothing to the record of what happened.
var skipTools = map[string]bool{
	"AskUserQuestion": true, "ListMcpResourcesTool": true, "Skill": true, "SlashCommand": true, "TodoWrite": true, "ToolSearch": true,
}

func skipTool(tool string, extra []string) bool {
	if skipTools[tool] || isMemoryTool(tool) || isContextTool(tool) {
		return true
	}
	for _, e := range extra {
		if e == tool {
			return true
		}
	}
	return false
}

// isContextTool catches Kenaz's context and account tools, under any host's prefix.
// Their results hold the member's own files, shared team context, or names: not this
// session's work, and not something a note may carry to another place.
func isContextTool(tool string) bool {
	for _, name := range []string{"context_read", "context_list", "context_write", "context_search", "context_pull", "context_share", "context_share_file", "context_pull_files", "context_promote", "team_members", "fleet_status", "approval_status"} {
		if tool == name || strings.HasSuffix(tool, "__"+name) {
			return true
		}
	}
	return false
}

// isMemoryTool catches memory tools, ours or another product's. Capturing them
// would feed notes back into notes.
func isMemoryTool(tool string) bool {
	t := strings.ToLower(tool)
	return strings.Contains(t, "memory_") || strings.Contains(t, "-mem") || strings.Contains(t, "_mem_") || strings.Contains(t, "mem-search")
}

// maxFieldBytes is the largest field worth scrubbing. A bigger one is dropped, not
// cut: scrubbing it would hold up a synchronous hook, and truncating first could
// leave half a credential.
const maxFieldBytes = 256 << 10

const droppedField = "[too large to keep]"

// rawText renders a tool field for scrubbing: a JSON string as its value, anything
// else as compact JSON, which Scrub reduces to its values.
func rawText(m json.RawMessage) string {
	if len(m) == 0 || string(m) == "null" {
		return ""
	}
	if len(m) > maxFieldBytes {
		return droppedField
	}
	var s string
	if json.Unmarshal(m, &s) == nil {
		return s
	}
	return string(m)
}

// scrubField scrubs one field, dropping it first if it is oversized.
func scrubField(s, home string) (string, int) {
	if len(s) > maxFieldBytes {
		return droppedField, 0
	}
	return Scrub(s, home)
}

// clip truncates s to at most n bytes on a rune boundary, keeping the start
// and the end, where the useful part of command output usually is.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "\n[...]\n"
	head := (n - len(mark)) * 2 / 3
	tail := n - len(mark) - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + mark + s[start:]
}

// newToolEvent builds the queued form of a tool call, or nil when the tool is
// skipped or the call touches a credentials file. It scrubs before it truncates,
// so a cut can never leave half a credential behind.
func newToolEvent(p hookPayload, base Event, home string, skip []string) *Event {
	if skipTool(p.ToolName, skip) {
		return nil
	}
	input := rawText(p.ToolInput)
	if touchesSensitivePath(input) {
		return nil
	}
	ev := base
	ev.Kind, ev.Tool = eventTool, p.ToolName
	var n int
	ev.Input, n = scrubField(input, home)
	ev.Redactions += n
	ev.Input = clip(ev.Input, maxInputBytes)
	ev.Output, n = scrubField(rawText(p.ToolResponse), home)
	ev.Redactions += n
	ev.Output = clip(ev.Output, maxOutputBytes)
	return &ev
}

// newStopEvent marks a turn's end, with what the host reports the assistant
// last said. The summary uses it.
func newStopEvent(p hookPayload, base Event, home string) *Event {
	ev := base
	ev.Kind = eventStop
	text, n := scrubField(p.LastAssistantMessage, home)
	ev.Output, ev.Redactions = clip(text, maxStopBytes), n
	return &ev
}

func newPromptEvent(p hookPayload, base Event, home string) *Event {
	text, n := scrubField(p.Prompt, home)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	ev := base
	ev.Kind, ev.Input, ev.Redactions = eventPrompt, clip(text, maxPromptBytes), n
	return &ev
}

func nowMillis(t time.Time) int64 { return t.UnixMilli() }
