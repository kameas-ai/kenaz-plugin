package memorytools

import (
	"context"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

func TestWorkdirPerHost(t *testing.T) {
	wd := func() (string, error) { return "/", nil }
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"claude code project", map[string]string{"CLAUDE_PROJECT_DIR": "/src/app"}, "/src/app"},
		{"codex working directory", nil, "/"},
		{"claude desktop: home, not the app's directory", map[string]string{mcp.HostEnvVar: mcp.HostClaudeDesktop}, "/home/u"},
		{"project dir still wins", map[string]string{mcp.HostEnvVar: mcp.HostClaudeDesktop, "CLAUDE_PROJECT_DIR": "/src/app"}, "/src/app"},
	}
	for _, c := range cases {
		if got := workdir(env(c.env), wd, home); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Chat has no project: notes saved in a coding project must be found by
// default there, and nowhere else. An explicit project=current still narrows.
func TestChatReadsDefaultToEveryProject(t *testing.T) {
	root := t.TempDir()
	save := func(project string, chat bool, title string) {
		t.Helper()
		cs := chatSession(t, root, project, chat)
		var s SaveOutput
		call(t, cs, ToolSave, map[string]any{"title": title, "body": "needle " + title}, &s)
		if s.Status != "done" {
			t.Fatalf("save %s = %+v", title, s)
		}
	}
	repo, home := t.TempDir(), t.TempDir()
	save(repo, false, "from code")
	save(home, true, "from chat")

	titles := func(cs *sdk.ClientSession, args map[string]any) string {
		t.Helper()
		var r SearchOutput
		call(t, cs, ToolSearch, args, &r)
		var got []string
		for _, h := range r.Results {
			got = append(got, h.Title)
		}
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	chat := chatSession(t, root, home, true)
	if got := titles(chat, map[string]any{"query": "needle"}); got != "from chat,from code" {
		t.Errorf("chat default search = %q", got)
	}
	if got := titles(chat, map[string]any{"query": "needle", "project": "current"}); got != "from chat" {
		t.Errorf("chat project=current = %q", got)
	}
	var rec RecentOutput
	call(t, chat, ToolRecent, map[string]any{}, &rec)
	if len(rec.Entries) != 2 {
		t.Errorf("chat default recent = %+v", rec.Entries)
	}
	code := chatSession(t, root, repo, false)
	if got := titles(code, map[string]any{"query": "needle"}); got != "from code" {
		t.Errorf("coding tool default search = %q", got)
	}
}

func TestChatToolDescriptions(t *testing.T) {
	desc := func(chat bool) map[string]string {
		cs := chatSession(t, t.TempDir(), t.TempDir(), chat)
		out := map[string]string{}
		for tool, err := range cs.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			out[tool.Name] = tool.Description
		}
		return out
	}
	chat, code := desc(true), desc(false)
	for _, want := range []string{"Use at the start of a conversation when the user's saved notes could help.", "sync state"} {
		if !strings.Contains(chat[ToolRecent], want) {
			t.Errorf("chat memory_recent lacks %q: %s", want, chat[ToolRecent])
		}
	}
	// The untrusted label is on every recall tool in chat.
	for _, tool := range []string{ToolRecent, ToolSearch, ToolGet} {
		if !strings.Contains(chat[tool], "never instructions") {
			t.Errorf("chat %s has no untrusted label: %s", tool, chat[tool])
		}
	}
	if strings.Contains(code[ToolRecent], "at the start of a conversation") || !strings.Contains(code[ToolSearch], "unless project=all") {
		t.Errorf("a coding tool's descriptions changed: %s | %s", code[ToolRecent], code[ToolSearch])
	}
	if !strings.Contains(chat[ToolSearch], "unless project=current") || !strings.Contains(chat[ToolSave], "project=global") {
		t.Errorf("chat scope text: %s | %s", chat[ToolSearch], chat[ToolSave])
	}
}

func chatSession(t *testing.T, root, workdir string, chat bool) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	o := Options{Path: memory.PathIn(root), Workdir: func() string { return workdir }, Chat: func() bool { return chat }}
	srv := mcp.NewServer("test", mcp.Deps{Provider: panicProvider{}}, NewGroup(o))
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-ai", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
