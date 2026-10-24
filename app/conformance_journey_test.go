package app

import (
	"bytes"
	"net/http"
	"testing"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
)

// TestConformanceOTelJourney is the OTel-export component journey the
// conformance harness replays per host: the host's recorded OTLP export
// goes through the real receiver, pipeline and consent gate to a fake
// Fleet, which must receive allowlisted batches attributed to that client
// kind and no planted content. Opted-out and signed-out members send
// nothing. It is supporting evidence, not an installed-host witness.
func TestConformanceOTelJourney(t *testing.T) {
	for _, c := range []struct {
		kind canon.ClientKind
		post func(t *testing.T, l *telemetryLane, rx http.Handler)
	}{
		{canon.ClaudeCode, postClaudeSession},
		{canon.Codex, func(t *testing.T, l *telemetryLane, rx http.Handler) {
			postLogs(t, rx, fixtures.CodexLogs(true))
			l.pipe.SessionEnd(canon.Codex, fixtures.CodexSessionID)
		}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			f := &laneFleet{Provider: stub.New(), optedIn: true}
			l, rx, _ := newTestLane(t, f)
			if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
				t.Fatal("member never signed in and admitted")
			}
			c.post(t, l, rx)
			if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
				t.Fatalf("Fleet received nothing; status %+v", l.Status())
			}
			al := allowlist.Telemetry()
			kinds := map[string]bool{}
			for _, b := range f.batches() {
				if err := al.Check(b); err != nil {
					t.Fatalf("batch outside the allowlist: %v", err)
				}
				for _, v := range fixtures.Canaries {
					if bytes.Contains(b.Body, []byte(v)) {
						t.Fatalf("planted content %q reached Fleet", v)
					}
				}
				for k := range clientKinds(t, b) {
					kinds[k] = true
				}
			}
			if !kinds[string(c.kind)] || len(kinds) != 1 {
				t.Fatalf("batches attributed to %v, want only %s", kinds, c.kind)
			}

			// Negative: after the member opts out, nothing more is sent.
			f.set(func(f *laneFleet) { f.optedIn = false })
			if !waitFor(t, func() bool { return !l.dec.SignedIn() }) {
				t.Fatal("opt-out not observed")
			}
			before := f.attemptCount()
			c.post(t, l, rx)
			if waitFor(t, func() bool { return f.attemptCount() > before }) {
				t.Fatal("an opted-out member's session reached Fleet")
			}
		})
	}
}

// clientKinds returns the kameas.client.kind resource values of a logs batch.
func clientKinds(t *testing.T, b fleetapi.ProjectedBatch) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if b.Signal != fleetapi.SignalLogs {
		return out
	}
	var m logsv1.LogsData
	if err := proto.Unmarshal(b.Body, &m); err != nil {
		t.Fatal(err)
	}
	for _, rl := range m.ResourceLogs {
		for _, kv := range rl.GetResource().GetAttributes() {
			if kv.Key == "kameas.client.kind" {
				out[kv.Value.GetStringValue()] = true
			}
		}
	}
	return out
}
