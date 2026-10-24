package contexttools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// textOnly calls a tool the way OpenCode 1.19.4 consumes MCP results: it
// hands only the text content to the model and ignores structuredContent.
func textOnly(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text = append(text, tc.Text)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.Join(text, "")), &out); err != nil {
		t.Fatalf("%s: text content is not the JSON result: %q", tool, text)
	}
	return out
}

// A host that reads only text content (OpenCode) still gets status
// "pending" and approve_url for the write tools, and nothing is written
// before approval.
func TestWriteToolsPendingForTextOnlyClients(t *testing.T) {
	f := newFake("enterprise", "org_admin", platform)
	cs := session(t, f, nil, "")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{ToolShare, map[string]any{"title": "Flaky e2e", "body": "Retry.", "kind": "guidance"}},
		{ToolPromote, map[string]any{"node_id": "n1"}},
	} {
		out := textOnly(t, cs, c.tool, c.args)
		url, _ := out["approve_url"].(string)
		if out["status"] != "pending" || !strings.HasPrefix(url, "http://localhost:") {
			t.Errorf("%s: text result lacks pending/approve_url: %v", c.tool, out)
		}
	}
	if f.writeCalls() != 0 {
		t.Fatalf("write ran before approval: writes=%d", f.writeCalls())
	}
}
