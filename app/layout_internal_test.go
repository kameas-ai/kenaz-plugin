package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// During `kenaz init` the report never tells the user to run init.
func TestReportMigrationDuringInit(t *testing.T) {
	res := config.MigrateResult{Moved: []string{"/old/state"}, LinkFailed: "no link to /new could be left at /old/state: denied"}
	cur := config.Paths{StateRoot: "/new", ConfigDir: "/new"}
	var b bytes.Buffer
	reportMigration(&b, res, cur, "init")
	if strings.Contains(b.String(), "init`") || !strings.Contains(b.String(), "registers the apps from /new") {
		t.Fatalf("init:\n%s", b.String())
	}
	b.Reset()
	reportMigration(&b, res, cur, "migrate")
	if !strings.Contains(b.String(), "init`") {
		t.Fatalf("migrate:\n%s", b.String())
	}
}

// `kenaz init` lists an older folder before it has registered the apps
// again, and that can still fail or be declined: it never says the folder
// can be deleted, and names `kenaz doctor`, which checks afterwards.
func TestInitAdviceDoesNotAssumeSetupSucceeded(t *testing.T) {
	old := t.TempDir()
	cur := config.Paths{StateRoot: t.TempDir()}
	var b bytes.Buffer
	reportMigration(&b, config.MigrateResult{Unused: []string{old}}, cur, "init")
	out := b.String()
	if strings.Contains(out, "delete") || !strings.Contains(out, "keep it for now") || !strings.Contains(out, "`"+variant.Current.Binary+" doctor`") {
		t.Fatalf("init:\n%s", out)
	}
}

// OpenCode's opencode.json embeds the relay state directory, so an old
// folder the adapter still reports to is in use until `kenaz setup
// opencode` runs from the root (init does not set OpenCode up).
func TestRegistrationsOfCountsOpenCode(t *testing.T) {
	was := features.OpenCode
	features.OpenCode = true
	t.Cleanup(func() { features.OpenCode = was })

	old := t.TempDir()
	cur := config.Paths{StateRoot: t.TempDir()}
	record := func(dir, stateDir string) {
		t.Helper()
		entry, _ := json.Marshal([]any{"/adapter", map[string]string{"state_dir": stateDir}})
		st, _ := json.Marshal(map[string]any{"version": 1, "variant": variant.Current.Name, "opencode": map[string]any{
			"path": "opencode.json", "keys": map[string]any{"plugin.kenaz": map[string]any{"written": string(entry)}}}})
		os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(filepath.Join(dir, "setup-state.json"), st, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record(filepath.Join(old, "dev"), filepath.Join(old, "dev"))
	want := variant.Current.Binary + " setup opencode"
	if variant.Current.DefaultEnv != "dev" { // the record is dev's
		want += " --env dev"
	}
	if fix, _ := registrationsOf(old, cur); len(fix) != 1 || fix[0] != want {
		t.Fatalf("old record: fix = %q, want %q", fix, want)
	}
	if got := unusedAdvice(old, cur, false); !strings.Contains(got, "do not delete it: run `"+want+"` first") {
		t.Fatalf("advice: %s", got)
	}
	// Set up again from the root: the old record no longer counts.
	record(cur.State("dev"), cur.State("dev"))
	if fix, re := registrationsOf(old, cur); len(fix) != 0 || !re {
		t.Fatalf("after setup from the root: fix = %q, reRegistered = %v", fix, re)
	}
	// Builds without OpenCode support never wrote such a record and ignore it.
	os.Remove(filepath.Join(cur.State("dev"), "setup-state.json"))
	features.OpenCode = false
	if fix, _ := registrationsOf(old, cur); len(fix) != 0 {
		t.Fatalf("without OpenCode support: fix = %q", fix)
	}
}

// The command that registers an app again acts on the build's default
// environment unless --env names another: the advice for a record in a
// non-default environment carries the flag, for the build that made the
// record (kenaz-dev's default is dev, kenaz's is prod).
func TestRegistrationsOfNamesTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, build, env, want string
	}{
		{"default env has no flag", "kenaz", "prod", "kenaz init"},
		{"record from before builds were recorded", "", "prod", "kenaz init"},
		{"kenaz dev", "kenaz", "dev", "kenaz init --env dev"},
		{"kenaz stage (retired): the undo", "kenaz", "stage", "kenaz setup --undo claude --env stage"},
		{"kenaz-dev default env has no flag", "kenaz-dev", "dev", "kenaz-dev init"},
		{"kenaz-dev stage (retired): the undo", "kenaz-dev", "stage", "kenaz-dev setup --undo claude --env stage"},
		{"an env the build cannot use is never named", "kenaz-dev", "prod", "kenaz-dev init"},
		{"unknown build", "other", "dev", "other init"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := t.TempDir()
			cur := config.Paths{StateRoot: t.TempDir()}
			dir := filepath.Join(old, tc.env)
			st, _ := json.Marshal(map[string]any{"version": 1, "variant": tc.build, "claude": map[string]any{
				"path": "x", "bundle": map[string]string{"rendered": filepath.Join(dir, "setup", "bundle", "x")}}})
			os.MkdirAll(dir, 0o700)
			if err := os.WriteFile(filepath.Join(dir, "setup-state.json"), st, 0o600); err != nil {
				t.Fatal(err)
			}
			if fix, _ := registrationsOf(old, cur); len(fix) != 1 || fix[0] != tc.want {
				t.Fatalf("fix = %q, want %q", fix, tc.want)
			}
			if got := unusedAdvice(old, cur, false); !strings.Contains(got, "do not delete it: run `"+tc.want+"` first") {
				t.Fatalf("advice: %s", got)
			}
		})
	}
}

