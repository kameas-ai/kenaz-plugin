package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/internal/variant/varianttest"
)

const testBundleVersion = "0.6.2"

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if d.Name() == "test" && rel == "test" { // adapter tests are not shipped
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// makeBundle lays out a release archive like the release packaging does:
// bin/, plugin/, .claude-plugin/, optionally opencode/, and bundle.json
// with every file's digest. The directory name contains a space.
func makeBundle(t *testing.T, parent, version string, opencode bool) string {
	t.Helper()
	root := filepath.Join(parent, "Kenaz Bundle", variant.Current.BundleName+"_"+version+"_"+runtime.GOOS+"_"+runtime.GOARCH)
	exe := tBin
	if runtime.GOOS == "windows" {
		exe = tBin + ".exe"
	}
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", exe), []byte("kenaz binary "+version), 0o755)
	if err := varianttest.WritePluginTree(filepath.Join("..", ".."), root, variant.Current); err != nil {
		t.Fatal(err)
	}
	if opencode {
		copyTree(t, filepath.Join("..", "..", "opencode"), filepath.Join(root, "opencode"))
	}
	if version != testBundleVersion {
		for _, rel := range []string{"plugin/.claude-plugin/plugin.json", "plugin/.codex-plugin/plugin.json", ".claude-plugin/marketplace.json", "opencode/package.json"} {
			p := filepath.Join(root, filepath.FromSlash(rel))
			if b, err := os.ReadFile(p); err == nil {
				os.WriteFile(p, bytes.ReplaceAll(b, []byte(`"`+testBundleVersion+`"`), []byte(`"`+version+`"`)), 0o644)
			}
		}
	}
	writeManifest(t, root, version, "bin/"+exe, opencode)
	return root
}

func writeManifest(t *testing.T, root, version, exe string, opencode bool) {
	t.Helper()
	files := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) == BundleFile {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)], _ = bundle.FileSHA(p)
		return nil
	})
	m := map[string]any{
		"schema": 1, "name": variant.Current.BundleName, "version": version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"executable": exe, "executable_sha256": files[exe], "opencode": opencode, "files": files,
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(root, BundleFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeHosts emulates `claude` and `codex` plugin registration on the
// host config files, the way the real CLIs edit them.
type fakeHosts struct {
	t     *testing.T
	o     *Options
	calls []string
}

func (f *fakeHosts) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	cmd := strings.Join(args[:len(args)-1], " ")
	last := args[len(args)-1]
	switch name {
	case "claude":
		p := f.o.path(harnessClaude)
		b, _, _ := readFile(p)
		var doc map[string]any
		if len(b) > 0 {
			json.Unmarshal(b, &doc)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		ensure := func(k string) map[string]any {
			m, _ := doc[k].(map[string]any)
			if m == nil {
				m = map[string]any{}
				doc[k] = m
			}
			return m
		}
		switch cmd {
		case "plugin marketplace add":
			ensure("extraKnownMarketplaces")[tMk] = map[string]any{"source": map[string]any{"source": "directory", "path": last}}
		case "plugin install":
			ensure("enabledPlugins")[last] = true
		case "plugin uninstall":
			if _, ok := ensure("enabledPlugins")[last]; !ok {
				return "not installed", errors.New("exit 1")
			}
			delete(ensure("enabledPlugins"), last)
		case "plugin marketplace remove":
			delete(ensure("extraKnownMarketplaces"), last)
		default:
			f.t.Fatalf("unexpected claude %v", args)
		}
		out, _ := json.MarshalIndent(doc, "", "  ")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, append(out, '\n'), 0o600)
	case "codex":
		p := f.o.path(harnessCodex)
		b, _, _ := readFile(p)
		s := string(b)
		mk := "\n[marketplaces." + tMk + "]\nsource_type = \"local\"\nsource = " + mustJSON(last) + "\n"
		pl := "\n[plugins.\"" + tID + "\"]\nenabled = true\n"
		switch cmd {
		case "plugin marketplace add":
			s += mk
		case "plugin add":
			s += pl
		case "plugin remove":
			s = strings.Replace(s, pl, "", 1)
		case "plugin marketplace remove":
			if i := strings.Index(s, "\n[marketplaces."+tMk+"]\n"); i >= 0 {
				j := strings.Index(s[i+1:], "\n[")
				if j < 0 {
					s = s[:i]
				} else {
					s = s[:i] + s[i+1+j:]
				}
			}
		default:
			f.t.Fatalf("unexpected codex %v", args)
		}
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(s), 0o600)
	}
	return "", nil
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func bundleEnv(t *testing.T, opencode bool) (*env, *fakeHosts, string) {
	t.Helper()
	e, root := openCodeEnv(t)
	b := makeBundle(t, root, testBundleVersion, opencode)
	e.opts.BundleRoot = b
	f := &fakeHosts{t: t, o: &e.opts}
	e.opts.HostRun = f.run
	return e, f, b
}

func TestLoadBundle(t *testing.T) {
	root := t.TempDir()
	b := makeBundle(t, root, testBundleVersion, true)
	got, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != testBundleVersion || !got.OpenCode || !filepath.IsAbs(got.Exe) || DetectBundle(got.Exe) != b {
		t.Fatalf("%+v", got)
	}

	// Any modified, missing or version-skewed file fails verification.
	os.WriteFile(filepath.Join(b, "plugin", "hooks", "hooks.json"), []byte("{}"), 0o644)
	if _, err := LoadBundle(b); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered: %v", err)
	}
	b2 := makeBundle(t, t.TempDir(), testBundleVersion, true)
	pj := filepath.Join(b2, "opencode", "package.json")
	data, _ := os.ReadFile(pj)
	os.WriteFile(pj, bytes.Replace(data, []byte(`"`+testBundleVersion+`"`), []byte(`"9.9.9"`), 1), 0o644)
	writeManifest(t, b2, testBundleVersion, "bin/"+filepath.Base(got.Exe), true)
	if _, err := LoadBundle(b2); err == nil || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("adapter version skew: %v", err)
	}
	if _, err := LoadBundle(t.TempDir()); err == nil {
		t.Fatal("non-bundle accepted")
	}
	if DetectBundle(filepath.Join(t.TempDir(), "bin", tBin)) != "" {
		t.Fatal("detected a bundle where there is none")
	}
}

