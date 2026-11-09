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
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
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
		if !strings.Contains(tl.Description, "stored on this device") || !strings.Contains(tl.Description, "only if you turned on memory sync (off by default)") ||
			!strings.Contains(tl.Description, "never as telemetry") {
			t.Errorf("%s: description must say where memory goes", tl.Name)
		}
		if strings.Contains(strings.ToLower(tl.Description), "private") {
			t.Errorf("%s: must not describe memory as private", tl.Name)
		}
	}
	if n != 11 {
		t.Fatalf("%d tools", n)
	}
}

// The tool group never calls Kameas Fleet itself: it imports none of the
// sending packages and never touches the provider in mcp.Deps. (Turning
// sync on goes through the approval gate and an injected memory.Remote;
// the relay's sync lane does the sending.)
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

// Automatically captured notes carry the untrusted label wherever they are
// recalled, and timeline finds what surrounds one.
func TestRecallLabelsAutomaticNotes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "memory", "memory.db")
	workdir := t.TempDir()
	o := Options{Path: path, Workdir: func() string { return workdir }, Enabled: func() bool { return true }}
	svc := memory.NewService(memory.ServiceOptions{StateRoot: root})
	p, err := o.project()
	if err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var autoID string
	for i, title := range []string{"before note", "captured signal", "after note"} {
		id, _, err := st.SaveAuto(ctx, memory.AutoNote{ProjectKey: p.Key, Title: title, Body: "body", Source: memory.SourceCodex, Kind: memory.KindObservation, Tags: []string{memory.AutoTag}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			autoID = id
		}
	}
	mine, err := st.Save(ctx, memory.NewNote{ProjectKey: p.Key, Title: "deliberate", Body: "body", Source: memory.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	got := runGet(ctx, o, svc, GetInput{IDs: []string{autoID, mine}})
	if len(got.Notes) != 2 || got.Notes[0].Untrusted != memory.UntrustedLabel || got.Notes[1].Untrusted != "" {
		t.Errorf("get: %+v", got)
	}
	found := runSearch(ctx, o, svc, SearchInput{Query: "captured"})
	if len(found.Results) != 1 || found.Results[0].Untrusted != memory.UntrustedLabel {
		t.Errorf("search: %+v", found)
	}
	tl := runTimeline(ctx, o, svc, TimelineInput{Anchor: autoID, Before: 1, After: 1})
	if tl.Error != nil || len(tl.Entries) != 3 || tl.Entries[0].Title != "before note" || tl.Entries[1].Untrusted == "" {
		t.Errorf("timeline: %+v", tl)
	}
	if q := runTimeline(ctx, o, svc, TimelineInput{Query: "captured", Before: -1, After: -1}); q.Anchor != autoID || len(q.Entries) != 1 {
		t.Errorf("timeline by query: %+v", q)
	}
	if none := runTimeline(ctx, o, svc, TimelineInput{}); none.Error == nil {
		t.Error("no anchor and no query should be refused")
	}
}

// The member's setting limits every read in a chat app, even project=all, and only
// in a chat app. Titles a chat app sees are scrubbed.
func TestChatRecallSetting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "memory", "memory.db")
	workdir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	limited, chat := true, true
	o := Options{Path: path, Workdir: func() string { return workdir }, Enabled: func() bool { return true },
		Chat: func() bool { return chat }, RecallNoProject: func() bool { return limited }}
	svc := memory.NewService(memory.ServiceOptions{StateRoot: root})
	st, err := memory.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := st.Save(ctx, memory.NewNote{ProjectKey: "p1", Title: "shared word project", Body: "body", Source: memory.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	global, err := st.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "shared word global at " + home + "/x from a@example.com", Body: "body", Source: memory.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	for _, p := range []string{"", "all", "current"} {
		if got := runSearch(ctx, o, svc, SearchInput{Query: "shared", Project: p}); len(got.Results) != 1 || got.Results[0].ID != global {
			t.Errorf("search project=%q: %+v", p, got)
		}
		if got := runRecent(ctx, o, svc, RecentInput{Project: p}); len(got.Entries) != 1 || got.Entries[0].ID != global {
			t.Errorf("recent project=%q: %+v", p, got)
		}
		if got := runTimeline(ctx, o, svc, TimelineInput{Anchor: global, Project: p, Before: 5, After: 5}); len(got.Entries) != 1 {
			t.Errorf("timeline project=%q: %+v", p, got)
		}
	}
	if got := runGet(ctx, o, svc, GetInput{IDs: []string{project, global}}); len(got.Notes) != 1 || got.Notes[0].ID != global || len(got.Missing) != 1 || got.Missing[0] != project {
		t.Errorf("get: %+v", got)
	}
	if got := runExport(ctx, o, svc, ExportInput{}); got.Total != 1 || len(got.Notes) != 1 || got.Notes[0].ID != global {
		t.Errorf("export: %+v", got)
	}
	if tl := runTimeline(ctx, o, svc, TimelineInput{Anchor: project, Project: "all"}); tl.Error == nil || len(tl.Entries) != 0 {
		t.Errorf("timeline reached a project note: %+v", tl)
	}
	got := runRecent(ctx, o, svc, RecentInput{})
	if title := got.Entries[0].Title; strings.Contains(title, home) || strings.Contains(title, "a@example.com") {
		t.Errorf("chat title not scrubbed: %q", title)
	}

	// Default: every project.
	limited = false
	if got := runRecent(ctx, o, svc, RecentInput{}); len(got.Entries) != 2 {
		t.Errorf("default in chat: %+v", got)
	}
	// Outside a chat app the setting does nothing, and titles are as saved.
	limited, chat = true, false
	if got := runRecent(ctx, o, svc, RecentInput{Project: "all"}); len(got.Entries) != 2 {
		t.Errorf("outside chat: %+v", got)
	}
	if got := runGet(ctx, o, svc, GetInput{IDs: []string{global}}); !strings.Contains(got.Notes[0].Title, home) {
		t.Errorf("outside chat the title changed: %q", got.Notes[0].Title)
	}
}

// A note an agent saves while the session-agent writer is on is labelled like any
// automatic note.
func TestSessionAgentSavesAreLabelled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workdir := t.TempDir()
	o := Options{Path: filepath.Join(root, "memory", "memory.db"), Workdir: func() string { return workdir }, Enabled: func() bool { return true },
		Disclose: func() string { return "" }, AutoSaves: func() bool { return true }}
	svc := memory.NewService(memory.ServiceOptions{StateRoot: root})
	out := runSave(ctx, o, svc, memory.SourceCodex, SaveInput{Title: "t", Body: "b"})
	if out.Error != nil {
		t.Fatal(out.Error)
	}
	if got := runGet(ctx, o, svc, GetInput{IDs: []string{out.ID}}); len(got.Notes) != 1 || got.Notes[0].Untrusted == "" || got.Notes[0].Kind != memory.KindObservation {
		t.Errorf("%+v", got)
	}
}

// The first read across every project in chat carries the notice, once. A limited chat,
// and a read that returned nothing, don't use it up.
func TestRecallNoticeOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "memory", "memory.db")
	limited, chat, claims := true, true, 0
	o := Options{Path: path, Workdir: func() string { return t.TempDir() }, Enabled: func() bool { return true },
		Chat: func() bool { return chat }, RecallNoProject: func() bool { return limited },
		RecallNotice: func() string {
			claims++
			if claims == 1 {
				return "the notice"
			}
			return ""
		}}
	svc := memory.NewService(memory.ServiceOptions{StateRoot: root})
	st, err := memory.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Save(ctx, memory.NewNote{ProjectKey: "p1", Title: "shared word", Body: "body", Source: memory.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if got := runSearch(ctx, o, svc, SearchInput{Query: "shared"}); got.NoticeForUser != "" || len(got.Results) != 0 || claims != 0 {
		t.Errorf("limited: %+v, %d claims", got, claims)
	}
	limited = false
	if got := runSearch(ctx, o, svc, SearchInput{Query: "nothing-matches"}); got.NoticeForUser != "" || claims != 0 {
		t.Errorf("no notes read: %+v, %d claims", got, claims)
	}
	if got := runRecent(ctx, o, svc, RecentInput{}); got.NoticeForUser != "the notice" || len(got.Entries) != 1 {
		t.Errorf("first read: %+v", got)
	}
	if got := runSearch(ctx, o, svc, SearchInput{Query: "shared"}); got.NoticeForUser != "" || len(got.Results) != 1 {
		t.Errorf("second read: %+v", got)
	}
	chat = false
	claims = 0
	if got := runGet(ctx, o, svc, GetInput{IDs: []string{id}}); got.NoticeForUser != "" || claims != 0 {
		t.Errorf("not chat: %+v, %d claims", got, claims)
	}
}

// However an attempt went, the status never says the erase failed or that notes were kept.
func TestSyncSummaryWhileErasing(t *testing.T) {
	for code, again := range map[string]bool{"": false, "erase_pending": false, "fleet_unavailable": true, "rate_limited": true} {
		st := memory.SyncStatus{SyncSettings: memory.SyncSettings{Enabled: true, ErasePending: true, StatusCode: code}}
		got := SyncSummary(st, true)
		if strings.Contains(got, usertext.EraseTryingAgain) != again {
			t.Errorf("%q: %s", code, got)
		}
		for _, bad := range []string{"failed", "kept", "couldn't", "didn't"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q says %q: %s", code, bad, got)
			}
		}
	}
}
