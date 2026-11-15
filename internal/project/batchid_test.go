package project_test

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
)

var batchIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// resources returns the resource groups of an encoded request.
func resources(t *testing.T, b fleetapi.ProjectedBatch) []*resourcev1.Resource {
	t.Helper()
	var out []*resourcev1.Resource
	switch b.Signal {
	case fleetapi.SignalLogs:
		var m logsv1.LogsData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.GetResourceLogs() {
			out = append(out, rl.GetResource())
		}
	case fleetapi.SignalTraces:
		var m tracev1.TracesData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.GetResourceSpans() {
			out = append(out, rs.GetResource())
		}
	default:
		t.Fatalf("unexpected signal %q", b.Signal)
	}
	if len(out) == 0 {
		t.Fatalf("%s request has no resource group", b.Signal)
	}
	return out
}

// batchID checks that every resource group of the request carries exactly
// one well-formed kameas.batch.id, that the request has one id in all, and
// returns it.
func batchID(t *testing.T, b fleetapi.ProjectedBatch) string {
	t.Helper()
	id := ""
	for i, r := range resources(t, b) {
		var vals []string
		for _, kv := range r.GetAttributes() {
			if kv.GetKey() == fleetschema.BatchIDAttr {
				vals = append(vals, kv.GetValue().GetStringValue())
			}
		}
		if len(vals) != 1 {
			t.Fatalf("%s resource %d: %d %s attributes, want 1", b.Signal, i, len(vals), fleetschema.BatchIDAttr)
		}
		if !batchIDRe.MatchString(vals[0]) {
			t.Fatalf("%s resource %d: batch id %q is not 32 lowercase hex characters", b.Signal, i, vals[0])
		}
		if vals[0] == "00000000000000000000000000000000" {
			t.Fatalf("%s resource %d: all-zero batch id", b.Signal, i)
		}
		if i > 0 && vals[0] != id {
			t.Fatalf("%s request carries two batch ids (%s, %s)", b.Signal, id, vals[0])
		}
		id = vals[0]
	}
	return id
}

