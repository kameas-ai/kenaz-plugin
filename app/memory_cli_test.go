package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

func (h *harness) memoryDB() string { return filepath.Join(h.home, "plugin", "memory", "memory.db") }

// mcpCall runs `kenaz mcp` with an in-memory client and calls one tool.
func (h *harness) mcpCall(t *testing.T, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	var got map[string]any
	var isErr bool
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			return err
		}
		isErr = res.IsError
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, &got)
	}
	if code := app.RunEnv(env, []string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit=%d err=%s", code, errb.String())
	}
	return got, isErr
}

// Memory works signed out on an unofficial build, end to end through the
// MCP server, the CLI and the session-start hook.
func TestMemoryCLIAndToolsSignedOut(t *testing.T) {
	h := newHarness(t, stub.New())
	db := h.memoryDB()

	saved, isErr := h.mcpCall(t, "memory_save", map[string]any{"title": "Use make check", "body": "Run make check before every push.", "tags": []string{"ci"}})
	if isErr || saved["id"] == nil {
		t.Fatalf("memory_save: %v", saved)
	}
	id := saved["id"].(string)
	if saved["notice"] != memory.Disclosure(db) {
		t.Fatalf("first save notice = %v", saved["notice"])
	}
	if fi, err := os.Stat(db); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store: %v %v", fi, err)
	}

	// The disclosure was shown once; the CLI does not repeat it.
	code, out, errb := h.run(t, "memory", "list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "Use make check") || errb != "" {
		t.Fatalf("list: %d %q %q", code, out, errb)
	}
	if code, out, _ := h.run(t, "memory", "search", "push"); code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "push") {
		t.Fatalf("search: %d %q", code, out)
	}
	if code, out, _ := h.run(t, "memory", "show", id); code != 0 || !strings.Contains(out, "Run make check before every push.") {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, out, _ := h.run(t, "memory", "path"); code != 0 || strings.TrimSpace(out) != db {
		t.Fatalf("path: %q", out)
	}

	// Session-start hook: index with title and id, never the body.
	if code, out, _ := h.run(t, "hook", "session-start"); code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "Use make check") || strings.Contains(out, "before every push") {
		t.Fatalf("hook: %d %q", code, out)
	}

	// status and doctor report it.
	if _, out, _ := h.run(t, "status"); !strings.Contains(out, "memory: on ("+db+", 1 note)") {
		t.Fatalf("status: %s", out)
	}
	if _, out, _ := h.run(t, "doctor"); !strings.Contains(out, "memory: on ("+db+", 1 note)") {
		t.Fatalf("doctor: %s", out)
	}

	// logout keeps memory and says how to delete it.
	_, out, _ = h.run(t, "logout")
	if !strings.Contains(out, memory.KeptLine(db)) {
		t.Fatalf("logout output: %q", out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("logout deleted memory")
	}

	// setup --undo says the same.
	if code, out, errb := h.run(t, "setup", "--undo", "claude", "--yes"); code != 0 || !strings.Contains(out, memory.KeptLine(db)) {
		t.Fatalf("setup --undo: %d %q %q", code, out, errb)
	}

	if code, out, _ := h.run(t, "memory", "forget", id); code != 0 || !strings.Contains(out, "Deleted") {
		t.Fatalf("forget: %d %q", code, out)
	}
	if code, _, _ := h.run(t, "memory", "forget", id); code == 0 {
		t.Fatal("second forget succeeded")
	}

	// purge needs a terminal or --yes.
	restore := app.SetMemoryStdin(false, strings.NewReader("y\n"))
	if code, _, _ := h.run(t, "memory", "purge"); code == 0 {
		t.Fatal("purge without --yes from a pipe succeeded")
	}
	restore()
	restore = app.SetMemoryStdin(true, strings.NewReader("n\n"))
	if code, out, _ := h.run(t, "memory", "purge"); code != 0 || !strings.Contains(out, "Nothing was deleted") {
		t.Fatalf("declined purge: %q", out)
	}
	restore()
	if _, err := os.Stat(db); err != nil {
		t.Fatal("declined purge deleted the store")
	}
	if code, _, _ := h.run(t, "memory", "purge", "--yes"); code != 0 {
		t.Fatal("purge --yes failed")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("purge left the store")
	}
	if code, out, _ := h.run(t, "memory", "list"); code != 0 || !strings.Contains(out, "No memories") {
		t.Fatalf("list after purge: %q", out)
	}
}

