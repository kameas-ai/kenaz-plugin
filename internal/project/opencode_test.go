package project_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The OpenCode adapter's OTLP output for two sessions recorded from
// OpenCode 1.19.4 (opencode/test/fixtures). The adapter's own tests prove
// it produces exactly these files from the recorded events.
var openCodeExports = []string{
	"../../opencode/test/fixtures/otlp-opencode-1.19.4-tool-session.json",
	"../../opencode/test/fixtures/otlp-opencode-1.19.4-api-error.json",
}

// Values that reached the adapter's input or the relay but must never
// reach Fleet: native session and call ids, the planted content canaries
// (which the adapter already strips) and the test provider's id.
var openCodeNeverSent = []string{
	"ses_e96b8a7a8ffeOSUxZPR2dkM75o", "ses_e96b84b7bffep7oQukPvSsAYSJ",
	"call_CANARY1", "msg_1694760df001YW0vEm1ruNDQ1c",
	"CANARYprompt", "CANARYfilecontent", "CANARYresponse", "CANARYerr", "CANARYpathdir",
}

func loadOpenCode(t *testing.T, path string) *logsv1.LogsData {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m logsv1.LogsData
	if err := protojson.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &m
}

func resourceValue(t *testing.T, b fleetapi.ProjectedBatch, key string) []string {
	t.Helper()
	var res [][]*commonv1.KeyValue
	switch b.Signal {
	case fleetapi.SignalLogs:
		var m logsv1.LogsData
		proto.Unmarshal(b.Body, &m)
		for _, rl := range m.GetResourceLogs() {
			res = append(res, rl.GetResource().GetAttributes())
		}
	case fleetapi.SignalTraces:
		var m tracev1.TracesData
		proto.Unmarshal(b.Body, &m)
		for _, rs := range m.GetResourceSpans() {
			res = append(res, rs.GetResource().GetAttributes())
		}
	}
	var out []string
	for _, kvs := range res {
		for _, kv := range kvs {
			if kv.GetKey() == key {
				out = append(out, kv.GetValue().GetStringValue())
			}
		}
	}
	return out
}

func TestOpenCodeCanary(t *testing.T) {
	withOpenCode(t)
	for _, enc := range []encoding{encProto, encJSON} {
		h := newHarness(t, fleetschema.SchemaRevOpenCode)
		for _, p := range openCodeExports {
			h.post(t, "/v1/logs", loadOpenCode(t, p), enc)
		}
		if len(h.batches) == 0 {
			t.Fatal("nothing emitted")
		}
		kinds := map[string]int{}
		for _, b := range h.batches {
			for _, bad := range openCodeNeverSent {
				if bytes.Contains(b.Body, []byte(bad)) {
					t.Errorf("enc %d: %q leaked in %s batch", enc, bad, b.Signal)
				}
			}
			checkAllowlisted(t, b, fleetschema.SchemaRevOpenCode, kinds)
			for _, k := range resourceValue(t, b, "kameas.client.kind") {
				if k != "opencode" {
					t.Errorf("client kind %q, want opencode", k)
				}
			}
			for _, v := range resourceValue(t, b, "kameas.client.version") {
				if v != "1.19.4" {
					t.Errorf("client version %q", v)
				}
			}
		}
		// Each session ends via the adapter's opencode.session.end record.
		want := map[string]int{
			fleetschema.KindConversationStarted: 2,
			fleetschema.KindConversationEnded:   2,
			fleetschema.KindToolInvoked:         1,
			fleetschema.KindError:               1,
			fleetschema.SpanGenAIChat:           2,
		}
		for k, n := range want {
			if kinds[k] != n {
				t.Errorf("enc %d: %s emitted %d times, want %d (all: %v)", enc, k, kinds[k], n, kinds)
			}
		}
	}
}

// A retried or replayed export (adapter retry, plugin reload) must not
// count model or tool calls twice.
func TestOpenCodeRetryDeduplicated(t *testing.T) {
	withOpenCode(t)
	h := newHarness(t, fleetschema.SchemaRevOpenCode)
	m := loadOpenCode(t, openCodeExports[0])
	// Drop the session end so the replay lands in the same open session.
	recs := m.ResourceLogs[0].ScopeLogs[0].LogRecords
	m.ResourceLogs[0].ScopeLogs[0].LogRecords = recs[:len(recs)-1]
	h.post(t, "/v1/logs", m, encProto)
	h.post(t, "/v1/logs", m, encProto)
	kinds := map[string]int{}
	for _, b := range h.batches {
		checkAllowlisted(t, b, fleetschema.SchemaRevOpenCode, kinds)
	}
	if kinds[fleetschema.SpanGenAIChat] != 2 || kinds[fleetschema.KindToolInvoked] != 1 || kinds[fleetschema.KindConversationStarted] != 1 {
		t.Fatalf("replay double counted: %v", kinds)
	}
}

// Until Fleet advertises the schema revision that accepts the opencode
// client kind, OpenCode records never leave the device.
func TestOpenCodeHeldBackFromOlderFleet(t *testing.T) {
	for _, rev := range []int{0, 1, fleetschema.SchemaRevCostKnown} {
		h := newHarness(t, rev)
		for _, p := range openCodeExports {
			h.post(t, "/v1/logs", loadOpenCode(t, p), encProto)
		}
		if len(h.batches) != 0 {
			t.Fatalf("rev %d: OpenCode records sent to a Fleet that does not accept them", rev)
		}
		if h.p.Counters()["dropped_client_unsupported"] == 0 {
			t.Fatalf("rev %d: drop not counted", rev)
		}
	}
}

// withOpenCode turns the OpenCode build switch on for one test (it is off
// in shipped builds).
func withOpenCode(t *testing.T) {
	t.Helper()
	old := features.OpenCode
	features.OpenCode = true
	t.Cleanup(func() { features.OpenCode = old })
}
