package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

// testRelease is the release the test kenaz was built from.
var testRelease = Release{Version: "0.6.1", Commit: "0123456789abcdef0123456789abcdef01234567"}

// githubClaudeUnpinned is what `claude plugin marketplace add
// kameas-ai/kenaz-plugin` and `claude plugin install kenaz@kenaz` leave in
// settings.json: it follows the repository's default branch.
const githubClaudeUnpinned = `{
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

// githubClaude is the GitHub plugin pinned to testRelease's commit, in the
// inline-catalog form print-managed deploys.
const githubClaude = `{
  "extraKnownMarketplaces": {
    "kenaz": {
      "source": {
        "source": "settings",
        "name": "kenaz",
        "plugins": [
          {
            "name": "kenaz",
            "source": {
              "source": "git-subdir",
              "url": "https://github.com/kameas-ai/kenaz-plugin.git",
              "path": "plugin",
              "ref": "v0.6.1",
              "sha": "0123456789abcdef0123456789abcdef01234567"
            }
          }
        ]
      }
    }
  },
  "enabledPlugins": {
    "kenaz@kenaz": true
  }
}
`

// githubCodexUnpinned is what `codex plugin marketplace add
// kameas-ai/kenaz-plugin` (Codex 0.164) leaves in config.toml, before
// `codex plugin add`.
const githubCodexUnpinned = "[marketplaces.kenaz]\nsource_type = \"git\"\nsource = \"https://github.com/kameas-ai/kenaz-plugin.git\"\n"

// githubCodex is the same with `--ref <testRelease commit>`.
const githubCodex = githubCodexUnpinned + "ref = \"0123456789abcdef0123456789abcdef01234567\"\n"

// seedCodexClone writes Codex's marketplace clone: .git/HEAD (a commit, or
// "ref: refs/heads/<name>" with that branch at branchAt) and, from a
// bundle root, its plugin tree.
func seedCodexClone(t *testing.T, o Options, head, branchAt, bundleRoot string) {
	t.Helper()
	dir := o.codexCloneDir()
	gd := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(gd, "refs", "heads"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(gd, "HEAD"), []byte(head+"\n"), 0o600)
	if name, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
		os.WriteFile(filepath.Join(gd, "packed-refs"), []byte("# pack-refs with: peeled fully-peeled sorted\n"+branchAt+" refs/heads/"+name+"\n"), 0o600)
	}
	if bundleRoot == "" {
		return
	}
	for _, sub := range []string{"plugin", ".claude-plugin"} {
		filepath.WalkDir(filepath.Join(bundleRoot, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(bundleRoot, p)
			b, _ := os.ReadFile(p)
			os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o700)
			return os.WriteFile(filepath.Join(dir, rel), b, 0o600)
		})
	}
}

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
	if m := codexMarketplace(githubCodex); !m.official() || m.Enabled || !m.pinnedTo(testRelease.Commit) {
		t.Fatalf("codex github marketplace: %+v", m)
	}
}

// Only a full commit equal to the release's pins a marketplace: no ref, a
// branch, a tag or another commit is moving.
func TestMarketplacePin(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	c := testRelease.Commit
	for name, tc := range map[string]struct {
		m    marketplace
		want bool
	}{
		"codex pinned":          {codexMarketplace(githubCodex), true},
		"codex unpinned":        {codexMarketplace(githubCodexUnpinned), false},
		"codex branch":          {codexMarketplace(githubCodexUnpinned + "ref = \"main\"\n"), false},
		"codex tag":             {codexMarketplace(githubCodexUnpinned + "ref = \"v0.6.1\"\n"), false},
		"codex other commit":    {codexMarketplace(githubCodexUnpinned + "ref = \"" + strings.Repeat("a", 40) + "\"\n"), false},
		"codex upper commit":    {codexMarketplace(githubCodexUnpinned + "ref = \"" + strings.ToUpper(c) + "\"\n"), true},
		"claude settings":       {claudeSourceOf(t, githubClaude), true},
		"claude github":         {claudeSourceOf(t, githubClaudeUnpinned), false},
		"claude github tag":     {claudeSource{Source: "github", Repo: OfficialRepo, Ref: "v0.6.1"}.marketplace(), false},
		"claude github #sha":    {claudeSource{Source: "github", Repo: OfficialRepo + "#" + c}.marketplace(), false},
		"claude github ref=sha": {claudeSource{Source: "github", Repo: OfficialRepo, Ref: c}.marketplace(), false},
		"claude settings tag":   {claudeSourceOf(t, strings.Replace(githubClaude, `"sha": "`+c+`"`, `"sha": ""`, 1)), false},
	} {
		if !tc.m.official() {
			t.Errorf("%s: not official: %+v", name, tc.m)
		}
		if got := tc.m.pinnedTo(c); got != tc.want {
			t.Errorf("%s: pinnedTo = %v, want %v (%+v)", name, got, tc.want, tc.m)
		}
		if tc.m.pinnedTo("") {
			t.Errorf("%s: pinned for a build with no release commit", name)
		}
	}
	// Another repository's inline catalog is not the GitHub plugin.
	other := strings.Replace(githubClaude, "kameas-ai/kenaz-plugin", "someone/kenaz-plugin", 1)
	if m := claudeSourceOf(t, other); m.official() {
		t.Fatalf("other repo official: %+v", m)
	}
}

