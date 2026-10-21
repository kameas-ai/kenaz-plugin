package project_test

import (
	"math"
	"testing"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
)

type rec struct {
	client string
	kind   string
	vals   map[string]any
}

func anyVal(v *commonv1.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_IntValue:
		return x.IntValue
	case *commonv1.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonv1.AnyValue_BoolValue:
		return x.BoolValue
	}
	return nil
}

func toMap(kvs []*commonv1.KeyValue) map[string]any {
	m := map[string]any{}
	for _, kv := range kvs {
		m[kv.GetKey()] = anyVal(kv.GetValue())
	}
	return m
}

func flatten(t *testing.T, bs []fleetapi.ProjectedBatch) []rec {
	t.Helper()
	var out []rec
	for _, b := range bs {
		switch b.Signal {
		case fleetapi.SignalLogs:
			var m logsv1.LogsData
			if err := proto.Unmarshal(b.Body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rl := range m.GetResourceLogs() {
				client, _ := toMap(rl.GetResource().GetAttributes())["kameas.client.kind"].(string)
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						out = append(out, rec{client, toMap(lr.GetAttributes())["kameas.event.kind"].(string),
							toMap(lr.GetBody().GetKvlistValue().GetValues())})
					}
				}
			}
		case fleetapi.SignalTraces:
			var m tracev1.TracesData
			if err := proto.Unmarshal(b.Body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rs := range m.GetResourceSpans() {
				client, _ := toMap(rs.GetResource().GetAttributes())["kameas.client.kind"].(string)
				for _, ss := range rs.GetScopeSpans() {
					for _, sp := range ss.GetSpans() {
						out = append(out, rec{client, sp.GetName(), toMap(sp.GetAttributes())})
					}
				}
			}
		}
	}
	return out
}

