// Package variant is the build's identity: every name, directory,
// keychain service, port, environment variable and Fleet environment that
// tells one build of kenaz apart from another. Current is selected at
// compile time by build tag, so a binary cannot change its identity at
// runtime.
//
// No other package spells these values out; they read them from Current.
// scripts/checks.py (`make check-names`) enforces that.
package variant

import (
	"slices"
	"strings"
)

// Identity is one build variant's identity.
type Identity struct {
	// Name identifies the variant.
	Name string
	// Binary is the executable's name, without ".exe". Hook commands in
	// the plugin start with it.
	Binary string
	// DisplayName is the product name people see.
	DisplayName string

	// BundleName is the name a release archive's bundle.json carries.
	BundleName string
	// Marketplace and Plugin name the Claude Code and Codex plugin
	// marketplace and the plugin in it (see PluginID).
	Marketplace string
	Plugin      string
	// MarketplaceRepo is the public GitHub repository that hosts the
	// marketplace, or "" when the variant is never published there.
	MarketplaceRepo string
	// MCPServer is the plugin's MCP server key and the MCP implementation
	// name the hosts show.
	MCPServer string

	// ConfigDir is the directory under kameas/ (in the platform's config
	// and state locations) that holds config.toml and the state root.
	ConfigDir string
	// ConsentDir is the consent store's directory directly under the user
	// config directory, or "" to keep it in the config root.
	ConsentDir string
	// KeychainService is the OS keychain service for every credential
	// the build stores (spool key, approvals key, sign-in tokens).
	KeychainService string
	// RelayPorts are the loopback ports the relay tries, in order; 0 is
	// an ephemeral port.
	RelayPorts []int
	// CaskToken is the Homebrew cask that installs the build.
	CaskToken string

	// EnvVar selects the Fleet environment; EnvVars is the full list,
	// in precedence order, of variables that do.
	EnvVar  string
	EnvVars []string
	// HomeVar overrides the config and state roots; StateDirVar the
	// consent store's directory; MCPHostVar names the MCP host.
	HomeVar     string
	StateDirVar string
	MCPHostVar  string

	// DefaultEnv is the Fleet environment used when nothing selects one;
	// AllowedEnvs are the ones this build connects to.
	DefaultEnv  string
	AllowedEnvs []string

	// ApprovalsCSRFCookie and EnrollCookiePrefix name the approval page's
	// cookies. Cookies on localhost are not scoped by port, so variants
	// that run side by side need their own.
	ApprovalsCSRFCookie string
	EnrollCookiePrefix  string
}

// PluginID is the plugin's host id, plugin@marketplace.
func (i Identity) PluginID() string { return i.Plugin + "@" + i.Marketplace }

// Allowed reports whether env is one of AllowedEnvs.
func (i Identity) Allowed(env string) bool { return slices.Contains(i.AllowedEnvs, env) }

// EnvChoices lists AllowedEnvs joined by sep.
func (i Identity) EnvChoices(sep string) string { return strings.Join(i.AllowedEnvs, sep) }

// ApprovalsName is the approval page's name, shown by the authenticator.
func (i Identity) ApprovalsName() string { return i.DisplayName + " approvals" }

// Ports returns a copy of RelayPorts.
func (i Identity) Ports() []int { return slices.Clone(i.RelayPorts) }

// Kenaz is the public build.
var Kenaz = Identity{
	Name:        "kenaz",
	Binary:      "kenaz",
	DisplayName: "Kenaz",

	BundleName:      "kenaz",
	Marketplace:     "kenaz",
	Plugin:          "kenaz",
	MarketplaceRepo: "kameas-ai/kenaz-plugin",
	MCPServer:       "kenaz",

	ConfigDir: "kenaz",
	// Not "kenaz": that directory belongs to the Kenaz desktop app.
	ConsentDir:      "kenaz-plugin",
	KeychainService: "kenaz-plugin",
	RelayPorts:      []int{4318, 47318, 0},
	CaskToken:       "kenaz",

	EnvVar: "KENAZ_ENV",
	// KENAZ_ENV is what the plugin's .mcp.json sets from the plugin's
	// fleet_env user option. Hook commands get no .mcp.json env block:
	// Claude Code exports plugin user options to every plugin subprocess
	// as CLAUDE_PLUGIN_OPTION_<KEY>, so hooks read the same fleet_env
	// through it. FLEET_PLUGIN_ENV is the original name.
	EnvVars:     []string{"KENAZ_ENV", "CLAUDE_PLUGIN_OPTION_FLEET_ENV", "FLEET_PLUGIN_ENV"},
	HomeVar:     "KENAZ_HOME",
	StateDirVar: "KENAZ_STATE_DIR",
	MCPHostVar:  "KENAZ_MCP_HOST",

	DefaultEnv:  "prod",
	AllowedEnvs: []string{"prod", "stage", "dev"},

	ApprovalsCSRFCookie: "kenaz_approvals_csrf",
	EnrollCookiePrefix:  "kenaz_enroll_",
}
