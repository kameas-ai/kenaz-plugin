package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	kenaz "github.com/kameas-ai/kenaz-plugin"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// The public marketplace repository and plugin id.
var (
	marketplaceName = variant.Current.Marketplace
	marketplaceRepo = variant.Current.MarketplaceRepo
	pluginID        = variant.Current.PluginID()
)

// managedRelease is the release print-managed pins to: this build's own,
// or --release/--commit (both, or neither).
func managedRelease(self Release, tag, commit string) (Release, error) {
	if tag == "" && commit == "" {
		if !self.Pinnable() {
			return Release{}, errors.New("print-managed pins the plugin to a signed release, and this build has no release commit (it is not an official release); pass --release vX.Y.Z --commit <full commit of that tag>")
		}
		return self, nil
	}
	r := Release{Version: strings.TrimPrefix(tag, "v"), Commit: strings.ToLower(commit)}
	if tag == "" || commit == "" || !r.Pinnable() {
		return Release{}, errors.New("--release and --commit go together: a release tag (vX.Y.Z) and the full 40-character commit it points at")
	}
	return r, nil
}

// managedPluginSource is the plugin source the managed Claude Code catalog
// installs: the public repository's plugin/ directory at the release tag,
// checked out at its commit (Claude Code checks out sha when both are set).
func managedPluginSource(r Release) map[string]string {
	return map[string]string{
		"source": "git-subdir",
		"url":    "https://github.com/" + marketplaceRepo + ".git",
		"path":   "plugin",
		"ref":    r.Tag(),
		"sha":    r.Commit,
	}
}

// ManagedClaude returns a managed-settings.json for Claude Code that
// installs the plugin pinned to release r and pins the relay endpoint
// fleet-wide. The env block is the same one `setup claude` writes, with the
// content switches always off.
//
// The marketplace is an inline ("settings") catalog rather than the GitHub
// marketplace: a github or git marketplace source takes a branch or tag but
// no commit, while a plugin source takes both ref and sha. autoUpdate is
// false and set, so users cannot turn it on; a new release is a new snippet.
func ManagedClaude(port int, r Release) ([]byte, error) {
	env := &object{}
	add := func(k, v string) {
		b, _ := encodeString(v)
		env.set(k, b)
	}
	for _, e := range claudeBaseEnv(port) {
		add(e.k, e.v)
	}
	for _, k := range claudeContentFlags {
		add(k, "0")
	}
	envRaw, err := env.render("")
	if err != nil {
		return nil, err
	}
	mk, _ := json.Marshal(map[string]any{
		marketplaceName: map[string]any{
			"source": map[string]any{
				"source": "settings",
				"name":   marketplaceName,
				"plugins": []map[string]any{{
					"name":        variant.Current.Plugin,
					"description": kenaz.MarketplaceDescription,
					"source":      managedPluginSource(r),
				}},
			},
			"autoUpdate": false,
		},
	})
	en, _ := json.Marshal(map[string]bool{pluginID: true})
	root := &object{}
	root.set("extraKnownMarketplaces", mk)
	root.set("enabledPlugins", en)
	root.set("env", envRaw)
	out, err := root.render("")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ManagedCodex returns the [otel] block for Codex's managed_config.toml,
// which overrides user config.
func ManagedCodex(port int) string {
	var b strings.Builder
	b.WriteString("[otel]\n")
	for _, e := range codexValues(port) {
		fmt.Fprintf(&b, "%s = %s\n", e.k, e.v)
	}
	return b.String()
}

// ManagedCodexMarketplace returns the [marketplaces] block that registers
// the public marketplace pinned to release r. Codex clones the repository
// and runs `git checkout <ref>`, so a full commit pins it.
func ManagedCodexMarketplace(r Release) string {
	return fmt.Sprintf("[marketplaces.%s]\nsource_type = \"git\"\nsource = \"https://github.com/%s.git\"\nref = \"%s\"\n", marketplaceName, marketplaceRepo, r.Commit)
}

const relayInstallNote = `Each user also needs the kenaz binary on their PATH (the plugin runs it
directly; install it with your package manager of choice or your MDM; see the
Kameas Fleet admin guide) and must sign in once with "kenaz login".
Admin configuration never signs anyone in or turns on telemetry classes for them:
nothing is sent until each member signs in and opts in.`

func printManaged(w io.Writer, h harness, port int, raw bool, r Release) int {
	if port < 1 || port > 65535 {
		fmt.Fprintln(w, variant.Current.Binary+" setup: --port must be 1-65535")
		return exitUsage
	}
	if h == harnessClaude {
		content, err := ManagedClaude(port, r)
		if err != nil {
			fmt.Fprintln(w, variant.Current.Binary+" setup:", err)
			return exitError
		}
		if raw {
			w.Write(content)
			return exitOK
		}
		fmt.Fprintf(w, `# Claude Code managed settings for Kenaz for Claude Code and Codex
#
# Deploy as managed-settings.json:
#   macOS: /Library/Application Support/ClaudeCode/managed-settings.json
#   Linux: /etc/claude-code/managed-settings.json
#   or paste into server-managed settings in the Claude admin console.
# Merge with any managed settings you already deploy. This installs the
# kenaz plugin for every user (they cannot disable it) and routes
# telemetry to the local relay on 127.0.0.1:%d.
#
# Notes:
# - Locking the OTLP destination against user overrides needs Claude Code
#   2.1.217 or later.
# - Claude Desktop sessions take environment variables from the app's
#   launch environment, which wins over settings "env".
# - If port %d is taken on some machines, the relay falls back to another
#   port and "kenaz doctor" reports the mismatch.
# - The plugin is pinned to release %s, commit %s,
#   with autoUpdate off. Deploy the snippet of each new release to upgrade;
#   do not register the GitHub marketplace without a pin, which follows the
#   repository's default branch.
#
%s

----- managed-settings.json -----
%s`, port, port, r.Tag(), r.Commit, commentLines(relayInstallNote), content)
		return exitOK
	}

	content := ManagedCodex(port)
	if raw {
		io.WriteString(w, content)
		return exitOK
	}
	fmt.Fprintf(w, `# Codex managed configuration for Kenaz for Claude Code and Codex
#
# Deploy the block below in managed_config.toml, which overrides each
# user's own [otel] settings:
#   macOS/Linux file: /etc/codex/managed_config.toml
#   macOS MDM: preference domain com.openai.codex, key config_toml_base64
#              (base64 of the block)
# Merge with any [otel] settings you already manage.
#
# To install the plugin for everyone, register the marketplace pinned to
# release %s (commit %s) in the system config.toml
# (/etc/codex/config.toml) or cloud-managed configuration, with the block
# after the managed_config.toml one, and enable %s for your users.
# Codex checks out "ref" after cloning. That is not tamper-proof: Codex
# runs "git checkout <ref>" without verifying the result, and git prefers a
# branch named like the commit, so whoever controls the repository could
# serve other content. Run "kenaz setup doctor" on a sample of machines: it
# checks that Codex's checkout is the release commit. Do not use an
# unpinned import of %s: it follows the repository's default
# branch. Plugin hooks still need each user to trust them once in Codex;
# telemetry works without them.
#
%s

----- managed_config.toml -----
%s
----- config.toml (marketplace, pinned) -----
%s`, r.Tag(), r.Commit, pluginID, marketplaceRepo, commentLines(relayInstallNote), content, ManagedCodexMarketplace(r))
	return exitOK
}

func commentLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "# " + l
	}
	return strings.Join(lines, "\n")
}