func pick(rs []rec, client, kind string) []rec {
	var out []rec
	for _, r := range rs {
		if r.client == client && r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestProjectedValues pins the session math and enum mapping for the
// synthetic Claude Code and Codex sessions.
func TestProjectedValues(t *testing.T) {
	h := newHarness(t, 2)
	h.post(t, "/v1/metrics", fixtures.ClaudeSessionStartMetric(), encProto)
	h.post(t, "/v1/logs", fixtures.ClaudeLogs(), encProto)
	h.post(t, "/v1/logs", fixtures.CodexLogs(false), encProto)
	h.post(t, "/v1/traces", fixtures.CodexTraces(), encProto)
	h.end(t, canon.ClaudeCode, fixtures.ClaudeSessionID)
	h.end(t, canon.Codex, fixtures.CodexSessionID)
	rs := flatten(t, h.batches)

	cl, cx := string(canon.ClaudeCode), string(canon.Codex)

	// Exactly one start and one end per session.
	for _, c := range []string{cl, cx} {
		if n := len(pick(rs, c, fleetschema.KindConversationStarted)); n != 1 {
			t.Errorf("%s: %d starts", c, n)
		}
		if n := len(pick(rs, c, fleetschema.KindConversationEnded)); n != 1 {
			t.Errorf("%s: %d ends", c, n)
		}
	}
	if p := pick(rs, cx, fleetschema.KindConversationStarted)[0].vals["model_provider"]; p != "openai" {
		t.Errorf("codex provider = %v (OpenAI should normalize to openai)", p)
	}

	ce := pick(rs, cl, fleetschema.KindConversationEnded)[0].vals
	// token_in = input + cache read + cache creation over both requests.
	if ce["token_in"] != int64(100+1000+200+10) || ce["token_out"] != int64(70) {
		t.Errorf("claude tokens = %v/%v", ce["token_in"], ce["token_out"])
	}
	if !near(ce["cost_usd"].(float64), 0.03) || ce["cost_known"] != true {
		t.Errorf("claude cost = %v known=%v", ce["cost_usd"], ce["cost_known"])
	}
	if ce["duration_ms"] != int64(8000) {
		t.Errorf("claude duration = %v", ce["duration_ms"])
	}

	xe := pick(rs, cx, fleetschema.KindConversationEnded)[0].vals
	// sse usage only (the turn-span fallback must be ignored): 1000 + 2000.
	if xe["token_in"] != int64(3000) || xe["token_out"] != int64(400) {
		t.Errorf("codex tokens = %v/%v", xe["token_in"], xe["token_out"])
	}
	// ChatGPT auth: no turn_cost and the relay never estimates one, so the
	// cost is sent as unknown.
	if xe["cost_usd"].(float64) != 0 || xe["cost_known"] != false {
		t.Errorf("codex cost = %v known=%v, want unknown", xe["cost_usd"], xe["cost_known"])
	}
	if n := len(pick(rs, cx, fleetschema.SpanGenAIChat)); n != 2 {
		t.Errorf("codex gen_ai.chat spans = %d, want 2 (fallback must not double count)", n)
	}

	tools := map[string][]string{}
	for _, c := range []string{cl, cx} {
		for _, r := range pick(rs, c, fleetschema.KindToolInvoked) {
			tools[c] = append(tools[c], r.vals["tool_name"].(string))
		}
	}
	if got := tools[cl]; len(got) != 2 || got[0] != "Bash" || got[1] != "mcp" {
		t.Errorf("claude tools = %v", got)
	}
	if got := tools[cx]; len(got) != 2 || got[0] != "shell" || got[1] != "mcp" {
		t.Errorf("codex tools = %v", got)
	}

	cats := map[string][]string{}
	for _, c := range []string{cl, cx} {
		for _, r := range pick(rs, c, fleetschema.KindError) {
			cats[c] = append(cats[c], r.vals["category"].(string))
		}
	}
	if got := cats[cl]; len(got) != 2 || got[0] != "tool_failure" || got[1] != "overloaded" {
		t.Errorf("claude errors = %v", got)
	}
	if got := cats[cx]; len(got) != 2 || got[0] != "tool_failure" || got[1] != "rate_limited" {
		t.Errorf("codex errors = %v", got)
	}

	span := pick(rs, cl, fleetschema.SpanGenAIChat)[0].vals
	if span["gen_ai.request.model"] != "claude-sonnet-4-5" || span["gen_ai.usage.cache_read.input_tokens"] != int64(1000) ||
		!near(span["kenaz.llm.cost_usd"].(float64), 0.01) || span["gen_ai.provider.name"] != "anthropic" {
		t.Errorf("claude span = %v", span)
	}
	xs := pick(rs, cx, fleetschema.SpanGenAIChat)[0].vals
	if xs["kenaz.llm.ttft_ms"] != int64(450) || xs["kenaz.llm.reasoning_tokens"] != int64(120) ||
		xs["gen_ai.usage.input_tokens"] != int64(400) || xs["gen_ai.usage.cache_read.input_tokens"] != int64(600) {
		t.Errorf("codex span = %v", xs)
	}
	if span["kenaz.session.hash"] != ce["conversation_id"] {
		t.Error("span session hash does not match conversation_id")
	}
}

// TestCodexTurnCostAndFallback: turn_cost is used when reported, and turn
// spans are used only when no sse usage arrived.
func TestCodexTurnCostAndFallback(t *testing.T) {
	h := newHarness(t, 2)
	h.post(t, "/v1/logs", fixtures.CodexLogs(true), encProto)
	h.end(t, canon.Codex, fixtures.CodexSessionID)
	xe := pick(flatten(t, h.batches), string(canon.Codex), fleetschema.KindConversationEnded)[0].vals
	if !near(xe["cost_usd"].(float64), 0.05) || xe["cost_known"] != true {
		t.Errorf("turn_cost not used: %v", xe)
	}

	h = newHarness(t, 2)
	h.post(t, "/v1/traces", fixtures.CodexTraces(), encProto) // traces only: fallback
	h.end(t, canon.Codex, fixtures.CodexSessionID)
	rs := flatten(t, h.batches)
	xe = pick(rs, string(canon.Codex), fleetschema.KindConversationEnded)[0].vals
	if xe["token_in"] != int64(3000) || xe["token_out"] != int64(400) {
		t.Errorf("fallback tokens = %v/%v", xe["token_in"], xe["token_out"])
	}
	if n := len(pick(rs, string(canon.Codex), fleetschema.SpanGenAIChat)); n != 1 {
		t.Errorf("fallback spans = %d, want 1", n)
	}
}

// TestSchemaRev1OmitsV2Fields: against an old Fleet, cost_known and
// kameas.client.* are never sent.
func TestSchemaRev1OmitsV2Fields(t *testing.T) {
	h := newHarness(t, 1)
	h.post(t, "/v1/logs", fixtures.ClaudeLogs(), encProto)
	h.end(t, canon.ClaudeCode, fixtures.ClaudeSessionID)
	for _, r := range flatten(t, h.batches) {
		if r.client != "" {
			t.Fatalf("client kind sent at rev 1")
		}
		if _, ok := r.vals[fleetschema.CostKnownKey]; ok {
			t.Fatalf("cost_known sent at rev 1")
		}
	}
}
