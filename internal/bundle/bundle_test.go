package bundle

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/internal/variant/varianttest"
)

// Render changes launch commands only: the rendered hooks and MCP files
// are the archive's files with "kenaz" replaced by the absolute
// executable, and nothing added or removed.
func TestRenderOnlyRewritesLaunchCommands(t *testing.T) {
	root := filepath.Join("..", "..")
	bin := variant.Current.Binary
	exe := filepath.Join(t.TempDir(), "Kenaz Bundle", "bin", bin)
	abs, _ := filepath.Abs(root)
	tree := readTree(t, abs)
	b := &Bundle{Root: abs, Version: "0", OS: "x", Arch: "y", Exe: exe, verified: readTree(t, abs)}
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := Render(b, dir, true); err != nil {
		t.Fatal(err)
	}
	parse := func(raw []byte) any {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, rel := range []string{".mcp.json", ".codex-mcp.json", "hooks/hooks.json"} {
		orig := parse(tree["plugin/"+rel])
		raw, err := os.ReadFile(filepath.Join(dir, "plugin", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		got := parse(raw)
		// Undo the one permitted edit and compare whole documents.
		raw, _ = json.Marshal(got)
		s := strings.ReplaceAll(string(raw), mustJSONInner(`"`+exe+`"`), bin)
		s = strings.ReplaceAll(s, mustJSONInner(exe), bin)
		var back any
		json.Unmarshal([]byte(s), &back)
		if !reflect.DeepEqual(orig, back) {
			t.Errorf("%s: rendered file differs beyond the launch command:\n%s", rel, raw)
		}
	}
	// Every other plugin file is copied byte for byte.
	for rel, a := range tree {
		switch rel {
		case "plugin/.mcp.json", "plugin/.codex-mcp.json", "plugin/hooks/hooks.json":
			continue
		}
		c, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(a) != string(c) {
			t.Errorf("%s not copied verbatim", rel)
		}
	}
}

// The staff bundle script's output is a bundle this program accepts, and it renders.
func TestStaffBundleScript(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	if variant.Current.BundleName != variant.Kenaz.BundleName {
		t.Skip("staff bundles are of the public identity")
	}
	bin := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "with space", "b")
	script := filepath.Join("..", "..", "scripts", "staff-bundle.py")
	if out, err := exec.Command(python, script, "--binary", bin, "--out", dir, "--suffix", "staff.test").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.Version, "-staff.test") {
		t.Errorf("version %q", b.Version)
	}
	// What only staff get is laid over plugin/ and covered by the manifest.
	if _, err := os.Stat(filepath.Join(dir, "plugin", "skills", "draft-context", "SKILL.md")); err != nil {
		t.Error(err)
	}
	overlay, err := os.ReadFile(filepath.Join(dir, "plugin", ".codex-mcp.json"))
	if err != nil || !strings.Contains(string(overlay), `"context_share_file"`) {
		t.Errorf("the staff bundle lost the Codex prompt: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil || !strings.Contains(string(manifest), "plugin/skills/draft-context/SKILL.md") {
		t.Errorf("bundle.json does not cover the skill: %v", err)
	}
	if _, err := Render(b, filepath.Join(t.TempDir(), "r"), true); err != nil {
		t.Error(err)
	}
}

func mustJSONInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func TestHookQuoteRefusesExpansion(t *testing.T) {
	for _, p := range []string{`/a"b/kenaz`, "/a$HOME/kenaz", "/a`x`/kenaz", `C:\100%\kenaz.exe`} {
		if _, err := hookQuote(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if q, err := hookQuote(`C:\Program Files\Kenaz\kenaz.exe`); err != nil || q != `"C:\Program Files\Kenaz\kenaz.exe"` {
		t.Fatalf("%q %v", q, err)
	}
}

// readTree loads the files Render copies from the repo's plugin sources as
// this build variant ships them, standing in for LoadBundle's verified
// bytes.
func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out, err := varianttest.PluginTree(root, variant.Current)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Render refuses a Bundle that LoadBundle did not verify.
func TestRenderNeedsVerifiedBundle(t *testing.T) {
	if _, err := Render(&Bundle{Root: t.TempDir(), Exe: "/x/kenaz"}, t.TempDir(), false); err == nil {
		t.Fatal("rendered an unverified bundle")
	}
}

// A bundle.json that names another build's bundle is refused with a
// message that says so (issue #34 F5); an unknown name is an unsupported
// bundle.
func TestLoadBundleOtherVariantName(t *testing.T) {
	for _, v := range variant.Variants {
		root := t.TempDir()
		man := `{"schema":1,"name":"` + v.BundleName + `","version":"1","os":"x","arch":"y"}`
		if err := os.WriteFile(filepath.Join(root, BundleFile), []byte(man), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadBundle(root)
		other := "is a " + v.BundleName + " bundle: " + variant.Current.Binary + " registers only " + variant.Current.BundleName + " bundles"
		switch {
		case v.Name == variant.Current.Name:
			// Its own name gets past the identity check, to the platform one.
			if err == nil || strings.Contains(err.Error(), "registers only") {
				t.Errorf("own bundle: %v", err)
			}
		case err == nil || !strings.Contains(err.Error(), other):
			t.Errorf("%s bundle: %v", v.Name, err)
		}
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, BundleFile), []byte(`{"schema":1,"name":"someone-else"}`), 0o644)
	if _, err := LoadBundle(root); err == nil || !strings.Contains(err.Error(), "unsupported bundle") {
		t.Errorf("unknown name: %v", err)
	}
}
