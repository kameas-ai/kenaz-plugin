package project_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/otlprx"
	"github.com/kameas-ai/kenaz-plugin/internal/pipeline"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

type allowAll struct{}

func (allowAll) SignedIn() bool                    { return true }
func (allowAll) HoldReason() string                { return "" }
func (allowAll) TierAllows(fleetschema.Class) bool { return true }
func (allowAll) OptedIn(fleetschema.Class) bool    { return true }

var testIdentity = project.Identity{UserID: "user-123", OrgID: "org-456", MachineID: "node-789", Tier: "pro", ServiceVersion: "0.1.0"}

type harness struct {
	rx      *otlprx.Receiver
	p       *pipeline.Pipeline
	batches []fleetapi.ProjectedBatch
}

func newHarness(t *testing.T, schemaRev int) *harness {
	t.Helper()
	h := &harness{}
	agg := session.New(session.Config{Key: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return fixtures.Base }})
	h.p = pipeline.New(pipeline.Config{
		Aggregator: agg,
		Decider:    allowAll{},
		Options:    func() project.Options { return project.Options{SchemaRev: schemaRev} },
		Identity:   func() (project.Identity, bool) { return testIdentity, true },
		Emit:       func(b []fleetapi.ProjectedBatch, _ time.Time) { h.batches = append(h.batches, b...) },
	})
	h.rx = otlprx.New(otlprx.Config{Sink: h.p, SessionEndToken: "tok"})
	return h
}

type encoding int

const (
	encProto encoding = iota
	encJSON
	encGzipProto
)

func (h *harness) post(t *testing.T, path string, m proto.Message, enc encoding) {
	t.Helper()
	var body []byte
	var err error
	ct := "application/x-protobuf"
	switch enc {
	case encJSON:
		ct = "application/json"
		body, err = protojson.Marshal(m)
	default:
		body, err = proto.Marshal(m)
	}
	if err != nil {
		t.Fatal(err)
	}
	if enc == encGzipProto {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(body)
		zw.Close()
		body = buf.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", ct)
	if enc == encGzipProto {
		req.Header.Set("Content-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	h.rx.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
}

func (h *harness) end(t *testing.T, kind canon.ClientKind, id string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, otlprx.SessionEndPath,
		strings.NewReader(`{"client_kind":"`+string(kind)+`","session_id":"`+id+`"}`))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set(otlprx.SessionEndHeader, "tok")
	w := httptest.NewRecorder()
	h.rx.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("session-end: %d", w.Code)
	}
}

// TestCanary is the canary check over synthetic fixtures with every harness
// content flag on: no canary may appear in any byte handed to the
// transport, and every emitted key must be in the public allowlist.
func TestCanary(t *testing.T) {
	for _, rev := range []int{1, 2} {
		for _, enc := range []encoding{encProto, encJSON, encGzipProto} {
			h := newHarness(t, rev)
			h.post(t, "/v1/metrics", fixtures.ClaudeSessionStartMetric(), enc)
			h.post(t, "/v1/logs", fixtures.ClaudeLogs(), enc)
			h.post(t, "/v1/traces", fixtures.ClaudeTraces(), enc)
			h.post(t, "/v1/logs", fixtures.CodexLogs(false), enc)
			h.post(t, "/v1/traces", fixtures.CodexTraces(), enc)
			h.end(t, canon.ClaudeCode, fixtures.ClaudeSessionID)
			h.end(t, canon.Codex, fixtures.CodexSessionID)

			if len(h.batches) == 0 {
				t.Fatalf("rev %d enc %d: nothing emitted", rev, enc)
			}
			kinds := map[string]int{}
			for _, b := range h.batches {
				for name, c := range fixtures.Canaries {
					if bytes.Contains(b.Body, []byte(c)) {
						t.Errorf("rev %d enc %d: canary %q leaked in %s batch", rev, enc, name, b.Signal)
					}
				}
				for _, bad := range []string{"example.com", "forged", "iTerm", "codex_cli_rs", "Error:ENOENT", "repl_main_thread"} {
					if bytes.Contains(b.Body, []byte(bad)) {
						t.Errorf("rev %d enc %d: source value %q leaked", rev, enc, bad)
					}
				}
				checkAllowlisted(t, b, rev, kinds)
			}
			for _, k := range []string{fleetschema.KindConversationStarted, fleetschema.KindConversationEnded,
				fleetschema.KindToolInvoked, fleetschema.KindError, fleetschema.SpanGenAIChat} {
				if kinds[k] == 0 {
					t.Errorf("rev %d enc %d: no %s emitted", rev, enc, k)
				}
			}
		}
	}
}

func keysOf(kvs []*commonv1.KeyValue) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.GetKey()
	}
	return out
}

func in(set []string, k string) bool {
	for _, s := range set {
		if s == k {
			return true
		}
	}
	return false
}

func checkAllowlisted(t *testing.T, b fleetapi.ProjectedBatch, rev int, kinds map[string]int) {
	t.Helper()
	al := allowlist.Telemetry()
	checkResource := func(kvs []*commonv1.KeyValue) {
		for _, kv := range kvs {
			if !in(al.ResourceAttributes, kv.GetKey()) {
				t.Errorf("resource attr %q not allowlisted", kv.GetKey())
			}
			if rev < fleetschema.SchemaRevCostKnown && strings.HasPrefix(kv.GetKey(), "kameas.client.") {
				t.Errorf("rev %d: %s sent to an old Fleet", rev, kv.GetKey())
			}
			switch kv.GetKey() {
			case "kameas.user.id":
				if kv.GetValue().GetStringValue() != testIdentity.UserID {
					t.Errorf("identity not stamped by relay: %v", kv.GetValue())
				}
			case "kameas.org.id":
				if kv.GetValue().GetStringValue() != testIdentity.OrgID {
					t.Errorf("org not stamped by relay: %v", kv.GetValue())
				}
			}
		}
	}
	switch b.Signal {
	case fleetapi.SignalLogs:
		var m logsv1.LogsData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.GetResourceLogs() {
			checkResource(rl.GetResource().GetAttributes())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					var kind string
					for _, k := range keysOf(lr.GetAttributes()) {
						if !in(al.LogAttributes, k) {
							t.Errorf("log attr %q not allowlisted", k)
						}
					}
					kind = lr.GetAttributes()[0].GetValue().GetStringValue()
					kinds[kind]++
					for _, k := range keysOf(lr.GetBody().GetKvlistValue().GetValues()) {
						if !in(al.BodyKeys[kind], k) {
							t.Errorf("%s body key %q not allowlisted", kind, k)
						}
						if rev < fleetschema.SchemaRevCostKnown && k == fleetschema.CostKnownKey {
							t.Errorf("cost_known sent at rev %d", rev)
						}
					}
				}
			}
		}
	case fleetapi.SignalTraces:
		var m tracev1.TracesData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.GetResourceSpans() {
			checkResource(rs.GetResource().GetAttributes())
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					kinds[sp.GetName()]++
					allowed, ok := al.SpanAttributes[sp.GetName()]
					if !ok {
						t.Errorf("span %q not allowlisted", sp.GetName())
					}
					for _, k := range keysOf(sp.GetAttributes()) {
						if !in(allowed, k) {
							t.Errorf("span %s attr %q not allowlisted", sp.GetName(), k)
						}
					}
					if len(sp.GetEvents()) > 0 || len(sp.GetLinks()) > 0 || sp.GetStatus().GetMessage() != "" {
						t.Errorf("span carries events/links/status message")
					}
				}
			}
		}
	default:
		t.Errorf("unexpected signal %q", b.Signal)
	}
}
