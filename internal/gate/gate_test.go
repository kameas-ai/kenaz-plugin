package gate

import (
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
)

type fake struct {
	signedIn bool
	hold     string
	tierOK   map[fleetschema.Class]bool
	optedIn  map[fleetschema.Class]bool
}

func (f fake) SignedIn() bool                      { return f.signedIn }
func (f fake) HoldReason() string                  { return f.hold }
func (f fake) TierAllows(c fleetschema.Class) bool { return f.tierOK[c] }
func (f fake) OptedIn(c fleetschema.Class) bool    { return f.optedIn[c] }

func recs() []project.Record {
	return []project.Record{
		{Class: fleetschema.ClassUsageCounts, EventKind: fleetschema.KindConversationStarted,
			Body: []project.Field{{Key: "conversation_id", Type: project.TString, Str: "0123456789abcdef"}}},
		{Class: fleetschema.ClassToolCalls, EventKind: fleetschema.KindToolInvoked,
			Body: []project.Field{{Key: "tool_name", Type: project.TString, Str: "Bash"}}},
		{Class: fleetschema.ClassToolCalls, EventKind: fleetschema.KindToolInvoked,
			Body: []project.Field{{Key: "tool_name", Type: project.TString, Str: "Read"}}},
		{Class: fleetschema.ClassModelCallTelemetry, SpanName: fleetschema.SpanGenAIChat,
			Attrs: []project.Field{{Key: "gen_ai.usage.input_tokens", Type: project.TInt, Int: 4111111111111111}}},
	}
}

var all = map[fleetschema.Class]bool{
	fleetschema.ClassUsageCounts: true, fleetschema.ClassToolCalls: true,
	fleetschema.ClassErrors: true, fleetschema.ClassModelCallTelemetry: true,
}

func TestGatingOrder(t *testing.T) {
	cases := []struct {
		name     string
		d        Decider
		send     int
		hold     int
		counters map[string]int
	}{
		{"nil decider drops all", nil, 0, 0, map[string]int{"not_signed_in": 4}},
		{"not signed in drops all", fake{hold: "org_paused", tierOK: all, optedIn: all}, 0, 0, map[string]int{"not_signed_in": 4}},
		{"hold beats tier and opt-in", fake{signedIn: true, hold: "lockdown"}, 0, 4, map[string]int{"held": 4}},
		{"tier drops", fake{signedIn: true, tierOK: map[fleetschema.Class]bool{fleetschema.ClassToolCalls: true}, optedIn: all},
			2, 0, map[string]int{"dropped_tier": 2, "passed": 2}},
		{"opt-out drops", fake{signedIn: true, tierOK: all, optedIn: map[fleetschema.Class]bool{fleetschema.ClassUsageCounts: true}},
			1, 0, map[string]int{"dropped_opt_out": 3, "passed": 1}},
		{"allowed and opted in passes", fake{signedIn: true, tierOK: all, optedIn: all},
			4, 0, map[string]int{"passed": 4}},
	}
	for _, c := range cases {
		var ctr Counters
		o := Apply(c.d, recs(), &ctr)
		if len(o.Send) != c.send || len(o.Hold) != c.hold {
			t.Errorf("%s: send %d hold %d", c.name, len(o.Send), len(o.Hold))
		}
		if c.hold > 0 && o.HoldReason == "" {
			t.Errorf("%s: hold without reason", c.name)
		}
		snap := ctr.Snapshot()
		for k, v := range c.counters {
			if snap[k] != v {
				t.Errorf("%s: counter %s = %d, want %d (%v)", c.name, k, snap[k], v, snap)
			}
		}
	}
}

type admissionFake struct {
	fake
	since time.Time
	ok    bool
}

func (a admissionFake) AdmittedSince() (time.Time, bool) { return a.since, a.ok }

// While the plan does not include the plugin every record is dropped (never held, even during a
// pause); once admitted, records generated before the admission moment
// (or without a timestamp) are dropped.
func TestAdmissionGate(t *testing.T) {
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := fake{signedIn: true, hold: "paused", tierOK: all, optedIn: all}
	var c Counters
	if o := Apply(admissionFake{fake: f}, recs(), &c); len(o.Send)+len(o.Hold) != 0 || c.Snapshot()["dropped_not_admitted"] != 4 {
		t.Fatalf("not admitted: %+v %v", o, c.Snapshot())
	}
	rs := []project.Record{
		{Class: fleetschema.ClassUsageCounts, Time: base.Add(-time.Second)},
		{Class: fleetschema.ClassUsageCounts, Time: base},
		{Class: fleetschema.ClassModelCallTelemetry, Time: base.Add(time.Second), Start: base.Add(-time.Second)},
		{Class: fleetschema.ClassUsageCounts},
		{Class: fleetschema.ClassUsageCounts, Time: base.Add(time.Minute)},
	}
	f.hold = ""
	c = Counters{}
	o := Apply(admissionFake{fake: f, since: base, ok: true}, rs, &c)
	if len(o.Send) != 2 || c.Snapshot()["dropped_before_admission"] != 3 {
		t.Fatalf("admitted: send %d, %v", len(o.Send), c.Snapshot())
	}
	if e := Earliest(o.Send); !e.Equal(base) {
		t.Fatalf("earliest %v", e)
	}
}
