package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

// githubClaude is what `claude plugin marketplace add kameas-ai/kenaz-plugin`
// and `claude plugin install kenaz@kenaz` leave in settings.json.
const githubClaude = `{
  "extraKnownMarketplaces": {
    "kenaz": {
      "source": {
        "source": "github",
        "repo": "kameas-ai/kenaz-plugin"
      }
    }
  },
  "enabledPlugins": {
    "kenaz@kenaz": true
  }
}
`

// githubCodex is what `codex plugin marketplace add kameas-ai/kenaz-plugin`
// (Codex 0.164) leaves in config.toml, before `codex plugin add`.
const githubCodex = "[marketplaces.kenaz]\nsource_type = \"git\"\nsource = \"https://github.com/kameas-ai/kenaz-plugin.git\"\n"

func TestOfficialSource(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	for src, want := range map[string]bool{
		"kameas-ai/kenaz-plugin":                        true,
		"Kameas-AI/Kenaz-Plugin":                        true,
		"kameas-ai/kenaz-plugin#v0.4.1":                 true,
		"https://github.com/kameas-ai/kenaz-plugin.git": true,
		"https://github.com/kameas-ai/kenaz-plugin":     true,
		"git@github.com:kameas-ai/kenaz-plugin.git":     true,
		"https://github.com/kameas-ai/kenaz-plugin-dev": false,
		"kameas-ai/kenaz-plugin-dev":                    false,
		"https://evil.example/kameas-ai/kenaz-plugin":   false,
		"https://github.com/someone/kenaz-plugin.git":   false,
		"": false,
		"/home/u/.local/state/kameas/kenaz/setup/bundle/0.4": false,
	} {
		if got := isOfficialSource(src); got != want {
			t.Errorf("isOfficialSource(%q) = %v, want %v", src, got, want)
		}
	}
	if m := codexMarketplace(githubCodex); !m.official() || m.Enabled {
		t.Fatalf("codex github marketplace: %+v", m)
	}
}

// A downloaded kenaz (bundle detected, no --bundle) adopts the GitHub
// marketplace the user already registered: no host command, no refusal,
// only the relay keys.
func TestAdoptGitHubMarketplaceClaude(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.ClaudeDir, 0o700)
	os.WriteFile(e.opts.path(harnessClaude), []byte(githubClaude), 0o600)
	onPath := true
	e.opts.KenazOnPath = func() bool { return onPath }

	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("host commands run: %v", f.calls)
	}
	if !strings.Contains(e.out.String(), "from GitHub") || e.err.Len() != 0 {
		t.Fatalf("out %q err %q", e.out, e.err)
	}
	got, _ := e.read(t, harnessClaude)
	if !bytes.Contains(got, []byte(`"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"`)) || !bytes.Contains(got, []byte(`"repo": "kameas-ai/kenaz-plugin"`)) {
		t.Fatalf("settings:\n%s", got)
	}
	st, _ := loadState(e.opts.StateDir)
	if st.Claude.Bundle != nil || st.Claude.Adopted == nil || st.Claude.Adopted.InstalledPlugin {
		t.Fatalf("state %+v", st.Claude)
	}
	if _, err := os.Stat(filepath.Join(e.opts.StateDir, "setup", "bundle")); !os.IsNotExist(err) {
		t.Fatal("rendered a bundle plugin while adopting")
	}

	// Idempotent.
	e.out.Reset()
	if err := Apply(e.opts, harnessClaude); err != nil || !strings.Contains(e.out.String(), "already configured") {
		t.Fatalf("second run: %v %s", err, e.out)
	}

	// Undo restores the file and leaves the user's marketplace alone.
	if err := Undo(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.read(t, harnessClaude); string(got) != githubClaude || len(f.calls) != 0 {
		t.Fatalf("undo: calls %v\n%s", f.calls, got)
	}

	// Not on PATH: warn, still configure.
	onPath = false
	e.err.Reset()
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.err.String(), "kenaz is not on PATH") || !strings.Contains(e.err.String(), "kenaz init") {
		t.Fatalf("no PATH warning: %q", e.err)
	}
}

