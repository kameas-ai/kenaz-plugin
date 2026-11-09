package project_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/otlprx"
	"github.com/kameas-ai/kenaz-plugin/internal/pipeline"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

// testdata/codex-0.166-exec.json is a sanitized recording of one real
// `codex exec` run (codex-cli 0.166.0-alpha.5.1): two shell commands and one
// MCP tool call. The recorder kept attribute names and types, record shape and
// timing. String values survive only for non-content metadata (event names,
// model, tool names); ids became aliases, and everything else is the literal
// REDACTED, including prompt, arguments, output, user.email and host.name.
// Repeated spans are thinned to one per shape and one per OS thread number.
//
// What makes it worth keeping over the synthetic fixtures:
//   - log records set the OTLP event_name field to a tracing callsite
//     ("event otel/src/...:NNN"); the real name is the event.name attribute
//   - most spans carry an integer thread.id, the OS thread number

type recordedAttr struct {
	K string   `json:"k"`
	S *string  `json:"s"`
	I *int64   `json:"i"`
	D *float64 `json:"d"`
	B *bool    `json:"b"`
}

type recordedRecord struct {
	Signal   string         `json:"signal"`
	Name     string         `json:"name"`
	StartMS  int64          `json:"start_ms"`
	EndMS    int64          `json:"end_ms"`
	Attrs    []recordedAttr `json:"attrs"`
	Resource []recordedAttr `json:"resource"`
}

func (r recordedRecord) skipped(names []string) bool {
	for _, a := range r.Attrs {
		if a.K != "event.name" || a.S == nil {
			continue
		}
		for _, n := range names {
			if *a.S == n {
				return true
			}
		}
	}
	return false
}

const recordedRedacted = "REDACTED"

// recordedCanary replaces every REDACTED value, so the replay also proves
// that none of those attributes can reach an upload.
const recordedCanary = "CANARYrecordedcodexcontent000001"

func recordedKVs(in []recordedAttr) []*commonv1.KeyValue {
	out := make([]*commonv1.KeyValue, 0, len(in))
	for _, a := range in {
		v := &commonv1.AnyValue{}
		switch {
		case a.S != nil:
			s := *a.S
			if s == recordedRedacted {
				s = recordedCanary
			}
			v.Value = &commonv1.AnyValue_StringValue{StringValue: s}
		case a.I != nil:
			v.Value = &commonv1.AnyValue_IntValue{IntValue: *a.I}
		case a.D != nil:
			v.Value = &commonv1.AnyValue_DoubleValue{DoubleValue: *a.D}
		case a.B != nil:
			v.Value = &commonv1.AnyValue_BoolValue{BoolValue: *a.B}
		}
		out = append(out, &commonv1.KeyValue{Key: a.K, Value: v})
	}
	return out
}

type recordedHarness struct {
	t       *testing.T
	rx      *otlprx.Receiver
	p       *pipeline.Pipeline
	now     time.Time
	batches []fleetapi.ProjectedBatch
}

func newRecordedHarness(t *testing.T) *recordedHarness {
	t.Helper()
	h := &recordedHarness{t: t, now: fixtures.Base}
	h.p = pipeline.New(pipeline.Config{
		Aggregator: session.New(session.Config{Key: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return h.now }}),
		Decider:    allowAll{},
		Options:    func() project.Options { return project.Options{SchemaRev: fleetschema.SchemaRevCostKnown} },
		Identity:   func() (project.Identity, bool) { return testIdentity, true },
		Emit: func(b []fleetapi.ProjectedBatch, _ time.Time, _ project.Identity) {
			h.batches = append(h.batches, b...)
		},
	})
	h.rx = otlprx.New(otlprx.Config{Sink: h.p})
	return h
}

func (h *recordedHarness) post(path string, m proto.Message) {
	h.t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		h.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	h.rx.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		h.t.Fatalf("POST %s: %d", path, w.Code)
	}
}

// replay sends the recording over the receiver's real protobuf path, except
// log records whose event.name is in skip.
func (h *recordedHarness) replay(skip ...string) {
	h.t.Helper()
	raw, err := os.ReadFile("testdata/codex-0.166-exec.json")
	if err != nil {
		h.t.Fatal(err)
	}
	var rec struct {
		Records []recordedRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		h.t.Fatal(err)
	}
	at := func(ms int64) uint64 {
		return uint64(fixtures.Base.Add(time.Duration(ms) * time.Millisecond).UnixNano())
	}
	for _, r := range rec.Records {
		if r.skipped(skip) {
			continue
		}
		res := &resourcev1.Resource{Attributes: recordedKVs(r.Resource)}
		switch r.Signal {
		case "log":
			h.post("/v1/logs", &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{
				Resource: res,
				ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: []*logsv1.LogRecord{{
					EventName: r.Name, TimeUnixNano: at(r.StartMS), Attributes: recordedKVs(r.Attrs),
				}}}},
			}}})
		case "span":
			h.post("/v1/traces", &tracev1.TracesData{ResourceSpans: []*tracev1.ResourceSpans{{
				Resource: res,
				ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{
					Name: r.Name, StartTimeUnixNano: at(r.StartMS), EndTimeUnixNano: at(r.EndMS), Attributes: recordedKVs(r.Attrs),
				}}}},
			}}})
		default:
			h.t.Fatalf("unknown signal %q", r.Signal)
		}
	}
}

