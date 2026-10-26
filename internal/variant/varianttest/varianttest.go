// Package varianttest gives tests the repository's plugin sources as a
// build variant ships them, so the same tests run for every variant
// (go test -tags kenaz_dev ./...).
//
// plugin/ and .claude-plugin/marketplace.json are the public build's
// sources. For another variant PluginTree renames what identifies the
// plugin (marketplace, plugin, MCP server and skill names, the hook and
// server executable, the fleet_env choices and description prefixes) and
// adds or removes nothing. The env variable the server reads is shared by
// every build (variant.Shared), so it stays.
package varianttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// PluginTree returns plugin/ and .claude-plugin/marketplace.json under
// repoRoot (keyed by slash path) as variant id ships them.
func PluginTree(repoRoot string, id variant.Identity) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(filepath.Join(repoRoot, "plugin"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(repoRoot, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		return nil, err
	}
	mk, err := os.ReadFile(filepath.Join(repoRoot, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, err
	}
	files[".claude-plugin/marketplace.json"] = mk
	if id.Name == variant.Kenaz.Name {
		return files, nil
	}
	return rename(files, variant.Kenaz, id)
}

// WritePluginTree writes PluginTree into dst (plugin/ and .claude-plugin/).
func WritePluginTree(repoRoot, dst string, id variant.Identity) error {
	files, err := PluginTree(repoRoot, id)
	if err != nil {
		return err
	}
	for rel, b := range files {
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func rename(in map[string][]byte, from, to variant.Identity) (map[string][]byte, error) {
	out := map[string][]byte{}
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sort.Strings(names)
	skillDir := "plugin/skills/" + from.Plugin + "/"
	for _, n := range names {
		b := in[n]
		var err error
		switch path.Base(n) {
		case "marketplace.json":
			b, err = editJSON(b, func(doc map[string]any) {
				doc["name"] = to.Marketplace
				if md, _ := doc["metadata"].(map[string]any); md != nil {
					md["description"] = prefix(md["description"], from, to)
				}
				ps, _ := doc["plugins"].([]any)
				for _, p := range ps {
					pm, _ := p.(map[string]any)
					if pm != nil && pm["name"] == from.Plugin {
						pm["name"] = to.Plugin
						pm["description"] = prefix(pm["description"], from, to)
					}
				}
			})
		case "plugin.json":
			b, err = editJSON(b, func(doc map[string]any) {
				doc["name"] = to.Plugin
				doc["description"] = prefix(doc["description"], from, to)
				uc, _ := doc["userConfig"].(map[string]any)
				if fe, _ := uc["fleet_env"].(map[string]any); fe != nil {
					opts := make([]any, len(to.AllowedEnvs))
					for i, e := range to.AllowedEnvs {
						opts[i] = e
					}
					fe["options"] = opts
					fe["default"] = to.DefaultEnv
				}
			})
		case ".mcp.json", ".codex-mcp.json":
			b, err = editJSON(b, func(doc map[string]any) {
				servers, _ := doc["mcpServers"].(map[string]any)
				srv, _ := servers[from.MCPServer].(map[string]any)
				if srv == nil {
					return
				}
				delete(servers, from.MCPServer)
				servers[to.MCPServer] = srv
				if srv["command"] == from.Binary {
					srv["command"] = to.Binary
				}
			})
		case "hooks.json":
			b, err = editJSON(b, func(doc map[string]any) {
				events, _ := doc["hooks"].(map[string]any)
				for _, groups := range events {
					gs, _ := groups.([]any)
					for _, g := range gs {
						gm, _ := g.(map[string]any)
						hs, _ := gm["hooks"].([]any)
						for _, h := range hs {
							hm, _ := h.(map[string]any)
							if c, _ := hm["command"].(string); strings.HasPrefix(c, from.Binary+" ") {
								hm["command"] = to.Binary + c[len(from.Binary):]
							}
						}
					}
				}
			})
		case "SKILL.md":
			b = bytes.Replace(b, []byte("\nname: "+from.Plugin+"\n"), []byte("\nname: "+to.Plugin+"\n"), 1)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", n, err)
		}
		if strings.HasSuffix(n, ".md") {
			b = bytes.ReplaceAll(b, []byte("`"+from.Binary+" "), []byte("`"+to.Binary+" "))
			b = bytes.ReplaceAll(b, []byte("`"+from.Binary+"`"), []byte("`"+to.Binary+"`"))
		}
		if strings.HasPrefix(n, skillDir) {
			n = "plugin/skills/" + to.Plugin + "/" + strings.TrimPrefix(n, skillDir)
		}
		out[n] = b
	}
	return out, nil
}

func prefix(v any, from, to variant.Identity) any {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, from.DisplayName) {
		return v
	}
	return to.DisplayName + strings.TrimPrefix(s, from.DisplayName)
}

func editJSON(b []byte, f func(map[string]any)) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	f(doc)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
