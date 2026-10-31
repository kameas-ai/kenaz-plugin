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
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
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
var (
	bundleMarketplace = variant.Current.Marketplace
	bundlePluginID    = variant.Current.PluginID()
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
	// Launcher is the executable the rendered plugin launches (Bundle.Launch:
	// a version-independent path where there is one). Empty in state
	// written before 0.6.0, whose plugin launched <Root>/bin/kenaz.
	Launcher string `json:"launcher,omitempty"`
	// Replaced is the GitHub marketplace source an explicit --bundle
	// replaced, so --undo can say how to add it back.
	Replaced string `json:"replaced,omitempty"`

	// formerRendered is Rendered before the state directory moved
	// (Windows state migration); the host may still point there.
	formerRendered string
}

func hostCLI(h harness) string {
	if h == harnessClaude {
		return "claude"
	}
	return "codex"
}

// registered is the rendered plugin path the host was given: Rendered as
// recorded, before loadState rebased it onto a moved state directory.
func (r *BundleReg) registered() string {
	if r.formerRendered != "" {
		return r.formerRendered
	}
	return r.Rendered
}

func registerCmds(h harness, rendered string) [][]string {
	if h == harnessClaude {
		return [][]string{{"plugin", "marketplace", "add", rendered}, {"plugin", "install", bundlePluginID}}
	}
	return [][]string{{"plugin", "marketplace", "add", rendered}, {"plugin", "add", bundlePluginID}}
}

// installCmd installs kenaz@kenaz from an already registered marketplace.
func installCmd(h harness) []string {
	return registerCmds(h, "")[1]
}

// uninstallCmd removes kenaz@kenaz and keeps the marketplace.
func uninstallCmd(h harness) []string {
	return unregisterCmds(h)[0]
}

func unregisterCmds(h harness) [][]string {
	if h == harnessClaude {
		return [][]string{{"plugin", "uninstall", bundlePluginID}, {"plugin", "marketplace", "remove", bundleMarketplace}}
	}
	return [][]string{{"plugin", "remove", bundlePluginID}, {"plugin", "marketplace", "remove", bundleMarketplace}}
}

// marketplace is the host's current "kenaz" marketplace registration, read
// from the host's own files only.
type marketplace struct {
	Present bool
	// Path is the local directory it points at ("" for a remote one).
	Path string
	// Remote is a git or GitHub source ("owner/repo" or a clone URL), ""
	// for a local one.
	Remote string
	// Enabled is whether kenaz@kenaz is enabled.
	Enabled bool
	// Installed is whether kenaz@kenaz is installed at all, enabled or
	// not (a user who disabled it still has it installed).
	Installed bool
	// Ref is the branch, tag or commit a remote source is pinned to (""
	// when it follows the default branch). Pin is the full commit it is
	// pinned to, "" when Ref is not a commit (a branch or tag can move).
	Ref, Pin string
	// Off is whether kenaz@kenaz is explicitly turned off (false in
	// enabledPlugins, enabled = false in Codex). Not listed is not off: a
	// managed enabledPlugins setup cannot read may enable it.
	Off bool
	// Inline is a Claude Code "settings" source: a catalog declared in a
	// settings file (typically managed settings), which setup never
	// replaces or duplicates.
	Inline bool
}

// official reports whether m is the public GitHub marketplace,
// github.com/kameas-ai/kenaz-plugin (any ref).
func (m marketplace) official() bool { return m.Present && isOfficialSource(m.Remote) }

// source renders m the way registeredMarketplace always has: "" when
// absent, the local path, or "?" for a remote source.
func (m marketplace) source() string {
	switch {
	case !m.Present:
		return ""
	case m.Path != "":
		return m.Path
	}
	return "?"
}

// OfficialRepo is the public GitHub repository that hosts the build's
// marketplace ("" when the build is never published there).
var OfficialRepo = variant.Current.MarketplaceRepo

var officialURL = regexp.MustCompile(`(?i)^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)` + regexp.QuoteMeta(OfficialRepo) + `(?:\.git)?/?(?:[#@].*)?$`)

