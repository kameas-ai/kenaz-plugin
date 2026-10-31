package contexttools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
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
	// The host prompt is not proof of confirmation: the write itself only
	// happens after approval on the page (tested below).
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
	if call(t, cs, ToolShare, args, &out); out.Error != nil {
		t.Fatalf("share: %+v", out.Error)
	}
	if out.Status != "pending" || out.RequestID == "" || !strings.HasPrefix(out.ApproveURL, "http://localhost:") || f.networkCalls() != 0 {
		t.Fatalf("share must only prepare: %+v calls=%d", out, f.networkCalls())
	}
	r := f.approvals.last()
	if r.Operation != ShareOperation || r.UserID != "sub-1" || r.OrgID != "o1" || r.Env != "dev" || r.TargetIDs[0] != "team:Platform" {
		t.Fatalf("request = %+v", r)
	}

	// The relay executes exactly the prepared node after approval.
	res, err := ExecuteShare(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0].TeamID != "team-plat" || f.pushes[0].Title != "Flaky e2e" || f.pushes[0].Body != "Retry with -count=3." || f.pushes[0].LoadPolicy != "on_demand" {
		t.Fatalf("push = %+v", f.pushes)
	}
	if string(res) != `{"classification":"team_shared","id":"srv-1"}` {
		t.Fatalf("result = %s", res)
	}
}

// A model-supplied user_confirmed=true is not confirmation: the call still
// only prepares a request, and nothing reaches Fleet.
func TestModelSuppliedUserConfirmedIgnored(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	var sh ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "t", "body": "b", "kind": "fact", "user_confirmed": true}, &sh)
	var pr PromoteOutput
	call(t, cs, ToolPromote, map[string]any{"node_id": "n1", "user_confirmed": true}, &pr)
	if sh.Status != "pending" || pr.Status != "pending" || f.writeCalls() != 0 {
		t.Fatalf("share=%+v promote=%+v writes=%d", sh, pr, f.writeCalls())
	}
}

// Credentials are not scanned on the device. Kameas Fleet's context push
// answers 422 lint_blocked for a note that looks like it holds one; the
// executor records only the machine code, never the note.
func TestShareFleetCredentialRejection(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	f.opErr = &fleetapi.APIError{Status: http.StatusUnprocessableEntity, Code: "lint_blocked", Message: "content blocked by lint"}
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "note", "body": "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "kind": "fact"}, &out)
	r := f.approvals.last()
	_, err := ExecuteShare(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey)
	if err == nil || !strings.Contains(err.Error(), CodeCredentialDetected) || strings.Contains(err.Error(), "AKIA") {
		t.Fatalf("error = %v", err)
	}
}

// Other 400/422 refusals keep only Fleet's machine reason, never its text;
// transport failures are an unknown outcome, not a refusal.
func TestShareFleetRejectionMessage(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "note", "body": "text", "kind": "fact"}, &out)
	r := f.approvals.last()
	f.opErr = &fleetapi.APIError{Status: http.StatusBadRequest, Code: "body_too_long", Message: "body exceeds the maximum length: SECRET-ECHO"}
	_, err := ExecuteShare(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey)
	if err == nil || !strings.Contains(err.Error(), CodeFleetRejected) || strings.Contains(err.Error(), "SECRET-ECHO") {
		t.Fatalf("error = %v", err)
	}
	f.opErr = io.ErrUnexpectedEOF
	if _, err := ExecuteShare(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); !errors.Is(err, approvals.ErrOutcomeUnknown) {
		t.Fatalf("transport error = %v", err)
	}
	// Reconciliation finds the node by its client-generated id.
	f.opErr = nil
	var node NodeInput
	json.Unmarshal(r.Payload, &node)
	f.nodes = []fleetapi.NodeContent{{Node: fleetapi.ContextNode{ID: node.ID, Classification: "team_shared"}}}
	if _, applied, err := ReconcileShare(f)(context.Background(), r.Payload, r.IdempotencyKey); !applied || err != nil {
		t.Fatalf("reconcile: %v %v", applied, err)
	}
}