// ended returns the int body fields of the only conversation_ended record.
func (h *recordedHarness) ended() map[string]int64 {
	h.t.Helper()
	var out map[string]int64
	for _, b := range h.batches {
		if b.Signal != fleetapi.SignalLogs {
			continue
		}
		var m logsv1.LogsData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			h.t.Fatal(err)
		}
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					if lr.GetAttributes()[0].GetValue().GetStringValue() != fleetschema.KindConversationEnded {
						continue
					}
					if out != nil {
						h.t.Fatal("more than one conversation_ended")
					}
					out = map[string]int64{}
					for _, kv := range lr.GetBody().GetKvlistValue().GetValues() {
						out[kv.GetKey()] = kv.GetValue().GetIntValue()
					}
				}
			}
		}
	}
	return out
}

func (h *recordedHarness) checkUploads() map[string]int {
	h.t.Helper()
	kinds := map[string]int{}
	for _, b := range h.batches {
		if bytes.Contains(b.Body, []byte(recordedCanary)) {
			h.t.Errorf("redacted source value leaked in %s batch", b.Signal)
		}
		for _, bad := range []string{"fleet_status", "mcp__kenaz", "codex_exec"} {
			if bytes.Contains(b.Body, []byte(bad)) {
				h.t.Errorf("source value %q leaked in %s batch", bad, b.Signal)
			}
		}
		checkAllowlisted(h.t, b, fleetschema.SchemaRevCostKnown, kinds)
	}
	return kinds
}

// TestRecordedCodexRun replays a real Codex recording end to end: one session,
// its tool calls, per-call usage from sse_event, and an idle close that carries
// the totals. The relay must not read anything else.
func TestRecordedCodexRun(t *testing.T) {
	h := newRecordedHarness(t)
	h.replay()

	before := h.checkUploads()
	if before[fleetschema.KindConversationEnded] != 0 {
		t.Fatalf("closed before the idle timeout: %v", before)
	}

	h.now = fixtures.Base.Add(session.DefaultIdle + time.Minute)
	h.p.Tick()

	kinds := h.checkUploads()
	want := map[string]int{
		fleetschema.KindConversationStarted: 1,
		fleetschema.KindConversationEnded:   1,
		// 3 exec_command, 2 exec (code mode), 1 kenaz MCP tool.
		fleetschema.KindToolInvoked: 6,
		// 4 response.completed; the turn-span fallback must not add to them.
		fleetschema.SpanGenAIChat: 4,
	}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s = %d, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	e := h.ended()
	if e["token_in"] != 60724 || e["token_out"] != 179 {
		t.Errorf("ended totals = in %d out %d, want in 60724 out 179", e["token_in"], e["token_out"])
	}
	if e["duration_ms"] != recordedDurationMS {
		t.Errorf("duration_ms = %d, want %d", e["duration_ms"], recordedDurationMS)
	}
}

// recordedDurationMS is the recorded session's length, first record to last
// session-bearing one. It needs the spans that carry thread.id twice.
const recordedDurationMS = 10426

// Without sse_event the turn span is the only usage source: its totals must
// reach Fleet as one model call. session_task.turn carries thread.id twice,
// the integer first.
func TestRecordedCodexRunFallsBackToTurnSpan(t *testing.T) {
	h := newRecordedHarness(t)
	h.replay("codex.sse_event")
	h.now = fixtures.Base.Add(session.DefaultIdle + time.Minute)
	h.p.Tick()

	kinds := h.checkUploads()
	if kinds[fleetschema.SpanGenAIChat] != 1 {
		t.Errorf("gen_ai.chat = %d, want 1 (all: %v)", kinds[fleetschema.SpanGenAIChat], kinds)
	}
	e := h.ended()
	if e["token_in"] != 60722 || e["token_out"] != 179 {
		t.Errorf("ended totals = in %d out %d, want in 60722 out 179", e["token_in"], e["token_out"])
	}
}

// A relay stopping before the idle timeout still reports the session.
func TestRecordedCodexRunFlushedOnStop(t *testing.T) {
	h := newRecordedHarness(t)
	h.replay()
	if h.p.OpenSessions() != 1 {
		t.Fatalf("open sessions = %d, want 1", h.p.OpenSessions())
	}
	h.p.Flush()
	if h.p.OpenSessions() != 0 {
		t.Fatalf("open sessions after flush = %d", h.p.OpenSessions())
	}
	if e := h.ended(); e["token_in"] != 60724 || e["token_out"] != 179 {
		t.Errorf("ended totals = %v", e)
	}
}
