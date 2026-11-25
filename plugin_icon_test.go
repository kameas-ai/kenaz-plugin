package kenaz

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

// pluginIcon is the listing icon Anthropic's directory reads from the plugin
// folder. The directory takes it only the first time the plugin is saved or
// submitted in the developer portal, so it must be in place before that.
const pluginIcon = "plugin/.claude-plugin/icon.png"

// TestPluginDirectoryIcon holds the icon to what `claude plugin validate`
// asks for: a square PNG, 512 to 2048 px on each side, under 2 MB. It is the
// Kenaz mark the desktop extension uses.
func TestPluginDirectoryIcon(t *testing.T) {
	b, err := os.ReadFile(pluginIcon)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= 2<<20 {
		t.Errorf("%s is %d bytes, over 2 MB", pluginIcon, len(b))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s is not a PNG: %v", pluginIcon, err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("%s is not a complete PNG: %v", pluginIcon, err)
	}
	if cfg.Width != cfg.Height || cfg.Width < 512 || cfg.Width > 2048 {
		t.Errorf("%s is %dx%d, want a square of 512 to 2048 px", pluginIcon, cfg.Width, cfg.Height)
	}
	mark, err := os.ReadFile("mcpb/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, mark) {
		t.Errorf("%s differs from mcpb/icon.png, the Kenaz mark", pluginIcon)
	}
}
