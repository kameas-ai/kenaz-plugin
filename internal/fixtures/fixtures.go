// Package fixtures builds SYNTHETIC OTLP exports shaped like Claude Code's
// and Codex's native telemetry, for tests.
//
// They are synthesized from the documented attribute lists, not recorded:
//   - Claude Code: code.claude.com/docs/en/monitoring-usage (2026-10-14)
//   - Codex: openai/codex@1b6e0d8 codex-rs/otel/src/{events/shared.rs,
//     events/session_telemetry.rs,tool_result.rs}, core/src/tasks/mod.rs
//
// Real recordings with every content flag on are still to do; when they
// exist they go in internal/project/testdata and run through the same
// canary test.
//
// Every content-bearing attribute carries a distinct 32-char canary so the
// canary suite can prove none of them leaves the device.
package fixtures

import (
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Canaries planted in content-bearing places. None may appear in output.
var Canaries = map[string]string{
	"prompt":         "CANARYprompt00000000000000000001",
	"tool_input":     "CANARYtoolinput0000000000000002",
	"tool_output":    "CANARYtooloutput000000000000003",
	"file_path":      "CANARYfilepath00000000000000004",
	"cwd":            "CANARYcwd0000000000000000000005",
	"mcp_server":     "CANARYmcpserver0000000000000006",
	"error_message":  "CANARYerrormessage0000000000007",
	"git_branch":     "CANARYgitbranch0000000000000008",
	"hostname":       "CANARYhostname00000000000000009",
	"email":          "CANARYemail000000000000000000010",
	"account":        "CANARYaccount0000000000000000011",
	"native_session": "CANARYsession0000000000000000012",
	"response":       "CANARYresponse000000000000000013",
	"custom_tool":    "CANARYcustomtool00000000000000014",
	"request_id":     "CANARYrequestid000000000000000015",
	"trace_id_hex":   "0123456789abcdef0123456789abcdef",
}

// Base is the fixed start time of every fixture session.
var Base = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func s(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func i(k string, v int64) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: v}}}
}

func f(k string, v float64) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: v}}}
}

func ts(d time.Duration) uint64 { return uint64(Base.Add(d).UnixNano()) }

func logRec(at time.Duration, body string, attrs []*commonv1.KeyValue) *logsv1.LogRecord {
	return &logsv1.LogRecord{TimeUnixNano: ts(at), Attributes: attrs,
		Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: body}}}
}

// ClaudeSessionID is the native session id used by ClaudeLogs.
var ClaudeSessionID = Canaries["native_session"] + "-claude"

func claudeStd(event string) []*commonv1.KeyValue {
	c := Canaries
	return []*commonv1.KeyValue{
		s("event.name", event),
		s("event.timestamp", "2026-10-14T12:00:00Z"),
		s("session.id", ClaudeSessionID),
		s("user.email", c["email"]+"@example.com"),
		s("user.account_uuid", c["account"]),
		s("organization.id", c["account"]),
		s("terminal.type", "iTerm.app"),
		s("prompt.id", c["request_id"]),
	}
}

func claudeResource() *resourcev1.Resource {
	return &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
		s("service.name", "claude-code"),
		s("service.version", "2.1.290"),
		s("host.name", Canaries["hostname"]),
		s("host.arch", "arm64"),
		// A forged identity attribute: must be overwritten, never forwarded.
		s("kameas.user.id", "forged-"+Canaries["account"]),
		s("kameas.org.id", "forged-org"),
	}}
}