func claudeSourceOf(t *testing.T, settings string) marketplace {
	t.Helper()
	var s struct {
		Extra map[string]struct {
			Source claudeSource `json:"source"`
		} `json:"extraKnownMarketplaces"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatal(err)
	}
	return s.Extra["kenaz"].Source.marketplace()
}

// A downloaded kenaz replaces a GitHub marketplace that follows a moving
// ref with its verified copy, with a warning, as --bundle would.
func TestUnpinnedGitHubMarketplaceReplaced(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	for _, tc := range []struct {
		name    string
		h       harness
		content string
		calls   []string
		says    string
	}{
		{"claude default branch", harnessClaude, githubClaudeUnpinned, []string{"claude plugin uninstall kenaz@kenaz", "claude plugin marketplace remove kenaz", "claude plugin marketplace add ", "claude plugin install kenaz@kenaz"}, "default branch"},
		{"codex branch", harnessCodex, githubCodexUnpinned + "ref = \"main\"\n", []string{"codex plugin remove kenaz@kenaz", "codex plugin marketplace remove kenaz", "codex plugin marketplace add ", "codex plugin add kenaz@kenaz"}, "ref main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := bundleEnv(t, false)
			os.MkdirAll(filepath.Dir(e.opts.path(tc.h)), 0o700)
			os.WriteFile(e.opts.path(tc.h), []byte(tc.content), 0o600)
			if err := Apply(e.opts, tc.h); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != len(tc.calls) {
				t.Fatalf("calls %v", f.calls)
			}
			for i, w := range tc.calls {
				if !strings.HasPrefix(f.calls[i], w) {
					t.Fatalf("call %d = %q, want %q...", i, f.calls[i], w)
				}
			}
			if !strings.Contains(e.err.String(), tc.says) || !strings.Contains(e.err.String(), "not this release's signed commit") {
				t.Fatalf("warning: %q", e.err)
			}
			if tc.h == harnessCodex && !strings.Contains(e.err.String(), "--ref "+testRelease.Commit) {
				t.Fatalf("no pinned command: %q", e.err)
			}
			st, _ := loadState(e.opts.StateDir)
			if fs := st.get(tc.h); fs.Bundle == nil || fs.Adopted != nil {
				t.Fatalf("state %+v", fs)
			}
		})
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
	if !strings.Contains(e.out.String(), "pinned to this release's commit") || e.err.Len() != 0 {
		t.Fatalf("out %q err %q", e.out, e.err)
	}
	got, _ := e.read(t, harnessClaude)
	if !bytes.Contains(got, []byte(`"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"`)) || !bytes.Contains(got, []byte(`"sha": "`+testRelease.Commit+`"`)) {
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
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)

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
	if e.err.Len() != 0 {
		t.Fatalf("pinned marketplace warned: %q", e.err)
	}

	// Unpinned and no bundle to fall back on: still adopted, with a warning.
	Undo(e.opts, harnessClaude)
	os.WriteFile(e.opts.path(harnessClaude), []byte(githubClaudeUnpinned), 0o600)
	if err := Apply(e.opts, harnessClaude); err != nil || len(f.calls) != 0 {
		t.Fatalf("%v %v", err, f.calls)
	}
	if !strings.Contains(e.err.String(), "can change without a release") {
		t.Fatalf("no warning: %q", e.err)
	}
}

// An explicit --bundle replaces the GitHub marketplace with the bundle's
// copy, and --undo says how to add the GitHub one back.
func TestExplicitBundleReplacesGitHubMarketplace(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	e.opts.BundleExplicit = true
	os.MkdirAll(e.opts.ClaudeDir, 0o700)
	os.WriteFile(e.opts.path(harnessClaude), []byte(githubClaudeUnpinned), 0o600)
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
			os.WriteFile(e.opts.path(harnessClaude), []byte(`{"extraKnownMarketplaces": {"kenaz": {"source": {"source": "settings", "name": "kenaz", "plugins": [{"name": "kenaz", "source": {"source": "git-subdir", "url": "kameas-ai/kenaz-plugin", "path": "plugin", "ref": "v0.6.1", "sha": "`+testRelease.Commit+`"}}]}}}}`), 0o600)
			os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "installed_plugins.json"), []byte(`{"version": 2, "plugins": {"kenaz@kenaz": [{"scope": "user", "version": "0.4.1"}]}}`), 0o600)
		}},
		{"codex disabled", harnessCodex, func(e *env) {
			os.MkdirAll(e.opts.CodexDir, 0o700)
			os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex+"\n[plugins.\"kenaz@kenaz\"]\nenabled = false\n"), 0o600)
			seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
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
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
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

// Codex's ref = "<commit>" is a claim: git checkout prefers a branch named
// like the commit, so setup verifies what Codex checked out before
// adopting, and doctor re-checks it.
func TestCodexPinVerifiesCheckout(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	other := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name   string
		seed   func(e *env)
		adopt  bool
		reason string
	}{
		{"detached at the release", func(e *env) { seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot) }, true, ""},
		{"branch at the release", func(e *env) {
			seedCodexClone(t, e.opts, "ref: refs/heads/main", testRelease.Commit, e.opts.BundleRoot)
		}, true, ""},
		{"branch named like the commit", func(e *env) {
			seedCodexClone(t, e.opts, "ref: refs/heads/"+testRelease.Commit, other, e.opts.BundleRoot)
		}, false, "Codex checked out " + other},
		{"no clone", func(e *env) {}, false, "cannot be verified"},
		{"plugin tree differs", func(e *env) {
			seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
			os.WriteFile(filepath.Join(e.opts.codexCloneDir(), "plugin", "hooks", "hooks.json"), []byte(`{"hooks": {"x": []}}`), 0o600)
		}, false, "differ from this release's verified copy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := bundleEnv(t, false)
			os.MkdirAll(e.opts.CodexDir, 0o700)
			os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex+"\n[plugins.\"kenaz@kenaz\"]\nenabled = true\n"), 0o600)
			tc.seed(e)
			if err := Apply(e.opts, harnessCodex); err != nil {
				t.Fatal(err)
			}
			st, _ := loadState(e.opts.StateDir)
			if got := st.Codex.Adopted != nil; got != tc.adopt {
				t.Fatalf("adopted %v, want %v; calls %v err %q", got, tc.adopt, f.calls, e.err)
			}
			if !tc.adopt && (st.Codex.Bundle == nil || !strings.Contains(e.err.String(), tc.reason)) {
				t.Fatalf("not replaced with %q: %q", tc.reason, e.err)
			}
		})
	}

	// Doctor: adopted while pinned, then the checkout moves.
	e, _, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex+"\n[plugins.\"kenaz@kenaz\"]\nenabled = true\n"), 0o600)
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	status := func() Check {
		for _, c := range doctorBundle(e.opts) {
			if c.Name == "Codex" {
				return c
			}
		}
		t.Fatal("no Codex check")
		return Check{}
	}
	if c := status(); c.Status != "ok" {
		t.Fatalf("pinned: %+v", c)
	}
	seedCodexClone(t, e.opts, "ref: refs/heads/"+testRelease.Commit, other, "")
	if c := status(); c.Status != "warn" || !strings.Contains(c.Detail, "Codex checked out") {
		t.Fatalf("moved checkout: %+v", c)
	}
}

// knownSettingsMarketplace is what ~/.claude/plugins/known_marketplaces.json
// holds for a marketplace declared inline (a "settings" source) in managed
// settings: source, installLocation, lastUpdated and autoUpdate
// (code.claude.com plugins/loading). Whether the inline plugins are kept in
// source is undocumented, so the strict case keeps only name.
const knownSettingsMarketplace = `{
  "kenaz": {
    "source": {
      "source": "settings",
      "name": "kenaz"
    },
    "installLocation": "/home/user/.claude/plugins/marketplaces/kenaz",
    "lastUpdated": "2026-10-20T10:00:00.000Z",
    "autoUpdate": false
  }
}
`

// A managed (inline) kenaz marketplace is never replaced or duplicated:
// Claude Code allows one marketplace per name.
func TestInlineMarketplaceNeverReplaced(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	for _, explicit := range []bool{false, true} {
		e, f, _ := bundleEnv(t, false)
		e.opts.BundleExplicit = explicit
		os.MkdirAll(filepath.Join(e.opts.ClaudeDir, "plugins"), 0o700)
		os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "known_marketplaces.json"), []byte(knownSettingsMarketplace), 0o600)
		if m := e.opts.kenazMarketplace(harnessClaude); !m.Inline || m.official() {
			t.Fatalf("read %+v", m)
		}
		if err := Apply(e.opts, harnessClaude); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("explicit=%v: host commands run: %v", explicit, f.calls)
		}
		if !strings.Contains(e.err.String(), "declares the Kenaz plugin marketplace inline") || !strings.Contains(e.err.String(), "print-managed claude") {
			t.Fatalf("warning: %q", e.err)
		}
		st, _ := loadState(e.opts.StateDir)
		if st.Claude.Bundle != nil || st.Claude.Adopted == nil || st.Claude.Adopted.InstalledPlugin {
			t.Fatalf("state %+v", st.Claude)
		}
		if got, _ := e.read(t, harnessClaude); !strings.Contains(string(got), "OTEL_EXPORTER_OTLP_ENDPOINT") {
			t.Fatalf("telemetry not configured:\n%s", got)
		}
		if err := Undo(e.opts, harnessClaude); err != nil || len(f.calls) != 0 {
			t.Fatalf("undo: %v %v", err, f.calls)
		}
	}

	// The same record with its inline plugins, pinned to this release: used.
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(filepath.Join(e.opts.ClaudeDir, "plugins"), 0o700)
	known := strings.Replace(knownSettingsMarketplace, `"name": "kenaz"
    },`, `"name": "kenaz",
      "plugins": [{"name": "kenaz", "source": {"source": "git-subdir", "url": "https://github.com/kameas-ai/kenaz-plugin.git", "path": "plugin", "ref": "v0.6.1", "sha": "`+testRelease.Commit+`"}}]
    },`, 1)
	os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "known_marketplaces.json"), []byte(known), 0o600)
	os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "installed_plugins.json"), []byte(`{"version": 2, "plugins": {"kenaz@kenaz": [{"scope": "managed", "version": "0.6.1"}]}}`), 0o600)
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || e.err.Len() != 0 || !strings.Contains(e.out.String(), "pinned to this release's commit") {
		t.Fatalf("calls %v out %q err %q", f.calls, e.out, e.err)
	}
	if c := doctorBundle(e.opts); len(c) != 1 || c[0].Status != "ok" {
		t.Fatalf("doctor %+v", c)
	}
}

// Without a verified copy (Homebrew, npm, --no-bundle) an unpinned GitHub
// marketplace is still used, and the warning names a pinned form.
func TestNoBundleWarningIsActionable(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	for _, tc := range []struct {
		h       harness
		content string
		want    string
	}{
		{harnessClaude, githubClaudeUnpinned, "print-managed claude --raw"},
		{harnessCodex, githubCodexUnpinned, "--ref " + testRelease.Commit},
	} {
		e := newEnv(t, 4318)
		f := &fakeHosts{t: t, o: &e.opts}
		e.opts.HostRun = f.run
		os.MkdirAll(filepath.Dir(e.opts.path(tc.h)), 0o700)
		os.WriteFile(e.opts.path(tc.h), []byte(tc.content), 0o600)
		if err := Apply(e.opts, tc.h); err != nil {
			t.Fatal(err)
		}
		if st, _ := loadState(e.opts.StateDir); st.get(tc.h).Adopted == nil {
			t.Fatal("not adopted")
		}
		if !strings.Contains(e.err.String(), tc.want) || !strings.Contains(e.err.String(), "release archive") {
			t.Fatalf("%v warning: %q", tc.h, e.err)
		}
	}
}