// withoutBatchID re-serializes a request with the batch id removed, so two
// requests can be compared on everything else.
func withoutBatchID(t *testing.T, b fleetapi.ProjectedBatch) []byte {
	t.Helper()
	strip := func(r *resourcev1.Resource) {
		kept := r.Attributes[:0]
		for _, kv := range r.Attributes {
			if kv.GetKey() != fleetschema.BatchIDAttr {
				kept = append(kept, kv)
			}
		}
		r.Attributes = kept
	}
	var m proto.Message
	switch b.Signal {
	case fleetapi.SignalLogs:
		d := &logsv1.LogsData{}
		if err := proto.Unmarshal(b.Body, d); err != nil {
			t.Fatal(err)
		}
		for _, rl := range d.GetResourceLogs() {
			strip(rl.GetResource())
		}
		m = d
	default:
		t.Fatalf("withoutBatchID: signal %q", b.Signal)
	}
	out, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func toolRecord(client canon.ClientKind) project.Record {
	return project.Record{
		Class: fleetschema.KindClass[fleetschema.KindToolInvoked], Client: client,
		Time: fixtures.Base, EventKind: fleetschema.KindToolInvoked,
		Body: []project.Field{
			{Key: "tool_name", Type: project.TString, Str: "builtin.shell"},
			{Key: "latency_ms", Type: project.TInt, Int: 12},
			{Key: "success", Type: project.TBool, Bool: true},
		},
	}
}

func chatSpanRecord(client canon.ClientKind) project.Record {
	return project.Record{
		Class: fleetschema.ClassModelCallTelemetry, Client: client,
		Start: fixtures.Base, Time: fixtures.Base.Add(time.Second), SpanName: fleetschema.SpanGenAIChat,
		Attrs: []project.Field{{Key: "gen_ai.operation.name", Type: project.TString, Str: "chat"}},
	}
}

// Every encoded logs and traces request carries one kameas.batch.id, at
// every schema revision, on every resource group, and still passes the
// public allowlist.
func TestBatchIDOnEveryRequest(t *testing.T) {
	recs := []project.Record{
		toolRecord(canon.ClaudeCode), toolRecord(canon.Codex),
		chatSpanRecord(canon.ClaudeCode), chatSpanRecord(canon.Codex),
	}
	al := allowlist.Telemetry()
	for _, rev := range []int{0, 1, 2} {
		bs, err := project.Encode(recs, testIdentity, project.Options{SchemaRev: rev})
		if err != nil {
			t.Fatal(err)
		}
		signals := map[fleetapi.Signal]int{}
		for _, b := range bs {
			signals[b.Signal]++
			batchID(t, b)
			if err := al.Check(b); err != nil {
				t.Errorf("rev %d: %s request fails the allowlist: %v", rev, b.Signal, err)
			}
		}
		if signals[fleetapi.SignalLogs] != 1 || signals[fleetapi.SignalTraces] != 1 {
			t.Fatalf("rev %d: requests = %v, want one logs and one traces", rev, signals)
		}
	}
}

// The same holds for what the whole pipeline hands to the transport, and
// no two requests share an id.
func TestBatchIDThroughPipeline(t *testing.T) {
	h := newHarness(t, 2)
	h.post(t, "/v1/logs", fixtures.ClaudeLogs(), encProto)
	h.post(t, "/v1/traces", fixtures.ClaudeTraces(), encProto)
	h.post(t, "/v1/logs", fixtures.CodexLogs(false), encProto)
	h.post(t, "/v1/traces", fixtures.CodexTraces(), encProto)
	h.end(t, canon.ClaudeCode, fixtures.ClaudeSessionID)
	h.end(t, canon.Codex, fixtures.CodexSessionID)
	seen := map[string]bool{}
	signals := map[fleetapi.Signal]int{}
	for _, b := range h.batches {
		signals[b.Signal]++
		id := batchID(t, b)
		if seen[id] {
			t.Errorf("batch id %s used by two requests", id)
		}
		seen[id] = true
	}
	if signals[fleetapi.SignalLogs] == 0 || signals[fleetapi.SignalTraces] == 0 {
		t.Fatalf("requests = %v, want logs and traces", signals)
	}
}

// Two distinct batches of identical records (two agent processes that each
// export the same tool call at the same instant) must not share bytes, or
// Fleet's dedup on the request body would drop the second as a replay. The
// batch id is the only difference.
func TestIdenticalRecordsEncodeToDifferentBodies(t *testing.T) {
	recs := []project.Record{toolRecord(canon.ClaudeCode)}
	opt := project.Options{SchemaRev: 2}
	first, err := project.Encode(recs, testIdentity, opt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := project.Encode(recs, testIdentity, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("requests = %d, %d, want 1 each", len(first), len(second))
	}
	if bytes.Equal(first[0].Body, second[0].Body) {
		t.Fatal("two encodes of identical records produced identical bodies")
	}
	if a, b := batchID(t, first[0]), batchID(t, second[0]); a == b {
		t.Fatalf("two encodes share batch id %s", a)
	}
	if !bytes.Equal(withoutBatchID(t, first[0]), withoutBatchID(t, second[0])) {
		t.Fatal("the two bodies differ in more than the batch id")
	}
}

// When Encode splits records into several requests, each request gets its
// own id.
func TestBatchIDPerChunk(t *testing.T) {
	var recs []project.Record
	for i := 0; i < 2*fleetschema.MaxRecordsPerRequest+1; i++ {
		recs = append(recs, toolRecord(canon.ClaudeCode))
	}
	for i := 0; i < fleetschema.MaxRecordsPerRequest+1; i++ {
		recs = append(recs, chatSpanRecord(canon.ClaudeCode))
	}
	bs, err := project.Encode(recs, testIdentity, project.Options{SchemaRev: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 5 {
		t.Fatalf("requests = %d, want 3 logs + 2 traces", len(bs))
	}
	seen := map[string]bool{}
	for _, b := range bs {
		seen[batchID(t, b)] = true
	}
	if len(seen) != len(bs) {
		t.Fatalf("%d distinct batch ids over %d requests", len(seen), len(bs))
	}
	// The first two logs chunks hold identical records: only the id keeps
	// their bodies apart.
	if bytes.Equal(bs[0].Body, bs[1].Body) {
		t.Fatal("two full chunks of identical records share bytes")
	}
}
