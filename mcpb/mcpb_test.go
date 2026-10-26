package mcpb

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type manifest struct {
	ManifestVersion string `json:"manifest_version"`
	Name            string `json:"name"`
	DisplayName     string `json:"display_name"`
	Version         string `json:"version"`
	Description     string `json:"description"`
	Author          struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"author"`
	Server struct {
		Type       string `json:"type"`
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcp_config"`
	} `json:"server"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tools"`
	Repository struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"repository"`
	Documentation   string   `json:"documentation"`
	Support         string   `json:"support"`
	Icon            string   `json:"icon"`
	Keywords        []string `json:"keywords"`
	PrivacyPolicies []string `json:"privacy_policies"`
	Compatibility   struct {
		ClaudeDesktop string   `json:"claude_desktop"`
		Platforms     []string `json:"platforms"`
	} `json:"compatibility"`
	UserConfig map[string]struct {
		Type        string `json:"type"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Required    *bool  `json:"required"`
		Default     any    `json:"default"`
	} `json:"user_config"`
}

// Top-level keys the MCPB v0.3 manifest schema allows (it is strict).
var v03Keys = map[string]bool{
	"$schema": true, "dxt_version": true, "manifest_version": true, "name": true, "display_name": true,
	"version": true, "description": true, "long_description": true, "author": true, "repository": true,
	"homepage": true, "documentation": true, "support": true, "icon": true, "icons": true, "screenshots": true,
	"localization": true, "server": true, "tools": true, "tools_generated": true, "prompts": true,
	"prompts_generated": true, "keywords": true, "license": true, "privacy_policies": true,
	"compatibility": true, "user_config": true, "_meta": true,
}

func readJSON(t *testing.T, rel string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

func render(t *testing.T, ver, platform string) (manifest, map[string]any) {
	t.Helper()
	b, err := Render(ver, platform)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	var raw map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("rendered %s manifest is not JSON: %v", platform, err)
	}
	json.Unmarshal(b, &raw)
	return m, raw
}

func TestTemplatePlaceholders(t *testing.T) {
	got := map[string]bool{}
	for _, p := range Placeholder.FindAllString(Template, -1) {
		got[p] = true
	}
	want := map[string]bool{"@VERSION@": true, "@PLATFORM@": true, "@EXE@": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template placeholders %v, want exactly %v", got, want)
	}
	var v any
	if err := json.Unmarshal([]byte(Template), &v); err != nil {
		t.Fatalf("the template itself must be JSON: %v", err)
	}
}

// approvedDescription is the Claude desktop app extension description
// Kameas approved. Use it exactly.
const approvedDescription = "Kenaz, a Kameas AI product. Brings your organization's Kameas Fleet skills, team context and your Kenaz memory to chat in the Claude desktop app. Sends no usage telemetry. Requires the kenaz program and a Kameas Fleet account. Claude is a trademark of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic."

