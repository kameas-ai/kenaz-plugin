package contexttools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

var (
	platform = fleetapi.TeamRef{ID: "team-plat", Name: "Platform"}
	payments = fleetapi.TeamRef{ID: "team-pay", Name: "Payments"}
)

// Write tools force a human prompt in Claude Code; read tools don't.
func TestWriteToolsRequireUserInteraction(t *testing.T) {
	cs := session(t, newFake("team", "member", platform), nil, "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sdk.Tool{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl
	}
	for _, name := range []string{ToolSearch, ToolPull, ToolShare, ToolPromote, ToolTeamMembers} {
		tl, ok := got[name]
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		forced := tl.Meta[MetaRequiresUserInteraction] == true
		isWrite := name == ToolShare || name == ToolPromote
		if forced != isWrite {
			t.Errorf("%s: requiresUserInteraction=%v, want %v", name, forced, isWrite)
		}
		if !isWrite && (tl.Annotations == nil || !tl.Annotations.ReadOnlyHint) {
			t.Errorf("%s: read tool must be marked read-only", name)
		}
	}
	// The flag must be the JSON boolean true on the wire.
	raw, _ := json.Marshal(got[ToolShare])
	if !strings.Contains(string(raw), `"anthropic/requiresUserInteraction":true`) {
		t.Fatalf("wire form: %s", raw)
	}
	// Codex: per-tool approval_mode is deferred; the
	// user_confirmed argument is the interim guard and is tested below.
}

// Search request building, ranking, snippet cap and the 16 KiB cap.
func TestSearch(t *testing.T) {
	f := newFake("team", "member", platform, payments)
	long := strings.Repeat("s", 2*MaxSnippetLen)
	for i := 0; i < 60; i++ {
		f.search = append(f.search, fleetapi.SearchResult{Node: fleetapi.ContextNode{ID: "n", Title: "t", Kind: "fact", Classification: "team_shared"}, Snippet: long, Rank: float64(i)})
	}
	cs := session(t, f, nil, "")
	var out SearchOutput
	if isErr := call(t, cs, ToolSearch, map[string]any{"query": "retries", "team": "Payments"}, &out); isErr || out.Error != nil {
		t.Fatalf("search error: %+v", out.Error)
	}
	if req := f.searches[0]; req.TeamID != "team-pay" || req.Query != "retries" || req.Limit != DefaultLimit {
		t.Fatalf("request = %+v", req)
	}
	if !out.Truncated || len(out.Results) == 0 || out.Results[0].Rank != 59 || len(out.Results[0].Snippet) != MaxSnippetLen {
		t.Fatalf("caps/order: truncated=%v n=%d first=%+v", out.Truncated, len(out.Results), out.Results[0].Rank)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > MaxSearchResult+2048 {
		t.Fatalf("result %d bytes exceeds the cap", len(raw))
	}

	// Several teams and no choice: team_required with choices, no request.
	f2 := newFake("team", "member", platform, payments)
	cs2 := session(t, f2, nil, "")
	call(t, cs2, ToolSearch, map[string]any{"query": "x"}, &out)
	if out.Error == nil || out.Error.Code != CodeTeamRequired || len(out.Error.Choices) != 2 || f2.networkCalls() != 0 {
		t.Fatalf("team_required: %+v calls=%d", out.Error, f2.networkCalls())
	}
	// Org scope drops team_id.
	f3 := newFake("enterprise", "member", platform)
	cs3 := session(t, f3, nil, "")
	out = SearchOutput{}
	call(t, cs3, ToolSearch, map[string]any{"query": "x", "scope": "org"}, &out)
	if out.Error != nil || f3.searches[0].TeamID != "" {
		t.Fatalf("org scope: %+v %+v", out.Error, f3.searches)
	}
}

// Pull filters to requested ids, caps bodies and reports missing ids.
func TestPull(t *testing.T) {
	f := newFake("team", "member", platform)
	f.nodes = []fleetapi.NodeContent{
		{Node: fleetapi.ContextNode{ID: "a", Title: "A", Kind: "fact", UpdatedAt: fixedNow}, Body: strings.Repeat("é", MaxPullBodyLen)},
		{Node: fleetapi.ContextNode{ID: "zzz", Title: "not requested"}, Body: "x"},
	}
	cs := session(t, f, nil, "")
	var out PullOutput
	if call(t, cs, ToolPull, map[string]any{"ids": []string{"a", "b"}}, &out); out.Error != nil {
		t.Fatalf("pull: %+v", out.Error)
	}
	if len(out.Nodes) != 1 || out.Nodes[0].ID != "a" || !out.Nodes[0].Truncated || len(out.Nodes[0].Body) > MaxPullBodyLen {
		t.Fatalf("nodes = %+v", out.Nodes)
	}
	if !strings.HasSuffix(out.Nodes[0].Body, "é") {
		t.Fatal("body cut mid-rune")
	}
	if len(out.Missing) != 1 || out.Missing[0] != "b" {
		t.Fatalf("missing = %v", out.Missing)
	}
	if out.Nodes[0].UpdatedAt != fixedNow.Format(time.RFC3339) {
		t.Fatalf("updated_at = %q", out.Nodes[0].UpdatedAt)
	}
	if f.pulls[0].TeamID != "team-plat" {
		t.Fatalf("pull team = %+v", f.pulls[0])
	}
}

func TestShare(t *testing.T) {
	f := newFake("team", "member", platform)
	cs := session(t, f, nil, "")
	args := map[string]any{"title": "Flaky e2e", "body": "Retry with -count=3.", "kind": "guidance"}

	var out ShareOutput
	call(t, cs, ToolShare, args, &out)
	if out.Error == nil || out.Error.Code != CodeNotConfirmed || f.networkCalls() != 0 {
		t.Fatalf("unconfirmed share must not upload: %+v calls=%d", out.Error, f.networkCalls())
	}

	args["user_confirmed"] = true
	out = ShareOutput{}
	if call(t, cs, ToolShare, args, &out); out.Error != nil {
		t.Fatalf("share: %+v", out.Error)
	}
	if len(f.pushes) != 1 || f.pushes[0].TeamID != "team-plat" || f.pushes[0].Title != "Flaky e2e" || f.pushes[0].Body != "Retry with -count=3." {
		t.Fatalf("push = %+v", f.pushes)
	}
	if out.ID != "srv-1" || out.Team != "Platform" || out.Classification != "team_shared" {
		t.Fatalf("out = %+v", out)
	}
}

// Credentials are not scanned on the device. Kameas Fleet's context push
// answers 422 lint_blocked for a note that looks like it holds one, and the tool says so plainly without
// echoing the note.
func TestShareFleetCredentialRejection(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	f.opErr = &fleetapi.APIError{Status: http.StatusUnprocessableEntity, Code: "lint_blocked", Message: "content blocked by lint"}
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "note", "body": "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "kind": "fact", "user_confirmed": true}, &out)
	if out.Error == nil || out.Error.Code != CodeCredentialDetected || !strings.Contains(out.Error.Message, "Kameas Fleet blocked this because it looks like it contains a secret") {
		t.Fatalf("error = %+v", out.Error)
	}
	if out.ID != "" {
		t.Fatalf("rejected share reported an id: %+v", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "AKIA") {
		t.Fatalf("error echoes the note: %s", raw)
	}
}

// Other 400/422 refusals show only Fleet's machine reason code, never its text.
func TestShareFleetRejectionMessage(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	f.opErr = &fleetapi.APIError{Status: http.StatusBadRequest, Code: "body_too_long", Message: "body exceeds the maximum length: SECRET-ECHO"}
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "note", "body": "text", "kind": "fact", "user_confirmed": true}, &out)
	if out.Error == nil || out.Error.Code != CodeFleetRejected || !strings.Contains(out.Error.Message, "(reason: body_too_long)") {
		t.Fatalf("error = %+v", out.Error)
	}
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "SECRET-ECHO") {
		t.Fatalf("Fleet's free text reached the user: %s", raw)
	}
	// A hostile Code never reaches the output either.
	f.opErr = &fleetapi.APIError{Status: http.StatusUnprocessableEntity, Code: "x SECRET-ECHO", Message: "m"}
	out = ShareOutput{}
	call(t, cs, ToolShare, map[string]any{"title": "note", "body": "text", "kind": "fact", "user_confirmed": true}, &out)
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "SECRET-ECHO") {
		t.Fatalf("a non-code Code reached the user: %s", raw)
	}
}