func TestMemoryDisableEnable(t *testing.T) {
	h := newHarness(t, stub.New())
	db := h.memoryDB()
	if saved, isErr := h.mcpCall(t, "memory_save", map[string]any{"title": "kept", "body": "b"}); isErr {
		t.Fatalf("save: %v", saved)
	}
	if code, _, _ := h.run(t, "memory", "disable"); code != 0 {
		t.Fatal("disable failed")
	}
	got, isErr := h.mcpCall(t, "memory_search", map[string]any{"query": "kept"})
	if e, _ := got["error"].(map[string]any); !isErr || e["message"] != memory.OffMessage {
		t.Fatalf("search while off: %v", got)
	}
	if _, out, _ := h.run(t, "hook", "session-start"); out != "" {
		t.Fatalf("index shown while off: %q", out)
	}
	if _, out, _ := h.run(t, "status"); !strings.Contains(out, "memory: off") {
		t.Fatalf("status: %s", out)
	}
	if code, _, errb := h.run(t, "memory", "list"); code == 0 || !strings.Contains(errb, memory.OffMessage) {
		t.Fatalf("list while off: %d %q", code, errb)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("disable deleted the store")
	}
	cfg, _ := os.ReadFile(filepath.Join(h.home, "plugin", "config.toml"))
	if !strings.Contains(string(cfg), `memory = "off"`) {
		t.Fatalf("config: %q", cfg)
	}
	if code, _, _ := h.run(t, "memory", "enable"); code != 0 {
		t.Fatal("enable failed")
	}
	if code, out, _ := h.run(t, "memory", "list"); code != 0 || !strings.Contains(out, "kept") {
		t.Fatalf("list after enable: %q", out)
	}
}

// The disclosure is shown once, by whichever use comes first.
func TestMemoryDisclosureOnceFromCLI(t *testing.T) {
	h := newHarness(t, stub.New())
	_, _, errb := h.run(t, "memory", "list")
	want := memory.Disclosure(h.memoryDB())
	if strings.TrimSpace(errb) != want {
		t.Fatalf("first use stderr %q", errb)
	}
	if !strings.Contains(want, "Kenaz memory is on: your AI tool can save notes to "+h.memoryDB()+" on this device. They stay on this device unless you turn on memory sync (`kenaz memory sync enable`). Turn off: `kenaz memory disable`. Clear: `kenaz memory purge`.") {
		t.Fatal("disclosure wording changed")
	}
	if _, _, errb := h.run(t, "memory", "list"); errb != "" {
		t.Fatalf("shown twice: %q", errb)
	}
	saved, _ := h.mcpCall(t, "memory_save", map[string]any{"title": "t", "body": "b"})
	if _, ok := saved["notice"]; ok {
		t.Fatal("memory_save repeated the disclosure")
	}
}

