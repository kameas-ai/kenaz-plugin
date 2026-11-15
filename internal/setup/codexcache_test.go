package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedCodexInstalled is a Codex config.toml with the GitHub marketplace
// pinned to testRelease and kenaz@kenaz installed and on.
const pinnedCodexInstalled = githubCodex + "\n[plugins.\"kenaz@kenaz\"]\nenabled = true\n"

// seedCodexCache writes the copy of the plugin Codex runs
// ($CODEX_HOME/plugins/cache/kenaz/kenaz/<version>): the clone's plugin/
// plus the skills Codex derives from the plugin's commands.
func seedCodexCache(t *testing.T, o Options, version string) string {
	t.Helper()
	dir := filepath.Join(o.codexCacheDir(), version)
	src := filepath.Join(o.codexCloneDir(), "plugin")
	n := 0
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, _ := os.ReadFile(p)
		n++
		os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o700)
		return os.WriteFile(filepath.Join(dir, rel), b, 0o600)
	})
	if err != nil || n == 0 {
		t.Fatalf("seeding the Codex cache: %d files, %v", n, err)
	}
	gen := filepath.Join(dir, filepath.FromSlash(codexGenerated), "source-command-login")
	os.MkdirAll(gen, 0o700)
	os.WriteFile(filepath.Join(gen, "SKILL.md"), []byte("# source-command-login\n"), 0o600)
	return dir
}

// codexWithCache makes the fake `codex plugin remove|add` maintain the
// plugin cache the way Codex does. refresh=false leaves the cache alone.
func codexWithCache(t *testing.T, e *env, f *fakeHosts, refresh bool) {
	t.Helper()
	e.opts.HostRun = func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := f.run(ctx, name, args...)
		if refresh && name == "codex" && len(args) == 3 && args[0] == "plugin" {
			switch args[1] {
			case "remove":
				os.RemoveAll(e.opts.codexCacheDir())
			case "add":
				seedCodexCache(t, e.opts, "0.6.1")
			}
		}
		return out, err
	}
}

func codexCheck(t *testing.T, o Options) Check {
	t.Helper()
	for _, c := range doctorBundle(o) {
		if c.Name == "Codex" {
			return c
		}
	}
	t.Fatal("no Codex check")
	return Check{}
}

// Codex runs the plugin from the copy it made at install time, not from
// the marketplace clone, and keeps that copy when the marketplace is
// removed and added again with --ref. A copy installed from a moving
// branch must not pass as the pinned release: setup reinstalls it and
// doctor reports it.
func TestCodexStaleCacheReinstalled(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	codexWithCache(t, e, f, true)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(pinnedCodexInstalled), 0o600)
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
	// Installed while the marketplace followed the default branch.
	stale := seedCodexCache(t, e.opts, "0.6.0")
	os.WriteFile(filepath.Join(stale, "hooks", "hooks.json"), []byte(`{"hooks": {"x": []}}`), 0o600)

	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "|"); got != "codex plugin remove kenaz@kenaz|codex plugin add kenaz@kenaz" {
		t.Fatalf("calls %q", got)
	}
	if !strings.Contains(e.out.String(), "reinstalls the plugin") || e.err.Len() != 0 {
		t.Fatalf("out %q err %q", e.out, e.err)
	}
	st, _ := loadState(e.opts.StateDir)
	if st.Codex.Adopted == nil || st.Codex.Adopted.InstalledPlugin {
		t.Fatalf("the user's install must stay theirs on --undo: %+v", st.Codex)
	}
	if got, _ := e.read(t, harnessCodex); !strings.Contains(string(got), `[plugins."kenaz@kenaz"]`) || !strings.Contains(string(got), "[otel]") {
		t.Fatalf("config:\n%s", got)
	}
	if c := codexCheck(t, e.opts); c.Status != "ok" {
		t.Fatalf("after reinstall: %+v", c)
	}

	// A second run finds the pinned copy (and the skills Codex generated
	// in it) and runs nothing.
	f.calls = nil
	if err := Apply(e.opts, harnessCodex); err != nil || len(f.calls) != 0 {
		t.Fatalf("second run: %v %v", err, f.calls)
	}

	// Doctor agrees with setup about each way the copy can differ.
	cur := filepath.Join(e.opts.codexCacheDir(), "0.6.1")
	hooks := filepath.Join(cur, "hooks", "hooks.json")
	orig, _ := os.ReadFile(hooks)
	for name, change := range map[string]func(){
		"changed file": func() { os.WriteFile(hooks, []byte("{}"), 0o600) },
		"extra file":   func() { os.WriteFile(filepath.Join(cur, "hooks", "extra.json"), []byte("{}"), 0o600) },
		"missing file": func() { os.Remove(hooks) },
		"older version beside it": func() {
			old := seedCodexCache(t, e.opts, "0.6.0")
			os.WriteFile(filepath.Join(old, "hooks", "hooks.json"), []byte("{}"), 0o600)
		},
	} {
		change()
		if c := codexCheck(t, e.opts); c.Status != "warn" || !strings.Contains(c.Detail, "copy of the Kenaz plugin that Codex runs") {
			t.Fatalf("%s: %+v", name, c)
		}
		os.RemoveAll(filepath.Join(e.opts.codexCacheDir(), "0.6.0"))
		os.Remove(filepath.Join(cur, "hooks", "extra.json"))
		os.WriteFile(hooks, orig, 0o600)
		if c := codexCheck(t, e.opts); c.Status != "ok" {
			t.Fatalf("%s, restored: %+v", name, c)
		}
	}
}

