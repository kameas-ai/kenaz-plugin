package contexttools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Writers use Kameas Fleet's canonical kinds. The names this plugin used
// before (fact, glossary_term) are still taken and sent as the new ones.
func TestShareKindsAreCanonical(t *testing.T) {
	if got := strings.Join(ShareKinds, ","); got != "glossary,explanation,guidance,procedure,doc" {
		t.Fatalf("ShareKinds = %s", got)
	}
	for in, want := range map[string]string{
		"glossary": "glossary", "explanation": "explanation", "guidance": "guidance", "procedure": "procedure", "doc": "doc",
		"fact": "explanation", "glossary_term": "glossary",
	} {
		if got, ok := CanonicalShareKind(in); !ok || got != want {
			t.Errorf("CanonicalShareKind(%q) = %q, %v; want %q", in, got, ok, want)
		}
		n, err := BuildShareNode(ShareInput{Title: "t", Body: "b", Kind: in}, "team-1", fixedNow, bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
		if err != nil || n.Kind != want || n.UnitKind != "doc" {
			t.Errorf("BuildShareNode(%q) = kind %q unit_kind %q, %v; want %q", in, n.Kind, n.UnitKind, err, want)
		}
		f, err := BuildFileNode(ShareInput{Title: "t", Body: "b", Kind: in}, "a.md", "team-1", "id-1", 1)
		if err != nil || f.Kind != want || f.UnitKind != "doc" {
			t.Errorf("BuildFileNode(%q) = kind %q unit_kind %q, %v; want %q", in, f.Kind, f.UnitKind, err, want)
		}
	}
	for _, bad := range []string{"", "decision", "Fact", "glossary_terms"} {
		if _, ok := CanonicalShareKind(bad); ok {
			t.Errorf("CanonicalShareKind(%q) was taken", bad)
		}
		_, err := BuildShareNode(ShareInput{Title: "t", Body: "b", Kind: bad}, "team-1", fixedNow, bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
		if err == nil || err.Error() != "kind must be one of glossary, explanation, guidance, procedure, doc" {
			t.Errorf("BuildShareNode(%q) = %v", bad, err)
		}
	}
}

// An agent or script that still passes an old name works end to end: the
// approval page and the push both carry the new name.
func TestShareToolTakesOldKindNames(t *testing.T) {
	for old, want := range map[string]string{"fact": "explanation", "glossary_term": "glossary"} {
		f := newFake("team", "member", platform)
		cs := session(t, f, nil, "")
		var out ShareOutput
		if call(t, cs, ToolShare, map[string]any{"title": "T", "body": "b", "kind": old}, &out); out.Error != nil {
			t.Fatalf("%s: %+v", old, out.Error)
		}
		r := f.approvals.last()
		if !strings.HasPrefix(r.Summary, map[string]string{"explanation": "Share an explanation note titled", "glossary": "Share a glossary note titled"}[want]) || !bytes.Contains(r.Payload, []byte(`"kind":"`+want+`"`)) {
			t.Fatalf("%s: summary %q payload %s", old, r.Summary, r.Payload)
		}
		if _, err := ExecuteShare(f, nil)(approvedCtx, r.Payload, r.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
		if len(f.pushes) != 1 || f.pushes[0].Kind != want || f.pushes[0].UnitKind != "doc" {
			t.Fatalf("%s: push = %+v", old, f.pushes)
		}
	}
}

// A share prepared by an older build and approved after the upgrade still
// holds the old name. It is still a valid share, shown and sent as the new one.
func TestOldKindInAStoredShare(t *testing.T) {
	stored := []byte(`{"id":"x","kind":"fact","title":"T","body":"b","metadata":{},"classification":"team_shared","team_id":"team-plat","version":1,"unit_kind":"doc","unit_scope":"global","load_policy":"on_demand"}`)
	view, err := RenderShare(nil)(stored)
	if err != nil || !strings.Contains(view.Summary, "Share an explanation note titled") {
		t.Fatalf("render = %+v, %v", view, err)
	}
	f := newFake("team", "member", platform)
	if _, err := ExecuteShare(f, nil)(approvedCtx, stored, ShareOperation+":x"); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0].Kind != "explanation" {
		t.Fatalf("push = %+v", f.pushes)
	}
	var n NodeInput
	json.Unmarshal(stored, &n)
	n.Kind = "glossary_term"
	if pr := ToPushRequest(n); pr.Kind != "glossary" {
		t.Fatalf("push kind = %q", pr.Kind)
	}
}

// context_share_file takes the old names too.
func TestShareFileTakesOldKindNames(t *testing.T) {
	g := newGraph()
	d := newDesk(t, g, "u1", platform)
	d.put(t, "Terms.md", "one\n")
	if out := d.share(ShareFileInput{Path: "personal/Terms.md", Kind: "glossary_term"}); out.Error != nil {
		t.Fatalf("%+v", out.Error)
	}
	if _, _, err := d.approve(t); err != nil {
		t.Fatal(err)
	}
	if n := onlyNode(t, g); n.req.Kind != "glossary" || n.req.UnitKind != "doc" {
		t.Fatalf("%+v", n.req)
	}
	if out := d.share(ShareFileInput{Path: "personal/Terms.md", Kind: "decision"}); out.Error == nil || !strings.Contains(out.Error.Message, "glossary, explanation, guidance, procedure, doc") {
		t.Fatalf("%+v", out)
	}
}

// Readers never drop or refuse a node over its kind: old names read as the
// new ones, and a kind this build doesn't know reads as doc.
func TestReadersKeepEveryKind(t *testing.T) {
	kinds := []string{"glossary", "explanation", "guidance", "procedure", "doc", "fact", "glossary_term", "decision", ""}
	want := []string{"glossary", "explanation", "guidance", "procedure", "doc", "explanation", "glossary", "doc", "doc"}
	for i, k := range kinds {
		if got := ReadKind(k); got != want[i] {
			t.Errorf("ReadKind(%q) = %q, want %q", k, got, want[i])
		}
	}

	f := newFake("team", "member", platform)
	f.nodes = nil
	ids := make([]string, len(kinds))
	for i, k := range kinds {
		ids[i] = string(rune('a' + i))
		node := fleetapi.ContextNode{ID: ids[i], Title: "t", Kind: k, Classification: "team_shared"}
		// Search answers best first; ranks keep the order of kinds.
		f.search = append(f.search, fleetapi.SearchResult{Node: node, Snippet: "s", Rank: float64(len(kinds) - i)})
		f.nodes = append(f.nodes, fleetapi.NodeContent{Node: node, Body: "b"})
	}
	cs := session(t, f, nil, "")
	var so SearchOutput
	if call(t, cs, ToolSearch, map[string]any{"query": "q", "limit": MaxSearchLimit}, &so); so.Error != nil || len(so.Results) != len(kinds) {
		t.Fatalf("search dropped a result: %+v", so)
	}
	for i, h := range so.Results {
		if h.ID != ids[i] || h.Kind != want[i] {
			t.Errorf("search result %d (%q) = %q %q, want %q", i, kinds[i], h.ID, h.Kind, want[i])
		}
	}
	var po PullOutput
	if call(t, cs, ToolPull, map[string]any{"ids": ids}, &po); po.Error != nil || len(po.Nodes) != len(kinds) || len(po.Missing) != 0 {
		t.Fatalf("pull dropped a node: %+v", po)
	}
	for i, n := range po.Nodes {
		if n.ID != ids[i] || n.Kind != want[i] {
			t.Errorf("pulled node %d (%q) = %q %q, want %q", i, kinds[i], n.ID, n.Kind, want[i])
		}
	}
}

// context_pull_files saves a shared file whatever its kind.
func TestPullFilesKeepsEveryKind(t *testing.T) {
	for _, kind := range []string{"fact", "glossary_term", "decision", ""} {
		g := newGraph()
		first := newDesk(t, g, "u1", platform)
		first.put(t, "My Doc.md", "one\n")
		first.share(ShareFileInput{Path: "personal/My Doc.md"})
		if _, _, err := first.approve(t); err != nil {
			t.Fatal(err)
		}
		g.mu.Lock()
		for _, n := range g.nodes {
			n.req.Kind = kind
		}
		g.mu.Unlock()
		second := newDesk(t, g, "u2", platform)
		got := runPullFiles(context.Background(), listing{second.m}, nil, second.opts, PullFilesInput{})
		if got.Error != nil || len(got.Files) != 1 || got.Files[0].Path != "team/platform/My-Doc.md" {
			t.Fatalf("kind %q: %+v", kind, got)
		}
	}
}
