package variant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// retiredStage is what each build says when the retired stage environment
// is selected: it names only what that build can use.
var retiredStage = map[string]string{
	"kenaz":     "The stage environment has been retired. Use dev or prod.",
	"kenaz-dev": "The stage environment has been retired. Use dev.",
}

// A machine an earlier release set up for stage: this build refuses to use
// stage and changes nothing there, and `setup --undo <host> --env stage`
// still removes the old registration from Claude Code and Codex.
func TestStageRegistrationCanStillBeRemoved(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	hosts := []string{"claude", "codex"}
	for _, h := range hosts {
		m.mustRun(bin, nil, "setup", h, "--yes")
	}
	// The setup record of an environment lives in its state folder, so the
	// same record under stage is what a release that still had stage left.
	if err := os.Rename(l.state(id.DefaultEnv), l.state("stage")); err != nil {
		t.Fatal(err)
	}
	registered := m.claudeReg(id) + " " + m.codexReg(id)
	if strings.Contains(registered, "marketplace=null") || strings.Contains(registered, "absent") {
		t.Fatalf("setup registered nothing: %s", registered)
	}
	all := func(string) bool { return true }
	stageBefore := m.tree(l.state("stage"), all)

	// Using stage is refused, in every way of selecting it, and nothing on
	// the machine changes: the stage folder is neither deleted nor moved.
	want := retiredStage[id.Name]
	for _, c := range []struct {
		env  []string
		args []string
		want string
	}{
		{nil, []string{"status", "--env", "stage"}, want},
		{nil, []string{"login", "--env=stage"}, want},
		{nil, []string{"init", "--env", "stage", "--yes"}, want},
		{nil, []string{"setup", "claude", "--env", "stage", "--yes"}, want},
		{nil, []string{"setup", "doctor", "--env", "stage"}, want},
		{nil, []string{"setup", "claude", "--env", "stage", "--dry-run"}, want},
		// Only an undo of a host takes stage, as setup itself reads its flags:
		// the last --undo wins, a flag's value is not a flag, the doctor is no undo.
		{nil, []string{"setup", "--undo", "--undo=false", "claude", "--env", "stage", "--yes"}, want},
		{nil, []string{"setup", "--undo", "claude", "--env", "stage", "--yes", "--undo=false"}, want},
		{nil, []string{"setup", "claude", "--env", "stage", "--yes", "--bundle", "--undo"}, want},
		{nil, []string{"setup", "--undo", "doctor", "--env", "stage"}, want},
		{nil, []string{"optin", "--env", "stage"}, want},
		{nil, []string{"skills", "list", "--env", "stage"}, want},
		{[]string{variant.Shared.EnvVar + "=stage"}, []string{"status"}, want + " (stage was selected by env)"},
		{[]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV=stage"}, []string{"doctor"}, want + " (stage was selected by env)"},
		// The relay is never started for stage. (The MCP server is: it runs
		// signed out, TestMCPServerStillStartsForStage.)
		{[]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV=stage"}, []string{"daemon"}, want + " (stage was selected by env)"},
	} {
		if r := m.run(bin, c.env, c.args...); r.Code != 64 || r.Stderr != c.want+"\n" || r.Stdout != "" {
			t.Errorf("%v %v: exit %d\nstdout %q\nstderr %q\nwant   %q", c.env, c.args, r.Code, r.Stdout, r.Stderr, c.want+"\n")
		}
	}
	// Commands that select no environment, and ones for the default, leave
	// the stage folder alone too.
	for _, args := range [][]string{{"version"}, {"status"}, {"doctor"}, {"migrate"}} {
		m.run(bin, nil, args...)
	}
	if after := m.tree(l.state("stage"), all); after != stageBefore {
		t.Errorf("the stage folder changed:\n--- before\n%s\n--- after\n%s", stageBefore, after)
	}
	if got := m.claudeReg(id) + " " + m.codexReg(id); got != registered {
		t.Errorf("the registration changed: %s", got)
	}

	// A dry run of the undo shows it without doing it.
	if r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--dry-run"); r.Code != 0 || strings.Contains(r.Stderr, "retired") {
		t.Fatalf("undo dry run: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := m.claudeReg(id) + " " + m.codexReg(id); got != registered {
		t.Fatalf("a dry run changed the registration: %s", got)
	}
	// The undo removes the registration and restores the hosts' settings.
	for _, h := range hosts {
		r := m.run(bin, nil, "setup", "--undo", h, "--env", "stage", "--yes")
		if r.Code != 0 || !strings.Contains(r.Stdout, "Setup will remove the ") || strings.Contains(r.Stderr, "retired") {
			t.Fatalf("undo %s: exit %d\n%s%s", h, r.Code, r.Stdout, r.Stderr)
		}
	}
	if c := m.claudeReg(id) + " " + m.codexReg(id); !strings.Contains(c, "marketplace=null plugin=null") || strings.Count(c, "absent") != 2 {
		t.Errorf("still registered: %s", c)
	}
	if got := m.claudeEndpoint(); got != "" {
		t.Errorf("Claude Code still exports to %s", got)
	}
	// The folder itself stays: only the person deletes it.
	if _, err := os.Stat(l.state("stage")); err != nil {
		t.Errorf("the stage folder went: %v", err)
	}
	// The env variable selects it for the undo as well.
	if r := m.run(bin, []string{variant.Shared.EnvVar + "=stage"}, "setup", "--undo", "claude", "--yes"); r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") {
		t.Errorf("undo by env var: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
}

// A stage registration recorded only in an earlier release's state folder,
// on a machine whose root already has its own files (so the old folder is
// no longer used): `kenaz migrate` says not to delete the folder and names
// the command that frees it. That command removes the registration, and
// the advice then goes away.
func TestMigrateAdviceForAnOldStageRegistrationWorks(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	os.Remove(filepath.Join(l.state(id.DefaultEnv), "daemon.json"))

	// Put the files where 0.6.x had them, as a setup for stage, registered
	// from there.
	old := m.legacyState()
	os.MkdirAll(filepath.Dir(old), 0o700)
	if err := os.Rename(l.StateRoot, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(old, id.DefaultEnv), filepath.Join(old, "stage")); err != nil {
		t.Fatal(err)
	}
	was, now := filepath.Join(l.StateRoot, id.DefaultEnv), filepath.Join(old, "stage")
	for _, f := range []string{filepath.Join(m.home, ".claude", "settings.json"), filepath.Join(now, "setup-state.json")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(f, []byte(strings.ReplaceAll(string(b), was, now)), 0o600)
	}
	if !strings.Contains(m.claudeReg(id), now) {
		t.Fatalf("not registered from the old stage folder: %s", m.claudeReg(id))
	}
	// The root has its own files, so the old folder is no longer used.
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	os.Remove(filepath.Join(l.state(id.DefaultEnv), "daemon.json"))
	os.WriteFile(filepath.Join(l.state(id.DefaultEnv), "consent.json"), []byte("{}"), 0o600)

	advised := id.Binary + " setup --undo claude --env stage"
	r := m.mustRun(bin, nil, "migrate")
	if !strings.Contains(r.Stdout, old+" is from an earlier release and an app still loads the plugin from it, so do not delete it: run `"+advised+"` first") {
		t.Fatalf("migrate:\n%s%s", r.Stdout, r.Stderr)
	}
	// The advised command, as printed (with --yes: there is no terminal).
	args := append(strings.Fields(advised)[1:], "--yes")
	r = m.run(bin, nil, args...)
	if r.Code != 0 || !strings.Contains(r.Stdout, "Setup will remove the Kenaz plugin registered from") || strings.Contains(r.Stdout, "Nothing to undo") {
		t.Fatalf("%s: exit %d\n%s%s", advised, r.Code, r.Stdout, r.Stderr)
	}
	if reg := m.claudeReg(id); !strings.Contains(reg, "marketplace=null plugin=null") {
		t.Fatalf("still registered: %s", reg)
	}
	// The telemetry settings stay: from a record in an old folder the undo
	// never restores or strips the app's settings, since they may be a
	// current setup's that left no record. It says so, and how to go on.
	if got := m.claudeEndpoint(); got == "" {
		t.Errorf("the undo of an old-folder record stripped Claude Code's telemetry settings")
	}
	if !strings.Contains(r.Stdout, oldRecordSettingsNote(m, id)+"Removed the record of the earlier setup.\n") {
		t.Errorf("no word about the settings:\n%s", r.Stdout)
	}
	// Nothing was set up or left in the root for stage.
	if _, err := os.Stat(filepath.Join(l.state("stage"), "setup-state.json")); !os.IsNotExist(err) {
		t.Errorf("a stage setup record appeared in the root: %v", err)
	}
	r = m.mustRun(bin, nil, "migrate")
	if strings.Contains(r.Stdout, "do not delete it") || strings.Contains(r.Stdout, "--env stage") {
		t.Fatalf("migrate still advises the undo:\n%s", r.Stdout)
	}
	// Run again, it has nothing left to do.
	if r := m.run(bin, nil, args...); r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") {
		t.Fatalf("second undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
}

// One refusal, one exit code: stage is refused by the env gate, with exit
// 64 and the one message, before any step runs, however the command line
// spells it. Setup takes flags after `--` and after its positional
// arguments, and setup and init have flags that take a value, which may
// itself look like another --env; each form here selects stage as the
// command itself reads it. Nothing on the machine changes: init's PATH
// step and sign-in do not run first.
func TestStageIsRefusedInEverySyntacticForm(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	// A shell profile init's PATH step would edit.
	if err := os.WriteFile(filepath.Join(m.home, ".zshrc"), []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shell := []string{"SHELL=/bin/zsh"}
	before := exact(t, m.home)
	want := retiredStage[id.Name] + "\n"
	for _, args := range [][]string{
		// The flags come after `--`: setup reads them all the same.
		{"setup", "--", "claude", "--env", "stage", "--yes"},
		{"setup", "--yes", "--", "claude", "--env=stage"},
		// --bundle takes the next argument as its value, so the last --env
		// the command sees is stage.
		{"setup", "claude", "--yes", "--env", "stage", "--bundle", "--env=dev"},
		{"setup", "--env", "stage", "claude", "--yes", "--bundle", "--env=dev"},
		{"setup", "codex", "--yes", "-env=stage", "--release", "--env=dev"},
		{"setup", "doctor", "--env", "stage", "--commit", "--env=dev"},
		{"init", "--yes", "--env", "stage", "--bundle", "--env=dev"},
		{"init", "--env", "stage", "--bundle", "--env=dev"},
		{"init", "--yes", "--no-path", "-env=stage", "-bundle", "-env=dev"},
		// Other commands read their flags once, up to `--`.
		{"status", "--env", "dev", "--env", "stage"},
		{"doctor", "--", "--env", "stage"},
		{"login", "--no-browser", "--env=stage"},
	} {
		r := m.run(bin, shell, args...)
		if r.Code != 64 || r.Stderr != want || r.Stdout != "" {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q\nwant   %q", args, r.Code, r.Stdout, r.Stderr, want)
		}
		if after := exact(t, m.home); after != before {
			t.Fatalf("%v changed the home directory:\n--- before\n%s--- after\n%s", args, before, after)
		}
	}
	// A value that only looks like stage to a scan is not stage to the
	// command: here --bundle's value is "--env=stage" and the environment is
	// the default, so the gate lets the command through (it fails on the
	// bundle directory, or declines, on its own; nothing is refused as stage).
	for _, args := range [][]string{
		{"setup", "claude", "--dry-run", "--bundle", "--env=stage"},
		{"setup", "--undo", "claude", "--env", "stage", "--dry-run"},
	} {
		if r := m.run(bin, shell, args...); strings.Contains(r.Stderr, "retired") {
			t.Errorf("%v was refused as stage: exit %d %q", args, r.Code, r.Stderr)
		}
	}
}

// An app that still starts the MCP server with the stage option, on a
// machine an earlier release set up for stage: the process the app starts
// is a working MCP server, not one that exits with a usage error the
// person never sees. It runs signed out, starts no relay, leaves the stage
// folder exactly as it was, and fleet_status and a Fleet tool say what is
// wrong and how to fix it.
func TestMCPServerStillStartsForStage(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	if err := os.Rename(l.state(id.DefaultEnv), l.state("stage")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(l.state("stage"), "daemon.json"))
	m.stopRelays()
	before := exact(t, l.state("stage"))
	// Claude Code stores the plugin's option and passes it on: that is what to change.
	fix := "To fix it, set the " + id.DisplayName + " plugin's Kameas Fleet environment option to " + id.DefaultEnv + " in Claude Code: run `/plugin configure " + id.PluginID() + "` there. Then restart Claude Code."
	notice := "This app is still set up for the stage environment of Kameas Fleet, which has been retired, so Kameas Fleet is not available here. " + fix

	os.MkdirAll(filepath.Join(m.root, "tmp"), 0o700)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(m.environ(), "CLAUDE_PLUGIN_OPTION_FLEET_ENV=stage", "CLAUDE_PLUGIN_ROOT="+filepath.Join(m.root, "plugin"))
	cmd.Dir = m.home
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("the MCP server did not start: %v\nstderr %q", err, stderr.String())
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(res)
		return string(b)
	}
	quoted, _ := json.Marshal(notice)
	inJSON := strings.Trim(string(quoted), `"`)
	status := call("fleet_status", map[string]any{})
	if !strings.Contains(status, inJSON) {
		t.Errorf("fleet_status: %s", status)
	}
	if got := call("skills_search", map[string]any{"query": "x"}); !strings.Contains(got, "env_retired: "+inJSON) {
		t.Errorf("skills_search: %s", got)
	}
	if got := call("memory_recent", map[string]any{}); strings.Contains(got, `"isError":true`) {
		t.Errorf("memory_recent: %s", got)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("the server did not exit cleanly: %v", err)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("exit: %v", cmd.ProcessState)
	}
	if stderr.String() != id.Binary+": "+notice+"\n" {
		t.Errorf("stderr %q", stderr.String())
	}
	if after := exact(t, l.state("stage")); after != before {
		t.Errorf("the stage folder changed:\n--- before\n%s--- after\n%s", before, after)
	}
	if left := relaysUnder(withResolved([]string{m.root})); len(left) != 0 {
		t.Errorf("a relay was started: %v", left)
	}
}

// An old stage record in an earlier release's folder, and Claude Code set
// up again since, from the root, for the build's own environment. An app
// has one plugin registration and one settings file for every
// environment, so they are the current setup's now. `kenaz migrate` no
// longer advises the stage undo, and that undo, run all the same, removes
// only the stale record: the registration and the app's settings are
// byte-identical afterwards.
func TestUndoOfAnOldStageRecordLeavesTheCurrentSetupAlone(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	os.Remove(filepath.Join(l.state(id.DefaultEnv), "daemon.json"))
	old := m.legacyState()
	os.MkdirAll(filepath.Dir(old), 0o700)
	if err := os.Rename(l.StateRoot, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(old, id.DefaultEnv), filepath.Join(old, "stage")); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(m.home, ".claude", "settings.json")
	was, now := filepath.Join(l.StateRoot, id.DefaultEnv), filepath.Join(old, "stage")
	for _, f := range []string{settings, filepath.Join(now, "setup-state.json")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(f, []byte(strings.ReplaceAll(string(b), was, now)), 0o600)
	}
	// The root has its own files, and the person sets Claude Code up again
	// from it: the old marketplace is removed with the app's own command
	// (what setup's refusal says to do), then setup.
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	hc := exec.Command(filepath.Join(built.hosts, "claude"), "plugin", "marketplace", "remove", id.Marketplace)
	hc.Env, hc.Dir = m.environ(), m.home
	if b, err := hc.CombinedOutput(); err != nil {
		t.Fatalf("marketplace remove: %v %s", err, b)
	}
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	m.stopRelays()
	reg, endpoint := m.claudeReg(id), m.claudeEndpoint()
	if !strings.Contains(reg, l.state(id.DefaultEnv)) || endpoint == "" {
		t.Fatalf("not set up from the root's %s folder: %s %s", id.DefaultEnv, reg, endpoint)
	}
	if !strings.Contains(readOptional(filepath.Join(now, "setup-state.json")), `"claude"`) {
		t.Fatal("the old stage record is gone before the undo")
	}

	// migrate no longer says an app loads the plugin from the old folder.
	if r := m.mustRun(bin, nil, "migrate"); strings.Contains(r.Stdout, "do not delete it") || strings.Contains(r.Stdout, "--env stage") {
		t.Errorf("migrate still advises the stage undo:\n%s", r.Stdout)
	}

	settingsBefore, homeBefore := readOptional(settings), exact(t, m.home)
	record := filepath.Join(strings.TrimPrefix(now, m.home+string(filepath.Separator)), "setup-state.json")
	onlyRecord := func(after string) {
		t.Helper()
		b, a := strings.Split(homeBefore, "\n"), strings.Split(after, "\n")
		if len(a) != len(b) {
			t.Fatalf("entries were added or removed:\n--- before\n%s--- after\n%s", homeBefore, after)
		}
		for i := range b {
			if a[i] != b[i] && !strings.HasPrefix(b[i], record+" ") {
				t.Errorf("changed: %s\n     was: %s", a[i], b[i])
			}
		}
	}
	// A dry run changes nothing at all.
	r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--dry-run")
	if r.Code != 0 || !strings.Contains(r.Stdout, "has been set up again since this earlier setup") || exact(t, m.home) != homeBefore {
		t.Fatalf("dry run: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	r = m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes")
	if r.Code != 0 || !strings.Contains(r.Stdout, "Claude Code has been set up again since this earlier setup (recorded in "+now+"), so its plugin registration and its settings are the current setup's and were not changed.") ||
		!strings.Contains(r.Stdout, "Removed only the record of the earlier setup.") || strings.Contains(r.Stdout, "Setup will remove") {
		t.Fatalf("undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := readOptional(settings); got != settingsBefore {
		t.Errorf("Claude Code's settings changed:\n--- before\n%s\n--- after\n%s", settingsBefore, got)
	}
	if got := m.claudeReg(id); got != reg {
		t.Errorf("the registration changed: %s", got)
	}
	if got := m.claudeEndpoint(); got != endpoint {
		t.Errorf("the telemetry endpoint changed: %q", got)
	}
	onlyRecord(exact(t, m.home))
	if strings.Contains(readOptional(filepath.Join(now, "setup-state.json")), `"claude"`) {
		t.Error("the stale record is still there")
	}
	// The host CLI was not asked to uninstall or remove anything by the undo.
	log := readOptional(m.log)
	if i := strings.LastIndex(log, "marketplace add"); i >= 0 && (strings.Contains(log[i:], "uninstall") || strings.Contains(log[i:], "marketplace remove")) {
		t.Errorf("the undo ran the host CLI:\n%s", log[i:])
	}
	if r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes"); r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") {
		t.Errorf("second undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
}

// toOldStage turns the root's setup for the build's own environment into an
// earlier release's stage setup, in the folder that release used.
func (m *machine) toOldStage(l layout, id variant.Identity) (stage string) {
	m.t.Helper()
	os.Remove(filepath.Join(l.state(id.DefaultEnv), "daemon.json"))
	old := m.legacyState()
	os.MkdirAll(filepath.Dir(old), 0o700)
	if err := os.Rename(l.StateRoot, old); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(old, id.DefaultEnv), filepath.Join(old, "stage")); err != nil {
		m.t.Fatal(err)
	}
	was := filepath.Join(l.StateRoot, id.DefaultEnv)
	stage = filepath.Join(old, "stage")
	for _, f := range []string{filepath.Join(m.home, ".claude", "settings.json"), filepath.Join(stage, "setup-state.json")} {
		b, err := os.ReadFile(f)
		if err != nil {
			m.t.Fatal(err)
		}
		os.WriteFile(f, []byte(strings.ReplaceAll(string(b), was, stage)), 0o600)
	}
	return stage
}

// An old stage record that registered no plugin of its own (kenaz from
// PATH; or the public marketplace the person had added, which setup
// adopted). A later setup from the root finds the settings it would write
// already there, says "already configured" and records nothing, so nothing
// tells the old record's work from the current setup's. The stage undo
// therefore never runs the host CLI and never touches the app's settings:
// it removes only the stale record, and says how to remove the current
// setup.
func TestUndoOfAnOldStageRecordWithoutABundleTouchesNothingInTheApp(t *testing.T) {
	id := variant.Current
	for _, adopted := range []bool{false, true} {
		name := "kenaz from PATH"
		if adopted {
			name = "an adopted marketplace"
		}
		t.Run(name, func(t *testing.T) {
			if adopted && id.MarketplaceRepo == "" {
				t.Skip("this build has no public marketplace")
			}
			m := newMachine(t)
			l := m.layout()
			m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
			if adopted {
				km := filepath.Join(m.home, ".claude", "plugins", "known_marketplaces.json")
				os.MkdirAll(filepath.Dir(km), 0o700)
				os.WriteFile(km, []byte(`{"`+id.Marketplace+`": {"source": {"source": "github", "repo": "`+id.MarketplaceRepo+`"}}}`), 0o600)
			}
			bin := built.kenaz
			m.mustRun(bin, nil, "setup", "claude", "--yes", "--no-bundle")
			stage := m.toOldStage(l, id)
			record := filepath.Join(stage, "setup-state.json")
			if rec := readOptional(record); !strings.Contains(rec, `"claude"`) || strings.Contains(rec, `"bundle"`) || strings.Contains(rec, `"installed_plugin": true`) != adopted {
				t.Fatalf("not the record this test is about:\n%s", rec)
			}
			// The root has its own files; setup from it changes nothing.
			m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
			if r := m.mustRun(bin, nil, "setup", "claude", "--yes", "--no-bundle"); !strings.Contains(r.Stdout, "already configured") {
				t.Fatalf("setup from the root:\n%s%s", r.Stdout, r.Stderr)
			}
			m.stopRelays()
			if rec := readOptional(filepath.Join(l.state(id.DefaultEnv), "setup-state.json")); strings.Contains(rec, `"claude"`) {
				t.Fatalf("the root has a record after all:\n%s", rec)
			}
			// migrate does not send anyone to the undo for such a record.
			if r := m.mustRun(bin, nil, "migrate"); strings.Contains(r.Stdout, "--env stage") {
				t.Errorf("migrate advises the stage undo:\n%s", r.Stdout)
			}

			settings := filepath.Join(m.home, ".claude", "settings.json")
			settingsBefore, logBefore, claudeBefore, rootBefore := readOptional(settings), readOptional(m.log), exact(t, filepath.Join(m.home, ".claude")), exact(t, l.StateRoot)
			if settingsBefore == "" || m.claudeEndpoint() == "" {
				t.Fatal("Claude Code is not set up")
			}
			want := "This earlier setup of Claude Code (recorded in " + stage + ") registered no plugin of its own, so the plugin and the settings Claude Code has now may be the current setup's. They were left as they are; to remove the current setup, run `" + id.Binary + " setup --undo claude`.\n"
			r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--dry-run")
			if r.Code != 0 || !strings.HasPrefix(r.Stdout, want+"(dry run: the record of the earlier setup was kept)\n") || !strings.Contains(readOptional(record), `"claude"`) {
				t.Fatalf("dry run: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
			}
			r = m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes")
			if r.Code != 0 || !strings.HasPrefix(r.Stdout, want+"Removed only the record of the earlier setup.\n") {
				t.Fatalf("undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
			}
			if got := readOptional(m.log); got != logBefore {
				t.Errorf("the undo ran the host CLI:\n%s", strings.TrimPrefix(got, logBefore))
			}
			if got := readOptional(settings); got != settingsBefore {
				t.Errorf("Claude Code's settings changed:\n--- before\n%s\n--- after\n%s", settingsBefore, got)
			}
			if got := exact(t, filepath.Join(m.home, ".claude")); got != claudeBefore {
				t.Errorf("Claude Code's folder changed:\n--- before\n%s--- after\n%s", claudeBefore, got)
			}
			if got := exact(t, l.StateRoot); got != rootBefore {
				t.Errorf("the root changed:\n--- before\n%s--- after\n%s", rootBefore, got)
			}
			if strings.Contains(readOptional(record), `"claude"`) {
				t.Error("the stale record is still there")
			}
			// The current setup is removed by the undo for its own environment.
			if r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes"); r.Code != 0 || !strings.Contains(r.Stdout, "Nothing to undo") {
				t.Errorf("second undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
			}
		})
	}
}

// An old stage record with a bundle, made by this build, and Claude Code
// set up since by the other build, which has its own plugin there but
// writes the same telemetry keys with the same values. The stage undo
// removes this build's stale plugin, which Claude Code still loads from the
// old folder, and leaves every setting alone: they are the other build's
// now.
func TestUndoOfAnOldStageRecordLeavesTheOtherBuildsSettings(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	var other variant.Identity
	for _, v := range variant.Variants {
		if v.Name != id.Name {
			other = v
		}
	}
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	stage := m.toOldStage(l, id)
	m.prepare(l, other.DefaultEnv, variant.Shared.RelayPorts[0]+1)
	obin := m.installBundle(other.BundleName, built.bins[other.Name], pluginTree(t, other))
	m.mustRun(obin, nil, "setup", "claude", "--yes")
	m.stopRelays()
	settings := filepath.Join(m.home, ".claude", "settings.json")
	env := func() string {
		var s struct{ Env map[string]any }
		json.Unmarshal([]byte(readOptional(settings)), &s)
		b, _ := json.MarshalIndent(s.Env, "", " ")
		return string(b)
	}
	envBefore, regOther := env(), m.claudeReg(other)
	if !strings.Contains(m.claudeReg(id), stage) || strings.Contains(regOther, "null") || !strings.Contains(envBefore, "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("not the state this test is about: %s; %s\n%s", m.claudeReg(id), regOther, envBefore)
	}
	r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes")
	if r.Code != 0 || !strings.Contains(r.Stdout, "Setup will remove the Kenaz plugin registered from") ||
		!strings.Contains(r.Stdout, oldRecordSettingsNote(m, id)+"Removed the record of the earlier setup.\n") {
		t.Fatalf("undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	// This build's stale plugin is gone; the other build's setup is whole.
	if reg := m.claudeReg(id); !strings.Contains(reg, "marketplace=null plugin=null") {
		t.Errorf("%s's stale plugin is still registered: %s", id.Name, reg)
	}
	if got := m.claudeReg(other); got != regOther {
		t.Errorf("%s's registration changed: %s", other.Name, got)
	}
	if got := env(); got != envBefore {
		t.Errorf("the telemetry settings changed:\n--- before\n%s\n--- after\n%s", envBefore, got)
	}
	if strings.Contains(readOptional(filepath.Join(stage, "setup-state.json")), `"claude"`) {
		t.Error("the stale record is still there")
	}
}

// oldRecordSettingsNote is what the undo of a record in an earlier
// release's folder says about Claude Code's settings.
func oldRecordSettingsNote(m *machine, id variant.Identity) string {
	return "The settings this earlier setup wrote to " + filepath.Join(m.home, ".claude", "settings.json") + " were left as they are: they may be the current setup's. If Claude Code is set up now, `" +
		id.Binary + " setup --undo claude` removes that setup with its settings; `" + id.Binary + " init` sets it up again from here. Settings no current setup owns can be deleted from that file by hand.\n"
}

// An old stage record WITH a bundle, whose plugin Claude Code still loads
// from the old folder, and a setup from the root since that found the
// settings as it would write them: "already configured", nothing changed,
// no record. The stage undo removes the stale plugin, which is the old
// record's own, and nothing else: the telemetry settings the later setup
// confirmed stay exactly as they are, no backup copy of the settings is
// made, and only the old record goes.
func TestUndoOfAnOldBundleRecordAfterANoOpSetupKeepsTheSettings(t *testing.T) {
	m := newMachine(t)
	id := variant.Current
	l := m.layout()
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	bin := m.installBundle(id.BundleName, built.kenaz, pluginTree(t, id))
	m.mustRun(bin, nil, "setup", "claude", "--yes")
	stage := m.toOldStage(l, id)
	m.prepare(l, id.DefaultEnv, variant.Shared.RelayPorts[0])
	if r := m.mustRun(bin, nil, "setup", "claude", "--yes", "--no-bundle"); !strings.Contains(r.Stdout, "already configured") {
		t.Fatalf("setup from the root:\n%s%s", r.Stdout, r.Stderr)
	}
	m.stopRelays()
	if rec := readOptional(filepath.Join(l.state(id.DefaultEnv), "setup-state.json")); strings.Contains(rec, `"claude"`) {
		t.Fatalf("the root has a record after all:\n%s", rec)
	}
	if !strings.Contains(m.claudeReg(id), stage) {
		t.Fatalf("Claude Code does not load the plugin from the old folder: %s", m.claudeReg(id))
	}
	settings := filepath.Join(m.home, ".claude", "settings.json")
	// What only the app's own commands change: the plugin entries. What the
	// undo must not touch: everything else in the file.
	// (Compared as values: the app's own command rewrites the file when it
	// removes its plugin entries, and may order the keys differently.)
	rest := func() string {
		var all map[string]any
		if err := json.Unmarshal([]byte(readOptional(settings)), &all); err != nil {
			t.Fatal(err)
		}
		delete(all, "enabledPlugins")
		delete(all, "extraKnownMarketplaces")
		b, _ := json.MarshalIndent(all, "", " ")
		return string(b)
	}
	restBefore, rootBefore := rest(), exact(t, l.StateRoot)
	if !strings.Contains(restBefore, "OTEL_EXPORTER_OTLP_ENDPOINT") || !strings.Contains(restBefore, "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("no telemetry settings to keep: %s", restBefore)
	}
	entries := func() string {
		ents, _ := os.ReadDir(filepath.Join(m.home, ".claude"))
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		return strings.Join(names, " ")
	}
	entriesBefore := entries()

	// A dry run changes nothing at all.
	whole := exact(t, m.home)
	if r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--dry-run"); r.Code != 0 || exact(t, m.home) != whole {
		t.Fatalf("dry run: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	os.Remove(m.log)
	r := m.run(bin, nil, "setup", "--undo", "claude", "--env", "stage", "--yes")
	if r.Code != 0 || !strings.Contains(r.Stdout, oldRecordSettingsNote(m, id)+"Removed the record of the earlier setup.\n") {
		t.Fatalf("undo: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	// The host was asked for exactly the removal of the stale plugin.
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(readOptional(m.log)), "\n") {
		if i := strings.Index(line, "plugin "); i >= 0 {
			calls = append(calls, line[i:])
		}
	}
	want := []string{"plugin uninstall " + id.PluginID(), "plugin marketplace remove " + id.Marketplace}
	if strings.Join(calls, "; ") != strings.Join(want, "; ") {
		t.Errorf("host commands: %q, want %q\n%s", calls, want, readOptional(m.log))
	}
	if reg := m.claudeReg(id); !strings.Contains(reg, "marketplace=null plugin=null") {
		t.Errorf("the stale plugin is still registered: %s", reg)
	}
	// Every other setting is as it was, and nothing was added beside the
	// file: no backup, no temporary copy.
	if got := rest(); got != restBefore {
		t.Errorf("the settings changed:\n--- before\n%s\n--- after\n%s", restBefore, got)
	}
	if got := entries(); got != entriesBefore {
		t.Errorf("files beside the settings changed: %q, were %q", got, entriesBefore)
	}
	if got := exact(t, l.StateRoot); got != rootBefore {
		t.Errorf("the root changed:\n--- before\n%s--- after\n%s", rootBefore, got)
	}
	if strings.Contains(readOptional(filepath.Join(stage, "setup-state.json")), `"claude"`) {
		t.Error("the stale record is still there")
	}
}
