package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// retiredStageMessage is what each build says when stage is selected: it
// names only the environments that build can use.
func retiredStageMessage(t *testing.T) string {
	t.Helper()
	msg := map[string]string{
		"kenaz":     "The stage environment has been retired. Use dev or prod.",
		"kenaz-dev": "The stage environment has been retired. Use dev.",
	}[variant.Current.Name]
	if msg == "" {
		t.Fatalf("no expectations for build %s", variant.Current.Name)
	}
	return msg
}

// runWith runs a command on h with its own env variables (h.run always
// selects dev).
func (h *harness) runWith(vars map[string]string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := h.env(context.Background(), &out, &errb)
	env.Getenv = func(k string) string {
		if k == variant.Shared.HomeVar {
			return h.home
		}
		return vars[k]
	}
	code := app.RunEnv(env, args)
	return code, out.String(), errb.String()
}

// listing is everything under dir: every folder, link (with its target)
// and file (with its size and modification time), to tell that nothing
// changed, moved or was created.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		info, ierr := os.Lstat(p)
		switch {
		case ierr != nil:
			b.WriteString(rel + " ?\n")
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			b.WriteString(rel + " -> " + target + "\n")
		case info.IsDir():
			b.WriteString(rel + "/\n")
		default:
			fmt.Fprintf(&b, "%s %d %s\n", rel, info.Size(), info.ModTime())
		}
		return nil
	})
	return b.String()
}

// seedStage leaves what an earlier release left for the stage environment:
// a state directory with a consent record and a queued spool segment.
func (h *harness) seedStage(t *testing.T) string {
	t.Helper()
	return h.seedStageIn(t, filepath.Join(h.home, "plugin"))
}

// seedStageIn is seedStage in the state root given: $KENAZ_HOME/state is
// where an earlier release kept it (config.toml in $KENAZ_HOME/config).
func (h *harness) seedStageIn(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "stage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "consent.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := spool.GetOrCreateKey(h.ks, "stage")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(dir, "spool"), key, spool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sp.Append("telemetry", []byte("x"))
	sp.Close()
	return dir
}

// The stage environment was retired. Selecting it fails early, the same
// way in every command and however it was selected, and leaves everything
// on disk exactly as it was: what is there for stage, and where an earlier
// release's files are. The commands here move no files in any case; that a
// refused `init`, which does, moves none either is
// TestRefusedInitMovesNothing here and, with the built binaries,
// TestRefusedStageMovesNothingOnAnOldLayout in test/variant.
func TestStageIsRefused(t *testing.T) {
	for _, l := range []struct{ name, state, config string }{
		{"", "plugin", "plugin"},
		{"earlier release's layout: ", "state", "config"},
	} {
		testStageIsRefused(t, l.name, l.state, l.config)
	}
}

