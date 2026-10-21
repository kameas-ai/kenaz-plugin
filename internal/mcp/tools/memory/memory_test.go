package memorytools

import (
	"context"
	"encoding/json"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// panicProvider fails the test if any tool touches Kameas Fleet.
type panicProvider struct{ fleetapi.Provider }

type env struct {
	path    string
	enabled bool
	shown   int
	work    string
}

func session(t *testing.T, e *env) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	g := NewGroup(Options{
		Path:    e.path,
		Enabled: func() bool { return e.enabled },
		Disclose: func() string {
			e.shown++
			if e.shown == 1 {
				return memory.Disclosure(e.path)
			}
			return ""
		},
		Workdir: func() string { return e.work },
	})
	// A nil-embedding provider panics on any method call.
	srv := mcp.NewServer("test", mcp.Deps{Provider: panicProvider{}}, g)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "codex-mcp-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func newEnv(t *testing.T) *env {
	return &env{path: filepath.Join(t.TempDir(), "memory", "memory.db"), enabled: true, work: t.TempDir()}
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, out any) (isError bool, size int) {
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
	return res.IsError, len(raw)
}

func TestToolsEndToEnd(t *testing.T) {
	e := newEnv(t)
	cs := session(t, e)

	var saved SaveOutput
	if isErr, _ := call(t, cs, ToolSave, map[string]any{"title": "Retry policy", "body": "Use exponential backoff capped at 30 s.", "tags": []string{"net"}}, &saved); isErr || saved.ID == "" {
		t.Fatalf("save: %+v", saved)
	}
	if saved.Notice != memory.Disclosure(e.path) {
		t.Fatalf("first save must carry the disclosure, got %q", saved.Notice)
	}
	var again SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "Global", "body": "applies everywhere", "project": "global"}, &again)
	if again.Notice != "" || again.Project != "global" {
		t.Fatalf("second save: %+v", again)
	}

	var found SearchOutput
	call(t, cs, ToolSearch, map[string]any{"query": "backoff"}, &found)
	if len(found.Results) != 1 || found.Results[0].ID != saved.ID || found.Results[0].Snippet == "" {
		t.Fatalf("search: %+v", found)
	}

	var got GetOutput
	call(t, cs, ToolGet, map[string]any{"ids": []string{saved.ID, "missing"}}, &got)
	if len(got.Notes) != 1 || got.Notes[0].Body == "" || got.Notes[0].Source != memory.SourceCodex || len(got.Missing) != 1 {
		t.Fatalf("get: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), e.work) {
		t.Fatal("the local project path must not leave the tool")
	}

	var rec RecentOutput
	call(t, cs, ToolRecent, map[string]any{}, &rec)
	if len(rec.Entries) != 2 {
		t.Fatalf("recent: %+v", rec)
	}

	var del ForgetOutput
	if isErr, _ := call(t, cs, ToolForget, map[string]any{"id": saved.ID}, &del); isErr || !del.Deleted {
		t.Fatalf("forget: %+v", del)
	}
	if isErr, _ := call(t, cs, ToolForget, map[string]any{"id": saved.ID}, &del); !isErr || del.Error.Code != "not_found" {
		t.Fatalf("second forget: %+v", del)
	}
}

func TestOtherProjectIsolation(t *testing.T) {
	e := newEnv(t)
	cs := session(t, e)
	var s SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "alpha", "body": "zebra notes"}, &s)
	e.work = t.TempDir() // a different project
	var found SearchOutput
	call(t, cs, ToolSearch, map[string]any{"query": "zebra"}, &found)
	if len(found.Results) != 0 {
		t.Fatalf("leaked across projects: %+v", found)
	}
	call(t, cs, ToolSearch, map[string]any{"query": "zebra", "project": "all"}, &found)
	if len(found.Results) != 1 {
		t.Fatalf("project=all: %+v", found)
	}
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	cs := session(t, e)
	var out SaveOutput
	for name, args := range map[string]map[string]any{
		"big body":   {"title": "t", "body": strings.Repeat("b", memory.MaxBodyBytes+1)},
		"long title": {"title": strings.Repeat("t", memory.MaxTitleChars+1), "body": "b"},
		"bad scope":  {"title": "t", "body": "b", "project": "team"},
	} {
		if isErr, _ := call(t, cs, ToolSave, args, &out); !isErr || out.Error == nil || out.Error.Code != "invalid_argument" {
			t.Errorf("%s: %+v", name, out)
		}
	}
	call(t, cs, ToolSave, map[string]any{"title": "creds", "body": "token " + "gh" + "p_" + strings.Repeat("a", 36)}, &out)
	if out.Error == nil || out.Error.Message != "Not saved: this looks like it contains a secret." {
		t.Fatalf("secret: %+v", out)
	}
	if _, err := os.Stat(e.path); !os.IsNotExist(err) {
		t.Fatal("a refused first save created the store")
	}
	if e.shown != 0 {
		t.Fatal("a refused save consumed the disclosure")
	}
}

