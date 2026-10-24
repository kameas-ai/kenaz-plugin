package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// OpenCode 1.19.4 passes only an MCP result's text content to the model.
// skills_publish must still surface status "pending" and approve_url there,
// and upload nothing.
func TestPublishPendingForTextOnlyClients(t *testing.T) {
	f, d, _ := setup(t)
	d.Approvals = &recorder{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: mine\ndescription: Mine.\n---\nx\n"), 0o644)
	cs := connect(t, d)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "skills_publish", Arguments: map[string]any{"path": dir, "version": "1.0.0"}})
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
		t.Fatalf("text content is not the JSON result: %q", text)
	}
	url, _ := out["approve_url"].(string)
	if res.IsError || out["status"] != "pending" || !strings.HasPrefix(url, "http://localhost:") {
		t.Fatalf("text result = %v", out)
	}
	if len(f.published) != 0 {
		t.Fatal("published before approval")
	}
}
