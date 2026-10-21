// Package claude decodes Claude Code's native OpenTelemetry into canonical
// events. It reads only the named
// attributes below and nothing else.
//
// Source: code.claude.com/docs/en/monitoring-usage (fetched 2026-10-14).
package claude

import (
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/attr"
)

// EventPrefix is the prefix of Claude Code event names.
const EventPrefix = "claude_code."

// ServiceNames are Claude Code's OTEL service.name values.
var ServiceNames = map[string]bool{"claude-code": true, "claude-code-desktop": true}

// SessionStartMetric is emitted once per session start.
const SessionStartMetric = "claude_code.session.count"

// Provider is the provider token for every Claude Code model call.
const Provider = "anthropic"

func base(a attr.Set, t time.Time) canon.Event {
	return canon.Event{
		Client:    canon.ClaudeCode,
		SessionID: a.String("session.id"),
		Time:      t,
		Provider:  Provider,
	}
}

// LogRecord decodes one log record. name is the event name with or without
// the "claude_code." prefix.
func LogRecord(a attr.Set, name string, t time.Time) []canon.Event {
	b := base(a, t)
	if b.SessionID == "" {
		return nil
	}
	seen := b
	seen.Kind = canon.KindSessionSeen
	out := []canon.Event{seen}

	switch strings.TrimPrefix(name, EventPrefix) {
	case "api_request":
		e := b
		e.Kind = canon.KindModelCall
		e.Model = a.String("model")
		e.InputTokens = a.NonNeg("input_tokens")
		e.OutputTokens = a.NonNeg("output_tokens")
		e.CacheRead = a.NonNeg("cache_read_tokens")
		e.CacheCreation = a.NonNeg("cache_creation_tokens")
		if c, ok := a.Float("cost_usd"); ok && c >= 0 {
			e.CostUSD, e.CostKnown = c, true
		}
		e.Duration = time.Duration(a.NonNeg("duration_ms")) * time.Millisecond
		out = append(out, e)

	case "tool_result":
		e := b
		e.Kind = canon.KindToolCall
		e.ToolName = a.String("tool_name")
		e.ToolIsMCP = IsMCPTool(e.ToolName)
		e.Success, _ = a.Bool("success")
		e.Duration = time.Duration(a.NonNeg("duration_ms")) * time.Millisecond
		out = append(out, e)

	case "api_error":
		e := b
		e.Kind = canon.KindAPIError
		if sc, ok := a.Int("status_code"); ok {
			e.StatusCode = int(sc)
		}
		out = append(out, e)
	}
	// Every other event (user_prompt, assistant_response, tool_decision,
	// api_request_body, …) contributes only session activity.
	return out
}

// Span decodes one span. Only session activity is taken from spans; the
// llm_request finish-reason enrichment is deferred (see package project).
func Span(a attr.Set, name string, t time.Time) []canon.Event {
	b := base(a, t)
	if b.SessionID == "" {
		return nil
	}
	b.Kind = canon.KindSessionSeen
	return []canon.Event{b}
}

// MetricPoint decodes one metric data point's attributes. Only the
// session-start metric is used; everything else is dropped.
func MetricPoint(a attr.Set, metric string, t time.Time) []canon.Event {
	if metric != SessionStartMetric {
		return nil
	}
	b := base(a, t)
	if b.SessionID == "" {
		return nil
	}
	b.Kind = canon.KindSessionSeen
	return []canon.Event{b}
}

// IsMCPTool reports whether a Claude Code tool name denotes an MCP tool.
// With OTEL_LOG_TOOL_DETAILS unset, MCP tools appear as "mcp_tool" (or
// "mcp_other" in tool_name_safe); with it set, as "mcp__<server>__<tool>".
func IsMCPTool(name string) bool {
	return name == "mcp_tool" || name == "mcp_other" || strings.HasPrefix(name, "mcp__")
}
