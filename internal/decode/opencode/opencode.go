// Package opencode decodes the OTLP log records that the Kenaz OpenCode
// adapter (opencode/ in this repository) sends to the local relay. OpenCode
// has no native OTLP exporter that the relay relies on: the adapter
// subscribes to OpenCode's plugin events and emits these records itself.
//
// The adapter already strips content, but this decoder still reads only
// the named attributes below, exactly like the Claude Code and Codex
// decoders. Anything else on a record is ignored.
//
// Contract (adapter → relay), all records are logs with event_name set and
// resource service.name = "opencode":
//
//	opencode.session.start   session.id
//	opencode.session.end     session.id
//	opencode.model_call      session.id, call.id, model, provider,
//	                         input_tokens, output_tokens, reasoning_tokens,
//	                         cache_read_tokens, cache_write_tokens,
//	                         cost_usd (only when OpenCode reported a cost),
//	                         duration_ms
//	opencode.tool_result     session.id, call.id, tool_name, success,
//	                         duration_ms
//	opencode.api_error       session.id, status_code (optional), timeout
//
// call.id carries OpenCode's message or tool-call id. It only deduplicates
// retried exports; the aggregator keeps a keyed hash of it and nothing
// leaves the device.
package opencode

import (
	"regexp"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/attr"
)

// EventPrefix is the prefix of adapter event names.
const EventPrefix = "opencode."

// ServiceName is the adapter's OTLP service.name.
const ServiceName = "opencode"

// Event names.
const (
	EventSessionStart = "opencode.session.start"
	EventSessionEnd   = "opencode.session.end"
	EventModelCall    = "opencode.model_call"
	EventToolResult   = "opencode.tool_result"
	EventAPIError     = "opencode.api_error"
)

// builtinTools are OpenCode's built-in tool ids. Any other tool id that
// contains an underscore is an MCP tool ("<server>_<tool>"); the projector
// reports those as "mcp" and never their names.
var builtinTools = map[string]bool{
	"bash": true, "read": true, "edit": true, "write": true, "glob": true, "grep": true,
	"list": true, "webfetch": true, "websearch": true, "codesearch": true, "task": true,
	"todowrite": true, "todoread": true, "skill": true, "patch": true, "apply_patch": true,
	"multiedit": true, "lsp": true, "question": true, "invalid": true, "batch": true,
}

// IsMCPTool reports whether an OpenCode tool id denotes an MCP tool.
func IsMCPTool(name string) bool {
	return !builtinTools[name] && strings.Contains(name, "_")
}

// idRe bounds the dedup id: OpenCode ids are short ASCII tokens.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func base(a attr.Set, t time.Time) canon.Event {
	return canon.Event{
		Client:    canon.OpenCode,
		SessionID: a.String("session.id"),
		Time:      t,
	}
}

func dedupKey(kind string, a attr.Set) string {
	id := a.String("call.id")
	if !idRe.MatchString(id) {
		return ""
	}
	return kind + ":" + id
}

// LogRecord decodes one adapter log record.
func LogRecord(a attr.Set, name string, t time.Time) []canon.Event {
	b := base(a, t)
	if b.SessionID == "" {
		return nil
	}
	if name == EventSessionEnd {
		b.Kind = canon.KindSessionEnd
		return []canon.Event{b}
	}
	seen := b
	seen.Kind = canon.KindSessionSeen
	out := []canon.Event{seen}

	switch name {
	case EventModelCall:
		e := b
		e.Kind = canon.KindModelCall
		e.Model = a.String("model")
		e.Provider = strings.ToLower(a.String("provider"))
		e.InputTokens = a.NonNeg("input_tokens")
		e.OutputTokens = a.NonNeg("output_tokens")
		e.Reasoning = a.NonNeg("reasoning_tokens")
		e.CacheRead = a.NonNeg("cache_read_tokens")
		e.CacheCreation = a.NonNeg("cache_write_tokens")
		if c, ok := a.Float("cost_usd"); ok && c >= 0 {
			e.CostUSD, e.CostKnown = c, true
		}
		e.Duration = time.Duration(a.NonNeg("duration_ms")) * time.Millisecond
		e.DedupKey = dedupKey("m", a)
		out[0].Provider = e.Provider
		out = append(out, e)

	case EventToolResult:
		e := b
		e.Kind = canon.KindToolCall
		e.ToolName = a.String("tool_name")
		e.ToolIsMCP = IsMCPTool(e.ToolName)
		e.Success, _ = a.Bool("success")
		e.Duration = time.Duration(a.NonNeg("duration_ms")) * time.Millisecond
		e.DedupKey = dedupKey("t", a)
		out = append(out, e)

	case EventAPIError:
		e := b
		e.Kind = canon.KindAPIError
		if sc, ok := a.Int("status_code"); ok && sc > 0 && sc < 1000 {
			e.StatusCode = int(sc)
		}
		e.Timeout, _ = a.Bool("timeout")
		out = append(out, e)
	}
	// opencode.session.start and unknown names contribute only activity.
	return out
}
