package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
)

// The approval page's own read of /me doesn't ask for teams the member only reads.
// If a provider returned one anyway, the page still never shows it as the team a
// share goes to: it has no name there, only its id.
func TestApprovalPageNeverNamesAReadOnlyTeamAsTheShareTarget(t *testing.T) {
	f := newSyncFleet()
	f.teams = syncTeamsWithInherited()
	p := fleetapi.Provider(f)

	if got := approvalTeams(p)().OwnName("team-1"); got != "Platform" {
		t.Fatalf("the member's own team: %q", got)
	}
	for _, id := range []string{"team-up", "team-down", "nope", ""} {
		if got := approvalTeams(p)().OwnName(id); got != "" {
			t.Errorf("%s was named %q as a team the person shares with", id, got)
		}
	}
	// A promotion says which team a note is shared with now, whoever's it is.
	if got := noteTeamNames(p)("team-up"); got != "Parent" {
		t.Fatalf("%q", got)
	}

	node, err := contexttools.BuildFileNode(contexttools.ShareInput{Title: "Guide", Body: "text\n", Kind: "doc"}, "guide.md", "team-up", "0192e4a0-0000-7000-8000-000000000001", 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"node": node, "path": "personal/guide.md", "ledger_key": "k"})
	view, err := contexttools.RenderShareFile(approvalTeams(p))(payload)
	if err != nil {
		t.Fatal(err)
	}
	const refusal = "You are not a member of the team with id team-up, so approving this will not share it: Kameas Fleet shares only with teams you belong to. Decline it, and share with one of your own teams."
	if strings.Contains(view.Summary+view.Impact, "Parent") || !strings.Contains(view.Summary, "with the team team-up in Kameas Fleet") {
		t.Fatalf("%q / %q", view.Summary, view.Impact)
	}
	// The page doesn't promise a share Fleet will refuse: it says so, plainly.
	if view.Impact != refusal || strings.Contains(view.Impact, "will be able to read") {
		t.Fatalf("%q", view.Impact)
	}
	// Text drafted from memory: the notice about that is required and is always
	// shown, with the refusal line after it.
	payload, _ = json.Marshal(map[string]any{"node": node, "path": "personal/guide.md", "ledger_key": "k", "from_memory": true})
	mview, err := contexttools.RenderShareFile(approvalTeams(p))(payload)
	if err != nil || mview.Impact != contexttools.FromMemoryBanner("this team")+"\n\n"+refusal {
		t.Fatalf("%v %q", err, mview.Impact)
	}
	note, _ := json.Marshal(map[string]any{"id": "0192e4a0-0000-7000-8000-000000000002", "kind": "doc", "title": "T", "body": "B", "classification": "team_shared", "team_id": "team-up", "load_policy": "on_demand"})
	nview, err := contexttools.RenderShare(approvalTeams(p))(note)
	if err != nil || nview.Impact != refusal || strings.Contains(nview.Summary, "Parent") {
		t.Fatalf("%v %q / %q", err, nview.Summary, nview.Impact)
	}
	node.TeamID = "team-1"
	payload, _ = json.Marshal(map[string]any{"node": node, "path": "personal/guide.md", "ledger_key": "k"})
	if view, err = contexttools.RenderShareFile(approvalTeams(p))(payload); err != nil || !strings.Contains(view.Summary, "with the team Platform (team-1)") || view.Impact == refusal || !strings.Contains(view.Impact, "Every member of Platform (team-1) will be able to read this file.") {
		t.Fatalf("%v %q / %q", err, view.Summary, view.Impact)
	}
	note, _ = json.Marshal(map[string]any{"id": "0192e4a0-0000-7000-8000-000000000002", "kind": "doc", "title": "T", "body": "B", "classification": "team_shared", "team_id": "team-1", "load_policy": "on_demand"})
	if nview, err = contexttools.RenderShare(approvalTeams(p))(note); err != nil || nview.Impact != "Every member of Platform (team-1) will be able to read this note." {
		t.Fatalf("%v %q", err, nview.Impact)
	}
	// With Fleet out of reach nothing is known, and the page says nothing either
	// way: for a team that isn't the member's as for one that is.
	down := &unreachableFleet{syncFleet: f}
	node.TeamID = "team-up"
	payload, _ = json.Marshal(map[string]any{"node": node, "path": "personal/guide.md", "ledger_key": "k"})
	if v, err := contexttools.RenderShareFile(approvalTeams(down))(payload); err != nil || strings.Contains(v.Impact, "not a member") || !strings.Contains(v.Impact, "Every member of team-up will be able to read this file.") {
		t.Fatalf("%v %q", err, v.Impact)
	}
	upNote, _ := json.Marshal(map[string]any{"id": "0192e4a0-0000-7000-8000-000000000002", "kind": "doc", "title": "T", "body": "B", "classification": "team_shared", "team_id": "team-up", "load_policy": "on_demand"})
	if v, err := contexttools.RenderShare(approvalTeams(down))(upNote); err != nil || v.Impact != "Every member of team-up will be able to read this note." {
		t.Fatalf("%v %q", err, v.Impact)
	}
	if down.asked == 0 {
		t.Fatal("the page never asked Fleet")
	}
	if v, err := contexttools.RenderShare(nil)(note); err != nil || v.Impact != "Every member of team-1 will be able to read this note." {
		t.Fatalf("%v %q", err, v.Impact)
	}
	if v, err := contexttools.RenderShare(func() contexttools.Teams { return contexttools.Teams{} })(note); err != nil || v.Impact != "Every member of team-1 will be able to read this note." {
		t.Fatalf("%v %q", err, v.Impact)
	}
}

// unreachableFleet is a Fleet whose /me can't be read.
type unreachableFleet struct {
	*syncFleet
	asked int
}

func (u *unreachableFleet) Me() fleetapi.Me { return unreachableMe{u} }

type unreachableMe struct{ u *unreachableFleet }

func (m unreachableMe) Get(context.Context) (fleetapi.MeInfo, error) {
	m.u.asked++
	return fleetapi.MeInfo{}, errors.New("connection refused")
}
func (m unreachableMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, errors.New("connection refused")
}
