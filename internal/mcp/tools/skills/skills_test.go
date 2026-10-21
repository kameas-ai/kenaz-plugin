package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	sk "github.com/kameas-ai/kenaz-plugin/internal/skills"
)

type fake struct {
	fleetapi.Provider
	items     map[string]fleetapi.CatalogItem
	published []fleetapi.PublishRequest
	caps      map[string]bool
}

func (f *fake) Catalog() fleetapi.Catalog { return cat{f} }
func (f *fake) Me() fleetapi.Me           { return me{f} }

type cat struct{ f *fake }

func (c cat) List(_ context.Context, q fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	var out []fleetapi.CatalogEntry
	for _, it := range c.f.items {
		if it.Kind == q.Kind {
			out = append(out, it.CatalogEntry)
		}
	}
	return out, nil
}
func (c cat) Get(_ context.Context, id, _ string) (fleetapi.CatalogItem, error) {
	it, ok := c.f.items[id]
	if !ok {
		return fleetapi.CatalogItem{}, &fleetapi.APIError{Status: 404}
	}
	return it, nil
}
func (c cat) Publish(_ context.Context, r fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	c.f.published = append(c.f.published, r)
	return fleetapi.CatalogEntry{ID: "new-1", Slug: r.Slug, Version: r.Version, Visibility: r.Visibility}, nil
}

type me struct{ f *fake }

func (m me) Get(context.Context) (fleetapi.MeInfo, error) { return fleetapi.MeInfo{}, nil }
func (m me) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: m.f.caps}, nil
}

func setup(t *testing.T) (*fake, mcp.Deps, string) {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	old := HomeDir
	HomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { HomeDir = old })
	doc := "---\nname: team-review\ndescription: Team review checklist.\nallowed-tools: Bash(*)\n---\nCheck it.\n"
	payload, _ := json.Marshal(sk.AgentSkill{Schema: sk.SchemaV1, Name: "team-review", Description: "Team review checklist.", Files: map[string]string{"SKILL.md": doc}})
	f := &fake{Provider: stub.New(), caps: map[string]bool{}, items: map[string]fleetapi.CatalogItem{
		"cat-1": {CatalogEntry: fleetapi.CatalogEntry{ID: "cat-1", Slug: "team-review", Kind: fleetapi.CatalogKindAgentSkill, Version: "2", Visibility: fleetapi.VisibilityTeam}, Payload: payload},
	}}
	return f, mcp.Deps{Provider: f, Env: fleetapi.EnvDev, StateDir: t.TempDir()}, home
}

func connect(t *testing.T, d mcp.Deps) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer("test", d, Group)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// skills_publish carries requiresUserInteraction, and
// no other tool does (install/search must stay usable without a prompt).
func TestPublishRequiresUserInteraction(t *testing.T) {
	_, d, _ := setup(t)
	cs := connect(t, d)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
		got, _ := tool.Meta["anthropic/requiresUserInteraction"].(bool)
		if (tool.Name == "skills_publish") != got {
			t.Errorf("%s requiresUserInteraction = %v", tool.Name, got)
		}
	}
	for _, n := range []string{"skills_search", "skills_install", "skills_uninstall", "skills_publish"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		var msg []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				msg = append(msg, tc.Text)
			}
		}
		return map[string]any{"error": strings.Join(msg, " ")}, true
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, false
}

func TestInstallSearchUninstall(t *testing.T) {
	_, d, home := setup(t)
	cs := connect(t, d)
	out, isErr := call(t, cs, "skills_install", map[string]any{"catalog_id": "cat-1"})
	if isErr {
		t.Fatalf("install: %v", out)
	}
	if !strings.Contains(out["message"].(string), "allowed-tools") {
		t.Fatalf("install must say what was stripped: %v", out)
	}
	doc, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "kameas-team-review", "SKILL.md"))
	if err != nil || strings.Contains(string(doc), "allowed-tools") {
		t.Fatalf("installed doc = %q err=%v", doc, err)
	}
	subs, _ := sk.NewStore(d.StateDir).Subscriptions()
	if _, ok := subs["cat-1"]; !ok {
		t.Fatal("subscription not saved")
	}
	if !sk.NewStore(d.StateDir).ReloadNeeded() {
		t.Fatal("install did not bump the reload generation")
	}

	out, _ = call(t, cs, "skills_search", map[string]any{})
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["installed"] != true {
		t.Fatalf("search = %v", out)
	}

	out, isErr = call(t, cs, "skills_uninstall", map[string]any{"catalog_id": "cat-1"})
	if isErr || out["removed"].(float64) != 2 {
		t.Fatalf("uninstall = %v", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "kameas-team-review")); !os.IsNotExist(err) {
		t.Fatal("codex copy not removed")
	}
}

func TestUninstallMandatedRefused(t *testing.T) {
	f, d, home := setup(t)
	m := &sk.Materializer{Roots: sk.DetectRoots(home)}
	it := f.items["cat-1"]
	if r := m.Install(sk.Item{CatalogID: "cat-1", Kind: it.Kind, Slug: it.Slug, Version: "2", Payload: it.Payload, Source: sk.SourceMandated}); r.Status != fleetapi.AckApplied {
		t.Fatalf("seed: %+v", r)
	}
	cs := connect(t, d)
	out, isErr := call(t, cs, "skills_uninstall", map[string]any{"catalog_id": "cat-1"})
	if !isErr || !strings.Contains(out["error"].(string), "organization requires") {
		t.Fatalf("uninstall mandated = %v", out)
	}
	if _, err := sk.ReadMarker(filepath.Join(home, ".claude", "skills", "kameas-team-review")); err != nil {
		t.Fatal("mandated skill removed")
	}
}

func TestPublish(t *testing.T) {
	f, d, _ := setup(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: mine\ndescription: Mine.\n---\nx\n"), 0o644)
	cs := connect(t, d)

	out, isErr := call(t, cs, "skills_publish", map[string]any{"path": dir, "version": "1.0.0"})
	if isErr || out["visibility"] != "private" {
		t.Fatalf("publish = %v", out)
	}
	if len(f.published) != 1 || f.published[0].Kind != fleetapi.CatalogKindAgentSkill || f.published[0].Visibility != fleetapi.VisibilityPrivate {
		t.Fatalf("published = %+v", f.published)
	}
	if _, err := sk.ParseAgentSkill(f.published[0].Payload); err != nil {
		t.Fatalf("published payload invalid: %v", err)
	}

	// Team visibility needs catalog_team_share.
	out, isErr = call(t, cs, "skills_publish", map[string]any{"path": dir, "version": "1.0.1", "visibility": "team"})
	if !isErr || !strings.Contains(out["error"].(string), "Team plan") {
		t.Fatalf("team publish without capability = %v", out)
	}
	f.caps[capTeamShare] = true
	if _, isErr = call(t, cs, "skills_publish", map[string]any{"path": dir, "version": "1.0.1", "visibility": "team"}); isErr {
		t.Fatal("team publish with capability failed")
	}

	if out, isErr = call(t, cs, "skills_publish", map[string]any{"path": "relative/dir", "version": "1"}); !isErr {
		t.Fatalf("relative path accepted: %v", out)
	}
}
