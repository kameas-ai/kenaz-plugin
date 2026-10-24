package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
)

// Self-contained archive installs (internal/bundle verifies the archive and
// renders the plugin copy). Setup in bundle mode registers the archive so
// every launch uses its executable by absolute path, with no PATH, npm or
// network dependency:
//
//   - Claude Code and Codex: the rendered per-user plugin and local
//     marketplace under the relay state directory are registered through
//     the host's own CLI (`plugin marketplace add`, `plugin install|add`).
//   - OpenCode: mcp.kenaz runs the archive executable and the plugin entry
//     points at the archive's opencode/ adapter.

// Bundle is a verified release archive.
type Bundle = bundle.Bundle

// LoadBundle and DetectBundle re-export internal/bundle.
var (
	LoadBundle   = bundle.LoadBundle
	DetectBundle = bundle.DetectBundle
)

// BundleFile is the bundle manifest's file name.
const BundleFile = bundle.BundleFile

// bundleMarketplace and bundlePluginID are the local marketplace and plugin
// the bundle registers in Claude Code and Codex.
const (
	bundleMarketplace = "kenaz"
	bundlePluginID    = "kenaz@kenaz"
)

func renderPlugin(b *Bundle, dir string, write bool) (string, error) {
	return bundle.Render(b, dir, write)
}

// renderDir is where the per-user rendered plugin for b lives.
func (o Options) renderDir(b *Bundle) string {
	return filepath.Join(o.StateDir, "setup", "bundle", b.ID())
}

// BundleReg records a host registration of a rendered bundle plugin.
type BundleReg struct {
	Root     string `json:"root"`
	Version  string `json:"version"`
	Rendered string `json:"rendered"`
	Digest   string `json:"digest"`
}

func hostCLI(h harness) string {
	if h == harnessClaude {
		return "claude"
	}
	return "codex"
}

func registerCmds(h harness, rendered string) [][]string {
	if h == harnessClaude {
		return [][]string{{"plugin", "marketplace", "add", rendered}, {"plugin", "install", bundlePluginID}}
	}
	return [][]string{{"plugin", "marketplace", "add", rendered}, {"plugin", "add", bundlePluginID}}
}

func unregisterCmds(h harness) [][]string {
	if h == harnessClaude {
		return [][]string{{"plugin", "uninstall", bundlePluginID}, {"plugin", "marketplace", "remove", bundleMarketplace}}
	}
	return [][]string{{"plugin", "remove", bundlePluginID}, {"plugin", "marketplace", "remove", bundleMarketplace}}
}

// registeredMarketplace returns the local path the host has registered for
// the "kenaz" marketplace ("" when none, "?" when it is not a local one),
// and whether kenaz@kenaz is enabled. It reads the host's own files only.
func (o Options) registeredMarketplace(h harness) (string, bool) {
	if h == harnessClaude {
		b, ok, _ := readFile(o.path(harnessClaude))
		if !ok {
			return "", false
		}
		var s struct {
			Extra map[string]struct {
				Source struct {
					Source string `json:"source"`
					Path   string `json:"path"`
				} `json:"source"`
			} `json:"extraKnownMarketplaces"`
			Enabled map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(b, &s) != nil {
			return "?", false
		}
		src := ""
		if m, ok := s.Extra[bundleMarketplace]; ok {
			src = "?"
			if m.Source.Source == "directory" {
				src = m.Source.Path
			}
		}
		if src == "" {
			// Marketplaces added with the CLI are also listed here.
			var known map[string]struct {
				Source struct {
					Source string `json:"source"`
					Path   string `json:"path"`
				} `json:"source"`
			}
			if kb, ok, _ := readFile(filepath.Join(o.ClaudeDir, "plugins", "known_marketplaces.json")); ok && json.Unmarshal(kb, &known) == nil {
				if m, ok := known[bundleMarketplace]; ok {
					src = "?"
					if m.Source.Source == "directory" {
						src = m.Source.Path
					}
				}
			}
		}
		return src, s.Enabled[bundlePluginID]
	}
	b, ok, _ := readFile(o.path(harnessCodex))
	if !ok {
		return "", false
	}
	return codexRegistration(string(b))
}

var (
	codexMarketplaceHdr = regexp.MustCompile(`^\s*\[\s*marketplaces\.("kenaz"|kenaz)\s*\]\s*(#.*)?$`)
	codexPluginHdr      = regexp.MustCompile(`^\s*\[\s*plugins\.("kenaz@kenaz"|'kenaz@kenaz')\s*\]\s*(#.*)?$`)
	codexStrKV          = regexp.MustCompile(`^\s*([A-Za-z_]+)\s*=\s*"((?:[^"\\]|\\.)*)"`)
	codexEnabledKV      = regexp.MustCompile(`^\s*enabled\s*=\s*true\b`)
)

func codexRegistration(text string) (string, bool) {
	section := ""
	src, typ, enabled := "", "", false
	sawMk := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			switch {
			case codexMarketplaceHdr.MatchString(t):
				section, sawMk = "mk", true
			case codexPluginHdr.MatchString(t):
				section = "pl"
			default:
				section = ""
			}
			continue
		}
		switch section {
		case "mk":
			if m := codexStrKV.FindStringSubmatch(t); m != nil {
				var v string
				if json.Unmarshal([]byte(`"`+m[2]+`"`), &v) != nil {
					v = m[2]
				}
				switch m[1] {
				case "source":
					src = v
				case "source_type":
					typ = v
				}
			}
		case "pl":
			if codexEnabledKV.MatchString(t) {
				enabled = true
			}
		}
	}
	if !sawMk {
		return "", enabled
	}
	if typ != "local" {
		return "?", enabled
	}
	return src, enabled
}

// hostRunner runs host CLIs with a timeout long enough for a plugin copy.
func hostRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return ExecRunner(ctx, name, args...)
}

func (o Options) run(h harness, args []string) error {
	run := o.HostRun
	if run == nil {
		run = hostRunner
	}
	out, err := run(context.Background(), hostCLI(h), args...)
	if err != nil {
		return fmt.Errorf("`%s %s` failed: %v: %s", hostCLI(h), strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// planRegistration checks the host's current kenaz marketplace and returns
// the CLI commands that make it point at rendered (none when it already
// does). A kenaz marketplace that setup did not register is refused, so
// the plugin is never registered twice.
func (o Options) planRegistration(h harness, b *Bundle, rendered, digest string, prev *BundleReg) ([][]string, error) {
	cur, enabled := o.registeredMarketplace(h)
	owned := prev != nil && cur == prev.Rendered
	switch {
	case cur == "":
		return registerCmds(h, rendered), nil
	case !owned && cur != rendered:
		where := cur
		if cur == "?" {
			where = "a non-local source"
		}
		return nil, fmt.Errorf("%s already has a %q plugin marketplace (%s) that setup did not register; remove it with `%s plugin marketplace remove %s`, then re-run setup",
			h.label(), bundleMarketplace, where, hostCLI(h), bundleMarketplace)
	case cur == rendered && enabled && prev != nil && prev.Digest == digest:
		return nil, nil
	}
	// Ours (or the same directory) but stale: re-register from scratch so
	// the host refreshes its plugin cache.
	return append(unregisterCmds(h), registerCmds(h, rendered)...), nil
}
