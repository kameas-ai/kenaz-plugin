package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Render changes launch commands only: the rendered hooks and MCP files
// are the archive's files with "kenaz" replaced by the absolute
// executable, and nothing added or removed.
func TestRenderOnlyRewritesLaunchCommands(t *testing.T) {
	root := filepath.Join("..", "..")
	exe := filepath.Join(t.TempDir(), "Kenaz Bundle", "bin", "kenaz")
	abs, _ := filepath.Abs(root)
	b := &Bundle{Root: abs, Version: "0", OS: "x", Arch: "y", Exe: exe, verified: readTree(t, abs)}
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := Render(b, dir, true); err != nil {
		t.Fatal(err)
	}
	load := func(p string) any {
		var v any
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, rel := range []string{".mcp.json", ".codex-mcp.json", "hooks/hooks.json"} {
		orig := load(filepath.Join(root, "plugin", filepath.FromSlash(rel)))
		got := load(filepath.Join(dir, "plugin", filepath.FromSlash(rel)))
		// Undo the one permitted edit and compare whole documents.
		raw, _ := json.Marshal(got)
		s := strings.ReplaceAll(string(raw), mustJSONInner(`"`+exe+`"`), "kenaz")
		s = strings.ReplaceAll(s, mustJSONInner(exe), "kenaz")
		var back any
		json.Unmarshal([]byte(s), &back)
		if !reflect.DeepEqual(orig, back) {
			t.Errorf("%s: rendered file differs beyond the launch command:\n%s", rel, raw)
		}
	}
	// Every other plugin file is copied byte for byte.
	filepath.WalkDir(filepath.Join(root, "plugin"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "plugin"), p)
		switch filepath.ToSlash(rel) {
		case ".mcp.json", ".codex-mcp.json", "hooks/hooks.json":
			return nil
		}
		a, _ := os.ReadFile(p)
		c, err := os.ReadFile(filepath.Join(dir, "plugin", rel))
		if err != nil || string(a) != string(c) {
			t.Errorf("%s not copied verbatim", rel)
		}
		return nil
	})
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

// readTree loads the files Render copies from a source tree (the repo),
// standing in for LoadBundle's verified bytes.
func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	filepath.WalkDir(filepath.Join(root, "plugin"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		return nil
	})
	out[".claude-plugin/marketplace.json"], _ = os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	return out
}

// Render refuses a Bundle that LoadBundle did not verify.
func TestRenderNeedsVerifiedBundle(t *testing.T) {
	if _, err := Render(&Bundle{Root: t.TempDir(), Exe: "/x/kenaz"}, t.TempDir(), false); err == nil {
		t.Fatal("rendered an unverified bundle")
	}
}