func TestRenderPluginAbsoluteLaunch(t *testing.T) {
	b, err := LoadBundle(makeBundle(t, t.TempDir(), testBundleVersion, false))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "rendered")
	d1, err := renderPlugin(b, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"plugin/.mcp.json", "plugin/.codex-mcp.json"} {
		var m struct {
			McpServers map[string]map[string]any `json:"mcpServers"`
		}
		raw, _ := os.ReadFile(filepath.Join(dir, rel))
		json.Unmarshal(raw, &m)
		if m.McpServers[variant.Current.MCPServer]["command"] != b.Exe {
			t.Errorf("%s: command %v", rel, m.McpServers[variant.Current.MCPServer]["command"])
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "plugin", ".codex-mcp.json"))
	if !bytes.Contains(raw, []byte(`"approval_mode": "prompt"`)) {
		t.Error("codex overlay tool settings lost")
	}
	hooks, _ := os.ReadFile(filepath.Join(dir, "plugin", "hooks", "hooks.json"))
	want, _ := json.Marshal(`"` + b.Exe + `" hook session-start`)
	if !bytes.Contains(hooks, want) {
		t.Errorf("hooks not absolute:\n%s", hooks)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err != nil {
		t.Error(err)
	}
	// The bundle itself is never modified.
	if _, err := LoadBundle(b.Root); err != nil {
		t.Fatalf("bundle changed by rendering: %v", err)
	}
	if d2, _ := renderPlugin(b, dir, true); d2 != d1 {
		t.Fatal("render not deterministic")
	}
}

