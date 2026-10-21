package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The public marketplace repository and plugin id.
const (
	marketplaceName = "kenaz"
	marketplaceRepo = "kameas-ai/kenaz-plugin"
	pluginID        = "kenaz@" + marketplaceName
)

// ManagedClaude returns a managed-settings.json for Claude Code that
// installs the plugin and pins the relay endpoint fleet-wide. The env block is the same one `setup claude` writes, with the
// content switches always off.
func ManagedClaude(port int) ([]byte, error) {
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
			"source":     map[string]string{"source": "github", "repo": marketplaceRepo},
			"autoUpdate": true,
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

const relayInstallNote = `Each user also needs the kenaz binary on their PATH (the plugin runs it
directly; install it with your package manager of choice or your MDM; see the
Kameas Fleet admin guide) and must sign in once with "kenaz login".
Admin configuration never signs anyone in or turns on telemetry classes for them:
nothing is sent until each member signs in and opts in.`

func printManaged(w io.Writer, h harness, port int, raw bool) int {
	if port < 1 || port > 65535 {
		fmt.Fprintln(w, "kenaz setup: --port must be 1-65535")
		return exitUsage
	}
	if h == harnessClaude {
		content, err := ManagedClaude(port)
		if err != nil {
			fmt.Fprintln(w, "kenaz setup:", err)
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
#
%s

----- managed-settings.json -----
%s`, port, port, commentLines(relayInstallNote), content)
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
# To install the plugin for everyone: in the ChatGPT admin console, import
# the GitHub marketplace %s and set kenaz to "Installed" for the
# roles that should have it. Plugin hooks still need each user to trust
# them once in Codex; telemetry works without them.
#
%s

----- managed_config.toml -----
%s`, marketplaceRepo, commentLines(relayInstallNote), content)
	return exitOK
}

func commentLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "# " + l
	}
	return strings.Join(lines, "\n")
}