// ClaudeLogs is a Claude Code session with every content flag on
// (OTEL_LOG_USER_PROMPTS, OTEL_LOG_TOOL_DETAILS, OTEL_LOG_TOOL_CONTENT,
// OTEL_LOG_RAW_API_BODIES): prompt, two API requests ($0.01 + $0.02), a
// successful Bash call, a failed MCP call, and an API error.
func ClaudeLogs() *logsv1.LogsData {
	c := Canaries
	with := func(ev string, extra ...*commonv1.KeyValue) []*commonv1.KeyValue {
		return append(claudeStd(ev), extra...)
	}
	recs := []*logsv1.LogRecord{
		logRec(0, "claude_code.user_prompt", with("user_prompt",
			s("prompt", c["prompt"]+" in "+c["file_path"]), i("prompt_length", 80))),
		logRec(time.Second, "claude_code.api_request", with("api_request",
			s("model", "claude-sonnet-4-5"), f("cost_usd", 0.01), i("duration_ms", 900),
			i("input_tokens", 100), i("output_tokens", 50), i("cache_read_tokens", 1000), i("cache_creation_tokens", 200),
			s("request_id", c["request_id"]), s("query_source", "repl_main_thread"))),
		logRec(2*time.Second, "claude_code.api_request_body", with("api_request_body",
			s("body", `{"messages":[{"content":"`+c["prompt"]+`"}]}`))),
		logRec(3*time.Second, "claude_code.assistant_response", with("assistant_response",
			s("response", c["response"]))),
		logRec(4*time.Second, "claude_code.tool_decision", with("tool_decision",
			s("tool_name", "Bash"), s("decision", "accept"), s("source", "config"))),
		logRec(5*time.Second, "claude_code.tool_result", with("tool_result",
			s("tool_name", "Bash"), s("success", "true"), i("duration_ms", 120),
			s("tool_parameters", `{"bash_command":"cd `+c["cwd"]+` && git checkout `+c["git_branch"]+`"}`),
			s("tool_input", c["tool_input"]), s("tool_output", c["tool_output"]),
			s("vcs.ref.head.name", c["git_branch"]))),
		logRec(6*time.Second, "claude_code.tool_result", with("tool_result",
			s("tool_name", "mcp__"+c["mcp_server"]+"__lookup"), s("success", "false"), i("duration_ms", 40),
			s("error", c["error_message"]), s("error_type", "Error:ENOENT"),
			s("tool_parameters", `{"mcp_server_name":"`+c["mcp_server"]+`"}`))),
		logRec(7*time.Second, "claude_code.api_request", with("api_request",
			s("model", "claude-sonnet-4-5"), f("cost_usd", 0.02), i("duration_ms", 1100),
			i("input_tokens", 10), i("output_tokens", 20))),
		logRec(8*time.Second, "claude_code.api_error", with("api_error",
			s("model", "claude-sonnet-4-5"), s("error", c["error_message"]), i("status_code", 529),
			i("duration_ms", 30), i("attempt", 2))),
	}
	return &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource:  claudeResource(),
		ScopeLogs: []*logsv1.ScopeLogs{{Scope: &commonv1.InstrumentationScope{Name: "com.anthropic.claude_code.events"}, LogRecords: recs}},
	}}}
}

// ClaudeSessionStartMetric is the claude_code.session.count metric.
func ClaudeSessionStartMetric() *metricsv1.MetricsData {
	return &metricsv1.MetricsData{ResourceMetrics: []*metricsv1.ResourceMetrics{{
		Resource: claudeResource(),
		ScopeMetrics: []*metricsv1.ScopeMetrics{{Metrics: []*metricsv1.Metric{
			{Name: "claude_code.session.count", Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{DataPoints: []*metricsv1.NumberDataPoint{{
				TimeUnixNano: ts(0), Attributes: []*commonv1.KeyValue{s("session.id", ClaudeSessionID), s("user.email", Canaries["email"])},
				Value: &metricsv1.NumberDataPoint_AsInt{AsInt: 1},
			}}}}},
			{Name: "claude_code.lines_of_code.count", Data: &metricsv1.Metric_Sum{Sum: &metricsv1.Sum{DataPoints: []*metricsv1.NumberDataPoint{{
				TimeUnixNano: ts(time.Second), Attributes: []*commonv1.KeyValue{s("session.id", ClaudeSessionID), s("type", "added")},
				Value: &metricsv1.NumberDataPoint_AsInt{AsInt: 12},
			}}}}},
		}}},
	}}}
}

