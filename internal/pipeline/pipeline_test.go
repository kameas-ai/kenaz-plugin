package pipeline

import (
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

type decider struct {
	hold string
	out  bool
}

func (d *decider) SignedIn() bool                    { return true }
func (d *decider) HoldReason() string                { return d.hold }
func (d *decider) TierAllows(fleetschema.Class) bool { return true }
func (d *decider) OptedIn(fleetschema.Class) bool    { return !d.out }

// TestHoldThenDrain: records are held during a pause/lockdown and emitted
// unchanged once the daemon re-gates them.
func TestHoldThenDrain(t *testing.T) {
	d := &decider{hold: "lockdown"}
	var held []project.Record
	var emitted []fleetapi.ProjectedBatch
	identityOK := true
	p := New(Config{
		Aggregator: session.New(session.Config{Key: []byte("k")}),
		Decider:    d,
		Identity: func() (project.Identity, bool) {
			return project.Identity{UserID: "u", OrgID: "o", MachineID: "m"}, identityOK
		},
		Emit: func(b []fleetapi.ProjectedBatch, _ time.Time, _ project.Identity) { emitted = append(emitted, b...) },
		Hold: func(r []project.Record, reason string) {
			if reason != "lockdown" {
				t.Errorf("reason %q", reason)
			}
			held = append(held, r...)
		},
	})
	p.Logs(fixtures.ClaudeLogs())
	p.SessionEnd(canon.ClaudeCode, fixtures.ClaudeSessionID)
	if len(emitted) != 0 || len(held) == 0 {
		t.Fatalf("emitted %d held %d during hold", len(emitted), len(held))
	}
	d.hold = ""
	p.Regate(held)
	if len(emitted) == 0 {
		t.Fatal("nothing drained after hold cleared")
	}
	if c := p.Counters(); c["held"] != len(held) || c["passed"] != len(held) || c["content_seen"] == 0 {
		t.Fatalf("counters = %v", c)
	}

	// No identity (signed out between gate and encode): dropped, counted.
	identityOK = false
	emitted = nil
	p.Regate(held)
	if len(emitted) != 0 || p.Counters()["dropped_no_identity"] == 0 {
		t.Fatal("emitted without identity")
	}
}

// A session that spans an opted-out window reports only what came after it.
func TestSummaryLeavesOutTheOptedOutWindow(t *testing.T) {
	d := &decider{}
	a := session.New(session.Config{Key: []byte("k")})
	p := New(Config{Aggregator: a, Decider: d})
	call := func(tok int64) canon.Event {
		return canon.Event{Kind: canon.KindModelCall, Client: canon.ClaudeCode, SessionID: "s", Time: fixtures.Base, InputTokens: tok}
	}
	tokens := func() int64 {
		var n int64
		for _, st := range a.Snapshot() {
			n += st.TokenIn
		}
		return n
	}
	p.ingest(decode.Result{Events: []canon.Event{call(100)}})
	if got := tokens(); got != 100 {
		t.Fatalf("opted in: %d", got)
	}
	d.out = true
	p.ingest(decode.Result{Events: []canon.Event{call(1000)}})
	if got := tokens(); got != 0 {
		t.Errorf("during the window: %d", got)
	}
	d.out = false
	p.ingest(decode.Result{Events: []canon.Event{call(7)}})
	if got := tokens(); got != 7 {
		t.Errorf("after the window: %d", got)
	}
}
