package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/mcpb"
)

// registeredTools lists what `kenaz mcp` serves, as an MCP client sees it.
func registeredTools(t *testing.T) map[string]*sdk.Tool {
	t.Helper()
	h := newHarness(t, stub.New())
	got := map[string]*sdk.Tool{}
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-ai", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		for tool, err := range cs.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			got[tool.Name] = tool
		}
		return nil
	}
	if code := app.RunEnv(env, []string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit=%d err=%s", code, errb.String())
	}
	if len(got) == 0 {
		t.Fatal("kenaz mcp registered no tools")
	}
	return got
}

// The Claude Desktop manifest lists exactly the tools `kenaz mcp` serves,
// each described by the tool's own title, so the two cannot drift.
func TestMCPBManifestToolsMatchServer(t *testing.T) {
	served := registeredTools(t)
	var m struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(mcpb.Template), &m); err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, tl := range m.Tools {
		if _, dup := listed[tl.Name]; dup {
			t.Errorf("manifest lists %s twice", tl.Name)
		}
		listed[tl.Name] = tl.Description
	}
	var missing, extra []string
	for name := range served {
		if _, ok := listed[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range listed {
		if _, ok := served[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("mcpb/manifest.json.tmpl tools drifted from kenaz mcp: missing %v, not served %v", missing, extra)
	}
	for name, tool := range served {
		want := tool.Title
		if want == "" && tool.Annotations != nil {
			want = tool.Annotations.Title
		}
		if want == "" {
			t.Errorf("%s has no title to describe it in the manifest", name)
		} else if listed[name] != want {
			t.Errorf("manifest describes %s as %q; the tool's title is %q", name, listed[name], want)
		}
	}
}
