package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// stageApp is an official build on a device an earlier release signed in to
// stage: the stored session is stage's, and every Fleet call counts,
// memory's included.
type stageApp struct {
	fleetCalls
}

func (p stageApp) Memory() fleetapi.Memory { return countedMemory{p.n} }

type countedMemory struct{ n *int }

func (c countedMemory) Settings(context.Context) (fleetapi.MemorySettings, error) {
	*c.n++
	return fleetapi.MemorySettings{}, nil
}
func (c countedMemory) PutSettings(context.Context, fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	*c.n++
	return fleetapi.MemorySettings{}, nil
}
func (c countedMemory) Pull(context.Context, fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	*c.n++
	return fleetapi.MemoryPullResponse{}, nil
}
func (c countedMemory) Push(context.Context, fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	*c.n++
	return fleetapi.MemoryPushResponse{}, nil
}
func (c countedMemory) Forget(context.Context, fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	*c.n++
	return fleetapi.MemoryForgetResponse{}, nil
}
func (c countedMemory) ForgetAll(context.Context, fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	*c.n++
	return fleetapi.MemoryForgetAllResponse{}, nil
}
func (c countedMemory) Export(context.Context, func(fleetapi.MemoryRecord) error) (fleetapi.MemoryExportTrailer, error) {
	*c.n++
	return fleetapi.MemoryExportTrailer{}, nil
}

// retiredHostEnv is an Env on a temp home with vars set, counting relay
// starts.
func retiredHostEnv(t *testing.T, p fleetapi.Provider, vars map[string]string, spawned *int) (Env, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	var out, errb bytes.Buffer
	return Env{Provider: p, Stdout: &out, Stderr: &errb, Ctx: context.Background(), KeyStore: &spool.Memory{},
		OpenBrowser: func(string) error { return nil },
		Spawn:       func(string, fleetapi.Env) error { *spawned++; return nil },
		Getenv: func(k string) string {
			if k == variant.Shared.HomeVar {
				return home
			}
			return vars[k]
		}}, home, &out, &errb
}