func TestPromote(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	var out PromoteOutput
	if call(t, cs, ToolPromote, map[string]any{"node_id": "n1", "user_confirmed": true}, &out); out.Error != nil {
		t.Fatalf("promote: %+v", out.Error)
	}
	if f.promotes[0].NodeID != "n1" || f.promotes[0].To != fleetapi.ClassificationOrgShared || out.Classification != "org_shared" {
		t.Fatalf("promote = %+v out=%+v", f.promotes, out)
	}
}

// Tier × role × tool matrix against the Fleet capability snapshot.
func TestTierRoleMatrix(t *testing.T) {
	type tc struct {
		tool string
		args map[string]any
	}
	tools := []tc{
		{ToolSearch, map[string]any{"query": "q"}},
		{ToolSearch + ":org", map[string]any{"query": "q", "scope": "org"}},
		{ToolPull, map[string]any{"ids": []string{"a"}}},
		{ToolShare, map[string]any{"title": "t", "body": "b", "kind": "fact", "user_confirmed": true}},
		{ToolPromote, map[string]any{"node_id": "n", "user_confirmed": true}},
		{ToolTeamMembers, map[string]any{}},
	}
	expect := func(tier, role, tool string) (code, need string) {
		team := tier == "team" || tier == "enterprise"
		ent := tier == "enterprise"
		admin := role == "org_admin"
		switch tool {
		case ToolSearch + ":org":
			if !ent {
				return CodeTierRequired, "enterprise"
			}
		case ToolPromote:
			if !ent {
				return CodeTierRequired, "enterprise"
			}
			if !admin {
				return CodeRoleRequired, "admin"
			}
		default:
			if !team {
				return CodeTierRequired, "team"
			}
		}
		return "", ""
	}
	for _, tier := range []string{"free", "pro", "team", "enterprise"} {
		for _, role := range []string{"member", "org_admin"} {
			for _, c := range tools {
				f := newFake(tier, role, platform)
				cs := session(t, f, nil, "")
				name := strings.TrimSuffix(c.tool, ":org")
				var out struct {
					Error *ToolError `json:"error"`
				}
				call(t, cs, name, c.args, &out)
				code, need := expect(tier, role, c.tool)
				gotCode, gotNeed := "", ""
				if out.Error != nil {
					gotCode, gotNeed = out.Error.Code, out.Error.Need
				}
				if gotCode != code || gotNeed != need {
					t.Errorf("%s/%s/%s: got %q/%q, want %q/%q", tier, role, c.tool, gotCode, gotNeed, code, need)
				}
				if code != "" && f.networkCalls() != 0 {
					t.Errorf("%s/%s/%s: refused call still reached Fleet", tier, role, c.tool)
				}
			}
		}
	}
}

