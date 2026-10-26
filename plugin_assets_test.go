package kenaz

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDisclaimerPresent enforces that the third-party
// disclaimer appears byte-for-byte in every listing surface.
func TestDisclaimerPresent(t *testing.T) {
	for _, path := range []string{"README.md", "plugin/README.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), Disclaimer) {
			t.Errorf("%s: missing disclaimer", path)
		}
	}

	for path, want := range map[string]string{
		"plugin/.claude-plugin/plugin.json": DisclaimerGAnthropic,
		"plugin/.codex-plugin/plugin.json":  Disclaimer,
	} {
		var pj struct{ Description string }
		readJSON(t, path, &pj)
		if !strings.HasSuffix(pj.Description, want) {
			t.Errorf("%s: description must end with its disclaimer", path)
		}
	}

	var mk struct {
		Metadata struct{ Description string }
		Plugins  []struct{ Description string }
	}
	readJSON(t, ".claude-plugin/marketplace.json", &mk)
	if len(mk.Plugins) == 0 || !strings.HasSuffix(mk.Plugins[0].Description, DisclaimerGAnthropicOpenAI) {
		t.Error("marketplace entry description must end with the Anthropic and OpenAI trademark notice")
	}
	if !strings.HasSuffix(mk.Metadata.Description, DisclaimerGAnthropicOpenAI) {
		t.Error("marketplace metadata description must end with the Anthropic and OpenAI trademark notice")
	}
}

// TestOpenCoreWording enforces the approved open-core wording now that this
// repository is public: the README carries the approved open-core and
// official-builds statements verbatim, the Agent Telemetry Notice editions
// carry the public-repository sentence, and no surface claims more than
// the approved wording allows.
func TestOpenCoreWording(t *testing.T) {
	readme := readText(t, "README.md")
	for _, want := range []string{OpenCoreStatement, OfficialBuildsStatement} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md: missing approved statement %q", want)
		}
	}

	for _, v := range []string{"v1", "v2", "v2-small"} {
		f := "notices/agent-telemetry/" + v + ".md"
		s := readText(t, f)
		if !strings.Contains(s, NoticeFilteringSentence) {
			t.Errorf("%s: missing the public-repository sentence", f)
		}
		if strings.Contains(s, noticeFilteringSentenceAlpha) {
			t.Errorf("%s: still carries the private-repo (alpha) sentence", f)
		}
	}

	files := []string{"README.md", "plugin/README.md", "plugin/.claude-plugin/plugin.json",
		"plugin/.codex-plugin/plugin.json", ".claude-plugin/marketplace.json"}
	for _, v := range []string{"v1", "v2", "v2-small"} {
		files = append(files, "notices/agent-telemetry/"+v+".md")
	}
	for _, f := range files {
		s := strings.ToLower(readText(t, f))
		// Phrasings that are not approved.
		for _, banned := range []string{
			"fully open source", "open-source plugin", "open source plugin",
			"nothing leaves your device that you can't audit",
			"what leaves your device is decided by open-source code",
		} {
			if strings.Contains(s, banned) {
				t.Errorf("%s: unapproved open-core wording %q", f, banned)
			}
		}
	}
}

// noticeFilteringSentenceAlpha is the sentence the notices carried while the
// repository was private. It must not come back.
const noticeFilteringSentenceAlpha = "This filtering happens on your device, and Fleet rejects anything outside this list again on arrival."

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestManifestVersionsAgree enforces that the plugin and the
// marketplace entry move together.
func TestManifestVersionsAgree(t *testing.T) {
	var pj struct{ Version string }
	readJSON(t, "plugin/.claude-plugin/plugin.json", &pj)
	var cx struct{ Version string }
	readJSON(t, "plugin/.codex-plugin/plugin.json", &cx)
	var mk struct {
		Plugins []struct{ Version string }
	}
	readJSON(t, ".claude-plugin/marketplace.json", &mk)

	if len(mk.Plugins) == 0 || mk.Plugins[0].Version != pj.Version {
		t.Errorf("marketplace version != plugin.json version %q", pj.Version)
	}
	if cx.Version != pj.Version {
		t.Errorf("Codex manifest version %q != plugin.json version %q", cx.Version, pj.Version)
	}
}