// An app that still starts the MCP server with the stage option gets a
// server, not a dead process: it serves what works signed out, never
// contacts Fleet or starts a relay, creates nothing for stage, and
// fleet_status and every Fleet tool say what is wrong and the commands
// that fix it.
func TestMCPForStageServesSignedOut(t *testing.T) {
	oldLib := features.ContextLibrary
	features.ContextLibrary = true
	t.Cleanup(func() { features.ContextLibrary = oldLib })
	bin := variant.Current.Binary
	what := "This app is still set up for the stage environment of Kameas Fleet, which has been retired, so Kameas Fleet is not available here. "
	id := variant.Current
	choices := id.EnvChoices(" or ")
	// Each source of the setting is changed in its own place.
	option := what + "To fix it, set the " + id.DisplayName + " plugin's Kameas Fleet environment option to " + id.DefaultEnv + " in Claude Code: run `/plugin configure " + id.PluginID() + "` there. Then restart Claude Code."
	variable := func(name, app string) string {
		return what + "To fix it, remove " + name + "=stage from the environment " + app + " starts in (for example your shell profile), or set it to " + choices + ", then restart " + app + "."
	}
	file := func(home, app string) string {
		return what + "To fix it, remove the line `env = \"stage\"` from " + filepath.Join(home, "plugin", "config.toml") + ", then restart " + app + "."
	}
	flagBoth := what + "To fix it, run `" + bin + " setup --undo claude --env stage` (in Codex: `" + bin + " setup --undo codex --env stage`), then `" + bin + " init`, and restart this app."
	flagCodex := what + "To fix it, run `" + bin + " setup --undo codex --env stage`, then `" + bin + " init`, and restart Codex."
	desktop := what + "To fix it, set the Kameas Fleet environment to " + choices + " in the Kenaz extension's settings in Claude Desktop, then restart Claude Desktop."
	ev := variant.Shared.EnvVar
	for _, c := range []struct {
		name string
		vars map[string]string
		args []string
		file string
		want func(home string) string
	}{
		{"Claude Code's stored plugin option", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage", ev: "stage", "CLAUDE_PLUGIN_ROOT": "/p"}, []string{"mcp"}, "",
			func(string) string { return option }},
		{"the plugin option alone", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, []string{"mcp"}, "",
			func(string) string { return option }},
		{"Claude Code, through the plugin's .mcp.json only", map[string]string{ev: "stage", "CLAUDE_PLUGIN_ROOT": "/p"}, []string{"mcp"}, "",
			func(string) string { return option }},
		// Codex passes no option: the variable is in the person's environment.
		{"Codex, an env variable", map[string]string{ev: "stage", "PLUGIN_ROOT": "/p", "CLAUDE_PLUGIN_ROOT": "/p"}, []string{"mcp"}, "",
			func(string) string { return variable(ev, "Codex") }},
		{"Codex, the original variable", map[string]string{"FLEET_PLUGIN_ENV": "stage", "PLUGIN_ROOT": "/p"}, []string{"mcp"}, "",
			func(string) string { return variable("FLEET_PLUGIN_ENV", "Codex") }},
		{"an env variable, app unknown", map[string]string{ev: "stage"}, []string{"mcp"}, "",
			func(string) string {
				return variable(ev, "this app") + " If this app is Claude Code, the value comes from the " + id.DisplayName + " plugin's Kameas Fleet environment option: change it with `/plugin configure " + id.PluginID() + "` there."
			}},
		{"config.toml", nil, []string{"mcp"}, "env = \"stage\"\n", func(home string) string { return file(home, "this app") }},
		{"config.toml, Codex", map[string]string{"PLUGIN_ROOT": "/p"}, []string{"mcp"}, "env = \"stage\"\n", func(home string) string { return file(home, "Codex") }},
		{"the flag", nil, []string{"mcp", "--env", "stage"}, "", func(string) string { return flagBoth }},
		{"the flag, Codex", map[string]string{"PLUGIN_ROOT": "/p"}, []string{"mcp", "--env=stage"}, "", func(string) string { return flagCodex }},
		{"the flag wins over an allowed variable", map[string]string{ev: id.DefaultEnv}, []string{"mcp", "--env", "stage"}, "", func(string) string { return flagBoth }},
		{"Claude Desktop's extension", map[string]string{ev: "stage", variant.Shared.MCPHostVar: "claude-desktop"}, []string{"mcp"}, "",
			func(string) string { return desktop }},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls, spawned := 0, 0
			p := stageApp{fleetCalls{stub.New(), fleetapi.EnvStage, true, &calls}}
			env, home, out, errb := retiredHostEnv(t, p, c.vars, &spawned)
			if c.file != "" {
				os.MkdirAll(filepath.Join(home, "plugin"), 0o700)
				if err := os.WriteFile(filepath.Join(home, "plugin", "config.toml"), []byte(c.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := c.want(home)
			served := false
			texts := map[string]string{}
			var status map[string]any
			env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
				served = true
				ct, st := sdk.NewInMemoryTransports()
				if _, err := srv.Connect(ctx, st, nil); err != nil {
					return err
				}
				cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
				if err != nil {
					return err
				}
				defer cs.Close()
				tools, err := cs.ListTools(ctx, nil)
				if err != nil {
					return err
				}
				for _, tool := range tools.Tools {
					args := map[string]any{}
					switch tool.Name {
					case "skills_search", "context_search", "memory_search":
						args["query"] = "x"
					case "context_pull":
						args["ids"] = []string{"n1"}
					}
					res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool.Name, Arguments: args})
					if err != nil {
						texts[tool.Name] = "protocol error: " + err.Error()
						continue
					}
					raw, _ := json.Marshal(res)
					texts[tool.Name] = string(raw)
					if tool.Name == "fleet_status" {
						b, _ := json.Marshal(res.StructuredContent)
						_ = json.Unmarshal(b, &status)
					}
				}
				return nil
			}
			if code := RunEnv(env, c.args); code != ExitOK || !served {
				t.Fatalf("mcp: exit %d, served %v\nstderr %q", code, served, errb.String())
			}
			if out.Len() != 0 {
				t.Errorf("mcp wrote to stdout (the protocol stream): %q", out.String())
			}
			// The host's log (stderr) says it too.
			if errb.String() != bin+": "+want+"\n" {
				t.Errorf("stderr %q\nwant   %q", errb.String(), bin+": "+want+"\n")
			}
			if calls != 0 {
				t.Errorf("%d Fleet call(s) were made for stage", calls)
			}
			if spawned != 0 {
				t.Errorf("%d relay start(s) for stage", spawned)
			}
			for _, name := range []string{"stage", "logs"} {
				if _, err := os.Lstat(filepath.Join(home, "plugin", name)); !os.IsNotExist(err) {
					t.Errorf("%s was created under the plugin folder: %v", name, err)
				}
			}

			// fleet_status: signed out, no relay, and the notice with the fix.
			notes, _ := json.Marshal(status["notes"])
			if status["session"] != "signed_out" || status["env"] != "stage" || !strings.Contains(string(notes), jsonText(want)) {
				t.Errorf("fleet_status: %s", texts["fleet_status"])
			}
			// There is no dashboard to send anyone to.
			if u, _ := status["dashboard_url"].(string); u != "" || strings.Contains(texts["fleet_status"], "stage.fleet") {
				t.Errorf("fleet_status links to a dashboard for stage: %s", texts["fleet_status"])
			}
			if strings.Contains(texts["fleet_status"], "starts automatically") {
				t.Errorf("fleet_status promises a relay: %s", texts["fleet_status"])
			}
			// Every Fleet tool's error is the notice.
			for _, tool := range []string{"skills_search", "context_search", "context_pull", "team_members"} {
				got, ok := texts[tool]
				if !ok {
					t.Errorf("%s is not served", tool)
					continue
				}
				if !strings.Contains(got, jsonText(want)) || !strings.Contains(got, `"isError":true`) {
					t.Errorf("%s: %s", tool, got)
				}
				// Each carries the same code: in the error field, or leading the text.
				if !strings.Contains(got, `env_retired`) {
					t.Errorf("%s has no env_retired code: %s", tool, got)
				}
			}
			// What works signed out still does: memory on this device, and the
			// context files on it.
			for _, tool := range []string{"memory_recent", "memory_search", "context_list"} {
				got, ok := texts[tool]
				if !ok || strings.Contains(got, `"isError":true`) || strings.Contains(got, "retired") {
					t.Errorf("%s (served %v): %s", tool, ok, got)
				}
			}
			for tool, got := range texts {
				if strings.HasPrefix(got, "protocol error") || strings.Contains(got, "panic") {
					t.Errorf("%s: %s", tool, got)
				}
			}
		})
	}
}