func testStageIsRefused(t *testing.T, layout, stateRoot, configDir string) {
	msg := retiredStageMessage(t)
	ev := variant.Shared.EnvVar
	for _, c := range []struct {
		name string
		vars map[string]string
		file string
		args []string
		want string
	}{
		// Only commands that read this harness's own temp home run here: were
		// the refusal ever lost, nothing would reach the real one. Setup,
		// init, login, doctor and mcp are covered with the built binaries on
		// an isolated machine (test/variant, TestStageRegistrationCanStillBeRemoved
		// and TestRefusedStageMovesNothingOnAnOldLayout).
		{"status --env", nil, "", []string{"status", "--env", "stage"}, msg},
		{"status --env=", nil, "", []string{"status", "--env=stage", "--json"}, msg},
		{"flag beats an allowed env var", map[string]string{ev: "dev"}, "", []string{"status", "--env", "STAGE"}, msg},
		{"env var", map[string]string{ev: "stage"}, "", []string{"status"}, msg + " (stage was selected by env)"},
		{"plugin option", map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, "", []string{"status"}, msg + " (stage was selected by env)"},
		{"env var, skills", map[string]string{ev: "stage"}, "", []string{"skills", "list"}, msg + " (stage was selected by env)"},
		{"env var, memory", map[string]string{ev: "stage"}, "", []string{"memory", "sync", "status"}, msg + " (stage was selected by env)"},
		{"config file", nil, "env = \"stage\"\n", []string{"status"}, msg + " (stage was selected by config)"},
		{"config file, approvals", nil, "env = \"stage\"\n", []string{"approvals"}, msg + " (stage was selected by config)"},
	} {
		t.Run(layout+c.name, func(t *testing.T) {
			h := newHarness(t, stub.New())
			h.seedStageIn(t, filepath.Join(h.home, stateRoot))
			if c.file != "" {
				if err := os.MkdirAll(filepath.Join(h.home, configDir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.home, configDir, "config.toml"), []byte(c.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := listing(t, h.home)
			code, out, errb := h.runWith(c.vars, c.args...)
			if code != app.ExitUsage || errb != c.want+"\n" || out != "" {
				t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant   %q", code, out, errb, c.want+"\n")
			}
			if after := listing(t, h.home); after != before {
				t.Fatalf("files changed:\n%s\nwere:\n%s", after, before)
			}
			if _, err := h.ks.Get("stage"); err != nil {
				t.Fatalf("the stage spool key went: %v", err)
			}
		})
	}

	// Nothing is created for stage where nothing was.
	h := newHarness(t, stub.New())
	if code, _, errb := h.runWith(nil, "status", "--env", "stage"); code != app.ExitUsage || errb != msg+"\n" {
		t.Fatalf("exit %d %q", code, errb)
	}
	if ents, _ := os.ReadDir(h.home); len(ents) != 0 {
		t.Fatalf("something was created: %v", ents)
	}
}

// A hook never fails or writes to a harness session, whatever the plugin
// was registered for; with stage selected it starts nothing for it.
func TestHookWithStageSelectedStaysQuiet(t *testing.T) {
	h := newHarness(t, stub.New())
	for _, event := range []string{"session-start", "session-end"} {
		code, _, errb := h.runWith(map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, "hook", event)
		if code != app.ExitOK || errb != "" {
			t.Fatalf("hook %s: exit %d stderr %q", event, code, errb)
		}
	}
	if _, err := os.Stat(filepath.Join(h.home, "plugin", "stage")); !os.IsNotExist(err) {
		t.Fatalf("a stage state directory was created: %v", err)
	}
}

// What an earlier sign-in left for stage can still be cleared: logout
// takes the retired environment, purges its queue and key, and leaves its
// folder and the other environments' state alone.
func TestLogoutStillWorksForStage(t *testing.T) {
	for _, c := range []struct {
		name string
		vars map[string]string
		args []string
	}{
		{"flag", nil, []string{"logout", "--env", "stage"}},
		{"env var", map[string]string{variant.Shared.EnvVar: "stage"}, []string{"logout"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, stub.New())
			dir := h.seedStage(t)
			devKey, _ := spool.GetOrCreateKey(h.ks, "dev")
			sp, _ := spool.Open(filepath.Join(h.state(), "spool"), devKey, spool.Options{})
			sp.Append("telemetry", []byte("x"))
			sp.Close()
			devBefore := listing(t, h.state())

			code, out, errb := h.runWith(c.vars, c.args...)
			if code != app.ExitUnofficial || !strings.Contains(out, "Local data purged") || strings.Contains(errb, "retired") {
				t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errb)
			}
			if ents, _ := os.ReadDir(filepath.Join(dir, "spool")); len(ents) != 0 {
				t.Fatalf("stage spool not purged: %v", ents)
			}
			if _, err := h.ks.Get("stage"); err != spool.ErrNoKey {
				t.Fatalf("stage spool key not deleted: %v", err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("the stage folder went: %v", err)
			}
			if after := listing(t, h.state()); after != devBefore {
				t.Fatalf("dev state changed:\n%s\nwas:\n%s", after, devBefore)
			}
			if _, err := h.ks.Get("dev"); err != nil {
				t.Fatalf("dev spool key went: %v", err)
			}
		})
	}
	_ = fleetapi.EnvStage
}

// On a machine that still has an earlier release's layout, logout for
// stage clears the stage queue and key where the files are: it is no
// `init` or `migrate`, so nothing moves and no new root appears.
func TestLogoutForStageOnAnOldLayoutMovesNothing(t *testing.T) {
	h := newHarness(t, stub.New())
	old := filepath.Join(h.home, "state")
	dir := h.seedStageIn(t, old)
	code, out, errb := h.runWith(nil, "logout", "--env", "stage")
	if code != app.ExitUnofficial || !strings.Contains(out, "Local data purged") || strings.Contains(errb, "retired") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errb)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "spool")); len(ents) != 0 {
		t.Fatalf("stage spool not purged: %v", ents)
	}
	if _, err := h.ks.Get("stage"); err != spool.ErrNoKey {
		t.Fatalf("stage spool key not deleted: %v", err)
	}
	if fi, err := os.Lstat(old); err != nil || !fi.IsDir() {
		t.Fatalf("the old state root is no longer a folder: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.home, "plugin")); !os.IsNotExist(err) {
		t.Fatalf("a new root was created: %v", err)
	}
}

