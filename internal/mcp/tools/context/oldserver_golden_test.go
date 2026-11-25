package contexttools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
)

// oldServerGolden is what every context tool answered, byte for byte, before the
// plugin read TeamRef.Membership, for a Fleet that sends no membership on any team.
// The file was written by running this same test on the commit before that change
// (KENAZ_UPDATE_GOLDEN=1); the test fails if any answer differs now. Regenerate it
// only from a commit known to hold the behaviour to keep.
const oldServerGolden = "testdata/oldserver_golden.json"

// oldServerFixtures are /me team lists as older Fleets send them: no Membership on
// any row, with and without the team tree fields.
func oldServerFixtures() map[string][]fleetapi.TeamRef {
	return map[string][]fleetapi.TeamRef{
		"no-teams":      nil,
		"one-flat":      {{ID: "team-plat", Name: "Platform"}},
		"two-flat":      {{ID: "team-plat", Name: "Platform"}, {ID: "team-pay", Name: "Payments"}},
		"tree-no-field": fleetapitest.SampleTeams(),
	}
}

func rawCall(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) json.RawMessage {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var text []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text = append(text, tc.Text)
		}
	}
	raw, err := json.Marshal(map[string]any{"is_error": res.IsError, "structured": res.StructuredContent, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOldServerAnswersAreUnchanged(t *testing.T) {
	got := map[string]json.RawMessage{}
	put := func(key string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got[key] = raw
	}
	for name, teams := range oldServerFixtures() {
		// Arguments a caller might pass for a team: nothing, a name in another case, an
		// id, and a team that isn't there.
		wants := []string{"", "Nope"}
		for _, tm := range teams {
			wants = append(wants, tm.ID, "  "+tm.Name+" ")
		}
		for _, def := range []string{"", "Payments", "Gone"} {
			for _, want := range wants {
				key := name + "/default=" + def + "/team=" + want + "/"
				args := func(extra map[string]any) map[string]any {
					if want != "" {
						extra["team"] = want
					}
					return extra
				}
				f := newFake("enterprise", "org_admin", teams...)
				f.members = []fleetapi.Member{{DisplayName: "Ada", Role: "lead"}}
				cs := session(t, f, nil, def)
				got[key+ToolTeamMembers] = rawCall(t, cs, ToolTeamMembers, args(map[string]any{}))
				got[key+ToolSearch] = rawCall(t, cs, ToolSearch, args(map[string]any{"query": "queue"}))
				got[key+ToolPull] = rawCall(t, cs, ToolPull, args(map[string]any{"ids": []string{"n1"}}))
				before := len(f.approvals.reqs)
				got[key+ToolShare] = rawCall(t, cs, ToolShare, args(map[string]any{"title": "Runbook", "body": "Restart the queue first.", "kind": "guidance"}))
				if len(f.approvals.reqs) > before {
					req := f.approvals.last()
					put(key+ToolShare+"/request", map[string]any{"targets": req.TargetIDs, "summary": req.Summary, "impact": req.Impact, "payload": json.RawMessage(req.Payload)})
				}
				put(key+"calls", map[string]any{"searches": f.searches, "pulls": f.pulls, "pushes": f.pushes})

				// A file share, through the approval a person gives.
				d := newDesk(t, newGraph(), "u1", teams...)
				d.opts.DefaultTeam = func() string { return def }
				d.put(t, "a.md", "one\n")
				out := d.share(ShareFileInput{Path: "personal/a.md", Team: want})
				put(key+"share_file", out)
				if out.Error == nil {
					req := d.m.approvals.last()
					put(key+"share_file/request", map[string]any{"targets": req.TargetIDs, "summary": req.Summary, "impact": req.Impact})
					view, res, err := d.approve(t)
					errText := ""
					if err != nil {
						errText = err.Error()
					}
					put(key+"share_file/approved", map[string]any{"summary": view.Summary, "impact": view.Impact, "result": json.RawMessage(res), "error": errText, "nodes": len(d.m.graph.nodes)})
				}
				put(key+"pull_files", d.pull(PullFilesInput{Team: want}))
			}
		}
	}
	for name, teams := range oldServerFixtures() {
		if len(teams) == 0 {
			continue
		}
		for _, tm := range teams {
			key := name + "/team=" + tm.ID + "/"

			// The context share grant, on: the share runs through the relay's own
			// check and executor, with no approval. Request ids are random, so they
			// are left out; the history line is what the grant recorded.
			gd := newGrantDesk(t, newGraph(), "u1", teams...)
			gd.on(t)
			gd.put(t, "a.md", "one\n")
			out := gd.share(ShareFileInput{Path: "personal/a.md", Team: tm.ID})
			out.RequestID, out.ApproveURL = "", ""
			put(key+"grant/share_file", out)
			var lines []map[string]any
			for _, e := range gd.grant.History(gd.acct) {
				lines = append(lines, map[string]any{"slug": e.Slug, "team_id": e.TeamID, "team_name": e.TeamName, "status": e.Status, "sha256": e.SHA256})
			}
			put(key+"grant/history", map[string]any{"lines": lines, "used": gd.grant.UsedThisHour(gd.acct, fixedNow), "nodes": len(gd.desk.m.graph.nodes), "pushes": gd.pushes()})
			// And an update of the same file, which the grant also takes.
			gd.put(t, "a.md", "two\n")
			out = gd.share(ShareFileInput{Path: "personal/a.md"})
			out.RequestID, out.ApproveURL = "", ""
			put(key+"grant/update", out)

			// A sync of the team's folder: a teammate's file comes down, a new local
			// file is planned and sent, and a second run finds nothing to do.
			g := newGraph()
			author := newDesk(t, g, "u2", teams...)
			author.put(t, "theirs.md", "from a teammate\n")
			if o := author.share(ShareFileInput{Path: "personal/theirs.md", Team: tm.ID}); o.Error != nil {
				t.Fatalf("%+v", o.Error)
			}
			if _, _, err := author.approve(t); err != nil {
				t.Fatal(err)
			}
			d := newDesk(t, g, "u1", teams...)
			d.opts.Rand = strings.NewReader(strings.Repeat("sync-device\x5a", 512))
			acct := contextlib.Account{Env: string(fleetapi.EnvDev), OrgID: "o1", UserID: "u1"}
			set := d.opts.set().WithViewer(&acct)
			for run := 1; run <= 2; run++ {
				sy := &Syncer{Store: NodeStore(listing{d.m}, nil, d.led), Set: set, Led: d.led, Acct: acct, Team: tm, Teams: TeamsOf(teams)}
				pulled, err := sy.Pull(d.ctx())
				if err != nil {
					t.Fatal(err)
				}
				if run == 1 {
					if _, err := set.Write("team/"+sy.Folder()+"/mine.md", []byte("mine\n"), contextlib.WriteOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				plan, err := sy.Plan(d.ctx())
				if err != nil {
					t.Fatal(err)
				}
				var sent []map[string]any
				for _, it := range plan.Items {
					res, err := sy.Send(d.ctx(), it)
					sent = append(sent, map[string]any{"name": it.Name, "new": it.New, "outcome": res.Outcome, "error": err != nil})
				}
				put(fmt.Sprintf("%ssync/run%d", key, run), map[string]any{
					"folder": sy.Folder(), "saved": pulled.Saved, "updated": pulled.Updated, "failed": pulled.Failed, "notes": pulled.Notes,
					"legacy_files": pulled.Legacy.Files, "unreadable": pulled.Legacy.Unreadable, "claimed": pulled.Legacy.Claim != nil,
					"unchanged": plan.Unchanged, "refused": len(plan.Refused), "other_team": plan.OtherTeam, "sent": sent, "nodes": len(g.nodes),
				})
			}
		}
	}
	// encoding/json writes map keys in order, so the file is stable.
	want, err := json.MarshalIndent(got, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("KENAZ_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(oldServerGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(oldServerGolden, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(oldServerGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		var old map[string]json.RawMessage
		_ = json.Unmarshal(have, &old)
		for k, v := range got {
			var a, b bytes.Buffer
			_ = json.Compact(&a, v)
			_ = json.Compact(&b, old[k])
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Errorf("%s\n now: %s\n was: %s", k, a.Bytes(), b.Bytes())
			}
		}
		t.Fatalf("answers for a Fleet that sends no membership changed (%d now, %d before)", len(got), len(old))
	}
}
