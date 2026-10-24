package pipeline

import (
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

type decider struct{ hold string }

func (d *decider) SignedIn() bool                    { return true }
func (d *decider) HoldReason() string                { return d.hold }
func (d *decider) TierAllows(fleetschema.Class) bool { return true }
func (d *decider) OptedIn(fleetschema.Class) bool    { return true }

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