// When the GitHub marketplace is registered but the plugin is not
// installed, setup installs it with the host's own command and uninstalls
// only that on --undo.
func TestAdoptGitHubMarketplaceCodexInstallsPlugin(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex), 0o600)

	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "|") != "codex plugin add kenaz@kenaz" {
		t.Fatalf("calls %v", f.calls)
	}
	got, _ := e.read(t, harnessCodex)
	if !strings.Contains(string(got), `source = "https://github.com/kameas-ai/kenaz-plugin.git"`) || !strings.Contains(string(got), "[otel]") {
		t.Fatalf("config:\n%s", got)
	}
	st, _ := loadState(e.opts.StateDir)
	if st.Codex.Adopted == nil || !st.Codex.Adopted.InstalledPlugin {
		t.Fatalf("state %+v", st.Codex)
	}

	f.calls = nil
	if err := Undo(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "|") != "codex plugin remove kenaz@kenaz" {
		t.Fatalf("undo calls %v", f.calls)
	}
	if got, _ := e.read(t, harnessCodex); !strings.Contains(string(got), "[marketplaces.kenaz]") || strings.Contains(string(got), "[otel]") {
		t.Fatalf("after undo:\n%s", got)
	}
}

// --no-bundle (no bundle root) with the GitHub marketplace adopts it too.
func TestAdoptWithoutBundle(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e := newEnv(t, 4318)
	f := &fakeHosts{t: t, o: &e.opts}
	e.opts.HostRun = f.run
	os.MkdirAll(e.opts.ClaudeDir, 0o700)
	os.WriteFile(e.opts.path(harnessClaude), []byte(githubClaude), 0o600)
	if err := Apply(e.opts, harnessClaude); err != nil || len(f.calls) != 0 {
		t.Fatalf("%v %v", err, f.calls)
	}
	if st, _ := loadState(e.opts.StateDir); st.Claude.Adopted == nil {
		t.Fatal("not recorded as adopted")
	}
}

// An explicit --bundle replaces the GitHub marketplace with the bundle's
// copy, and --undo says how to add the GitHub one back.
func TestExplicitBundleReplacesGitHubMarketplace(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	e.opts.BundleExplicit = true
	os.MkdirAll(e.opts.ClaudeDir, 0o700)
	os.WriteFile(e.opts.path(harnessClaude), []byte(githubClaude), 0o600)
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	want := []string{"claude plugin uninstall kenaz@kenaz", "claude plugin marketplace remove kenaz", "claude plugin marketplace add ", "claude plugin install kenaz@kenaz"}
	if len(f.calls) != len(want) {
		t.Fatalf("calls %v", f.calls)
	}
	for i, w := range want {
		if !strings.HasPrefix(f.calls[i], w) {
			t.Fatalf("call %d = %q, want %q...", i, f.calls[i], w)
		}
	}
	st, _ := loadState(e.opts.StateDir)
	if st.Claude.Bundle == nil || st.Claude.Bundle.Replaced != "kameas-ai/kenaz-plugin" || st.Claude.Adopted != nil {
		t.Fatalf("state %+v", st.Claude)
	}
	if src, en := e.opts.registeredMarketplace(harnessClaude); src != st.Claude.Bundle.Rendered || !en {
		t.Fatalf("registered %q %v", src, en)
	}
	e.out.Reset()
	if err := Undo(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "claude plugin marketplace add kameas-ai/kenaz-plugin") {
		t.Fatalf("no hint: %s", e.out)
	}
}

// Setup starts the relay itself when it is not running.
func TestSetupStartsRelay(t *testing.T) {
	e := newEnv(t, 4318)
	os.Remove(filepath.Join(e.opts.StateDir, daemonstate.FileName))
	started := 0
	e.opts.StartRelay = func() error {
		started++
		e.setPort(t, 47318)
		return nil
	}
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	got, _ := e.read(t, harnessClaude)
	if started != 1 || !bytes.Contains(got, []byte("127.0.0.1:47318")) || !strings.Contains(e.out.String(), "Starting the Kenaz relay") {
		t.Fatalf("started %d out %s\n%s", started, e.out, got)
	}

	// A relay that never comes up is a clear error, and nothing is written.
	e2 := newEnv(t, 4318)
	os.Remove(filepath.Join(e2.opts.StateDir, daemonstate.FileName))
	e2.opts.StartRelay = func() error { return nil }
	e2.opts.RelayWait = 100 * time.Millisecond
	if err := Apply(e2.opts, harnessClaude); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("got %v", err)
	}
	e3 := newEnv(t, 4318)
	os.Remove(filepath.Join(e3.opts.StateDir, daemonstate.FileName))
	e3.opts.StartRelay = func() error { return errors.New("boom") }
	if err := Apply(e3.opts, harnessClaude); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	if _, ok := e3.read(t, harnessClaude); ok {
		t.Fatal("wrote without a relay")
	}
}

