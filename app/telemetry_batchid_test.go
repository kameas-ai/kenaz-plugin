package app

import (
	"regexp"
	"testing"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

var laneBatchIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// laneBatchID returns the one kameas.batch.id of an encoded request body.
func laneBatchID(t *testing.T, signal fleetapi.Signal, body []byte) string {
	t.Helper()
	var groups [][]*commonv1.KeyValue
	switch signal {
	case fleetapi.SignalLogs:
		var m logsv1.LogsData
		if err := proto.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.GetResourceLogs() {
			groups = append(groups, rl.GetResource().GetAttributes())
		}
	case fleetapi.SignalTraces:
		var m tracev1.TracesData
		if err := proto.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.GetResourceSpans() {
			groups = append(groups, rs.GetResource().GetAttributes())
		}
	default:
		t.Fatalf("unexpected signal %q", signal)
	}
	ids := map[string]bool{}
	for _, kvs := range groups {
		n := 0
		for _, kv := range kvs {
			if kv.GetKey() == fleetschema.BatchIDAttr {
				n++
				ids[kv.GetValue().GetStringValue()] = true
			}
		}
		if n != 1 {
			t.Fatalf("%s resource group carries %d batch ids, want 1", signal, n)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("%s request carries %d distinct batch ids, want 1", signal, len(ids))
	}
	for id := range ids {
		if !laneBatchIDRe.MatchString(id) {
			t.Fatalf("batch id %q is not 32 lowercase hex characters", id)
		}
		return id
	}
	return ""
}

// A batch whose upload fails is spooled as its serialized body and sent
// again with the same bytes, so it keeps its batch id: Fleet can recognize
// the retry as the batch it may already have. Checked for a retry by the
// running relay and for one by the next relay over the same spool.
func TestTelemetryLaneRetryKeepsBatchID(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same relay"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: &fleetapi.APIError{Status: 503}}
			dir, ks := t.TempDir(), &spool.Memory{}
			l, rx, stop := newTestLaneIn(t, f, dir, ks, nil)
			if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
				t.Fatal("lane never admitted")
			}
			postClaudeSession(t, l, rx)
			if !waitFor(t, func() bool { return f.attemptCount() > 0 && !l.sp.Empty() }) {
				t.Fatalf("transient failure not spooled: attempts=%d", f.attemptCount())
			}
			if restart {
				stop()
				l.sp.Close()
				f.set(func(f *laneFleet) { f.postErr = nil })
				l, _, _ = newTestLaneIn(t, f, dir, ks, nil)
			} else {
				f.set(func(f *laneFleet) { f.postErr = nil })
			}
			if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
				t.Fatalf("spooled batch never sent; status %+v", l.Status())
			}

			// Every body Fleet finally accepted, by its bytes.
			sent := map[string]fleetapi.Signal{}
			sentIDs := map[string]bool{}
			for _, b := range f.batches() {
				sent[string(b.Body)] = b.Signal
				sentIDs[laneBatchID(t, b.Signal, b.Body)] = true
			}
			if len(sentIDs) != len(sent) {
				t.Fatalf("%d distinct bodies carry %d batch ids", len(sent), len(sentIDs))
			}
			f.mu.Lock()
			failed := append([][]byte(nil), f.failed...)
			f.mu.Unlock()
			if len(failed) == 0 {
				t.Fatal("no upload failed, so nothing was retried")
			}
			for _, body := range failed {
				signal, ok := sent[string(body)]
				if !ok {
					t.Fatal("a failed batch was not sent again with identical bytes")
				}
				// Identical bytes imply the same id; state it anyway.
				if id := laneBatchID(t, signal, body); !sentIDs[id] {
					t.Fatalf("retry did not keep batch id %s", id)
				}
			}
		})
	}
}