func TestPromote(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	var out PromoteOutput
	if call(t, cs, ToolPromote, map[string]any{"node_id": "n1"}, &out); out.Error != nil || out.Status != "pending" {
		t.Fatalf("promote: %+v", out)
	}
	if len(f.promotes) != 0 {
		t.Fatal("promote ran before approval")
	}
	r := f.approvals.last()
	if _, err := ExecutePromote(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if f.promotes[0].NodeID != "n1" || f.promotes[0].To != fleetapi.ClassificationOrgShared {
		t.Fatalf("promote = %+v", f.promotes)
	}
	// The approval page names the note and its current team scope.
	v, err := RenderPromote(func(string) string { return "Platform" })(r.Payload)
	if err != nil || !strings.Contains(v.Summary, `"Runbook"`) || !strings.Contains(v.Summary, "Platform (team-plat)") || !strings.Contains(v.Impact, "whole organization") {
		t.Fatalf("render: %+v %v", v, err)
	}
	if _, err := RenderPromote(nil)([]byte(`{"node_id":"n1","to":"org_shared"}`)); err == nil {
		t.Fatal("rendered a promote without title or team")
	}
	// Reconciliation: applied once the node is organization-shared.
	if _, applied, _ := ReconcilePromote(f)(context.Background(), r.Payload, ""); applied {
		t.Fatal("reconciler claims a team note was promoted")
	}
	f.nodes[0].Node.Classification = fleetapi.ClassificationOrgShared
	if _, applied, err := ReconcilePromote(f)(context.Background(), r.Payload, ""); !applied || err != nil {
		t.Fatalf("reconcile: %v %v", applied, err)
	}
	f.nodes[0].Node.Classification = fleetapi.ClassificationTeamShared
	// A note renamed after the request was prepared is not promoted.
	f.nodes[0].Node.Title = "Renamed"
	if _, err := ExecutePromote(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); err == nil || !strings.Contains(err.Error(), "node_changed") {
		t.Fatalf("changed note promoted: %v", err)
	}
	f.nodes[0].Node.Title = "Runbook"
	// Nor is a note whose text changed, and the page shows the text.
	if len(v.Details) != 1 || v.Details[0].Text != "Restart the queue first." {
		t.Fatalf("details: %+v", v.Details)
	}
	f.nodes[0].Body = "Delete the queue first."
	if _, err := ExecutePromote(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); err == nil || !strings.Contains(err.Error(), "node_changed") {
		t.Fatalf("edited note promoted: %v", err)
	}
	f.nodes[0].Body = "Restart the queue first."
	// The executor rechecks role at execution time.
	f.me.Role = "member"
	if _, err := ExecutePromote(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); err == nil || len(f.promotes) != 1 {
		t.Fatalf("demoted admin promoted: %v", err)
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
		"plan required":   {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: map[string]string{"capability": fleetapi.PluginCapability}}, CodePlanRequired},
		"legacy refusal":  {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeLegacyEarlyAccessRequired}, CodePlanRequired},
		"feature limit":   {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: map[string]string{"capability": CapOrgGraphSharing}}, CodeTierRequired},
		"unnamed limit":   {&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier}, CodeTierRequired},
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
		if c.code == CodePlanRequired && (out.Error.Message != fleetapi.PlanRequiredMessage || out.Error.Remedy != "") {
			t.Errorf("%s: message %q remedy %q", name, out.Error.Message, out.Error.Remedy)
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

// approvedCtx is the context the relay runs an executor in for the fake's
// approved request.
var approvedCtx = approvals.ContextWithIdentity(context.Background(), approvals.Identity{Env: "dev", OrgID: "o1", UserID: "sub-1"})

func TestExecutorsRecheckAccount(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	var out ShareOutput
	call(t, cs, ToolShare, map[string]any{"title": "t", "body": "b", "kind": "fact"}, &out)
	r := f.approvals.last()
	other := approvals.ContextWithIdentity(context.Background(), approvals.Identity{Env: "dev", OrgID: "o1", UserID: "sub-2"})
	if _, err := ExecuteShare(f, nil)(other, r.Payload, r.IdempotencyKey); err == nil || !strings.Contains(err.Error(), "account_changed") {
		t.Fatalf("share: %v", err)
	}
	pl, _ := json.Marshal(promotePayload{NodeID: "n1", To: "org_shared", TeamID: "team-plat", Title: "Runbook", Body: "Restart the queue first."})
	if _, err := ExecutePromote(f, nil)(other, pl, "k"); err == nil {
		t.Fatal("promote for another account")
	}
	if f.networkCalls() != 0 {
		t.Fatal("wrote for another account")
	}
	// The page text comes from the payload, not from the tool's summary.
	v, err := RenderShare(func(string) string { return "Platform" })(r.Payload)
	if err != nil || !strings.Contains(v.Summary, `"t"`) || !strings.Contains(v.Summary, "Platform (team-plat)") {
		t.Fatalf("render: %+v %v", v, err)
	}
	if _, err := RenderShare(nil)([]byte(`{"id":"x","team_id":"t","classification":"org_shared","load_policy":"always","kind":"fact"}`)); err == nil {
		t.Fatal("rendered a payload the executor would refuse")
	}
}

// A per-feature capability_not_in_tier shows the per-feature text, never
// the whole-plugin plan-required text.
func TestMapErrorPerFeatureTierLimit(t *testing.T) {
	cases := map[string]struct {
		details map[string]string
		need    string
		msg     string
	}{
		"named":  {map[string]string{"capability": CapOrgGraphSharing}, CapOrgGraphSharing, fleetapi.TierLimitMessage + " (feature: org_graph_sharing)"},
		"none":   {nil, "", fleetapi.TierLimitMessage},
		"unsafe": {map[string]string{"capability": "Org Graph <b>"}, "", fleetapi.TierLimitMessage},
	}
	for name, c := range cases {
		f := newFake("team", "member", platform)
		f.opErr = &fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text", Details: c.details}
		cs := session(t, f, nil, "")
		var out SearchOutput
		call(t, cs, ToolSearch, map[string]any{"query": "q"}, &out)
		e := out.Error
		if e == nil || e.Code != CodeTierRequired || e.Need != c.need || e.Message != c.msg {
			t.Errorf("%s: %+v", name, e)
			continue
		}
		if e.Message == fleetapi.PlanRequiredMessage || strings.Contains(e.Message, "fleet text") {
			t.Errorf("%s: message %q", name, e.Message)
		}
	}
}