// ClaudeTraces is a beta-tracing llm_request span with content attributes.
func ClaudeTraces() *tracev1.TracesData {
	c := Canaries
	return &tracev1.TracesData{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource: claudeResource(),
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{
			TraceId: []byte("0123456789abcdef"), SpanId: []byte("01234567"),
			Name: "claude_code.llm_request", StartTimeUnixNano: ts(time.Second), EndTimeUnixNano: ts(2 * time.Second),
			Attributes: []*commonv1.KeyValue{
				s("session.id", ClaudeSessionID), s("gen_ai.response.id", c["request_id"]),
				s("new_context", c["prompt"]), s("response.model_output", c["response"]),
				s("system_prompt_preview", c["file_path"]),
			},
		}}}},
	}}}
}

// CodexSessionID is the native conversation id used by the Codex fixtures.
var CodexSessionID = Canaries["native_session"] + "-codex"

func codexCommon(event string) []*commonv1.KeyValue {
	c := Canaries
	return []*commonv1.KeyValue{
		s("event.name", event),
		s("event.timestamp", "2026-10-14T12:00:00.000Z"),
		s("conversation.id", CodexSessionID),
		s("app.version", "0.147.0"),
		s("auth_mode", "chatgpt"),
		s("originator", "codex_cli_rs"),
		s("user.account_id", c["account"]),
		s("user.email", c["email"]+"@example.com"),
		s("terminal.type", "iTerm.app"),
		s("model", "gpt-5-codex"),
		s("slug", "gpt-5-codex"),
	}
}

func codexResource() *resourcev1.Resource {
	return &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
		s("service.name", "codex_cli_rs"),
		s("service.version", "0.147.0"),
		s("host.name", Canaries["hostname"]),
		s("env", "dev"),
	}}
}