// An app has one plugin registration for every environment. A stage record
// in the old folder stops counting once the same build set the app up from
// the root, for any environment; another build's setup, or another app's,
// changes nothing for it.
func TestRegistrationsOfStopsOnceTheAppIsSetUpFromTheRoot(t *testing.T) {
	old := t.TempDir()
	cur := config.Paths{StateRoot: t.TempDir()}
	record := func(dir, build, host string) {
		t.Helper()
		st, _ := json.Marshal(map[string]any{"version": 1, "variant": build, host: map[string]any{
			"path": "x", "bundle": map[string]string{"rendered": filepath.Join(dir, "setup", "bundle", "x")}}})
		os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(filepath.Join(dir, "setup-state.json"), st, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record(filepath.Join(old, "stage"), "kenaz", "claude")
	undo := "kenaz setup --undo claude --env stage"
	if fix, _ := registrationsOf(old, cur); len(fix) != 1 || fix[0] != undo {
		t.Fatalf("old stage record alone: %q", fix)
	}
	// Codex set up from the root, and Claude Code by the other build: the
	// record still stands.
	record(cur.State("dev"), "kenaz-dev", "claude")
	if fix, _ := registrationsOf(old, cur); len(fix) != 1 || fix[0] != undo {
		t.Fatalf("after another build's setup: %q", fix)
	}
	record(cur.State("prod"), "kenaz", "codex")
	if fix, _ := registrationsOf(old, cur); len(fix) != 1 || fix[0] != undo {
		t.Fatalf("after another app's setup: %q", fix)
	}
	// Claude Code set up from the root for prod, by the same build.
	record(cur.State("prod"), "kenaz", "claude")
	if fix, re := registrationsOf(old, cur); len(fix) != 0 || !re {
		t.Fatalf("after the app was set up from the root: fix %q, reRegistered %v", fix, re)
	}
	if got := unusedAdvice(old, cur, false); strings.Contains(got, "do not delete it") || strings.Contains(got, "--env stage") {
		t.Fatalf("advice: %s", got)
	}
}
