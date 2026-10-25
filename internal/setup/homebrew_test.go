package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
)

// cask simulates a Homebrew cask install of kenaz under a fake prefix.
// Nothing outside the test's temp directories is touched.
type cask struct {
	t      *testing.T
	prefix string
}

func newCask(t *testing.T) *cask {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew casks do not exist on Windows (see TestHomebrewNoOpOffMacOS)")
	}
	prev := bundle.HomebrewCasks
	bundle.HomebrewCasks = true
	t.Cleanup(func() { bundle.HomebrewCasks = prev })
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	return &cask{t: t, prefix: filepath.Join(tmp, "opt", "homebrew")}
}

func (c *cask) versionDir(v string) string {
	return filepath.Join(c.prefix, "Caskroom", "kenaz", v)
}

func (c *cask) link() string { return filepath.Join(c.prefix, "bin", "kenaz") }

// install unpacks the v archive into <prefix>/Caskroom/kenaz/<v>/ and
// links <prefix>/bin/kenaz to its bin/kenaz, the way `brew install` and
// `brew upgrade` do. It returns the bundle root.
func (c *cask) install(v string) string {
	c.t.Helper()
	staged := makeBundle(c.t, c.t.TempDir(), v, false)
	root := filepath.Join(c.versionDir(v), filepath.Base(staged))
	os.MkdirAll(filepath.Dir(root), 0o755)
	if err := os.Rename(staged, root); err != nil {
		c.t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(c.link()), 0o755)
	os.Remove(c.link())
	if err := os.Symlink(filepath.Join(root, "bin", "kenaz"), c.link()); err != nil {
		c.t.Skip("symlinks unavailable:", err)
	}
	return root
}

// upgrade is `brew upgrade kenaz`: the new version is installed and the
// old version directory deleted.
func (c *cask) upgrade(from, to string) string {
	root := c.install(to)
	if err := os.RemoveAll(c.versionDir(from)); err != nil {
		c.t.Fatal(err)
	}
	return root
}

// optsFor fills what defaultOptions derives from the running executable:
// the resolved path mapped by StablePath, and the bundle detected from it.
func optsFor(e *env, exe string) {
	r, err := filepath.EvalSymlinks(exe)
	if err != nil {
		panic(err)
	}
	e.opts.KenazBin = bundle.StablePath(r)
	e.opts.BundleRoot = DetectBundle(e.opts.KenazBin)
}

// launchCommands returns the executables the plugin a host has registered
// launches (MCP server and every hook), read from the registered
// marketplace directory.
func launchCommands(t *testing.T, e *env, h harness) []string {
	t.Helper()
	src, enabled := e.opts.registeredMarketplace(h)
	if src == "" || src == "?" || !enabled {
		t.Fatalf("%s: no local registration (%q, %v)", h.label(), src, enabled)
	}
	var out []string
	mcp := ".mcp.json"
	if h == harnessCodex {
		mcp = ".codex-mcp.json"
	}
	var m struct {
		MCPServers map[string]struct{ Command string } `json:"mcpServers"`
	}
	raw, err := os.ReadFile(filepath.Join(src, "plugin", mcp))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		t.Fatalf("%s: %v", mcp, err)
	}
	out = append(out, m.MCPServers["kenaz"].Command)
	raw, _ = os.ReadFile(filepath.Join(src, "plugin", "hooks", "hooks.json"))
	var hk struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	json.Unmarshal(raw, &hk)
	for _, gs := range hk.Hooks {
		for _, g := range gs {
			for _, x := range g.Hooks {
				q := strings.TrimPrefix(x.Command, `"`)
				out = append(out, q[:strings.Index(q, `"`)])
			}
		}
	}
	return out
}

// assertResolves fails unless every launch command of h's registration
// resolves to the bundle executable of version v.
func assertResolves(t *testing.T, e *env, c *cask, h harness, v string) {
	t.Helper()
	for _, cmd := range launchCommands(t, e, h) {
		if cmd != c.link() {
			t.Fatalf("%s launches %s, want %s", h.label(), cmd, c.link())
		}
		r, err := filepath.EvalSymlinks(cmd)
		if err != nil {
			t.Fatalf("%s: %s does not resolve: %v", h.label(), cmd, err)
		}
		if !strings.HasPrefix(r, c.versionDir(v)+string(filepath.Separator)) {
			t.Fatalf("%s: %s resolves to %s, want version %s", h.label(), cmd, r, v)
		}
	}
}