func isOfficialSource(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" || OfficialRepo == "" {
		return false
	}
	if officialURL.MatchString(src) {
		return true
	}
	repo, _, _ := strings.Cut(src, "#")
	repo, _, _ = strings.Cut(repo, "@")
	return strings.EqualFold(repo, OfficialRepo)
}

// claudeSource is a Claude Code marketplace source object. Plugins is the
// inline catalog of a "settings" source (what print-managed deploys).
type claudeSource struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	Repo    string `json:"repo"`
	URL     string `json:"url"`
	Ref     string `json:"ref"`
	Plugins []struct {
		Name   string          `json:"name"`
		Source json.RawMessage `json:"source"`
	} `json:"plugins"`
}

// claudePluginSource is a git-based plugin source (github, url,
// git-subdir) with its ref and sha.
type claudePluginSource struct {
	Source string `json:"source"`
	Repo   string `json:"repo"`
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
}

func (s claudeSource) marketplace() marketplace {
	m := marketplace{Present: true}
	switch s.Source {
	case "directory":
		m.Path = s.Path
	case "github":
		// ref is a branch or tag name, never a commit pin, even when it
		// looks like one.
		m.Remote, m.Ref = s.Repo, s.Ref
	case "settings":
		// An inline catalog (managed settings, or a user's settings.json):
		// Claude Code allows one "kenaz" marketplace, and setup never adds
		// a second or removes this one. It is the GitHub plugin when its
		// kenaz entry fetches the public repository, and pinned only by
		// that entry's sha (Claude Code checks out sha when it is set).
		// known_marketplaces.json may record the source without its
		// plugins; then it is inline but not recognized as official.
		m.Inline = true
		m.Remote = "settings"
		for _, p := range s.Plugins {
			var ps claudePluginSource
			if p.Name != variant.Current.Plugin || json.Unmarshal(p.Source, &ps) != nil {
				continue
			}
			m.Remote = ps.Repo
			if m.Remote == "" {
				m.Remote = ps.URL
			}
			m.Ref = ps.Ref
			if isCommit(ps.SHA) {
				m.Ref, m.Pin = ps.SHA, strings.ToLower(ps.SHA)
			}
		}
	default:
		m.Remote, m.Ref = s.URL, s.Ref
		if m.Remote == "" {
			m.Remote = s.Source
		}
	}
	return m
}

// registeredMarketplace returns the local path the host has registered for
// the "kenaz" marketplace ("" when none, "?" when it is not a local one),
// and whether kenaz@kenaz is enabled. It reads the host's own files only.
func (o Options) registeredMarketplace(h harness) (string, bool) {
	m := o.kenazMarketplace(h)
	return m.source(), m.Enabled
}

// kenazMarketplace reads the host's "kenaz" marketplace registration.
func (o Options) kenazMarketplace(h harness) marketplace {
	if h == harnessClaude {
		b, ok, _ := readFile(o.path(harnessClaude))
		var s struct {
			Extra map[string]struct {
				Source claudeSource `json:"source"`
			} `json:"extraKnownMarketplaces"`
			Enabled map[string]json.RawMessage `json:"enabledPlugins"`
		}
		if ok && json.Unmarshal(b, &s) != nil {
			return marketplace{Present: true}
		}
		var m marketplace
		if e, ok := s.Extra[bundleMarketplace]; ok {
			m = e.Source.marketplace()
		} else {
			// Marketplaces added with the CLI are also listed here.
			var known map[string]struct {
				Source claudeSource `json:"source"`
			}
			if kb, ok, _ := readFile(filepath.Join(o.ClaudeDir, "plugins", "known_marketplaces.json")); ok && json.Unmarshal(kb, &known) == nil {
				if e, ok := known[bundleMarketplace]; ok {
					m = e.Source.marketplace()
				}
			}
		}
		raw, listed := s.Enabled[bundlePluginID]
		m.Enabled = listed && string(raw) == "true"
		m.Off = listed && string(raw) == "false"
		m.Installed = listed || o.claudeInstalled()
		return m
	}
	b, ok, _ := readFile(o.path(harnessCodex))
	if !ok {
		return marketplace{}
	}
	return codexMarketplace(string(b))
}