// jsonText is s as it appears inside a JSON string.
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// An environment this build does not connect to is still a usage error for
// mcp, and so is stage for every command an app does not start itself.
func TestOnlyHostStartedCommandsRunForStage(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"mcp"}, true},
		{[]string{"mcp", "--env", "stage"}, true},
		{[]string{"memory", "capture", "worker"}, true},
		{[]string{"memory", "capture", "status"}, false},
		{[]string{"memory", "capture"}, false},
		{[]string{"memory", "sync", "status"}, false},
		{[]string{"daemon"}, false},
		{[]string{"status"}, false},
		{[]string{"doctor"}, false},
		{[]string{"login"}, false},
	} {
		if got := hostStarted(c.args[0], c.args[1:]); got != c.want {
			t.Errorf("hostStarted(%v) = %v, want %v", c.args, got, c.want)
		}
	}
	calls, spawned := 0, 0
	p := stageApp{fleetCalls{stub.New(), fleetapi.EnvStage, true, &calls}}
	for _, args := range [][]string{{"daemon"}, {"memory", "capture", "status"}, {"memory", "sync", "status"}, {"status"}} {
		env, home, _, errb := retiredHostEnv(t, p, map[string]string{variant.Shared.EnvVar: "stage"}, &spawned)
		want := variant.Current.RetiredMessage("stage") + " (stage was selected by env)\n"
		if code := RunEnv(env, args); code != ExitUsage || errb.String() != want {
			t.Errorf("%v: exit %d %q", args, code, errb.String())
		}
		if ents, _ := os.ReadDir(home); len(ents) != 0 {
			t.Errorf("%v created %v", args, ents)
		}
	}
	if variant.Current.Allowed("prod") {
		return
	}
	env, _, _, errb := retiredHostEnv(t, p, map[string]string{variant.Shared.EnvVar: "prod"}, &spawned)
	env.ServeMCP = func(context.Context, *sdk.Server) error { t.Error("mcp served for prod"); return nil }
	if code := RunEnv(env, []string{"mcp"}); code != ExitUsage || !strings.Contains(errb.String(), "connects only to") {
		t.Errorf("mcp for prod: exit %d %q", code, errb.String())
	}
}

