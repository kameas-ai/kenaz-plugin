package contexttools

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The share node is fully built in public code, in Fleet's wire shape.
func TestBuildShareNode(t *testing.T) {
	n, err := BuildShareNode(ShareInput{Title: "  Retry flaky e2e  ", Body: "Use -count=3.", Kind: "guidance"}, "team-1", fixedNow, bytes.NewReader(bytes.Repeat([]byte{0xff}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if !uuidV7.MatchString(n.ID) {
		t.Fatalf("id %q is not a UUIDv7", n.ID)
	}
	want := NodeInput{ID: n.ID, Kind: "guidance", Title: "Retry flaky e2e", Body: "Use -count=3.", Metadata: map[string]any{},
		Classification: "team_shared", TeamID: "team-1", Version: 1, UnitKind: "doc", UnitScope: "global", LoadPolicy: "on_demand"}
	if n.Kind != want.Kind || n.Title != want.Title || n.Body != want.Body || n.Classification != want.Classification ||
		n.TeamID != want.TeamID || n.Version != 1 || n.UnitKind != "doc" || n.UnitScope != "global" || n.LoadPolicy != "on_demand" || len(n.Metadata) != 0 {
		t.Fatalf("node = %+v", n)
	}
	// The first 48 bits encode fixedNow in milliseconds.
	if want := fmt.Sprintf("%012x", fixedNow.UnixMilli()); !strings.HasPrefix(strings.ReplaceAll(n.ID, "-", ""), want) {
		t.Fatalf("unexpected UUIDv7 time prefix: %s", n.ID)
	}
	if pr := ToPushRequest(n); pr.TeamID != "team-1" || pr.Title != n.Title || pr.Body != n.Body {
		t.Fatalf("push request = %+v", pr)
	}
}

func TestBuildShareNodeRejects(t *testing.T) {
	ok := ShareInput{Title: "t", Body: "b", Kind: "fact"}
	cases := map[string]struct {
		in   ShareInput
		team string
	}{
		"empty title":   {ShareInput{Body: "b", Kind: "fact"}, "t"},
		"long title":    {ShareInput{Title: strings.Repeat("x", MaxTitleLen+1), Body: "b", Kind: "fact"}, "t"},
		"empty body":    {ShareInput{Title: "t", Body: "  ", Kind: "fact"}, "t"},
		"big body":      {ShareInput{Title: "t", Body: strings.Repeat("x", MaxShareBodyLen+1), Kind: "fact"}, "t"},
		"bad kind":      {ShareInput{Title: "t", Body: "b", Kind: "skill"}, "t"},
		"artifact kind": {ShareInput{Title: "t", Body: "b", Kind: "artifact"}, "t"},
		"no team":       {ok, ""},
		"bad utf8":      {ShareInput{Title: "t", Body: "\xff", Kind: "fact"}, "t"},
	}
	for name, c := range cases {
		if _, err := BuildShareNode(c.in, c.team, fixedNow, nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	for _, k := range ShareKinds {
		if _, err := BuildShareNode(ShareInput{Title: "t", Body: "b", Kind: k}, "t", fixedNow, nil); err != nil {
			t.Errorf("kind %s: %v", k, err)
		}
	}
}

func TestBuildSearch(t *testing.T) {
	r, err := BuildSearch(" q ", ScopeTeam, "t1", 0)
	if err != nil || r.Query != "q" || r.TeamID != "t1" || r.Limit != DefaultLimit {
		t.Fatalf("team: %+v %v", r, err)
	}
	r, err = BuildSearch("q", ScopeOrg, "t1", 25)
	if err != nil || r.TeamID != "" || r.Limit != 25 {
		t.Fatalf("org scope must drop team_id: %+v %v", r, err)
	}
	for _, bad := range []struct {
		q     string
		s     Scope
		team  string
		limit int
	}{{"", ScopeTeam, "t", 0}, {strings.Repeat("q", MaxQueryLen+1), ScopeTeam, "t", 0}, {"q", ScopeTeam, "", 0}, {"q", ScopeTeam, "t", 26}, {"q", ScopeTeam, "t", -1}, {"q", "world", "t", 0}} {
		if _, err := BuildSearch(bad.q, bad.s, bad.team, bad.limit); err == nil {
			t.Errorf("expected error for %+v", bad)
		}
	}
}

func TestBuildPullAndPromote(t *testing.T) {
	r, err := BuildPull([]string{"a", " a ", "b"}, "t1")
	if err != nil || len(r.IDs) != 2 || r.TeamID != "t1" {
		t.Fatalf("pull dedupe: %+v %v", r, err)
	}
	if _, err := BuildPull(nil, ""); err == nil {
		t.Fatal("empty ids accepted")
	}
	if _, err := BuildPull(make([]string, MaxPullIDs+1), ""); err == nil {
		t.Fatal("too many ids accepted")
	}
	p, err := BuildPromote(" n1 ")
	if err != nil || p.NodeID != "n1" || p.To != fleetapi.ClassificationOrgShared {
		t.Fatalf("promote: %+v %v", p, err)
	}
	if _, err := BuildPromote(""); err == nil {
		t.Fatal("empty node id accepted")
	}
}