// TestPluginLaunchesInstalledBinary pins how the plugin starts the relay.
// The MCP server and hooks run the installed kenaz binary from PATH
// as a bare command (no ${CLAUDE_PLUGIN_ROOT}, which Codex
// does not expand). They must not depend on npx: the @kameas npm scope is
// not provisioned, and an npx launch also fails without a Node toolchain or
// network. A shell fallback (sh -c "... || npx ...") is not portable to
// Windows, where neither harness spawns MCP servers through a POSIX shell.
func TestPluginLaunchesInstalledBinary(t *testing.T) {
	for _, path := range []string{"plugin/.mcp.json", "plugin/.codex-mcp.json"} {
		var mcp struct {
			McpServers map[string]struct {
				Command string
				Args    []string
			} `json:"mcpServers"`
		}
		readJSON(t, path, &mcp)
		srv, ok := mcp.McpServers["kenaz"]
		if !ok {
			t.Fatalf("%s: no kenaz server", path)
		}
		if srv.Command != "kenaz" || len(srv.Args) != 1 || srv.Args[0] != "mcp" {
			t.Errorf("%s: want command kenaz args [mcp], got %q %v", path, srv.Command, srv.Args)
		}
	}

	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	readJSON(t, "plugin/hooks/hooks.json", &hooks)
	want := map[string]string{
		"SessionStart": "kenaz hook session-start",
		"SessionEnd":   "kenaz hook session-end",
	}
	for event, cmd := range want {
		var got []string
		for _, m := range hooks.Hooks[event] {
			for _, h := range m.Hooks {
				got = append(got, h.Command)
			}
		}
		if len(got) != 1 || got[0] != cmd {
			t.Errorf("hooks.json %s: want [%q], got %q", event, cmd, got)
		}
	}

	for _, path := range []string{"plugin/.mcp.json", "plugin/.codex-mcp.json", "plugin/hooks/hooks.json", "plugin/README.md"} {
		if s := readText(t, path); strings.Contains(s, "npx") || strings.Contains(s, "@kameas/fleet") {
			t.Errorf("%s: must not launch through npx or the @kameas/fleet npm package", path)
		}
	}
}

// codexDescription is the approved Codex plugin description (unchanged by
// the newer listing text: it names Claude Code and Codex). The Claude
// plugin and the marketplace use ClaudePluginDescription and
// MarketplaceDescription (legal.go).
const codexDescription = "Kenaz, a Kameas AI product. Connects your Claude Code and Codex sessions to your organization's Kameas Fleet."

// TestPluginIdentity pins the product name everywhere a harness or a user
// sees it: plugin and marketplace names (install id kenaz@kenaz), the
// skill directory, the binary directory and the approved descriptions.
func TestPluginIdentity(t *testing.T) {
	for path, want := range map[string]string{
		"plugin/.claude-plugin/plugin.json": ClaudePluginDescription,
		"plugin/.codex-plugin/plugin.json":  codexDescription + " " + Disclaimer,
	} {
		var pj struct{ Name, Description string }
		readJSON(t, path, &pj)
		if pj.Name != "kenaz" {
			t.Errorf("%s: name %q, want kenaz", path, pj.Name)
		}
		if pj.Description != want {
			t.Errorf("%s: description must be the approved text, got %q", path, pj.Description)
		}
	}
	var mk struct {
		Name     string
		Metadata struct{ Description string }
		Plugins  []struct{ Name, Description string }
	}
	readJSON(t, ".claude-plugin/marketplace.json", &mk)
	if mk.Name != "kenaz" {
		t.Errorf("marketplace name %q, want kenaz", mk.Name)
	}
	if len(mk.Plugins) != 1 || mk.Plugins[0].Name != "kenaz" || mk.Plugins[0].Description != MarketplaceDescription {
		t.Errorf("marketplace must list exactly the kenaz plugin with the approved description, got %+v", mk.Plugins)
	}
	if mk.Metadata.Description != MarketplaceMetadataDescription {
		t.Errorf("marketplace metadata description %q", mk.Metadata.Description)
	}
	for _, dir := range []string{"plugin/skills/kenaz", "cmd/kenaz"} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s: missing", dir)
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
