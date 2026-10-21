package project_test

import (
	"bytes"
	"testing"
	"unicode/utf8"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
)

// plantedKeys is every attribute key either decoder reads, plus the
// content-bearing and identity keys both harnesses emit. Each one carries
// the planted value in TestPlanted / FuzzPlanted. "model" is excluded on
// purpose: the model id is the one harness string the closed schema
// forwards (gen_ai.request.model, IDRe; Kameas Fleet re-checks it on arrival).
var plantedKeys = []string{
	// Claude Code
	"session.id", "input_tokens", "output_tokens", "cache_read_tokens",
	"cache_creation_tokens", "cost_usd", "duration_ms", "tool_name", "success",
	"status_code", "user.email", "user.account_uuid", "user.id", "organization.id",
	"terminal.type", "prompt.id", "prompt", "tool_parameters", "tool_input",
	"error", "decision", "source", "mcp_server_scope", "skill_name",
	// Codex
	"conversation.id", "thread.id", "turn.id", "provider_name", "event.kind",
	"input_token_count", "cached_token_count", "cache_write_token_count",
	"output_token_count", "reasoning_token_count", "ttft_ms",
	"http.response.status_code", "error.message", "mcp_server", "tool_origin",
	"usage.estimated_usd", "arguments", "output", "call_id", "auth_mode",
	"slug", "originator", "terminal.type", "user.account_id",
	"codex.turn.token_usage.input_tokens", "codex.turn.token_usage.cached_input_tokens",
	"codex.turn.token_usage.output_tokens", "codex.turn.token_usage.reasoning_output_tokens",
	"codex.turn.token_usage.cache_write_input_tokens",
	// Resource / generic
	"app.version", "service.version", "host.name", "host.id", "os.type",
	"process.command_args", "process.executable.path", "cwd", "git.branch",
	"gen_ai.response.id", "response.id", "kameas.user.id", "kameas.org.id",
	"kameas.machine.id", "kameas.client.kind", "kameas.client.version",
}

func plantedAttrs(v string, extra ...*commonv1.KeyValue) []*commonv1.KeyValue {
	out := append([]*commonv1.KeyValue(nil), extra...)
	for _, k := range plantedKeys {
		out = append(out, &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}})
	}
	return out
}

func kv(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func plantedResource(svc, v string) *resourcev1.Resource {
	return &resourcev1.Resource{Attributes: plantedAttrs(v, kv("service.name", svc))}
}

// runPlanted sends Claude Code and Codex exports in which every attribute,
// log body and resource attribute carries v, closes both sessions, and
// returns every byte handed to the transport.
func runPlanted(t *testing.T, v string, rev int) [][]byte {
	t.Helper()
	h := newHarness(t, rev)
	body := &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}

	claudeEvents := []string{"claude_code.api_request", "claude_code.tool_result", "claude_code.api_error",
		"claude_code.user_prompt", "claude_code.tool_decision"}
	var clr []*logsv1.LogRecord
	for _, e := range claudeEvents {
		clr = append(clr, &logsv1.LogRecord{
			TimeUnixNano: uint64(fixtures.Base.UnixNano()),
			EventName:    e,
			Body:         body,
			Attributes:   plantedAttrs(v, kv("model", "claude-sonnet-4-5")),
		})
	}
	codexEvents := []string{"codex.conversation_starts", "codex.sse_event", "codex.api_request",
		"codex.tool_result", "codex.turn_cost", "codex.user_prompt", "codex.tool_decision"}
	var xlr []*logsv1.LogRecord
	for _, e := range codexEvents {
		attrs := plantedAttrs(v, kv("event.name", e), kv("model", "gpt-5-codex"))
		if e == "codex.sse_event" {
			attrs = append([]*commonv1.KeyValue{kv("event.kind", "response.completed")}, attrs...)
		}
		xlr = append(xlr, &logsv1.LogRecord{TimeUnixNano: uint64(fixtures.Base.UnixNano()), Body: body, Attributes: attrs})
	}
	h.post(t, "/v1/logs", &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{
		{Resource: plantedResource("claude-code", v), ScopeLogs: []*logsv1.ScopeLogs{{Scope: &commonv1.InstrumentationScope{Name: v, Version: v}, LogRecords: clr}}},
		{Resource: plantedResource("codex_cli_rs", v), ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: xlr}}},
	}}, encProto)

	turn := &tracev1.Span{
		Name: "session_task.turn", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8),
		StartTimeUnixNano: uint64(fixtures.Base.UnixNano()), EndTimeUnixNano: uint64(fixtures.Base.UnixNano()) + 1e9,
		Attributes: plantedAttrs(v, kv("model", "gpt-5-codex"), &commonv1.KeyValue{Key: "codex.turn.token_usage.input_tokens",
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 10}}}),
		Events: []*tracev1.Span_Event{{Name: v, Attributes: plantedAttrs(v)}},
		Status: &tracev1.Status{Message: v},
	}
	other := &tracev1.Span{Name: v, Attributes: plantedAttrs(v), StartTimeUnixNano: uint64(fixtures.Base.UnixNano())}
	h.post(t, "/v1/traces", &tracev1.TracesData{ResourceSpans: []*tracev1.ResourceSpans{
		{Resource: plantedResource("codex_cli_rs", v), ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{turn, other}}}},
		{Resource: plantedResource("claude-code", v), ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{other}}}},
	}}, encProto)

	h.post(t, "/v1/metrics", &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource: plantedResource("claude-code", v),
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{{
			Name: "claude_code.session.count", Description: v, Unit: v,
			Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{DataPoints: []*metricsv1.NumberDataPoint{{
				Attributes: plantedAttrs(v), TimeUnixNano: uint64(fixtures.Base.UnixNano()),
			}}}},
		}}}},
	}}}, encProto)

	h.p.SessionEnd(canon.ClaudeCode, v)
	h.p.SessionEnd(canon.Codex, v)
	var out [][]byte
	for _, b := range h.batches {
		out = append(out, b.Body)
	}
	return out
}

// TestPlanted is the "planted string in every attribute" variant of the
// canary suite: nothing a harness puts in any attribute, body, scope or
// resource may reach the transport, even values that look
// like ordinary identifiers or version strings.
func TestPlanted(t *testing.T) {
	for _, v := range []string{
		"PLANTdevbox.local",
		"PLANT1.2.3",
		"2.1.217-PLANTrepo-name",
		"PLANTacme-secret-repo",
	} {
		for _, rev := range []int{1, 2} {
			bodies := runPlanted(t, v, rev)
			if len(bodies) == 0 {
				t.Fatalf("%q rev %d: nothing emitted", v, rev)
			}
			for _, b := range bodies {
				if bytes.Contains(b, []byte("PLANT")) {
					t.Errorf("planted value %q leaked at rev %d", v, rev)
				}
			}
		}
	}
}

// FuzzPlanted generalizes TestPlanted to arbitrary planted values.
func FuzzPlanted(f *testing.F) {
	f.Add("x")
	f.Add("1.0.0")
	f.Add("anthropic")
	f.Add("a/b@c:d")
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return // OTLP string fields must be UTF-8; the receiver rejects the rest
		}
		v := "PLANT" + s
		for _, b := range runPlanted(t, v, 2) {
			if bytes.Contains(b, []byte("PLANT")) {
				t.Fatalf("planted value %q leaked", v)
			}
		}
	})
}