// Signing out of stage where nothing was ever set up or signed in for it
// leaves no trace: no stage folder, and nothing else that was not there.
// (The undo of a host's setup is checked the same way with the built
// binaries: TestUndoOfNothingForStageLeavesNoTrace in test/variant.)
func TestCleanupOfNothingForStageLeavesNoTrace(t *testing.T) {
	for _, args := range [][]string{
		{"logout", "--env", "stage"},
		{"logout", "--env=STAGE"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t, stub.New())
			// A device in use for another environment.
			if code, _, errb := h.run(t, "status"); code != app.ExitOK {
				t.Fatalf("status: %d %s", code, errb)
			}
			before := listing(t, h.home)
			code, out, errb := h.runWith(nil, args...)
			if strings.Contains(errb, "retired") || code != app.ExitUnofficial || !strings.Contains(out, "Local data purged") {
				t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errb)
			}
			if _, err := os.Lstat(filepath.Join(h.home, "plugin", "stage")); !os.IsNotExist(err) {
				t.Errorf("a stage folder was created: %v", err)
			}
			if after := listing(t, h.home); after != before {
				t.Errorf("files changed:\n%s\nwere:\n%s", after, before)
			}
		})
	}
	// On a device with nothing at all, nothing is created.
	h := newHarness(t, stub.New())
	h.runWith(nil, "logout", "--env", "stage")
	if ents, _ := os.ReadDir(h.home); len(ents) != 0 {
		t.Errorf("logout for stage on an empty device created: %s", listing(t, h.home))
	}
}

// `kenaz context` uses no Fleet environment for the files on this device,
// but with stage selected the device is misconfigured, and it says so like
// every other command: nothing is listed, synced or granted for it.
func TestContextIsRefusedForStage(t *testing.T) {
	contextFeature(t, true)
	msg := retiredStageMessage(t)
	ev := variant.Shared.EnvVar
	for _, c := range []struct {
		vars map[string]string
		file string
		args []string
		want string
	}{
		{map[string]string{ev: "stage"}, "", []string{"context", "list"}, msg + " (stage was selected by env)"},
		{map[string]string{ev: "stage"}, "", []string{"context", "sync"}, msg + " (stage was selected by env)"},
		{map[string]string{"CLAUDE_PLUGIN_OPTION_FLEET_ENV": "stage"}, "", []string{"context", "grant", "status"}, msg + " (stage was selected by env)"},
		{nil, "env = \"stage\"\n", []string{"context", "list", "--json"}, msg + " (stage was selected by config)"},
	} {
		h := newHarness(t, stub.New())
		if c.file != "" {
			os.MkdirAll(filepath.Join(h.home, "plugin"), 0o700)
			if err := os.WriteFile(filepath.Join(h.home, "plugin", "config.toml"), []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before := listing(t, h.home)
		code, out, errb := h.runWith(c.vars, c.args...)
		if code != app.ExitUsage || errb != c.want+"\n" || out != "" {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q\nwant   %q", c.args, code, out, errb, c.want+"\n")
		}
		if after := listing(t, h.home); after != before {
			t.Errorf("%v changed files:\n%s\nwere:\n%s", c.args, after, before)
		}
	}
	// An environment that exists but this build does not connect to is no
	// reason to stop reading local files: only a retired one is.
	if !variant.Current.Allowed("prod") {
		h := newHarness(t, stub.New())
		if code, _, errb := h.runWith(map[string]string{ev: "prod"}, "context", "list"); code != app.ExitOK {
			t.Errorf("context list with prod selected: exit %d %q", code, errb)
		}
	}
}

// `kenaz migrate` moves the files of every environment and connects to
// none, so a retired selection does not stop it. It does not go on
// silently either: it says that it ignores the selection.
func TestMigrateSaysItIgnoresAStageSelection(t *testing.T) {
	for _, c := range []struct {
		name string
		vars map[string]string
		file string
		note string
	}{
		{"env var", map[string]string{variant.Shared.EnvVar: "stage"}, "", " (stage was selected by env)"},
		{"config file", nil, "env = \"stage\"\n", " (stage was selected by config)"},
		{"nothing selected", nil, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, stub.New())
			// An earlier release's layout, with a stage folder among the others.
			h.seedStageIn(t, filepath.Join(h.home, "state"))
			os.MkdirAll(filepath.Join(h.home, "config"), 0o700)
			if err := os.WriteFile(filepath.Join(h.home, "config", "config.toml"), []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errb := h.runWith(c.vars, "migrate")
			if code != app.ExitOK || !strings.Contains(out, "moved its files to "+filepath.Join(h.home, "plugin")) {
				t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errb)
			}
			bin := variant.Current.Binary
			want := ""
			if c.note != "" {
				want = bin + ": " + retiredStageMessage(t) + c.note + " `" + bin + " migrate` uses no Fleet environment and moves the files of all of them, so it goes on.\n"
			}
			if errb != want {
				t.Errorf("stderr %q\nwant   %q", errb, want)
			}
			if _, err := os.Stat(filepath.Join(h.home, "plugin", "stage", "consent.json")); err != nil {
				t.Errorf("the stage folder did not move with the rest: %v", err)
			}
		})
	}
}