// The memory capture worker is started by a hook with the hook's
// environment, so an app still set up for stage starts it for stage. The
// queue it works on is the device's, though: one per device, whatever
// started the worker. So for stage it asks Fleet nothing and touches
// nothing, unless what is recorded was left by a stage account: then the
// events queued under that account are removed unprocessed and counted as
// dropped, as after a sign-out. A device signed in to prod with capture
// on, where one app still has the stage option, keeps its queue and its
// offer exactly as they were.
func TestCaptureWorkerForStageTouchesOnlyWhatStageLeft(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
	if !captureAvailable() {
		t.Skip("capture is not available on this platform")
	}
	live := fleetapi.Env(variant.Current.DefaultEnv)
	key := func(env fleetapi.Env) string {
		return memory.Profile{Env: string(env), OrgID: "org-1", UserID: "alice"}.Key()
	}
	tree := func(dir string) string {
		var b strings.Builder
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if d.Name() == "worker.lock" {
				return nil // a worker that ran took and released its lock
			}
			rel, _ := filepath.Rel(dir, p)
			c, _ := os.ReadFile(p)
			b.WriteString(rel + ": " + string(c) + "\n")
			return nil
		})
		return b.String()
	}
	for _, c := range []struct {
		name    string
		session fleetapi.Env // the stored session
		offer   string       // the profile the recorded offer is for
		purged  bool
	}{
		{"a live account's queue, stage option in one app", live, key(live), false},
		{"a live account's queue, stage session stored", fleetapi.EnvStage, key(live), false},
		{"the device notebook's queue", live, "local", false},
		{"a stage account's queue", fleetapi.EnvStage, key(fleetapi.EnvStage), true},
		{"a stage account's queue, signed in elsewhere since", live, key(fleetapi.EnvStage), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls, spawned := 0, 0
			p := stageApp{fleetCalls{stub.New(), c.session, true, &calls}}
			env, home, _, errb := retiredHostEnv(t, p, map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, &spawned)
			root := filepath.Join(home, "plugin")
			os.MkdirAll(root, 0o700)
			for k, v := range map[string]string{capture.KeySwitch: "on", capture.KeyAck: capture.AckVersion, capture.KeyWriter: capture.WriterLocal, capture.KeyCommand: "x"} {
				if err := config.SetValue(filepath.Join(root, "config.toml"), k, v); err != nil {
					t.Fatal(err)
				}
			}
			paths := capture.PathsIn(root)
			if err := capture.RecordOffered(paths, time.Now(), c.offer); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := capture.Enqueue(paths, capture.Event{At: time.Now().UnixMilli(), Session: "s", Kind: "tool"}); err != nil {
					t.Fatal(err)
				}
			}
			if n := capture.QueueLen(paths); n != 2 {
				t.Fatalf("queued %d", n)
			}
			before := tree(paths.Dir)
			done := make(chan int, 1)
			go func() { done <- RunEnv(env, []string{"memory", "capture", "worker"}) }()
			select {
			case code := <-done:
				if code != ExitOK || errb.Len() != 0 {
					t.Fatalf("worker: exit %d %q", code, errb.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the worker did not finish")
			}
			if calls != 0 {
				t.Errorf("%d Fleet call(s) were made", calls)
			}
			if spawned != 0 {
				t.Errorf("%d relay start(s)", spawned)
			}
			if _, err := os.Lstat(filepath.Join(root, "stage")); !os.IsNotExist(err) {
				t.Errorf("a stage folder was created: %v", err)
			}
			if n, err := countNotes(root); err != nil || n != 0 {
				t.Errorf("notes written: %d %v", n, err)
			}
			dropped, _ := os.ReadFile(filepath.Join(paths.Dir, "dropped"))
			if !c.purged {
				if after := tree(paths.Dir); after != before {
					t.Errorf("the queue, the offer or the state changed:\n--- before\n%s--- after\n%s", before, after)
				}
				if st := capture.LoadState(paths); st.OfferedFor != c.offer || !st.Offered(time.Now()) {
					t.Errorf("the offer was reset: %+v", st)
				}
				return
			}
			if n := capture.QueueLen(paths); n != 0 {
				t.Errorf("%d event(s) still queued", n)
			}
			if len(dropped) != 2 {
				t.Errorf("%d event(s) counted as dropped, want 2", len(dropped))
			}
		})
	}
}