func TestBundleClaudeCodexApplyIdempotentUndo(t *testing.T) {
	for _, c := range []struct {
		h       harness
		fixture string
	}{{harnessClaude, "claude/permissions.json"}, {harnessCodex, "codex/comments.toml"}, {harnessClaude, ""}} {
		t.Run(c.h.label()+c.fixture, func(t *testing.T) {
			e, f, broot := bundleEnv(t, false)
			in := e.seed(t, c.h, c.fixture)
			if err := Apply(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			b, _ := LoadBundle(broot)
			src, enabled := e.opts.registeredMarketplace(c.h)
			if src != e.opts.renderDir(b) || !enabled || len(f.calls) != 2 {
				t.Fatalf("registration: %q %v %v", src, enabled, f.calls)
			}
			got, _ := e.read(t, c.h)
			if c.h == harnessClaude && !bytes.Contains(got, []byte("OTEL_EXPORTER_OTLP_ENDPOINT")) {
				t.Fatalf("telemetry keys missing:\n%s", got)
			}
			if c.h == harnessCodex && !bytes.Contains(got, []byte("[otel]")) {
				t.Fatalf("telemetry keys missing:\n%s", got)
			}
			if c.fixture != "" && c.h == harnessCodex && !bytes.HasPrefix(got, in[:bytes.Index(in, []byte("\n"))]) {
				t.Fatal("unrelated config changed")
			}

			// Second run: nothing to do, no host commands.
			e.out.Reset()
			if err := Apply(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 2 || !strings.Contains(e.out.String(), "already configured") {
				t.Fatalf("not idempotent: %v %s", f.calls, e.out)
			}

			if err := Undo(e.opts, c.h); err != nil {
				t.Fatal(err)
			}
			after, exists := e.read(t, c.h)
			if c.fixture == "" {
				if exists {
					t.Fatalf("undo left a file:\n%s", after)
				}
			} else if !bytes.Equal(after, in) {
				t.Fatalf("undo not byte-identical:\n%s", unifiedDiff("in", "after", string(in), string(after)))
			}
			if _, err := os.Stat(e.opts.renderDir(b)); !os.IsNotExist(err) {
				t.Fatal("rendered plugin left behind")
			}
		})
	}
}

func TestBundleRefusesForeignMarketplace(t *testing.T) {
	e, f, _ := bundleEnv(t, false)
	in := []byte(`{"extraKnownMarketplaces": {"` + tMk + `": {"source": {"source": "github", "repo": "example/other-kenaz"}}}, "enabledPlugins": {"` + tID + `": true}}`)
	os.MkdirAll(e.opts.ClaudeDir, 0o700)
	os.WriteFile(e.opts.path(harnessClaude), in, 0o600)
	err := Apply(e.opts, harnessClaude)
	if err == nil || !strings.Contains(err.Error(), "did not register") || len(f.calls) != 0 {
		t.Fatalf("got %v, calls %v", err, f.calls)
	}
	got, _ := e.read(t, harnessClaude)
	if !bytes.Equal(got, in) {
		t.Fatal("file modified")
	}
}

func TestBundleUpgradeReregisters(t *testing.T) {
	e, f, _ := bundleEnv(t, false)
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	old, _ := LoadBundle(e.opts.BundleRoot)
	e.opts.BundleRoot = makeBundle(t, t.TempDir(), "0.3.2", false)
	if err := Apply(e.opts, harnessCodex); err != nil {
		t.Fatal(err)
	}
	nb, _ := LoadBundle(e.opts.BundleRoot)
	src, enabled := e.opts.registeredMarketplace(harnessCodex)
	if src != e.opts.renderDir(nb) || !enabled {
		t.Fatalf("not re-registered: %q %v", src, enabled)
	}
	if len(f.calls) != 6 {
		t.Fatalf("calls: %v", f.calls)
	}
	if _, err := os.Stat(e.opts.renderDir(old)); !os.IsNotExist(err) {
		t.Fatal("old rendered plugin kept")
	}
	b, _ := e.read(t, harnessCodex)
	if strings.Count(string(b), "[marketplaces."+tMk+"]") != 1 {
		t.Fatalf("duplicate registration:\n%s", b)
	}
	cs := doctorBundle(e.opts)
	if len(cs) != 1 || cs[0].Status != "ok" || !strings.Contains(cs[0].Detail, "0.3.2") {
		t.Fatalf("doctor: %+v", cs)
	}
	os.WriteFile(nb.Exe, []byte("tampered"), 0o755)
	if cs := doctorBundle(e.opts); cs[0].Status != "fail" {
		t.Fatalf("doctor missed a tampered bundle: %+v", cs)
	}
}

func TestBundleOpenCode(t *testing.T) {
	skipOpenCode(t)
	e, f, broot := bundleEnv(t, true)
	e.opts.KenazBin, e.opts.AdapterDir = "/elsewhere/kenaz", ""
	if err := Apply(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}
	b, _ := LoadBundle(broot)
	got, _ := e.read(t, harnessOpenCode)
	var cfg struct {
		MCP    map[string]struct{ Command []string } `json:"mcp"`
		Plugin []json.RawMessage                     `json:"plugin"`
	}
	json.Unmarshal(got, &cfg)
	if cfg.MCP["kenaz"].Command[0] != b.Exe || pluginSpec(cfg.Plugin[0]) != b.AdapterDir() || len(f.calls) != 0 {
		t.Fatalf("opencode not pointed at the bundle:\n%s", got)
	}
	if err := Undo(e.opts, harnessOpenCode); err != nil {
		t.Fatal(err)
	}

	e2, _, _ := bundleEnv(t, false)
	if err := Apply(e2.opts, harnessOpenCode); err == nil || !strings.Contains(err.Error(), "no OpenCode adapter") {
		t.Fatalf("bundle without adapter: %v", err)
	}
}

func TestCodexRegistrationParse(t *testing.T) {
	src, en := codexRegistration("model = \"x\"\n[marketplaces." + tMk + "]\nsource_type = \"local\"\nsource = \"/a b/c\"\n\n[plugins.\"" + tID + "\"]\nenabled = true\n")
	if src != "/a b/c" || !en {
		t.Fatalf("%q %v", src, en)
	}
	if src, _ := codexRegistration("[marketplaces." + tMk + "]\nsource_type = \"git\"\nsource = \"https://x\"\n"); src != "?" {
		t.Fatalf("remote: %q", src)
	}
	if src, en := codexRegistration("[otel]\n"); src != "" || en {
		t.Fatal("phantom registration")
	}
}

// Every file under bin/, plugin/ and opencode/ must be listed in
// bundle.json, and none may be a symlink: an extra skill or command, an
// unlisted adapter module (even "modified"), or a symlink fails
// verification.
func TestLoadBundleRejectsUnlistedAndSymlinks(t *testing.T) {
	cases := map[string]func(t *testing.T, b string){
		"extra skill": func(t *testing.T, b string) {
			p := filepath.Join(b, "plugin", "skills", "extra", "SKILL.md")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("---\nname: extra\n---\n"), 0o644)
		},
		"extra command": func(t *testing.T, b string) {
			os.WriteFile(filepath.Join(b, "plugin", "commands", "extra.md"), []byte("run something"), 0o644)
		},
		"look-alike of .DS_Store": func(t *testing.T, b string) {
			os.WriteFile(filepath.Join(b, "plugin", "skills", ".DS_Store.md"), []byte("x"), 0o644)
		},
		"extra file in bin": func(t *testing.T, b string) {
			os.WriteFile(filepath.Join(b, "bin", "helper"), []byte("x"), 0o755)
		},
		"modified unlisted adapter module": func(t *testing.T, b string) {
			os.WriteFile(filepath.Join(b, "opencode", "src", "extra.js"), []byte("export default {}\n"), 0o644)
		},
		"adapter module dropped from the list and modified": func(t *testing.T, b string) {
			raw, _ := os.ReadFile(filepath.Join(b, BundleFile))
			var m map[string]any
			json.Unmarshal(raw, &m)
			delete(m["files"].(map[string]any), "opencode/src/map.js")
			out, _ := json.Marshal(m)
			os.WriteFile(filepath.Join(b, BundleFile), out, 0o644)
			os.WriteFile(filepath.Join(b, "opencode", "src", "map.js"), []byte("/* modified */\n"), 0o644)
		},
		"symlinked plugin file": func(t *testing.T, b string) {
			p := filepath.Join(b, "plugin", "hooks", "hooks.json")
			data, _ := os.ReadFile(p)
			outside := filepath.Join(t.TempDir(), "hooks.json")
			os.WriteFile(outside, data, 0o644) // same bytes: the digest matches
			os.Remove(p)
			if err := os.Symlink(outside, p); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		},
		"symlinked directory": func(t *testing.T, b string) {
			src := filepath.Join(b, "plugin", "skills")
			moved := filepath.Join(t.TempDir(), "skills")
			if err := os.Rename(src, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, src); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		},
	}
	// What release archives and installs legitimately carry is accepted:
	// unlisted bundle.json and SHA256SUMS at the root, and Finder's
	// .DS_Store inside a directory.
	ok := makeBundle(t, t.TempDir(), testBundleVersion, true)
	os.WriteFile(filepath.Join(ok, "SHA256SUMS"), []byte("x  bin/kenaz\n"), 0o644)
	os.WriteFile(filepath.Join(ok, "plugin", "skills", ".DS_Store"), []byte{0, 0, 0, 1}, 0o644)
	if _, err := LoadBundle(ok); err != nil {
		t.Fatalf("release archive layout refused: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := makeBundle(t, t.TempDir(), testBundleVersion, true)
			if _, err := LoadBundle(b); err != nil {
				t.Fatalf("clean bundle: %v", err)
			}
			mutate(t, b)
			if _, err := LoadBundle(b); err == nil {
				t.Fatal("verification passed")
			}
		})
	}
}