// CodexLogs is a Codex session (ChatGPT auth, so no turn_cost unless
// withTurnCost) with log_user_prompt=true and tool_result.max_bytes=65536:
// two completed responses, a shell call whose arguments and output carry
// canaries, a failed MCP call, and a 429.
func CodexLogs(withTurnCost bool) *logsv1.LogsData {
	c := Canaries
	with := func(ev string, extra ...*commonv1.KeyValue) []*commonv1.KeyValue {
		return append(codexCommon(ev), extra...)
	}
	recs := []*logsv1.LogRecord{
		logRec(0, "", with("codex.conversation_starts",
			s("provider_name", "OpenAI"), s("reasoning_effort", "medium"), s("approval_policy", "on-request"),
			s("sandbox_policy", "workspace-write"), s("mcp_servers", c["mcp_server"]+",other"))),
		logRec(time.Second, "", with("codex.user_prompt",
			i("prompt_length", 64), s("prompt", c["prompt"]+" "+c["file_path"]))),
		logRec(2*time.Second, "", with("codex.api_request",
			s("duration_ms", "800"), s("http.response.status_code", "200"), s("attempt", "1"), s("endpoint", "/responses"))),
		logRec(3*time.Second, "", with("codex.sse_event",
			s("event.kind", "response.completed"), s("input_token_count", "1000"), s("output_token_count", "300"),
			s("cached_token_count", "600"), s("cache_write_token_count", "0"), s("reasoning_token_count", "120"),
			s("tool_token_count", "1300"), s("ttft_ms", "450"))),
		logRec(4*time.Second, "", with("codex.tool_decision",
			s("tool_name", "shell"), s("call_id", c["request_id"]), s("decision", "approved"), s("source", "config"))),
		logRec(5*time.Second, "", with("codex.tool_result",
			s("tool_result_seq", "1"), s("tool_name", "shell"), s("tool_namespace", "functions"), s("call_id", c["request_id"]),
			s("duration_ms", "250"), s("success", "true"), s("output_truncated", "false"),
			s("agent_name", "default"), s("arguments", `{"command":["bash","-lc","cd `+c["cwd"]+` && cat `+c["file_path"]+` # `+c["tool_input"]+`"]}`),
			s("output", c["tool_output"]+" on branch "+c["git_branch"]), s("mcp_server", ""), s("mcp_server_origin", ""))),
		logRec(6*time.Second, "", with("codex.tool_result",
			s("tool_result_seq", "2"), s("tool_name", c["custom_tool"]), s("tool_namespace", c["mcp_server"]),
			s("call_id", c["request_id"]), s("duration_ms", "75"), s("success", "false"),
			s("arguments", c["tool_input"]), s("output", c["error_message"]),
			s("mcp_server", c["mcp_server"]), s("mcp_server_origin", "stdio"))),
		logRec(7*time.Second, "", with("codex.api_request",
			s("duration_ms", "50"), s("http.response.status_code", "429"), s("error.message", c["error_message"]), s("attempt", "1"))),
		logRec(8*time.Second, "", with("codex.sse_event",
			s("event.kind", "response.completed"), s("input_token_count", "2000"), s("output_token_count", "100"),
			s("cached_token_count", "1500"), s("cache_write_token_count", "0"), s("reasoning_token_count", "0"))),
		logRec(9*time.Second, "", with("codex.sse_event",
			s("event.kind", "response.output_text.delta"), s("duration_ms", "1"))),
		logRec(10*time.Second, "", with("codex.agent_response", s("response", c["response"]))),
	}
	if withTurnCost {
		recs = append(recs, logRec(11*time.Second, "", with("codex.turn_cost", s("usage.estimated_usd", "0.05"))))
	}
	return &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource:  codexResource(),
		ScopeLogs: []*logsv1.ScopeLogs{{Scope: &commonv1.InstrumentationScope{Name: "codex_otel"}, LogRecords: recs}},
	}}}
}

// CodexTraces carries a turn span (fallback usage), handle_responses spans
// and a tool dispatch span, plus a span event copying tool_result.
func CodexTraces() *tracev1.TracesData {
	c := Canaries
	common := func(extra ...*commonv1.KeyValue) []*commonv1.KeyValue {
		return append([]*commonv1.KeyValue{s("thread.id", CodexSessionID)}, extra...)
	}
	spans := []*tracev1.Span{
		{Name: "session_task.turn", StartTimeUnixNano: ts(time.Second), EndTimeUnixNano: ts(9 * time.Second),
			Attributes: common(s("turn.id", "1"), s("model", "gpt-5-codex"),
				i("codex.turn.token_usage.input_tokens", 3000), i("codex.turn.token_usage.cached_input_tokens", 2100),
				i("codex.turn.token_usage.cache_write_input_tokens", 0), i("codex.turn.token_usage.output_tokens", 400),
				i("codex.turn.token_usage.reasoning_output_tokens", 120))},
		{Name: "response.completed", StartTimeUnixNano: ts(3 * time.Second), EndTimeUnixNano: ts(3 * time.Second),
			Attributes: common(i("gen_ai.usage.input_tokens", 1000), s("gen_ai.response.id", c["request_id"]))},
		{Name: "shell", StartTimeUnixNano: ts(4 * time.Second), EndTimeUnixNano: ts(5 * time.Second),
			Attributes: common(s("tool_name", "shell"), s("call_id", c["request_id"])),
			Events: []*tracev1.Span_Event{{Name: "codex.tool_result", Attributes: []*commonv1.KeyValue{
				s("arguments_length", "120"), s("tool_origin", "builtin"), s("output", c["tool_output"])}}}},
	}
	return &tracev1.TracesData{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource:   codexResource(),
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: spans}},
	}}}
}
