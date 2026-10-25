// Command fakehost stands in for the Claude Code and Codex CLIs in the
// variant tests. Installed under the name of the CLI it plays, it edits
// that host's config files the way the real plugin commands do, so setup
// can register, verify and undo without a real host or network. It reads
// the marketplace name from the registered directory, like the real CLIs,
// and so serves any build variant.
//
// Every invocation is appended to $FAKEHOST_LOG when it is set.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	if p := os.Getenv("FAKEHOST_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(f, "%s %s\n", name, strings.Join(args, " "))
			f.Close()
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("99.0.0")
		return
	}
	var err error
	switch name {
	case "claude":
		err = claude(args)
	case "codex":
		err = codex(args)
	default:
		err = fmt.Errorf("fakehost: unknown host %q", name)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func cmd(args []string) (string, string) {
	if len(args) < 2 {
		return strings.Join(args, " "), ""
	}
	return strings.Join(args[:len(args)-1], " "), args[len(args)-1]
}

// marketplaceName reads the name a local marketplace directory declares.
func marketplaceName(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return "", err
	}
	var m struct{ Name string }
	if err := json.Unmarshal(b, &m); err != nil || m.Name == "" {
		return "", fmt.Errorf("%s: no marketplace name", dir)
	}
	return m.Name, nil
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

func claude(args []string) error {
	p := filepath.Join(claudeDir(), "settings.json")
	doc := map[string]any{}
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
	}
	ensure := func(k string) map[string]any {
		m, _ := doc[k].(map[string]any)
		if m == nil {
			m = map[string]any{}
			doc[k] = m
		}
		return m
	}
	c, last := cmd(args)
	switch c {
	case "plugin list":
		if last != "--json" {
			return fmt.Errorf("unexpected claude %v", args)
		}
		var ids []string
		for id, on := range ensure("enabledPlugins") {
			if on == true {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		var out []map[string]any
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "enabled": true})
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		return nil
	case "plugin marketplace add":
		n, err := marketplaceName(last)
		if err != nil {
			return err
		}
		ensure("extraKnownMarketplaces")[n] = map[string]any{"source": map[string]any{"source": "directory", "path": last}}
	case "plugin install":
		ensure("enabledPlugins")[last] = true
	case "plugin uninstall":
		if _, ok := ensure("enabledPlugins")[last]; !ok {
			return fmt.Errorf("plugin %s is not installed", last)
		}
		delete(ensure("enabledPlugins"), last)
	case "plugin marketplace remove":
		delete(ensure("extraKnownMarketplaces"), last)
	default:
		return fmt.Errorf("unexpected claude %v", args)
	}
	for _, k := range []string{"extraKnownMarketplaces", "enabledPlugins"} {
		if m, _ := doc[k].(map[string]any); m != nil && len(m) == 0 {
			delete(doc, k)
		}
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, append(out, '\n'), 0o600)
}

func codexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// removeTable drops one TOML table, from its header line to the next
// header (or the end), exactly reversing appendTable.
func removeTable(s, header string) string {
	i := strings.Index("\n"+s, "\n"+header+"\n")
	if i < 0 {
		return s
	}
	j := strings.Index(s[i+len(header):], "\n[")
	if j < 0 {
		return s[:i]
	}
	return s[:i] + s[i+len(header)+j+1:]
}

// appendTable adds a table at the end of s.
func appendTable(s, header, body string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s + header + "\n" + body
}

func codex(args []string) error {
	p := filepath.Join(codexHome(), "config.toml")
	b, _ := os.ReadFile(p)
	s := string(b)
	c, last := cmd(args)
	switch c {
	case "plugin":
		if last != "list" {
			return fmt.Errorf("unexpected codex %v", args)
		}
		for _, m := range regexp.MustCompile(`(?m)^\[plugins\."([^"]+)"\]\nenabled = true$`).FindAllStringSubmatch(s, -1) {
			fmt.Println(m[1])
		}
		return nil
	case "plugin marketplace add":
		n, err := marketplaceName(last)
		if err != nil {
			return err
		}
		q, _ := json.Marshal(last)
		s = appendTable(s, "[marketplaces."+n+"]", "source_type = \"local\"\nsource = "+string(q)+"\n")
	case "plugin add":
		s = appendTable(s, "[plugins.\""+last+"\"]", "enabled = true\n")
	case "plugin remove":
		s = removeTable(s, "[plugins.\""+last+"\"]")
	case "plugin marketplace remove":
		s = removeTable(s, "[marketplaces."+last+"]")
	default:
		return fmt.Errorf("unexpected codex %v", args)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(s), 0o600)
}
