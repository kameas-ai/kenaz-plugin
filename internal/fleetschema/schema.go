// Package fleetschema is the public snapshot of the parts of Kameas Fleet's
// closed telemetry schema that the projector targets. It is hand-maintained for now from
// Fleet's telemetry schema v1 (log bodies, span specs, classes and tier
// gates). A Fleet-side CI job will regenerate it and open a PR here (TODO).
//
// Everything here is data the projector may emit; nothing outside it is
// ever sent.
package fleetschema

import "regexp"

// Log event kinds (kameas.event.kind) the projector emits.
const (
	KindConversationStarted = "harness.conversation_started"
	KindConversationEnded   = "harness.conversation_ended"
	KindToolInvoked         = "harness.tool_invoked"
	KindError               = "harness.error"
)

// SpanGenAIChat is the only span the projector emits.
const SpanGenAIChat = "gen_ai.chat"

// Class is a Fleet telemetry opt-in class (class.go:84-93).
type Class string

const (
	ClassUsageCounts        Class = "harness.usage_counts"
	ClassToolCalls          Class = "harness.tool_calls"
	ClassErrors             Class = "harness.errors"
	ClassModelCallTelemetry Class = "model_call_telemetry"
)

// KindClass maps each emitted log kind to its opt-in class.
var KindClass = map[string]Class{
	KindConversationStarted: ClassUsageCounts,
	KindConversationEnded:   ClassUsageCounts,
	KindToolInvoked:         ClassToolCalls,
	KindError:               ClassErrors,
}

// MinTier is the lowest tier allowed for each class (tier_gate.go:39-48).
// All four classes the projector uses are Pro+.
var MinTier = map[Class]string{
	ClassUsageCounts:        "pro",
	ClassToolCalls:          "pro",
	ClassErrors:             "pro",
	ClassModelCallTelemetry: "pro",
}

// BodyKeys is the closed body key set per log kind (body.go). CostKnownKey
// is added to conversation_ended only when Fleet advertises the schema
// revision that introduced it.
var BodyKeys = map[string][]string{
	KindConversationStarted: {"conversation_id", "model_provider"},
	KindConversationEnded:   {"conversation_id", "duration_ms", "token_in", "token_out", "cost_usd"},
	KindToolInvoked:         {"tool_name", "latency_ms", "success"},
	KindError:               {"category", "recoverable"},
}

// CostKnownKey is the body field on conversation_ended.
const CostKnownKey = "cost_known"

// SchemaRevCostKnown is the telemetry_schema capability revision at which
// Fleet accepts cost_known and the kameas.client.* resource attributes.
// Below it, both are omitted.
const SchemaRevCostKnown = 2

// GenAIChatAttrs is the subset of the gen_ai.chat span budget
// (span_specs.go:244-290) the projector emits. gen_ai.response.id is
// deliberately absent.
var GenAIChatAttrs = []string{
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.request.model",
	"gen_ai.response.finish_reasons",
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
	"gen_ai.usage.cache_read.input_tokens",
	"gen_ai.usage.cache_creation.input_tokens",
	"kenaz.llm.cost_usd",
	"kenaz.llm.reasoning_tokens",
	"kenaz.llm.ttft_ms",
	"kenaz.session.hash",
}

// ResourceAttrs are stamped by the relay, never copied from a harness.
// ClientResourceAttrs are emitted only at
// SchemaRevCostKnown or later.
var (
	ResourceAttrs       = []string{"kameas.user.id", "kameas.org.id", "kameas.machine.id", "kameas.tier", "kameas.vm.id", "service.name", "service.version"}
	ClientResourceAttrs = []string{"kameas.client.kind", "kameas.client.version"}
)

// LogAttrs are per-record log attributes.
var LogAttrs = []string{"kameas.event.kind"}

// KnownProviders is Fleet's closed provider enum (span_specs.go:95-101).
var KnownProviders = enum(
	"openai", "anthropic", "google", "google-vertex", "google-ai-studio", "mistral", "meta",
	"cohere", "deepseek", "xai", "groq", "together", "fireworks", "deepinfra", "perplexity",
	"amazon-bedrock", "aws.bedrock", "azure", "azure.ai.openai", "openrouter", "ollama",
	"lmstudio", "vllm", "novita", "lambda", "cerebras", "sambanova", "nebius", "hyperbolic",
	"parasail", "chutes", "baseten", "cloudflare", "inference-net", "friendli", "ai21",
	"alibaba", "moonshotai", "z-ai", "qwen", "nvidia", ProviderOther,
)

// ProviderOther is the provider catch-all.
const ProviderOther = "other"

// FinishReasons is Fleet's closed finish-reason enum (span_specs.go:83-87).
var FinishReasons = enum(
	"stop", "end_turn", "stop_sequence", "max_tokens", "length", "tool_use", "tool_calls",
	"function_call", "content_filter", "refusal", "pause_turn", "model_context_window_exceeded",
	"safety", "recitation", "blocklist", "prohibited_content", "spii", "malformed_function_call",
	"language", "image_safety", "error", "other", "unknown", "cancelled",
)

var (
	// IDRe matches ids and model ids, <=128 chars (span_specs.go:67).
	IDRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]{1,128}$`)
	// SessionHashRe matches kenaz.session.hash (span_specs.go:70).
	SessionHashRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// MaxRecordsPerRequest is Fleet's per-request cap (receiver.go:240-242).
const MaxRecordsPerRequest = 1000

// MaxBatchesPerMinute is Fleet's per-(org, machine) rate (receiver.go:62).
const MaxBatchesPerMinute = 100

// NormalizeProvider maps p onto KnownProviders, else ProviderOther.
func NormalizeProvider(p string) string {
	if KnownProviders[p] {
		return p
	}
	return ProviderOther
}

func enum(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// Output is the projector's complete closed output set:
// every key it can ever emit, at the highest schema revision it supports.
// fleetapi/allowlist exports it; the projector's tests assert every
// emitted key is in it.
type Output struct {
	ResourceAttributes []string
	LogAttributes      []string
	BodyKeys           map[string][]string
	SpanAttributes     map[string][]string
}

// ClosedOutput returns the closed output set.
func ClosedOutput() Output {
	body := make(map[string][]string, len(BodyKeys))
	for k, v := range BodyKeys {
		body[k] = append([]string(nil), v...)
	}
	body[KindConversationEnded] = append(body[KindConversationEnded], CostKnownKey)
	return Output{
		ResourceAttributes: append(append([]string(nil), ResourceAttrs...), ClientResourceAttrs...),
		LogAttributes:      append([]string(nil), LogAttrs...),
		BodyKeys:           body,
		SpanAttributes:     map[string][]string{SpanGenAIChat: append([]string(nil), GenAIChatAttrs...)},
	}
}
