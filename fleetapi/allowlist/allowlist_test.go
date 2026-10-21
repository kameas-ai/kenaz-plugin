package allowlist

import (
	"strings"
	"testing"
)

func TestTelemetryClosedSet(t *testing.T) {
	al := Telemetry()
	want := map[string]string{
		"harness.conversation_started": "conversation_id,model_provider",
		"harness.conversation_ended":   "conversation_id,cost_known,cost_usd,duration_ms,token_in,token_out",
		"harness.tool_invoked":         "latency_ms,success,tool_name",
		"harness.error":                "category,recoverable",
	}
	if len(al.BodyKeys) != len(want) {
		t.Fatalf("kinds = %v", al.BodyKeys)
	}
	for kind, keys := range want {
		if got := strings.Join(al.BodyKeys[kind], ","); got != keys {
			t.Errorf("%s = %s, want %s", kind, got, keys)
		}
	}
	if len(al.SpanAttributes) != 1 || len(al.SpanAttributes["gen_ai.chat"]) == 0 {
		t.Fatalf("spans = %v", al.SpanAttributes)
	}
	for _, k := range al.SpanAttributes["gen_ai.chat"] {
		if k == "gen_ai.response.id" {
			t.Fatal("gen_ai.response.id must never be allowlisted")
		}
	}
	if strings.Join(al.LogAttributes, ",") != "kameas.event.kind" {
		t.Fatalf("log attrs = %v", al.LogAttributes)
	}
}
