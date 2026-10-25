package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// OpenCode configuration: two entries in the user's global opencode.json
// and nothing else.
//
//   - plugin: one ["<adapter dir>", {"state_dir": "<relay state>"}] entry
//     that loads the Kenaz OpenCode adapter. OpenCode has no native OTLP
//     export the relay relies on; the adapter reports metadata-only
//     records to the relay itself and finds it through daemon.json in
//     state_dir.
//   - mcp.kenaz: the kenaz MCP server (context, skills, memory and
//     status tools), launched by absolute path. OpenCode has no plugin
//     manifest that could carry it, so setup registers it, once.
//
// Every other key, including other plugins, MCP servers, tool and agent
// settings and any telemetry collector, is preserved byte for byte in
// value. Setup never writes tool enable/disable lists or permission
// settings.
//
// OpenCode merges config.json, opencode.json and opencode.jsonc from its
// config directory. Setup edits opencode.json only, which must be plain
// JSON (comments are refused rather than lost).

// openCodeServerName is the MCP server name under "mcp".
var openCodeServerName = variant.Current.MCPServer

// openCodeAdapterPackage is the adapter's package name.
const openCodeAdapterPackage = "@kameas/kenaz-opencode"

// State keys for the two owned entries.
const (
	openCodeKeyMCP    = "mcp.kenaz"
	openCodeKeyPlugin = "plugin.kenaz"
)

// minOpenCode is the oldest OpenCode release whose plugin and config
// interfaces the adapter was checked against (events recorded from it).
var minOpenCode = [3]int{1, 19, 4}

// openCodeConfig is what setup renders into opencode.json.
type openCodeConfig struct {
	Bin        string // absolute path of the kenaz binary
	AdapterDir string // absolute path of the adapter package directory
	StateDir   string // relay state directory (daemon.json)
	Env        string // Fleet env passed to the MCP server; "" = default selection
}

func (c openCodeConfig) mcpEntry() (json.RawMessage, error) {
	o := &object{}
	o.set("type", json.RawMessage(`"local"`))
	cmd, err := json.Marshal([]string{c.Bin, "mcp"})
	if err != nil {
		return nil, err
	}
	o.set("command", cmd)
	if c.Env != "" {
		env, err := json.Marshal(map[string]string{variant.Current.EnvVar: c.Env})
		if err != nil {
			return nil, err
		}
		o.set("environment", env)
	}
	o.set("enabled", json.RawMessage("true"))
	return o.render("")
}

func (c openCodeConfig) pluginEntry() (json.RawMessage, error) {
	opts := &object{}
	sd, err := encodeString(c.StateDir)
	if err != nil {
		return nil, err
	}
	opts.set("state_dir", sd)
	o, err := opts.render("")
	if err != nil {
		return nil, err
	}
	spec, err := encodeString(c.AdapterDir)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("[")
	buf.Write(spec)
	buf.WriteString(", ")
	buf.Write(o)
	buf.WriteString("]")
	return buf.Bytes(), nil
}

// checkAdapter verifies dir holds the Kenaz OpenCode adapter.
func checkAdapter(dir string) error {
	if dir == "" {
		return errors.New("the Kenaz OpenCode adapter was not found next to the kenaz binary; pass --adapter <dir> (the opencode/ directory of the release archive)")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("adapter directory %q must be an absolute path", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return fmt.Errorf("adapter directory %s: %v", dir, err)
	}
	var pkg struct {
		Name string `json:"name"`
		Main string `json:"main"`
	}
	if json.Unmarshal(b, &pkg) != nil || pkg.Name != openCodeAdapterPackage {
		return fmt.Errorf("%s is not the Kenaz OpenCode adapter (package.json name must be %s)", dir, openCodeAdapterPackage)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(pkg.Main, "./")))); pkg.Main == "" || err != nil {
		return fmt.Errorf("adapter directory %s has no entry point", dir)
	}
	return nil
}

// pluginSpec returns the specifier of one "plugin" array element.
func pluginSpec(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var tuple []json.RawMessage
	if json.Unmarshal(raw, &tuple) == nil && len(tuple) > 0 {
		if json.Unmarshal(tuple[0], &s) == nil {
			return s
		}
	}
	return ""
}

