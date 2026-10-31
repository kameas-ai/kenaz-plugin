package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTreeDigestAndPluginFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"plugin/hooks/hooks.json":           "{}\n",
		"plugin/.claude-plugin/plugin.json": `{"name":"kenaz"}`,
		".claude-plugin/marketplace.json":   `{"name":"kenaz"}`,
	}
	for rel, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700)
		os.WriteFile(filepath.Join(root, rel), []byte(c), 0o600)
	}
	os.WriteFile(filepath.Join(root, "README.md"), []byte("not covered"), 0o600)
	got, err := PluginFiles(root)
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", err, got)
	}
	// The release pipeline's recipe: sha256 of sha256sum lines sorted by path.
	line := func(rel string) string {
		s := sha256.Sum256([]byte(files[rel]))
		return hex.EncodeToString(s[:]) + "  " + rel + "\n"
	}
	want := sha256.Sum256([]byte(line(".claude-plugin/marketplace.json") + line("plugin/.claude-plugin/plugin.json") + line("plugin/hooks/hooks.json")))
	if TreeDigest(got) != hex.EncodeToString(want[:]) {
		t.Fatal("digest differs from the documented recipe")
	}
	if runtime.GOOS != "windows" {
		os.Symlink("/etc/hosts", filepath.Join(root, "plugin", "x"))
		if _, err := PluginFiles(root); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}
