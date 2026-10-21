// Package canon is the small internal event union between the harness
// decoders and the projector.
//
// Decoders read only named source attributes and emit these structs. The
// only strings in the union are the native session id (hashed before it
// leaves the aggregator), the model id, the provider token, the raw tool
// name (mapped to an enum by the projector) and nothing else. There is no
// field that can carry a prompt, response, tool argument, tool output, path,
// cwd or error message, so a forgotten source attribute cannot leak.
package canon

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ClientKind identifies the producing harness. It is also the
// value of the kameas.client.kind resource attribute.
type ClientKind string

const (
	ClaudeCode ClientKind = "claude-code"
	Codex      ClientKind = "codex"
)

// Valid reports whether k is a known client kind.
func (k ClientKind) Valid() bool { return k == ClaudeCode || k == Codex }

// Kind discriminates Event.
type Kind int

const (
	// KindSessionSeen marks activity for a session (start on first sight).
	KindSessionSeen Kind = iota + 1
	// KindModelCall is one completed model call with usage.
	KindModelCall
	// KindToolCall is one completed tool call.
	KindToolCall
	// KindAPIError is one failed model API request.
	KindAPIError
	// KindCostDelta adds provider-reported cost to the session.
	KindCostDelta
)

// Event is one canonical event. Exactly the fields relevant to Kind are set.
type Event struct {
	Kind   Kind
	Client ClientKind
	// SessionID is the harness's native session/conversation id. It never
	// leaves the relay: the aggregator replaces it with an HMAC.
	SessionID string
	Time      time.Time

	// Provider is a provider token hint (e.g. "anthropic", "openai").
	Provider string

	// Model call (KindModelCall).
	Model         string
	InputTokens   int64 // excluding cache read/write
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
	Reasoning     int64
	CostUSD       float64
	CostKnown     bool
	Duration      time.Duration
	TTFT          time.Duration
	HasTTFT       bool
	FinishReasons []string
	// TurnID dedupes Codex turn-span usage against sse_event usage.
	// Never sent.
	TurnID string
	// FromTurnSpan marks usage taken from a Codex turn span (fallback).
	FromTurnSpan bool

	// Tool call (KindToolCall).
	ToolName  string // raw name, mapped to an enum by the projector
	ToolIsMCP bool
	Success   bool

	// API error (KindAPIError). StatusCode 0 means no HTTP status.
	StatusCode int
	Timeout    bool
}

// LocalSessionHash is the unkeyed, on-device-only session reference the
// SessionEnd hook sends to the daemon: the hook never passes a
// native session id over the control channel. It is never sent to Fleet;
// the aggregator maps it back to its keyed hash.
func LocalSessionHash(nativeID string) string {
	sum := sha256.Sum256([]byte(nativeID))
	return hex.EncodeToString(sum[:])[:32]
}