func isNpmAdapter(spec string) bool {
	return spec == openCodeAdapterPackage || strings.HasPrefix(spec, openCodeAdapterPackage+"@")
}

// launchesKenazMCP reports whether an MCP entry runs `kenaz mcp`.
func launchesKenazMCP(raw json.RawMessage) bool {
	var e struct {
		Command []string `json:"command"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Command) < 2 || e.Command[len(e.Command)-1] != "mcp" {
		return false
	}
	base := strings.ToLower(filepath.Base(e.Command[0]))
	return base == variant.Current.Binary || base == variant.Current.Binary+".exe"
}

func renderArray(elems []json.RawMessage) json.RawMessage {
	if len(elems) == 0 {
		return json.RawMessage("[]")
	}
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = string(e)
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

func (fs *FileState) created(container string) bool {
	for _, c := range fs.Containers {
		if c == container {
			return true
		}
	}
	return false
}

func openCodeApply(cur []byte, c openCodeConfig, prev *FileState) ([]byte, *FileState, bool, error) {
	obj, err := parseObject(cur)
	if err != nil {
		return nil, nil, false, fmt.Errorf("opencode.json is not plain JSON (%v); setup does not edit files with comments. Nothing was changed", err)
	}
	fs := prev.clone()
	if fs == nil {
		fs = &FileState{Keys: map[string]KeyState{}}
	}
	changed := false

	// mcp.kenaz
	mcp := &object{}
	if raw, ok := obj.get("mcp"); ok {
		if mcp, err = parseObject(raw); err != nil {
			return nil, nil, false, errors.New(`"mcp" in opencode.json is not an object; nothing was changed`)
		}
	} else if !fs.created("mcp") {
		fs.Containers = append(fs.Containers, "mcp")
	}
	for _, m := range mcp.members {
		if m.Key != openCodeServerName && launchesKenazMCP(m.Val) {
			return nil, nil, false, fmt.Errorf("mcp.%s already runs the kenaz MCP server; remove it so the tools are not registered twice, then re-run setup", m.Key)
		}
	}
	wantMCP, err := c.mcpEntry()
	if err != nil {
		return nil, nil, false, err
	}
	curMCP, present := mcp.get(openCodeServerName)
	if present && !sameJSON(curMCP, wantMCP) {
		ks, owned := fs.Keys[openCodeKeyMCP]
		if !(owned && sameJSON(curMCP, json.RawMessage(ks.Written))) && !launchesKenazMCP(curMCP) {
			return nil, nil, false, fmt.Errorf("mcp.%s already exists and does not run kenaz; rename or remove it, then re-run setup", openCodeServerName)
		}
	}
	fs.record(openCodeKeyMCP, present, string(curMCP), string(compact(wantMCP)))
	if !present || !sameJSON(curMCP, wantMCP) {
		mcp.set(openCodeServerName, wantMCP)
		changed = true
	}

	// plugin entry
	var plugins []json.RawMessage
	if raw, ok := obj.get("plugin"); ok {
		if json.Unmarshal(raw, &plugins) != nil {
			return nil, nil, false, errors.New(`"plugin" in opencode.json is not an array; nothing was changed`)
		}
	} else if !fs.created("plugin") {
		fs.Containers = append(fs.Containers, "plugin")
	}
	wantPlugin, err := c.pluginEntry()
	if err != nil {
		return nil, nil, false, err
	}
	ours := map[string]bool{c.AdapterDir: true}
	if ks, ok := fs.Keys[openCodeKeyPlugin]; ok {
		ours[pluginSpec(json.RawMessage(ks.Written))] = true
	}
	idx := -1
	for i, p := range plugins {
		spec := pluginSpec(p)
		if isNpmAdapter(spec) {
			return nil, nil, false, fmt.Errorf("plugin %q already loads the Kenaz adapter from npm; remove it so events are not reported twice, then re-run setup", spec)
		}
		if ours[spec] {
			if idx >= 0 {
				return nil, nil, false, fmt.Errorf("the Kenaz adapter is listed more than once under \"plugin\"; remove the duplicates, then re-run setup")
			}
			idx = i
		}
	}
	prior := ""
	if idx >= 0 {
		prior = string(plugins[idx])
	}
	fs.record(openCodeKeyPlugin, idx >= 0, prior, string(compact(wantPlugin)))
	switch {
	case idx < 0:
		plugins = append(plugins, wantPlugin)
		changed = true
	case !sameJSON(plugins[idx], wantPlugin):
		plugins[idx] = wantPlugin
		changed = true
	}

	if !changed {
		return cur, prev, false, nil
	}
	mcpRaw, err := mcp.render("")
	if err != nil {
		return nil, nil, false, err
	}
	obj.set("mcp", mcpRaw)
	obj.set("plugin", renderArray(plugins))
	out, err := obj.render("")
	if err != nil {
		return nil, nil, false, err
	}
	return append(out, '\n'), fs, true, nil
}

func compact(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// openCodeUndo restores the owned entries that still hold what setup
// wrote, and warns about (and keeps) any the user changed since.
func openCodeUndo(cur []byte, fs *FileState) ([]byte, []string, error) {
	obj, err := parseObject(cur)
	if err != nil {
		return nil, nil, fmt.Errorf("opencode.json is not plain JSON (%v); nothing was changed", err)
	}
	var warnings []string
	changed := false

	if ks, owned := fs.Keys[openCodeKeyMCP]; owned {
		if raw, ok := obj.get("mcp"); ok {
			mcp, err := parseObject(raw)
			if err != nil {
				return nil, nil, errors.New(`"mcp" in opencode.json is not an object; nothing was changed`)
			}
			if v, ok := mcp.get(openCodeServerName); ok {
				switch {
				case !sameJSON(v, json.RawMessage(ks.Written)):
					warnings = append(warnings, fmt.Sprintf("mcp.%s changed since setup; left as is", openCodeServerName))
				case ks.PriorPresent:
					if !sameJSON(v, json.RawMessage(ks.Prior)) {
						mcp.set(openCodeServerName, json.RawMessage(ks.Prior))
						changed = true
					}
				default:
					mcp.del(openCodeServerName)
					changed = true
				}
			}
			if fs.created("mcp") && mcp.len() == 0 {
				obj.del("mcp")
				changed = true
			} else if changed {
				r, err := mcp.render("")
				if err != nil {
					return nil, nil, err
				}
				obj.set("mcp", r)
			}
		}
	}

	if ks, owned := fs.Keys[openCodeKeyPlugin]; owned {
		if raw, ok := obj.get("plugin"); ok {
			var plugins []json.RawMessage
			if json.Unmarshal(raw, &plugins) != nil {
				return nil, nil, errors.New(`"plugin" in opencode.json is not an array; nothing was changed`)
			}
			idx := -1
			for i, p := range plugins {
				if sameJSON(p, json.RawMessage(ks.Written)) {
					idx = i
					break
				}
			}
			pchanged := false
			switch {
			case idx < 0:
				warnings = append(warnings, "the Kenaz adapter entry under \"plugin\" changed since setup; left as is")
			case ks.PriorPresent:
				if !sameJSON(plugins[idx], json.RawMessage(ks.Prior)) {
					plugins[idx] = json.RawMessage(ks.Prior)
					pchanged = true
				}
			default:
				plugins = append(plugins[:idx], plugins[idx+1:]...)
				pchanged = true
			}
			switch {
			case fs.created("plugin") && len(plugins) == 0:
				obj.del("plugin")
				changed = true
			case pchanged:
				obj.set("plugin", renderArray(plugins))
				changed = true
			}
		}
	}

	if !changed {
		return cur, warnings, nil
	}
	out, err := obj.render("")
	if err != nil {
		return nil, nil, err
	}
	return append(out, '\n'), warnings, nil
}

var openCodeVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseVersion extracts the first x.y.z from s.
func parseVersion(s string) ([3]int, bool) {
	m := openCodeVersionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// defaultAdapterDir is <archive>/opencode for a binary at <archive>/bin/kenaz.
func defaultAdapterDir(bin string) string {
	if bin == "" {
		return ""
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(bin)), "opencode")
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return ""
	}
	return dir
}

// openCodeConfigDir is OpenCode's global config directory:
// $XDG_CONFIG_HOME/opencode, else ~/.config/opencode (on every OS).
func openCodeConfigDir(home string, getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "opencode")
	}
	return filepath.Join(home, ".config", "opencode")
}