// `kenaz mcp` must not leave goroutines writing into the state directory
// after it returns. Its daemon pinger used to log "daemon unavailable" to
// logs/mcp.log a little later, which raced t.TempDir's cleanup
// ("directory not empty") and made TestMemoryDisclosureOnceFromCLI flaky.
func TestMCPLeavesNoWriterBehind(t *testing.T) {
	h := newHarness(t, stub.New())
	h.mcpCall(t, "memory_save", map[string]any{"title": "t", "body": "b"})
	before := listTree(t, h.home)
	time.Sleep(300 * time.Millisecond) // the leaked write landed well within this
	if after := listTree(t, h.home); !slices.Equal(before, after) {
		t.Fatalf("state changed after kenaz mcp returned:\nbefore %v\nafter  %v", before, after)
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMemoryHelpCarriesNotice(t *testing.T) {
	for _, args := range [][]string{{"memory", "--help"}, {"memory", "list", "--help"}, {"memory", "purge", "-h"}} {
		code, out, _ := run(args...)
		if code != 0 || !strings.Contains(out, memory.Notice) {
			t.Errorf("%v: %d %q", args, code, out)
		}
		if regexp.MustCompile(`(?i)\bprivate\b`).MatchString(out) {
			t.Errorf("%v: memory described as private", args)
		}
	}
	if _, out, _ := run("help"); !strings.Contains(out, "memory") {
		t.Fatal("top-level help lacks memory")
	}
}

func TestSetupActionClassifies(t *testing.T) {
	for args, want := range map[string]string{
		"claude":               "apply",
		"codex --yes":          "apply",
		"--undo claude":        "undo",
		"claude --undo --yes":  "undo",
		"claude --dry-run":     "",
		"print-managed claude": "",
		"doctor":               "",
		"--env dev":            "",
	} {
		if got := app.SetupAction(strings.Fields(args)); got != want {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
}

// The CLI is truthful about sync on a build that cannot sync, and a
// terminal answer never authorizes anything account-wide.
func TestMemorySyncCLIUnofficial(t *testing.T) {
	h := newHarness(t, stub.New())
	if code, out, _ := h.run(t, "memory", "sync", "status"); code != 0 || !strings.Contains(out, "never syncs") {
		t.Fatalf("status = %d %q", code, out)
	}
	if code, _, errs := h.run(t, "memory", "sync", "enable"); code == 0 || !strings.Contains(errs, "not available in this build") {
		t.Fatalf("enable = %d %q", code, errs)
	}
	restore := app.SetMemoryStdin(true, strings.NewReader("y\ny\ny\n"))
	defer restore()
	if code, _, errs := h.run(t, "memory", "forget-all"); code == 0 || !strings.Contains(errs, "kenaz memory purge") {
		t.Fatalf("forget-all signed out = %d %q", code, errs)
	}
	for _, sub := range []string{"approve", "adopt"} {
		args := []string{"memory", "sync", sub}
		if sub == "adopt" {
			args = append(args, "mem-x")
		}
		if code, _, errs := h.run(t, args...); code == 0 || !strings.Contains(strings.ToLower(errs), "sign in") {
			t.Fatalf("%s signed out = %d %q", sub, code, errs)
		}
	}
}

// update and export on the device notebook stay plain CLI commands.
func TestMemoryCLIUpdateExport(t *testing.T) {
	h := newHarness(t, stub.New())
	s, err := memory.Open(h.memoryDB())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s.Save(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "first", Source: memory.SourceCLI})
	s.Close()
	if code, out, errs := h.run(t, "memory", "update", id, "--body", "second", "--tags", "a, b"); code != 0 || !strings.Contains(out, "Updated memory") {
		t.Fatalf("update = %d %q %q", code, out, errs)
	}
	code, out, _ := h.run(t, "memory", "export")
	if code != 0 || !strings.Contains(out, `"body":"second"`) || !strings.Contains(out, `"tags":["a","b"]`) || !strings.Contains(out, `"profile":"device"`) {
		t.Fatalf("export = %d %q", code, out)
	}
	if code, out, _ := h.run(t, "memory", "forget", id); code != 0 || !strings.Contains(out, "Deleted memory") {
		t.Fatalf("forget = %d %q", code, out)
	}
}

// purge --yes clears only the device notebook: an account's store can be
// deleted only by an approved forget-all.
func TestMemoryPurgeKeepsAccountStores(t *testing.T) {
	h := newHarness(t, stub.New())
	root := filepath.Dir(filepath.Dir(h.memoryDB()))
	acct := memory.Profile{Env: "dev", OrgID: "o", UserID: "u"}
	st, err := memory.OpenProfile(memory.ProfilePath(root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	st.Save(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "never synced", Source: memory.SourceCLI})
	st.Close()
	nb, _ := memory.Open(h.memoryDB())
	nb.Save(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "notebook", Source: memory.SourceCLI})
	nb.Close()
	code, out, _ := h.run(t, "memory", "purge", "--yes")
	if code != 0 || !strings.Contains(out, "was kept") {
		t.Fatalf("purge = %d %q", code, out)
	}
	if _, err := os.Stat(h.memoryDB()); !os.IsNotExist(err) {
		t.Fatal("notebook not deleted")
	}
	st, err = memory.OpenProfile(memory.ProfilePath(root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n, _ := st.Count(context.Background()); n != 1 {
		t.Fatal("purge deleted an account's notes")
	}
}