// brew install 0.5.0, kenaz init, brew upgrade to 0.5.1 (0.5.0 deleted):
// Claude Code and Codex still launch kenaz (through <prefix>/bin/kenaz,
// now 0.5.1). Doctor reports the stale plugin files, the next kenaz init
// refreshes them, and --undo removes exactly what setup added.
func TestHomebrewUpgradeKeepsRegistration(t *testing.T) {
	c := newCask(t)
	e, f, _ := bundleEnv(t, false)
	before := map[harness][]byte{}
	for _, h := range []harness{harnessClaude, harnessCodex} {
		fixture := map[harness]string{harnessClaude: "claude/permissions.json", harnessCodex: "codex/comments.toml"}[h]
		before[h] = e.seed(t, h, fixture)
	}

	root050 := c.install("0.5.0")
	optsFor(e, c.link())
	if e.opts.KenazBin != c.link() || e.opts.BundleRoot != root050 {
		t.Fatalf("detected %s / %s", e.opts.KenazBin, e.opts.BundleRoot)
	}
	for _, h := range []harness{harnessClaude, harnessCodex} {
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
		assertResolves(t, e, c, h, "0.5.0")
	}
	if strings.Contains(e.err.String(), "brew upgrade") {
		t.Fatalf("unexpected warning: %s", e.err)
	}
	if !strings.Contains(e.out.String(), "(launching "+c.link()+")") {
		t.Fatalf("output does not name the stable launcher:\n%s", e.out)
	}

	c.upgrade("0.5.0", "0.5.1")
	if _, err := os.Stat(c.versionDir("0.5.0")); !os.IsNotExist(err) {
		t.Fatal("old version directory still there")
	}
	optsFor(e, c.link())
	for _, h := range []harness{harnessClaude, harnessCodex} {
		assertResolves(t, e, c, h, "0.5.1")
	}
	if b := BrokenRegistrations(e.opts.StateDir); len(b) != 0 {
		t.Fatalf("startup check after upgrade: %v", b)
	}
	cs := doctorBundle(e.opts)
	if len(cs) != 2 {
		t.Fatalf("doctor: %+v", cs)
	}
	for _, ch := range cs {
		if ch.Status != "warn" || !strings.Contains(ch.Detail, "from kenaz 0.5.0 and this kenaz is 0.5.1") {
			t.Fatalf("doctor after upgrade: %+v", ch)
		}
	}

	// kenaz init after the upgrade refreshes the plugin files.
	calls := len(f.calls)
	for _, h := range []harness{harnessClaude, harnessCodex} {
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
		assertResolves(t, e, c, h, "0.5.1")
	}
	if len(f.calls) != calls+8 {
		t.Fatalf("refresh did not re-register: %v", f.calls[calls:])
	}
	for _, ch := range doctorBundle(e.opts) {
		if ch.Status != "ok" || !strings.Contains(ch.Detail, "0.5.1") {
			t.Fatalf("doctor after refresh: %+v", ch)
		}
	}

	// --undo removes exactly what setup added, with the version it was
	// registered from long gone.
	c.upgrade("0.5.1", "0.5.2")
	optsFor(e, c.link())
	for _, h := range []harness{harnessClaude, harnessCodex} {
		if err := Undo(e.opts, h); err != nil {
			t.Fatal(err)
		}
		after, _ := e.read(t, h)
		if !bytes.Equal(after, before[h]) {
			t.Fatalf("%s undo:\n%s", h.label(), unifiedDiff("before", "after", string(before[h]), string(after)))
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(e.opts.StateDir, "setup", "bundle")); len(ents) != 0 {
		t.Fatalf("rendered plugins left behind: %v", ents)
	}
}

// A 0.5.0 setup (before this fix) registered the plugin launching the
// versioned Caskroom executable and recorded no launcher. After brew
// upgrade deleted that version, the startup check and doctor name the
// problem, and the next kenaz init (setup) re-registers through
// <prefix>/bin/kenaz.
func TestHomebrewLegacyRegistrationSelfHeals(t *testing.T) {
	c := newCask(t)
	e, _, _ := bundleEnv(t, false)

	// What 0.5.0 did: no Homebrew mapping, the versioned path everywhere.
	c.install("0.5.0")
	bundle.HomebrewCasks = false
	optsFor(e, c.link())
	for _, h := range []harness{harnessClaude, harnessCodex} {
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
	}
	bundle.HomebrewCasks = true
	st, _ := loadState(e.opts.StateDir)
	for _, fs := range []*FileState{st.Claude, st.Codex} {
		if !strings.Contains(fs.Bundle.Launcher, "Caskroom") {
			t.Fatalf("legacy registration not versioned: %s", fs.Bundle.Launcher)
		}
		fs.Bundle.Launcher = "" // 0.5.0 did not record it
	}
	if err := saveState(e.opts.StateDir, st); err != nil {
		t.Fatal(err)
	}
	if cs := doctorBundle(e.opts); cs[0].Status != "warn" || !strings.Contains(cs[0].Detail, "upgrade") {
		t.Fatalf("doctor before upgrade: %+v", cs)
	}

	c.upgrade("0.5.0", "0.5.1")
	broken := BrokenRegistrations(e.opts.StateDir)
	if len(broken) != 2 || !strings.Contains(broken[0], "Claude Code") || !strings.Contains(broken[1], "Codex") || !strings.Contains(broken[0], "kenaz init") {
		t.Fatalf("startup check: %v", broken)
	}
	optsFor(e, c.link())
	for _, ch := range doctorBundle(e.opts) {
		if ch.Status != "fail" || !strings.Contains(ch.Detail, "kenaz init") {
			t.Fatalf("doctor: %+v", ch)
		}
	}

	// kenaz init (setup claude, setup codex) from 0.5.1 heals both.
	for _, h := range []harness{harnessClaude, harnessCodex} {
		if err := Apply(e.opts, h); err != nil {
			t.Fatal(err)
		}
		assertResolves(t, e, c, h, "0.5.1")
	}
	if b := BrokenRegistrations(e.opts.StateDir); len(b) != 0 {
		t.Fatalf("still broken: %v", b)
	}
	for _, ch := range doctorBundle(e.opts) {
		if ch.Status != "ok" {
			t.Fatalf("doctor after heal: %+v", ch)
		}
	}
	// And the next upgrade no longer breaks anything.
	c.upgrade("0.5.1", "0.5.2")
	for _, h := range []harness{harnessClaude, harnessCodex} {
		assertResolves(t, e, c, h, "0.5.2")
	}
}

// Homebrew installed but the cask's link missing (or pointing at another
// kenaz): setup keeps the versioned path, which it can verify, and warns
// that brew upgrade will break it.
func TestHomebrewMissingLinkWarns(t *testing.T) {
	c := newCask(t)
	e, _, _ := bundleEnv(t, false)
	root := c.install("0.5.0")
	exe := filepath.Join(root, "bin", "kenaz")
	os.Remove(c.link())
	optsFor(e, exe)
	if e.opts.KenazBin != exe || e.opts.BundleRoot != root {
		t.Fatalf("%s / %s", e.opts.KenazBin, e.opts.BundleRoot)
	}
	if err := Apply(e.opts, harnessClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.err.String(), "stop working after `brew upgrade`") {
		t.Fatalf("no warning: %s", e.err)
	}
}

// Off macOS (Linux, Windows) a Caskroom-shaped path is not special: setup
// registers the executable it runs from, as before.
func TestHomebrewNoOpOffMacOS(t *testing.T) {
	prev := bundle.HomebrewCasks
	bundle.HomebrewCasks = false
	t.Cleanup(func() { bundle.HomebrewCasks = prev })
	e, _, _ := bundleEnv(t, false)
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(tmp, "Caskroom", "kenaz", "0.5.0", "kenaz_0.5.0")
	staged := makeBundle(t, t.TempDir(), testBundleVersion, false)
	os.MkdirAll(filepath.Dir(root), 0o755)
	if err := os.Rename(staged, root); err != nil {
		t.Fatal(err)
	}
	name := "kenaz"
	if runtime.GOOS == "windows" {
		name = "kenaz.exe"
	}
	exe := filepath.Join(root, "bin", name)
	e.opts.KenazBin, e.opts.BundleRoot = bundle.StablePath(exe), DetectBundle(exe)
	if e.opts.KenazBin != exe {
		t.Fatalf("KenazBin = %s", e.opts.KenazBin)
	}
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(e.opts.StateDir)
	if st.Codex.Bundle.Launcher != exe || strings.Contains(e.err.String(), "brew") {
		t.Fatalf("launcher %s, stderr %s", st.Codex.Bundle.Launcher, e.err)
	}
}