// A reinstall that leaves the stale copy in place is reported.
func TestCodexStaleCacheSurvivesReinstall(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	codexWithCache(t, e, f, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(pinnedCodexInstalled), 0o600)
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
	stale := seedCodexCache(t, e.opts, "0.6.0")
	os.WriteFile(filepath.Join(stale, "README.md"), []byte("moved\n"), 0o600)

	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || !strings.Contains(e.err.String(), "still is not the pinned marketplace's plugin") {
		t.Fatalf("calls %v err %q", f.calls, e.err)
	}
}

// A [plugins."kenaz@kenaz"] table with no enabled key is an enabled plugin
// (Codex's default), and setup and doctor read "turned off" the same way.
func TestCodexPluginTableWithoutEnabledKey(t *testing.T) {
	table := "\n[plugins.\"" + tID + "\"]\n"
	git := "[marketplaces." + tMk + "]\nsource_type = \"git\"\nsource = \"https://example.com/x.git\"\n"
	local := "[marketplaces." + tMk + "]\nsource_type = \"local\"\nsource = \"/x\"\n"
	for text, want := range map[string]marketplace{
		git + table:                               {Enabled: true, Installed: true},
		git + table + "# enabled = false\n":       {Enabled: true, Installed: true},
		git + table + "enabled = true\n":          {Enabled: true, Installed: true},
		git + table + "enabled = false\n":         {Installed: true, Off: true},
		git + table + "enabled=false # for now\n": {Installed: true, Off: true},
		git: {},
		git + "\n[plugins.\"x@y\"]\nenabled = false\n": {},
		table:                               {Enabled: true, Installed: true},
		table + "enabled = false\n[otel]\n": {Installed: true, Off: true},
		local + table:                       {Enabled: true, Installed: true},
	} {
		got := codexMarketplace(text)
		if got.Enabled != want.Enabled || got.Installed != want.Installed || got.Off != want.Off {
			t.Errorf("%q: enabled %v installed %v off %v", text, got.Enabled, got.Installed, got.Off)
		}
	}

	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex+table), 0o600)
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || strings.Contains(e.err.String(), "turned off") {
		t.Fatalf("calls %v err %q", f.calls, e.err)
	}
	if c := codexCheck(t, e.opts); c.Status != "ok" {
		t.Fatalf("doctor: %+v", c)
	}

	// Explicitly off: both say so, in the same words.
	cfg, _ := e.read(t, harnessCodex)
	os.WriteFile(e.opts.path(harnessCodex), bytes.Replace(cfg, []byte(table), []byte(table+"enabled = false\n"), 1), 0o600)
	e.err.Reset()
	if err := Apply(e.opts, harnessCodex); err != nil || !strings.Contains(e.err.String(), "installed in Codex but turned off") {
		t.Fatalf("setup: %v %q", err, e.err)
	}
	if c := codexCheck(t, e.opts); c.Status != "warn" || !strings.Contains(c.Detail, "installed but turned off") || !strings.Contains(c.Detail, "enabled = true") {
		t.Fatalf("doctor: %+v", c)
	}

	// Uninstalled, then the marketplace removed.
	os.WriteFile(e.opts.path(harnessCodex), []byte(githubCodex), 0o600)
	if c := codexCheck(t, e.opts); c.Status != "warn" || !strings.Contains(c.Detail, "no longer installed") {
		t.Fatalf("doctor, uninstalled: %+v", c)
	}
	os.WriteFile(e.opts.path(harnessCodex), []byte("model = \"x\"\n"), 0o600)
	if c := codexCheck(t, e.opts); c.Status != "warn" || !strings.Contains(c.Detail, "no longer registered") {
		t.Fatalf("doctor, removed: %+v", c)
	}
}