// countNotes is how many notes the device's own (signed-out) memory holds.
func countNotes(stateRoot string) (int, error) {
	if _, err := os.Stat(memory.ProfilePath(stateRoot, memory.Local)); os.IsNotExist(err) {
		return 0, nil
	}
	s, err := memory.OpenProfile(memory.ProfilePath(stateRoot, memory.Local), memory.Local)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return memory.NewService(memory.ServiceOptions{StateRoot: stateRoot}).Count(context.Background())
}

// The gate and mcp read the same --env: mcp's own. `mcp --env <allowed> --
// --env stage` is a server for the allowed environment, as mcp itself
// parses it, and not one in the retired-environment mode.
func TestMCPGateReadsWhatMCPParses(t *testing.T) {
	allowed := variant.Current.DefaultEnv
	calls, spawned := 0, 0
	p := stageApp{fleetCalls{stub.New(), fleetapi.Env(allowed), true, &calls}}
	env, home, _, errb := retiredHostEnv(t, p, nil, &spawned)
	served := ""
	env.ServeMCP = func(ctx context.Context, srv *sdk.Server) error {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			return err
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return err
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fleet_status", Arguments: map[string]any{}})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res)
		served = string(raw)
		// A Fleet tool reaches Fleet: the provider is the real one.
		res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "skills_search", Arguments: map[string]any{"query": "x"}})
		if err != nil {
			return err
		}
		raw, _ = json.Marshal(res)
		served += string(raw)
		return nil
	}
	if code := RunEnv(env, []string{"mcp", "--env", allowed, "--", "--env", "stage"}); code != ExitOK {
		t.Fatalf("exit %d %q", code, errb.String())
	}
	if strings.Contains(errb.String(), "retired") || strings.Contains(served, "retired") || !strings.Contains(served, `\"env\":\"`+allowed+`\"`) {
		t.Errorf("served as if for stage:\nstderr %q\nfleet_status %s", errb.String(), served)
	}
	if calls == 0 {
		t.Error("the Fleet tool never reached Fleet: the server ran in the retired-environment mode")
	}
	if _, err := os.Stat(filepath.Join(home, "plugin", allowed)); err != nil {
		t.Errorf("no state folder for %s: %v", allowed, err)
	}
}
