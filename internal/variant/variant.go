// Package variant is the build's identity: every name, environment
// variable, directory, keychain service, port and Fleet environment that
// identifies a build of kenaz. Current is selected at compile time by build
// tag, so a binary cannot change its identity at runtime.
//
// Builds differ only where they must: the executable and the host plugin
// names (Claude Code and Codex have one slot per plugin and marketplace
// name), the Fleet environments a build connects to, and the staff banner.
// Everything else is shared. Like the Kenaz harness, the plugin keeps its
// files per Fleet environment, not per build: ~/.kenaz/plugin/<env> holds
// the dev state for whichever build talks to dev (see Shared).
//
// No other package spells these values out; they read them from Current
// or Shared. scripts/checks.py (`make check-names`) enforces that.
package variant

import (
	"slices"
	"strings"
)

// Identity is one build variant's identity: only what differs between
// builds.
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
	// CaskToken is the Homebrew cask that installs the build.
	CaskToken string

	// DefaultEnv is the Fleet environment used when nothing selects one;
	// AllowedEnvs are the ones this build connects to.
	DefaultEnv  string
	AllowedEnvs []string

	// StaffOnly marks a build for Kameas AI staff: status and doctor say
	// so, and the SessionStart banner is prefixed with BannerTag and the
	// Fleet environment.
	StaffOnly bool
	BannerTag string
}

// SharedValues is what every build uses for the same Fleet environment.
// Two builds that talk to one environment read and write the same files,
// keychain items and relay (only one build at a time can run that relay:
// see the daemon package).
type SharedValues struct {
	// RootDir is the directory, in the user's home directory on every
	// platform, that all Kenaz products share (the harness uses
	// <RootDir>/harness/<env>); HomeVar overrides it. ProductDir is the
	// plugin's slice of it: <RootDir>/<ProductDir> holds config.toml and
	// one state directory per Fleet environment.
	RootDir    string
	ProductDir string
	// HomeVar overrides RootDir (the archive installer honors it too).
	HomeVar string

	// EnvVar selects the Fleet environment; EnvVars is the full list,
	// in precedence order, of variables that do.
	EnvVar  string
	EnvVars []string
	// MCPHostVar names the MCP host when it cannot be told apart.
	MCPHostVar string

	// KeychainService is the OS keychain service for every credential
	// (spool key, approvals key, sign-in tokens); the account is the
	// Fleet environment.
	KeychainService string
	// RelayPorts are the loopback ports a relay tries, in order (the
	// first free one wins; daemon.json records it); 0 is an ephemeral port.
	RelayPorts []int

	// ApprovalsCSRFCookie and EnrollCookiePrefix name the approval page's
	// cookies for prod. Cookies on localhost are not scoped by port, so
	// relays of different environments, which run side by side, use
	// ApprovalsCSRFCookieFor.
	ApprovalsCSRFCookie string
	EnrollCookiePrefix  string
}

// Shared is the identity every build shares.
var Shared = SharedValues{
	RootDir:    ".kenaz",
	ProductDir: "plugin",
	HomeVar:    "KENAZ_HOME",

	EnvVar: "KENAZ_ENV",
	// KENAZ_ENV is what the plugin's .mcp.json sets from the plugin's
	// fleet_env user option. Hook commands get no .mcp.json env block:
	// Claude Code exports plugin user options to every plugin subprocess
	// as CLAUDE_PLUGIN_OPTION_<KEY>, so hooks read the same fleet_env
	// through it. FLEET_PLUGIN_ENV is the original name.
	EnvVars:    []string{"KENAZ_ENV", "CLAUDE_PLUGIN_OPTION_FLEET_ENV", "FLEET_PLUGIN_ENV"},
	MCPHostVar: "KENAZ_MCP_HOST",

	KeychainService: "kenaz-plugin",
	RelayPorts:      []int{4318, 47318, 0},

	ApprovalsCSRFCookie: "kenaz_approvals_csrf",
	EnrollCookiePrefix:  "kenaz_enroll_",
}

// Ports returns a copy of RelayPorts.
func (s SharedValues) Ports() []int { return slices.Clone(s.RelayPorts) }

// ApprovalsCSRFCookieFor is the approval page's CSRF cookie for a relay of
// Fleet environment env: ApprovalsCSRFCookie for prod (unchanged from
// earlier releases), with the environment in it otherwise, so a dev and a
// prod relay's pages open in one browser never overwrite each other's.
func (s SharedValues) ApprovalsCSRFCookieFor(env string) string {
	if env == "" || env == "prod" {
		return s.ApprovalsCSRFCookie
	}
	return strings.Replace(s.ApprovalsCSRFCookie, "_", "_"+env+"_", 1)
}

// PluginID is the plugin's host id, plugin@marketplace.
func (i Identity) PluginID() string { return i.Plugin + "@" + i.Marketplace }

// Allowed reports whether env is one of AllowedEnvs.
func (i Identity) Allowed(env string) bool { return slices.Contains(i.AllowedEnvs, env) }

// EnvChoices lists AllowedEnvs joined by sep.
func (i Identity) EnvChoices(sep string) string { return strings.Join(i.AllowedEnvs, sep) }

// ApprovalsName is the approval page's name, shown by the authenticator.
func (i Identity) ApprovalsName() string { return i.DisplayName + " approvals" }

// For returns the variant (other than i) that connects to env, if any.
func (i Identity) For(env string) (Identity, bool) {
	for _, v := range Variants {
		if v.Name != i.Name && v.Allowed(env) {
			return v, true
		}
	}
	return Identity{}, false
}

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
	CaskToken:       "kenaz",

	DefaultEnv:  "prod",
	AllowedEnvs: []string{"prod", "stage", "dev"},
}

// KenazDev is the staff-only dev build (build tag kenaz_dev). It installs
// side by side with kenaz under its own executable and host plugin names,
// connects only to the dev and stage Fleet environments, and is never
// published to a public marketplace. It shares everything else with kenaz
// (Shared): its dev state is the same ~/.kenaz/plugin/dev a kenaz --env dev
// uses.
var KenazDev = Identity{
	Name:        "kenaz-dev",
	Binary:      "kenaz-dev",
	DisplayName: "Kenaz (dev)",

	BundleName:  "kenaz-dev",
	Marketplace: "kenaz-dev",
	Plugin:      "kenaz-dev",
	MCPServer:   "kenaz-dev",
	CaskToken:   "kenaz-dev",

	DefaultEnv:  "dev",
	AllowedEnvs: []string{"dev", "stage"},

	StaffOnly: true,
	BannerTag: "kenaz-dev",
}

// Variants lists every build variant.
var Variants = []Identity{Kenaz, KenazDev}

// Named returns the variant called name ("" is kenaz: files written before
// builds recorded their variant were all written by kenaz).
func Named(name string) (Identity, bool) {
	if name == "" {
		name = Kenaz.Name
	}
	for _, v := range Variants {
		if v.Name == name {
			return v, true
		}
	}
	return Identity{}, false
}