// Claude Code: a plugin that is installed but not listed in the user's
// enabledPlugins is not "off" for setup (managed settings may enable it),
// so doctor must not report it as no longer enabled.
func TestDoctorClaudeNotListedIsNotOff(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e := newEnv(t, 4318)
	f := &fakeHosts{t: t, o: &e.opts}
	e.opts.HostRun = f.run
	os.MkdirAll(filepath.Join(e.opts.ClaudeDir, "plugins"), 0o700)
	settings := strings.Replace(githubClaudeUnpinned, `"kenaz@kenaz": true`, `"other@x": true`, 1)
	os.WriteFile(e.opts.path(harnessClaude), []byte(settings), 0o600)
	os.WriteFile(filepath.Join(e.opts.ClaudeDir, "plugins", "installed_plugins.json"), []byte(`{"version": 2, "plugins": {"kenaz@kenaz": [{"scope": "user", "version": "0.6.1"}]}}`), 0o600)
	if err := Apply(e.opts, harnessClaude); err != nil || len(f.calls) != 0 {
		t.Fatalf("%v %v", err, f.calls)
	}
	if strings.Contains(e.err.String(), "turned off") {
		t.Fatalf("setup: %q", e.err)
	}
	check := func() Check {
		for _, c := range doctorBundle(e.opts) {
			if c.Name == "Claude Code" {
				return c
			}
		}
		t.Fatal("no Claude Code check")
		return Check{}
	}
	if c := check(); strings.Contains(c.Detail, "no longer") || strings.Contains(c.Detail, "turned off") {
		t.Fatalf("doctor: %+v", c)
	}
	cur, _ := e.read(t, harnessClaude)
	os.WriteFile(e.opts.path(harnessClaude), bytes.Replace(cur, []byte(`"other@x": true`), []byte(`"kenaz@kenaz": false`), 1), 0o600)
	if c := check(); c.Status != "warn" || !strings.Contains(c.Detail, "installed but turned off") {
		t.Fatalf("doctor, off: %+v", c)
	}
}

// git on Windows checks text files out with CRLF by default
// (core.autocrlf), so a pinned checkout that differs from the verified
// copy only in line endings is this release; any other difference is not.
func TestCodexPinAcceptsCRLFCheckout(t *testing.T) {
	skipWithoutPublicMarketplace(t)
	e, f, _ := bundleEnv(t, false)
	os.MkdirAll(e.opts.CodexDir, 0o700)
	os.WriteFile(e.opts.path(harnessCodex), []byte(pinnedCodexInstalled), 0o600)
	seedCodexClone(t, e.opts, testRelease.Commit, "", e.opts.BundleRoot)
	clone := e.opts.codexCloneDir()
	changed := 0
	for _, sub := range []string{"plugin", ".claude-plugin"} {
		filepath.WalkDir(filepath.Join(clone, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(p)
			if crlf := bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")); !bytes.Equal(crlf, b) {
				changed++
				return os.WriteFile(p, crlf, 0o600)
			}
			return nil
		})
	}
	if changed == 0 {
		t.Fatal("no file had a line ending to convert")
	}
	// Codex copied the CRLF checkout when it installed the plugin.
	seedCodexCache(t, e.opts, "0.6.1")
	b, _ := LoadBundle(e.opts.BundleRoot)
	mk := e.opts.kenazMarketplace(harnessCodex)
	if ok, why := e.opts.releasePin(harnessCodex, mk, b); !ok {
		t.Fatalf("CRLF checkout not accepted: %s", why)
	}
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	if st, _ := loadState(e.opts.StateDir); st.Codex.Adopted == nil || len(f.calls) != 0 || e.err.Len() != 0 {
		t.Fatalf("not adopted as is: calls %v err %q", f.calls, e.err)
	}

	// A real difference still fails, CRLF or not.
	os.WriteFile(filepath.Join(clone, "plugin", "hooks", "hooks.json"), []byte("{\"hooks\": {\"x\": []}}\r\n"), 0o600)
	if ok, why := e.opts.releasePin(harnessCodex, mk, b); ok || !strings.Contains(why, "differ from this release's verified copy") {
		t.Fatalf("modified CRLF checkout accepted: %v %q", ok, why)
	}
}
