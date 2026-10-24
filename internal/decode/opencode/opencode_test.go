package opencode

import (
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/attr"
)

func s(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func i(k string, v int64) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: v}}}
}

func b(k string, v bool) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: v}}}
}

var at = time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)

func TestModelCall(t *testing.T) {
	a := attr.Of([]*commonv1.KeyValue{
		s("session.id", "ses_1"), s("call.id", "msg_1"), s("model", "mock-1"), s("provider", "Mock"),
		i("input_tokens", 1000), i("output_tokens", 30), i("reasoning_tokens", 7),
		i("cache_read_tokens", 200), i("cache_write_tokens", 3), i("duration_ms", 1500),
		// Content an adapter must never send; the decoder must not read it.
		s("prompt", "CANARY"), s("output", "CANARY"),
	})
	evs := LogRecord(a, EventModelCall, at)
	if len(evs) != 2 || evs[0].Kind != canon.KindSessionSeen || evs[1].Kind != canon.KindModelCall {
		t.Fatalf("got %+v", evs)
	}
	e := evs[1]
	if e.Client != canon.OpenCode || e.Model != "mock-1" || e.Provider != "mock" || e.InputTokens != 1000 ||
		e.OutputTokens != 30 || e.Reasoning != 7 || e.CacheRead != 200 || e.CacheCreation != 3 ||
		e.Duration != 1500*time.Millisecond || e.DedupKey != "m:msg_1" {
		t.Fatalf("bad model call %+v", e)
	}
	if e.CostKnown {
		t.Fatal("no cost_usd attribute must mean cost unknown")
	}
	if evs[0].Provider != "mock" {
		t.Fatal("provider should reach the session")
	}
}

func TestToolResultAndMCP(t *testing.T) {
	for _, c := range []struct {
		name string
		mcp  bool
	}{{"read", false}, {"apply_patch", false}, {"github_create_issue", true}, {"custom", false}} {
		a := attr.Of([]*commonv1.KeyValue{s("session.id", "ses_1"), s("call.id", "call_1"), s("tool_name", c.name), b("success", false), i("duration_ms", 25)})
		evs := LogRecord(a, EventToolResult, at)
		e := evs[len(evs)-1]
		if e.Kind != canon.KindToolCall || e.ToolIsMCP != c.mcp || e.Success || e.Duration != 25*time.Millisecond || e.DedupKey != "t:call_1" {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
}

func TestSessionEndAndErrors(t *testing.T) {
	evs := LogRecord(attr.Of([]*commonv1.KeyValue{s("session.id", "ses_1")}), EventSessionEnd, at)
	if len(evs) != 1 || evs[0].Kind != canon.KindSessionEnd {
		t.Fatalf("end: %+v", evs)
	}
	evs = LogRecord(attr.Of([]*commonv1.KeyValue{s("session.id", "ses_1"), i("status_code", 429)}), EventAPIError, at)
	if evs[1].Kind != canon.KindAPIError || evs[1].StatusCode != 429 {
		t.Fatalf("error: %+v", evs)
	}
	evs = LogRecord(attr.Of([]*commonv1.KeyValue{s("session.id", "ses_1"), i("status_code", 99999)}), EventAPIError, at)
	if evs[1].StatusCode != 0 {
		t.Fatalf("out-of-range status kept: %+v", evs)
	}
	if LogRecord(attr.Of(nil), EventModelCall, at) != nil {
		t.Fatal("records without a session id must be dropped")
	}
	long := make([]byte, 200)
	for j := range long {
		long[j] = 'x'
	}
	evs = LogRecord(attr.Of([]*commonv1.KeyValue{s("session.id", "s"), s("call.id", string(long))}), EventModelCall, at)
	if evs[1].DedupKey != "" {
		t.Fatal("oversized call id must not be used")
	}
}