// Writes refuse locally during lockdown, org pause or a blocking
// approval; reads still go to Fleet.
func TestHeldStatesBlockWritesOnly(t *testing.T) {
	states := map[string]daemon.Status{
		CodeLockdown:         {Lockdown: true},
		CodeOrgPaused:        {OrgPaused: true, PausedCategory: "billing"},
		CodeBlockingApproval: {Lanes: []daemon.LaneStatus{{Name: "telemetry", State: "held", Reason: "approval_pending"}}},
	}
	for want, st := range states {
		f := newFake("enterprise", "org_admin", platform)
		cs := session(t, f, fakeStatus{st}, "")
		var sh ShareOutput
		call(t, cs, ToolShare, map[string]any{"title": "t", "body": "b", "kind": "fact", "user_confirmed": true}, &sh)
		var pr PromoteOutput
		call(t, cs, ToolPromote, map[string]any{"node_id": "n", "user_confirmed": true}, &pr)
		if sh.Error == nil || sh.Error.Code != want || pr.Error == nil || pr.Error.Code != want {
			t.Errorf("%s: share=%+v promote=%+v", want, sh.Error, pr.Error)
		}
		if f.networkCalls() != 0 {
			t.Errorf("%s: write reached Fleet", want)
		}
		var se SearchOutput
		call(t, cs, ToolSearch, map[string]any{"query": "q"}, &se)
		if se.Error != nil || len(f.searches) != 1 {
			t.Errorf("%s: read blocked: %+v", want, se.Error)
		}
	}
}

func TestFleetErrorsMapped(t *testing.T) {
	cases := map[string]struct {
		err  error
		code string
	}{
		"not signed in":   {fleetapi.ErrNotSignedIn, CodeNotSignedIn},
		"unofficial":      {fleetapi.ErrUnofficialBuild, CodeUnofficialBuild},
		"org scope":       {&fleetapi.APIError{Status: 403, Code: "org_scope_requires_enterprise"}, CodeTierRequired},
		"paused":          {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused, Details: map[string]string{"paused_category": "x"}}, CodeOrgPaused},
		"not provisioned": {&fleetapi.APIError{Status: 403, Code: "user_not_provisioned"}, CodeNotSignedIn},
		"expired":         {&fleetapi.APIError{Status: 401}, CodeNotSignedIn},
		"other":           {&fleetapi.APIError{Status: 500, Code: "boom"}, CodeFleetError},
	}
	for name, c := range cases {
		f := newFake("team", "member", platform)
		f.opErr = c.err
		cs := session(t, f, nil, "")
		var out SearchOutput
		call(t, cs, ToolSearch, map[string]any{"query": "q"}, &out)
		if out.Error == nil || out.Error.Code != c.code {
			t.Errorf("%s: %+v", name, out.Error)
		}
	}
	// Sign-in failure on /me stops before any content call.
	f := newFake("team", "member", platform)
	f.meErr = fleetapi.ErrNotSignedIn
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "t", "body": "b", "kind": "fact", "user_confirmed": true}, &out)
	if out.Error == nil || out.Error.Code != CodeNotSignedIn || f.networkCalls() != 0 {
		t.Fatalf("me error: %+v", out.Error)
	}
}

func TestTeamMembers(t *testing.T) {
	f := newFake("team", "member", platform, payments)
	f.members = []fleetapi.Member{{DisplayName: "Ada", Role: "admin"}, {DisplayName: "bob@example.com", Role: "member"}}
	cs := session(t, f, nil, "Platform")
	var out TeamMembersOutput
	if call(t, cs, ToolTeamMembers, map[string]any{}, &out); out.Error != nil {
		t.Fatalf("members: %+v", out.Error)
	}
	if out.Team != "Platform" || len(out.Teams) != 2 || len(out.Members) != 2 {
		t.Fatalf("out = %+v", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "@") {
		t.Fatalf("email leaked: %s", raw)
	}
}
