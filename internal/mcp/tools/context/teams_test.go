package contexttools

import (
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
)

func TestResolveTeam(t *testing.T) {
	a := fleetapi.TeamRef{ID: "id-a", Name: "Platform"}
	b := fleetapi.TeamRef{ID: "id-b", Name: "Payments"}

	if _, e := ResolveTeam("", "", nil); e == nil || e.Code != CodeTeamRequired {
		t.Fatalf("no teams: %+v", e)
	}
	if got, e := ResolveTeam("", "", []fleetapi.TeamRef{a}); e != nil || got.ID != a.ID {
		t.Fatalf("single team: %+v %+v", got, e)
	}
	if _, e := ResolveTeam("", "", []fleetapi.TeamRef{a, b}); e == nil || e.Code != CodeTeamRequired || len(e.Choices) != 2 {
		t.Fatalf("many teams, no choice: %+v", e)
	}
	if got, e := ResolveTeam("payments", "", []fleetapi.TeamRef{a, b}); e != nil || got.ID != b.ID {
		t.Fatalf("by name: %+v %+v", got, e)
	}
	if got, e := ResolveTeam("id-a", "", []fleetapi.TeamRef{a, b}); e != nil || got.ID != a.ID {
		t.Fatalf("by id: %+v %+v", got, e)
	}
	if got, e := ResolveTeam("", "Payments", []fleetapi.TeamRef{a, b}); e != nil || got.ID != b.ID {
		t.Fatalf("configured default: %+v %+v", got, e)
	}
	if _, e := ResolveTeam("Growth", "Payments", []fleetapi.TeamRef{a, b}); e == nil || e.Code != CodeTeamRequired {
		t.Fatalf("unknown explicit team must not fall back: %+v", e)
	}
	if got, e := ResolveTeam("", "Old team", []fleetapi.TeamRef{a}); e != nil || got.ID != a.ID {
		t.Fatalf("stale default with one team: %+v %+v", got, e)
	}
	dup := fleetapi.TeamRef{ID: "id-c", Name: "platform"}
	if _, e := ResolveTeam("Platform", "", []fleetapi.TeamRef{a, dup}); e == nil {
		t.Fatal("ambiguous name resolved")
	}
}

// A name two of the member's teams share is refused with their ids, asked for or
// configured, and never settled by taking the first. An id still picks its team.
func TestResolveTeamAmbiguousName(t *testing.T) {
	root := fleetapi.TeamRef{ID: "id-root", Name: "Everyone", IsDefault: true, Role: "member"}
	a := fleetapi.TeamRef{ID: "id-b", Name: "Docs", Role: "member", Depth: 1}
	b := fleetapi.TeamRef{ID: "id-a", Name: "docs", Role: "lead", ParentTeamID: "id-x", Depth: 2, Path: []string{"id-x"}}
	teams := []fleetapi.TeamRef{root, a, b}
	for name, args := range map[string][2]string{"asked for": {"Docs", ""}, "configured": {"", "DOCS"}, "asked for over a good default": {"docs", "Everyone"}} {
		got, e := ResolveTeam(args[0], args[1], teams)
		if e == nil || e.Code != CodeTeamRequired || got.ID != "" {
			t.Fatalf("%s: %+v %+v", name, got, e)
		}
		if len(e.Choices) != 2 || e.Choices[0] != "id-a" || e.Choices[1] != "id-b" {
			t.Fatalf("%s: choices are not the two ids: %+v", name, e.Choices)
		}
		if !strings.Contains(e.Message, "id-a") || !strings.Contains(e.Message, "id-b") || strings.Contains(e.Message, "id-root") {
			t.Fatalf("%s: message doesn't list the ids: %s", name, e.Message)
		}
	}
	if got, e := ResolveTeam("id-a", "Docs", teams); e != nil || got.ID != "id-a" || got.Depth != 2 {
		t.Fatalf("by id: %+v %+v", got, e)
	}
	// An unambiguous argument wins over an ambiguous configured default.
	if got, e := ResolveTeam("Everyone", "Docs", teams); e != nil || got.ID != "id-root" {
		t.Fatalf("argument over ambiguous default: %+v %+v", got, e)
	}
}

func TestSharedWithWholeOrg(t *testing.T) {
	tree := fleetapitest.SampleTeams()
	root, platform, payments := tree[0], tree[1], tree[2]
	if everyone, known := SharedWithWholeOrg(tree, root.ID); !everyone || !known {
		t.Fatalf("default team: %v %v", everyone, known)
	}
	for _, tm := range []fleetapi.TeamRef{platform, payments} {
		if everyone, known := SharedWithWholeOrg(tree, tm.ID); everyone || !known {
			t.Fatalf("%s: %v %v", tm.Name, everyone, known)
		}
	}
	// The flag decides, not the position: the default team listed last.
	if everyone, known := SharedWithWholeOrg([]fleetapi.TeamRef{payments, platform, root}, root.ID); !everyone || !known {
		t.Fatalf("default team last: %v %v", everyone, known)
	}
	if everyone, known := SharedWithWholeOrg([]fleetapi.TeamRef{payments, platform, root}, payments.ID); everyone || !known {
		t.Fatalf("first team, not default: %v %v", everyone, known)
	}
	// A Fleet that sends no flag: not known, whatever the team is called or
	// wherever it is listed.
	old := []fleetapi.TeamRef{{ID: root.ID, Name: "Everyone"}, {ID: platform.ID, Name: "Platform"}}
	for _, tm := range old {
		if everyone, known := SharedWithWholeOrg(old, tm.ID); everyone || known {
			t.Fatalf("no flag, %s: %v %v", tm.Name, everyone, known)
		}
	}
	// The tree without the default team in it (roles sent, no flag): still unknown.
	if everyone, known := SharedWithWholeOrg([]fleetapi.TeamRef{platform, payments}, platform.ID); everyone || known {
		t.Fatalf("no default team listed: %v %v", everyone, known)
	}
	if everyone, known := SharedWithWholeOrg(tree, ""); everyone || !known {
		t.Fatalf("empty id: %v %v", everyone, known)
	}
	if everyone, known := SharedWithWholeOrg(nil, root.ID); everyone || known {
		t.Fatalf("no teams: %v %v", everyone, known)
	}
}