// claudeInstalled reports whether Claude Code lists kenaz@kenaz in
// plugins/installed_plugins.json (installed, whether or not enabled).
func (o Options) claudeInstalled() bool {
	b, ok, _ := readFile(filepath.Join(o.ClaudeDir, "plugins", "installed_plugins.json"))
	if !ok {
		return false
	}
	var f struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(b, &f) != nil {
		return false
	}
	raw, ok := f.Plugins[bundlePluginID]
	return ok && string(raw) != "null" && string(raw) != "[]"
}

func codexRegistration(text string) (string, bool) {
	m := codexMarketplace(text)
	return m.source(), m.Enabled
}

var (
	codexMarketplaceHdr = regexp.MustCompile(`^\s*\[\s*marketplaces\.("` + regexp.QuoteMeta(bundleMarketplace) + `"|` + regexp.QuoteMeta(bundleMarketplace) + `)\s*\]\s*(#.*)?$`)
	codexPluginHdr      = regexp.MustCompile(`^\s*\[\s*plugins\.("` + regexp.QuoteMeta(bundlePluginID) + `"|'` + regexp.QuoteMeta(bundlePluginID) + `')\s*\]\s*(#.*)?$`)
	codexStrKV          = regexp.MustCompile(`^\s*([A-Za-z_]+)\s*=\s*"((?:[^"\\]|\\.)*)"`)
	codexEnabledKV      = regexp.MustCompile(`^\s*enabled\s*=\s*true\b`)
)

func codexMarketplace(text string) marketplace {
	section := ""
	src, typ, ref, enabled := "", "", "", false
	sawMk, sawPl := false, false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			switch {
			case codexMarketplaceHdr.MatchString(t):
				section, sawMk = "mk", true
			case codexPluginHdr.MatchString(t):
				section, sawPl = "pl", true
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
				case "ref":
					ref = v
				}
			}
		case "pl":
			if codexEnabledKV.MatchString(t) {
				enabled = true
			}
		}
	}
	// Codex keeps an installed plugin's [plugins."id"] table with
	// enabled = false when it is turned off.
	if !sawMk {
		return marketplace{Enabled: enabled, Installed: sawPl, Off: sawPl && !enabled}
	}
	if typ != "local" {
		m := marketplace{Present: true, Remote: src, Ref: ref, Enabled: enabled, Installed: sawPl, Off: sawPl && !enabled}
		if isCommit(ref) {
			// A claim only: releasePin checks what Codex checked out.
			m.Pin = strings.ToLower(ref)
		}
		return m
	}
	return marketplace{Present: true, Path: src, Enabled: enabled, Installed: sawPl, Off: sawPl && !enabled}
}

// hostRunner runs host CLIs with a timeout long enough for a plugin copy.
func hostRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return ExecRunner(ctx, name, args...)
}

func (o Options) run(h harness, args []string) error {
	run := o.HostRun
	name := hostCLI(h)
	if run == nil {
		run = hostRunner
		if p := o.HostBins[name]; p != "" {
			// `claude` or `codex` is not on PATH, but an install of it was
			// found (the native installer's directory, or the copy the
			// desktop app ships).
			name = p
		}
	}
	out, err := run(context.Background(), name, args...)
	if err != nil {
		return fmt.Errorf("`%s %s` failed: %v: %s", hostCLI(h), strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// planRegistration checks the host's current kenaz marketplace and returns
// the CLI commands that make it point at rendered (none when it already
// does). The public GitHub marketplace is replaced (setup only gets here
// for it with an explicit --bundle); any other kenaz marketplace that setup
// did not register is refused, so the plugin is never registered twice.
func (o Options) planRegistration(h harness, mk marketplace, rendered, digest string, prev *BundleReg) ([][]string, error) {
	cur, enabled := mk.source(), mk.Enabled
	owned := prev != nil && (cur == prev.Rendered || (prev.formerRendered != "" && cur == prev.formerRendered))
	switch {
	case cur == "":
		return registerCmds(h, rendered), nil
	case mk.official():
		return append(unregisterCmds(h), registerCmds(h, rendered)...), nil
	case !owned && cur != rendered:
		where := cur
		if cur == "?" {
			where = "a non-local source"
			if mk.Remote != "" {
				where = mk.Remote
			}
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