func TestDisabled(t *testing.T) {
	e := newEnv(t)
	e.enabled = false
	cs := session(t, e)
	for tool, args := range map[string]map[string]any{
		ToolSave:   {"title": "t", "body": "b"},
		ToolSearch: {"query": "q"},
		ToolGet:    {"ids": []string{"x"}},
		ToolForget: {"id": "x"},
		ToolRecent: {},
	} {
		var out struct{ Error *ToolError }
		if isErr, _ := call(t, cs, tool, args, &out); !isErr || out.Error == nil || out.Error.Message != memory.OffMessage {
			t.Errorf("%s while off: %+v", tool, out.Error)
		}
	}
	if _, err := os.Stat(e.path); !os.IsNotExist(err) {
		t.Fatal("disabled memory created the store")
	}
}

func TestResultCap(t *testing.T) {
	e := newEnv(t)
	cs := session(t, e)
	var ids []string
	body := strings.Repeat("lorem ipsum ", memory.MaxBodyBytes/12)
	for i := 0; i < 10; i++ {
		var s SaveOutput
		call(t, cs, ToolSave, map[string]any{"title": strings.Repeat("é", memory.MaxTitleChars), "body": body,
			"tags": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", strings.Repeat("t", 40)}}, &s)
		if s.ID == "" {
			t.Fatalf("save %d: %+v", i, s.Error)
		}
		ids = append(ids, s.ID)
	}
	var got GetOutput
	if _, n := call(t, cs, ToolGet, map[string]any{"ids": ids}, &got); n > 16<<10 {
		t.Fatalf("memory_get returned %d bytes", n)
	}
	if len(got.Notes)+len(got.Omitted) != 10 || len(got.Omitted) == 0 {
		t.Fatalf("notes %d omitted %d", len(got.Notes), len(got.Omitted))
	}
	var found SearchOutput
	if _, n := call(t, cs, ToolSearch, map[string]any{"query": "lorem", "limit": 20}, &found); n > 16<<10 {
		t.Fatalf("memory_search returned %d bytes", n)
	}
	for _, h := range found.Results {
		if len(h.Snippet) > memory.MaxSnippet {
			t.Fatalf("snippet %d bytes", len(h.Snippet))
		}
	}
}

func TestDescriptionsSayOnDevice(t *testing.T) {
	cs := session(t, newEnv(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tl := range res.Tools {
		n++
		if !strings.Contains(tl.Description, "only on this device") || !strings.Contains(tl.Description, "never sent to Kameas Fleet") {
			t.Errorf("%s: description must say memory stays on this device", tl.Name)
		}
		if strings.Contains(strings.ToLower(tl.Description), "private") {
			t.Errorf("%s: must not describe memory as private", tl.Name)
		}
	}
	if n != 5 {
		t.Fatalf("%d tools", n)
	}
}

// The tool group must not reach Kameas Fleet: it imports none of the
// sending packages and never touches the provider in mcp.Deps.
func TestToolGroupNeverReachesFleet(t *testing.T) {
	bp, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range bp.Imports {
		for _, bad := range []string{"/fleetapi", "/internal/forward", "/internal/spool", "/internal/daemon"} {
			if strings.HasSuffix(imp, bad) || strings.Contains(imp, bad+"/") {
				t.Errorf("imports %s", imp)
			}
		}
	}
	for _, f := range bp.GoFiles {
		src, _ := os.ReadFile(f)
		if strings.Contains(string(src), "d.Provider") || strings.Contains(string(src), ".Provider.") {
			t.Errorf("%s uses the Fleet provider", f)
		}
	}
}