// Render writes the bytes LoadBundle verified: a file swapped after
// verification is not what gets rendered.
func TestRenderCopiesVerifiedBytes(t *testing.T) {
	b := makeBundle(t, t.TempDir(), testBundleVersion, false)
	got, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	cmds, _ := filepath.Glob(filepath.Join(b, "plugin", "commands", "*.md"))
	if len(cmds) == 0 {
		t.Skip("no command files")
	}
	orig, _ := os.ReadFile(cmds[0])
	os.WriteFile(cmds[0], []byte("swapped after verification"), 0o644)
	dir := filepath.Join(t.TempDir(), "rendered")
	if _, err := bundle.Render(got, dir, true); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "plugin", "commands", filepath.Base(cmds[0])))
	if !bytes.Equal(out, orig) {
		t.Fatal("rendered bytes were re-read from disk after verification")
	}
}

// An installer-managed archive (install-state.json, versions/A and
// versions/B, current -> B) registers through install/current, so an
// upgrade or rollback (switching current) reaches the hosts and pruning an
// old version breaks nothing. A plain archive keeps its resolved path.
func TestRegistersThroughInstallCurrent(t *testing.T) {
	install := filepath.Join(t.TempDir(), "install")
	a := makeBundle(t, filepath.Join(install, "staging-a"), testBundleVersion, false)
	b := makeBundle(t, filepath.Join(install, "staging-b"), testBundleVersion, false)
	os.MkdirAll(filepath.Join(install, "versions"), 0o755)
	va, vb := filepath.Join(install, "versions", "A"), filepath.Join(install, "versions", "B")
	if err := os.Rename(a, va); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(b, vb); err != nil {
		t.Fatal(err)
	}
	exe := tBin
	if runtime.GOOS == "windows" {
		exe = tBin + ".exe"
	}
	if err := os.Symlink(filepath.Join("versions", "B"), filepath.Join(install, "current")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	resolved := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	exeB := resolved(filepath.Join(vb, "bin", exe))

	// Without install-state.json it is not an installer root.
	if got := bundle.StablePath(exeB); got != exeB {
		t.Fatalf("no install state: %s", got)
	}
	os.WriteFile(filepath.Join(install, bundle.InstallStateFile), []byte(`{}`), 0o644)
	want := filepath.Join(resolved(install), "current", "bin", exe)
	if got := bundle.StablePath(exeB); got != want {
		t.Fatalf("StablePath = %s, want %s", got, want)
	}
	// A version current does not point at is left alone.
	exeA := resolved(filepath.Join(va, "bin", exe))
	if got := bundle.StablePath(exeA); got != exeA {
		t.Fatalf("non-current version rerouted: %s", got)
	}
	// The bundle detected from the stable path loads and renders through
	// current: every launch command goes through install/current.
	root := DetectBundle(want)
	if filepath.Base(root) != "current" || bundle.StableRoot(vb) != filepath.Join(resolved(install), "current") {
		t.Fatalf("bundle root = %s / %s", root, bundle.StableRoot(vb))
	}
	bb, err := LoadBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	if bb.Exe != want {
		t.Fatalf("bundle exe = %s", bb.Exe)
	}
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := bundle.Render(bb, dir, true); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".mcp.json", ".codex-mcp.json", "hooks/hooks.json"} {
		raw, _ := os.ReadFile(filepath.Join(dir, "plugin", filepath.FromSlash(rel)))
		if !strings.Contains(string(raw), mustJSONInner(filepath.Join("current", "bin", exe))) || strings.Contains(string(raw), mustJSONInner(filepath.Join("versions", "B"))) {
			t.Fatalf("%s does not launch through current: %s", rel, raw)
		}
	}
	// A plain (non-installer) archive keeps its absolute resolved path.
	plain := makeBundle(t, t.TempDir(), testBundleVersion, false)
	pexe := resolved(filepath.Join(plain, "bin", exe))
	if bundle.StablePath(pexe) != pexe || bundle.StableRoot(plain) != plain {
		t.Fatal("plain archive rerouted")
	}
}

func mustJSONInner(s string) string { b, _ := json.Marshal(s); return string(b[1 : len(b)-1]) }
