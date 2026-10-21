// Package codex decodes OpenAI Codex's native OpenTelemetry into canonical
// events. It reads only the named
// attributes below. In particular codex.tool_result's "arguments" and
// "output" (full tool input and a 2 KiB output preview by default) are
// never read.
//
// Source: openai/codex@1b6e0d8 codex-rs/otel/src (events/shared.rs,
// events/session_telemetry.rs, tool_result.rs), core/src/tasks/mod.rs.
package codex

import (
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/attr"
)

// EventPrefix is the prefix of Codex event names.
const EventPrefix = "codex."

// IsServiceName reports whether service.name is a Codex originator
// (codex-rs/core/src/otel_init.rs: service.name = originator, e.g.
// "codex_cli_rs", "codex_vscode").
func IsServiceName(s string) bool { return strings.HasPrefix(s, "codex") }

// DefaultProvider is used when conversation_starts did not name one.
const DefaultProvider = "openai"

func base(a attr.Set, sessionKey string, t time.Time) canon.Event {
	return canon.Event{
		Client:    canon.Codex,
		SessionID: a.String(sessionKey),
		Time:      t,
	}
}

// LogRecord decodes one Codex log record.
func LogRecord(a attr.Set, name string, t time.Time) []canon.Event {
	b := base(a, "conversation.id", t)
	if b.SessionID == "" {
		return nil
	}
	seen := b
	seen.Kind = canon.KindSessionSeen
	out := []canon.Event{seen}

	switch name {
	case "codex.conversation_starts":
		out[0].Provider = strings.ToLower(a.String("provider_name"))

	case "codex.sse_event":
		if a.String("event.kind") != "response.completed" {
			break
		}
		e := b
		e.Kind = canon.KindModelCall
		e.Model = a.String("model")
		in := a.NonNeg("input_token_count")
		cached := a.NonNeg("cached_token_count")
		if cached > in {
			cached = in
		}
		e.InputTokens = in - cached // OpenAI input_tokens includes cached
		e.CacheRead = cached
		e.CacheCreation = a.NonNeg("cache_write_token_count")
		e.OutputTokens = a.NonNeg("output_token_count")
		e.Reasoning = a.NonNeg("reasoning_token_count")
		if v, ok := a.Int("ttft_ms"); ok && v >= 0 {
			e.TTFT, e.HasTTFT = time.Duration(v)*time.Millisecond, true
		}
		out = append(out, e)

	case "codex.api_request":
		sc, hasStatus := a.Int("http.response.status_code")
		failed := a.Has("error.message") || (hasStatus && (sc < 200 || sc > 299))
		if !failed {
			break
		}
		e := b
		e.Kind = canon.KindAPIError
		if hasStatus {
			e.StatusCode = int(sc)
		}
		out = append(out, e)

	case "codex.tool_result":
		e := b
		e.Kind = canon.KindToolCall
		e.ToolName = a.String("tool_name")
		e.ToolIsMCP = a.String("mcp_server") != "" || a.String("tool_origin") == "mcp"
		e.Success, _ = a.Bool("success")
		e.Duration = time.Duration(a.NonNeg("duration_ms")) * time.Millisecond
		out = append(out, e)

	case "codex.turn_cost":
		if usd, ok := a.Float("usage.estimated_usd"); ok && usd >= 0 {
			e := b
			e.Kind = canon.KindCostDelta
			e.CostUSD, e.CostKnown = usd, true
			out = append(out, e)
		}
	}
	return out
}

// TurnSpan reports whether a span is Codex's per-turn span, whose
// otel.name is the task kind (e.g. "session_task.turn").
func TurnSpan(name string, a attr.Set) bool {
	return (name == "turn" || strings.HasPrefix(name, "session_task.")) &&
		a.Has("codex.turn.token_usage.input_tokens")
}

// Span decodes one Codex span. Turn spans yield a fallback model call
// (used only when no sse_event usage arrived for the session);
// handle_responses and all other spans yield only session activity.
func Span(a attr.Set, name string, start, end time.Time) []canon.Event {
	b := base(a, "thread.id", end)
	if b.SessionID == "" {
		return nil
	}
	seen := b
	seen.Kind = canon.KindSessionSeen
	out := []canon.Event{seen}
	if !TurnSpan(name, a) {
		return out
	}
	e := b
	e.Kind = canon.KindModelCall
	e.FromTurnSpan = true
	e.TurnID = a.String("turn.id")
	e.Model = a.String("model")
	in := a.NonNeg("codex.turn.token_usage.input_tokens")
	cached := a.NonNeg("codex.turn.token_usage.cached_input_tokens")
	if cached > in {
		cached = in
	}
	e.InputTokens = in - cached
	e.CacheRead = cached
	e.CacheCreation = a.NonNeg("codex.turn.token_usage.cache_write_input_tokens")
	e.OutputTokens = a.NonNeg("codex.turn.token_usage.output_tokens")
	e.Reasoning = a.NonNeg("codex.turn.token_usage.reasoning_output_tokens")
	if end.After(start) {
		e.Duration = end.Sub(start)
	}
	return append(out, e)
}
