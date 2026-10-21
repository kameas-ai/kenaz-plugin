package contexttools

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

func TestResolveTeam(t *testing.T) {
	a := fleetapi.TeamRef{ID: "id-a", Name: "Platform"}
	b := fleetapi.TeamRef{ID: "id-b", Name: "Payments"}

	if _, e := ResolveTeam("", "", nil); e == nil || e.Code != CodeTeamRequired {
		t.Fatalf("no teams: %+v", e)
	}
	if got, e := ResolveTeam("", "", []fleetapi.TeamRef{a}); e != nil || got != a {
		t.Fatalf("single team: %+v %+v", got, e)
	}
	if _, e := ResolveTeam("", "", []fleetapi.TeamRef{a, b}); e == nil || e.Code != CodeTeamRequired || len(e.Choices) != 2 {
		t.Fatalf("many teams, no choice: %+v", e)
	}
	if got, e := ResolveTeam("payments", "", []fleetapi.TeamRef{a, b}); e != nil || got != b {
		t.Fatalf("by name: %+v %+v", got, e)
	}
	if got, e := ResolveTeam("id-a", "", []fleetapi.TeamRef{a, b}); e != nil || got != a {
		t.Fatalf("by id: %+v %+v", got, e)
	}
	if got, e := ResolveTeam("", "Payments", []fleetapi.TeamRef{a, b}); e != nil || got != b {
		t.Fatalf("configured default: %+v %+v", got, e)
	}
	if _, e := ResolveTeam("Growth", "Payments", []fleetapi.TeamRef{a, b}); e == nil || e.Code != CodeTeamRequired {
		t.Fatalf("unknown explicit team must not fall back: %+v", e)
	}
	if got, e := ResolveTeam("", "Old team", []fleetapi.TeamRef{a}); e != nil || got != a {
		t.Fatalf("stale default with one team: %+v %+v", got, e)
	}
	dup := fleetapi.TeamRef{ID: "id-c", Name: "platform"}
	if _, e := ResolveTeam("Platform", "", []fleetapi.TeamRef{a, dup}); e == nil {
		t.Fatal("ambiguous name resolved")
	}
}