func TestRenderEveryPlatform(t *testing.T) {
	var plugin struct {
		Description string `json:"description"`
		Author      struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"author"`
		Homepage   string `json:"homepage"`
		UserConfig map[string]struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Default     any    `json:"default"`
			Options     []string
		} `json:"userConfig"`
	}
	readJSON(t, "plugin/.claude-plugin/plugin.json", &plugin)
	var mcpJSON struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	readJSON(t, "plugin/.mcp.json", &mcpJSON)
	cc := mcpJSON.MCPServers["kenaz"]

	for platform, exe := range Executables {
		t.Run(platform, func(t *testing.T) {
			m, raw := render(t, "1.2.3-rc.1", platform)
			for k := range raw {
				if !v03Keys[k] {
					t.Errorf("key %q is not in the MCPB v0.3 manifest schema", k)
				}
			}
			if m.ManifestVersion != "0.3" || m.Name != "kenaz" || m.DisplayName != "Kenaz" || m.Version != "1.2.3-rc.1" {
				t.Errorf("identity: %q %q %q %q", m.ManifestVersion, m.Name, m.DisplayName, m.Version)
			}
			// Approved text only: the Claude Desktop extension description,
			// extended trademark notice included. It is its own
			// text, not the plugin description (which is unchanged).
			if m.Description != approvedDescription || m.Description == plugin.Description {
				t.Errorf("description must be the approved Claude Desktop extension description, got %q", m.Description)
			}
			if m.Author.Name != "Kameas AI, Inc." || m.Author != plugin.Author {
				t.Errorf("author %+v, plugin %+v", m.Author, plugin.Author)
			}
			if raw["homepage"] != plugin.Homepage {
				t.Errorf("homepage %v", raw["homepage"])
			}
			// The bundle ships the official executable under its own licences
			// (licenses/), not the plugin's.
			if raw["license"] != "SEE LICENSE IN licenses/LICENSE" {
				t.Errorf("license %v: must point at the official build's licence in licenses/", raw["license"])
			}
			s := m.Server
			if s.Type != "binary" || s.EntryPoint != "server/"+exe || s.MCPConfig.Command != "${__dirname}/server/"+exe {
				t.Errorf("server %+v", s)
			}
			if !reflect.DeepEqual(s.MCPConfig.Args, []string{"mcp"}) || !reflect.DeepEqual(s.MCPConfig.Args, cc.Args) {
				t.Errorf("args %v, plugin %v", s.MCPConfig.Args, cc.Args)
			}
			// The Claude Code plugin's env, plus the host marker.
			want := map[string]string{"KENAZ_MCP_HOST": "claude-desktop"}
			for k, v := range cc.Env {
				want[k] = v
			}
			if !reflect.DeepEqual(s.MCPConfig.Env, want) {
				t.Errorf("env %v, want %v", s.MCPConfig.Env, want)
			}
			if !reflect.DeepEqual(m.Compatibility.Platforms, []string{platform}) {
				t.Errorf("platforms %v", m.Compatibility.Platforms)
			}
			// The published privacy policy (Anthropic's
			// policy rejects an extension without one), the bundled icon,
			// and the same support page as the winget listing.
			if !reflect.DeepEqual(m.PrivacyPolicies, []string{"https://kenaz.kameas.ai/privacy.html"}) {
				t.Errorf("privacy_policies %v", m.PrivacyPolicies)
			}
			if m.Icon != IconFile {
				t.Errorf("icon %q, want %q (packed at the bundle root)", m.Icon, IconFile)
			}
			if m.Support != "https://docs.kameas.ai/docs/kenaz-plugin/troubleshooting" ||
				m.Documentation != "https://docs.kameas.ai/docs/kenaz-plugin/overview" {
				t.Errorf("support %q, documentation %q", m.Support, m.Documentation)
			}
			if m.Repository.Type != "git" || m.Repository.URL != "https://github.com/kameas-ai/kenaz-plugin.git" {
				t.Errorf("repository %+v", m.Repository)
			}
			if len(m.Keywords) == 0 {
				t.Error("keywords are empty")
			}
			if !strings.HasPrefix(m.Compatibility.ClaudeDesktop, ">=") {
				t.Errorf("compatibility.claude_desktop %q: want a minimum version", m.Compatibility.ClaudeDesktop)
			}
			var keys, pkeys []string
			for k := range m.UserConfig {
				keys = append(keys, k)
			}
			for k := range plugin.UserConfig {
				pkeys = append(pkeys, k)
			}
			sort.Strings(keys)
			sort.Strings(pkeys)
			if !reflect.DeepEqual(keys, pkeys) {
				t.Fatalf("user_config %v, plugin userConfig %v", keys, pkeys)
			}
			for k, u := range m.UserConfig {
				p := plugin.UserConfig[k]
				// Claude Desktop requires a title and a description for every field.
				if u.Type != "string" || u.Title != p.Title || u.Description == "" || u.Required == nil || *u.Required {
					t.Errorf("user_config.%s = %+v", k, u)
				}
				// An unset field without a default reaches the server as the
				// literal "${user_config.x}"; every field has a default.
				if u.Default == nil {
					t.Errorf("user_config.%s has no default", k)
				}
				if p.Default != nil && u.Default != p.Default {
					t.Errorf("user_config.%s default %v, plugin %v", k, u.Default, p.Default)
				}
				for _, o := range p.Options {
					if !strings.Contains(u.Description, o) {
						t.Errorf("user_config.%s description must name option %q (MCPB has no choice lists)", k, o)
					}
				}
			}
			names := map[string]bool{}
			for _, tl := range m.Tools {
				if tl.Name == "" || tl.Description == "" || names[tl.Name] {
					t.Errorf("tool entry %+v", tl)
				}
				names[tl.Name] = true
			}
		})
	}
}

func TestRenderRefusesBadInput(t *testing.T) {
	for _, c := range [][2]string{{"v1.2.3", "darwin"}, {"1.2", "darwin"}, {"1.2.3", "linux"}, {"1.2.3", ""}} {
		if _, err := Render(c[0], c[1]); err == nil {
			t.Errorf("Render(%q, %q) accepted", c[0], c[1])
		}
	}
}

// The icon Claude Desktop shows: a 512x512 PNG with an alpha channel
// (Anthropic's MCPB guidance: 512x512 recommended, PNG with transparency).
func TestIcon(t *testing.T) {
	b, err := os.ReadFile(IconFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) || len(b) < 33 || string(b[12:16]) != "IHDR" {
		t.Fatalf("%s is not a PNG", IconFile)
	}
	w, h := binary.BigEndian.Uint32(b[16:20]), binary.BigEndian.Uint32(b[20:24])
	depth, color := b[24], b[25]
	if w != 512 || h != 512 || depth != 8 || color != 6 {
		t.Errorf("%s is %dx%d, bit depth %d, colour type %d; want 512x512 8-bit RGBA", IconFile, w, h, depth, color)
	}
	for _, c := range []string{"eXIf", "tEXt", "iTXt", "zTXt", "tIME"} {
		if bytes.Contains(b, []byte(c)) {
			t.Errorf("%s carries a %s metadata chunk", IconFile, c)
		}
	}
}