// After the state directory moves, a bundle registration recorded
// under the old location is still recognized as setup's own and is
// re-registered from the new location instead of being refused.
func TestStateMoveRebasesBundleRegistration(t *testing.T) {
	e, f, _ := bundleEnv(t, false)
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	oldState := e.opts.StateDir
	newState := filepath.Join(filepath.Dir(oldState), "moved", "state")
	os.MkdirAll(filepath.Dir(newState), 0o700)
	if err := os.Rename(oldState, newState); err != nil {
		t.Fatal(err)
	}
	e.opts.StateDir = newState
	st, err := loadState(newState)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.Claude.Bundle.Rendered, newState) || !strings.HasPrefix(st.Claude.Bundle.formerRendered, oldState) {
		t.Fatalf("not rebased: %+v", st.Claude.Bundle)
	}
	if st.Claude.Snapshot != "" && !strings.HasPrefix(st.Claude.Snapshot, newState) {
		t.Fatalf("snapshot not rebased: %s", st.Claude.Snapshot)
	}
	f.calls = nil
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 || !strings.Contains(f.calls[2], newState) {
		t.Fatalf("calls %v", f.calls)
	}
	if src, _ := e.opts.registeredMarketplace(harnessClaude); !strings.HasPrefix(src, newState) {
		t.Fatalf("registered %s", src)
	}
}

// A plugin the user installed and then turned off is theirs: setup does
// not install, enable or (on --undo) uninstall it, and says it is off.
func TestAdoptInstalledButDisabled(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	cases := []struct {
		name string
		h    harness
		seed func(e *env)
	}{
		{"claude disabled", harnessClaude, func(e *env) {
			os.MkdirAll(e.opts.ClaudeDir, 0o700)
			os.WriteFile(e.opts.path(harnessClaude), []byte(strings.Replace(githubClaude, `"kenaz@kenaz": true`, `"kenaz@kenaz": false`, 1)), 0o600)
		}},
		{"claude installed, not listed in settings", harnessClaude, func(e *env) {
			os.MkdirAll(filepath.Join(e.opts.ClaudeDir, "plugins"), 0o700)
			os.WriteFile(e.opts.path(harnessClaude), []byte(`{"extraKnownMarketplaces": {"kenaz": {"source": {"source": "github", "repo": "kameas-ai/kenaz-plugin"}}}}`), 0o600)
			os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "installed_plugins.json"), []byte(`{"version": 2, "plugins": {"kenaz@kenaz": [{"scope": "user", "version": "0.4.1"}]}}`), 0o600)
		}},
		{"codex disabled", harnessCodex, func(e *env) {
			os.MkdirAll(e.opts.CodexDir, 0o700)
			os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex+"\n[plugins.\"kenaz@kenaz\"]\nenabled = false\n"), 0o600)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, f, _ := bundleEnv(t, false)
			c.seed(e)
			before, _ := e.read(t, c.h)
			if err := Apply(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("host commands run: %v", f.calls)
			}
			st, _ := loadState(e.opts.StateDir)
			if fs := st.get(c.h); fs.Adopted == nil || fs.Adopted.InstalledPlugin {
				t.Fatalf("state %+v", fs)
			}
			if strings.Contains(c.name, "disabled") && !strings.Contains(e.err.String(), "turned off") {
				t.Fatalf("no warning: %q", e.err)
			}
			if err := Undo(e.opts, c.h); err != nil || len(f.calls) != 0 {
				t.Fatalf("undo: %v %v", err, f.calls)
			}
			if after, _ := e.read(t, c.h); !bytes.Equal(after, before) {
				t.Fatalf("undo not byte-identical:\n%s", after)
			}
		})
	}
}

// An uninstall that fails on --undo is reported (non-nil error, warning),
// after the configuration has still been restored.
func TestUndoReportsUninstallError(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex), 0o600)
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	e.opts.HostRun = func(ctx context.Context, name string, args ...string) (string, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		return "plugin is locked", errors.New("exit status 1")
	}
	err := Undo(e.opts, harnessCodex)
	if err == nil || !strings.Contains(err.Error(), "may still be installed") || !strings.Contains(e.err.String(), "codex plugin remove kenaz@kenaz") {
		t.Fatalf("got %v, stderr %q", err, e.err)
	}
	if got, _ := e.read(t, harnessCodex); strings.Contains(string(got), "[otel]") {
		t.Fatalf("configuration not restored:\n%s", got)
	}
}
