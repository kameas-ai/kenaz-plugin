package contexttools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// tierCaps snapshots Fleet's per-tier capability table for the
// graph capabilities. "free" has no tier entry.
var tierCaps = map[string][]string{
	"free":       nil,
	"pro":        nil,
	"team":       {CapSharedTeamGraph, CapTeamGraphSharing},
	"enterprise": {CapSharedTeamGraph, CapTeamGraphSharing, CapOrgGraphSharing},
}

// fakeFleet is a recording fleetapi.Provider. Unused interfaces fall back
// to the stub (which refuses everything).
type fakeFleet struct {
	fleetapi.Provider
	mu       sync.Mutex
	me       fleetapi.MeInfo
	caps     fleetapi.Capabilities
	meErr    error
	search   []fleetapi.SearchResult
	nodes    []fleetapi.NodeContent
	members  []fleetapi.Member
	opErr    error
	calls    []string
	searches []fleetapi.SearchRequest
	pulls    []fleetapi.PullRequest
	pushes   []fleetapi.PushRequest
	promotes []fleetapi.PromoteRequest
}

func newFake(tier, role string, teams ...fleetapi.TeamRef) *fakeFleet {
	keys := map[string]bool{}
	for _, k := range tierCaps[tier] {
		keys[k] = true
	}
	return &fakeFleet{
		Provider: stub.New(),
		me:       fleetapi.MeInfo{UserID: "u1", OrgID: "o1", Role: role, Tier: tier, Teams: teams},
		caps:     fleetapi.Capabilities{Keys: keys},
	}
}

func (f *fakeFleet) record(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }

// networkCalls counts calls that would carry user content to Fleet.
func (f *fakeFleet) networkCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.searches) + len(f.pulls) + len(f.pushes) + len(f.promotes)
}

func (f *fakeFleet) Me() fleetapi.Me             { return fakeMe{f} }
func (f *fakeFleet) Context() fleetapi.Context   { return fakeCtx{f} }
func (f *fakeFleet) Team() fleetapi.Team         { return fakeTeam{f} }
func (f *fakeFleet) Flavor() fleetapi.Flavor     { return f.Provider.Flavor() }
func (f *fakeFleet) Pollers() fleetapi.Pollers   { return f.Provider.Pollers() }
func (f *fakeFleet) Catalog() fleetapi.Catalog   { return f.Provider.Catalog() }
func (f *fakeFleet) OTLP() fleetapi.OTLPPoster   { return f.Provider.OTLP() }
func (f *fakeFleet) OptIns() fleetapi.OptIns     { return f.Provider.OptIns() }
func (f *fakeFleet) Enroller() fleetapi.Enroller { return f.Provider.Enroller() }
func (f *fakeFleet) Auth() fleetapi.Auth         { return f.Provider.Auth() }

type fakeMe struct{ f *fakeFleet }

func (m fakeMe) Get(context.Context) (fleetapi.MeInfo, error) {
	m.f.record("me")
	return m.f.me, m.f.meErr
}
func (m fakeMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	m.f.record("caps")
	return m.f.caps, m.f.meErr
}

type fakeCtx struct{ f *fakeFleet }

func (c fakeCtx) Search(_ context.Context, r fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	c.f.mu.Lock()
	c.f.searches = append(c.f.searches, r)
	c.f.mu.Unlock()
	return c.f.search, c.f.opErr
}
func (c fakeCtx) Pull(_ context.Context, r fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	c.f.mu.Lock()
	c.f.pulls = append(c.f.pulls, r)
	c.f.mu.Unlock()
	return c.f.nodes, c.f.opErr
}
func (c fakeCtx) Push(_ context.Context, r fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	c.f.mu.Lock()
	c.f.pushes = append(c.f.pushes, r)
	c.f.mu.Unlock()
	return fleetapi.ContextNode{ID: "srv-1", Title: r.Title, Classification: fleetapi.ClassificationTeamShared, TeamID: r.TeamID}, c.f.opErr
}
func (c fakeCtx) Promote(_ context.Context, r fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	c.f.mu.Lock()
	c.f.promotes = append(c.f.promotes, r)
	c.f.mu.Unlock()
	return fleetapi.PromoteResult{Node: fleetapi.ContextNode{ID: r.NodeID, Classification: r.To}, Warnings: []string{"w"}}, c.f.opErr
}

type fakeTeam struct{ f *fakeFleet }

func (t fakeTeam) Members(context.Context, string) ([]fleetapi.Member, error) {
	t.f.record("members")
	return t.f.members, t.f.opErr
}

type fakeStatus struct{ st daemon.Status }

func (s fakeStatus) Status(context.Context) (daemon.Status, error) { return s.st, nil }

var fixedNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// session connects an MCP client to the context group over memory.
func session(t *testing.T, f fleetapi.Provider, st mcp.StatusSource, defaultTeam string) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	g := NewGroup(Options{DefaultTeam: func() string { return defaultTeam }, Now: func() time.Time { return fixedNow }, Rand: strings.NewReader(strings.Repeat("\x5a", 64))})
	srv := mcp.NewServer("test", mcp.Deps{Provider: f, Env: fleetapi.EnvDev, Status: st}, g)
	ct, stT := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, stT, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes tool and decodes the structured output into out.
func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, out any) (isError bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, raw, err)
	}
	return res.IsError
}
