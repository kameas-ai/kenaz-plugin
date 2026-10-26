package variant_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/pathenv"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// TestGoldenIdentity pins the identity values packages expose, computed by
// those packages for every platform rather than read from one table.
func TestGoldenIdentity(t *testing.T) {
	var b strings.Builder
	p := func(k string, v any) { fmt.Fprintf(&b, "%s: %v\n", k, v) }
	p("variant", variant.Current.Name)
	p("keychain service", spool.KeychainService)
	p("relay ports", daemon.DefaultPorts)
	p("env vars", config.EnvVars)
	p("mcp server", mcp.ServerName)
	p("mcp host env var", mcp.HostEnvVar)
	p("official repo", setup.OfficialRepo)
	for _, env := range daemonstate.Environments {
		p("approvals csrf cookie "+env, variant.Shared.ApprovalsCSRFCookieFor(env))
	}

	none := func(string) string { return "" }
	for _, goos := range []string{"darwin", "linux", "windows"} {
		r := daemonstate.Resolver{Home: "/h", GOOS: goos, Getenv: func(k string) string {
			if k == "LocalAppData" {
				return `C:\L`
			}
			return ""
		}}
		for _, env := range daemonstate.Environments {
			p("relay state "+goos+" "+env, filepath.ToSlash(r.StateDir(env)))
		}
		p("fish profile "+goos, filepath.ToSlash(pathenv.ProfileFor(pathenv.Fish, goos, "/h", none, nil)))
	}
	for _, kv := range [][2]string{{"", ""}, {variant.Shared.EnvVar, "dev"}, {variant.Shared.EnvVar, "stage"}, {variant.Shared.EnvVar, "prod"}, {variant.Shared.EnvVar, "bogus"}} {
		r := daemonstate.Resolver{Home: "/h", GOOS: "linux", Getenv: func(k string) string {
			if k == kv[0] {
				return kv[1]
			}
			return ""
		}}
		env, err := r.Env()
		if err != nil {
			env = "error: " + err.Error()
		}
		p("relay env "+kv[0]+"="+kv[1], env)
	}
	blk, _ := pathenv.Block(pathenv.Zsh, "/d")
	p("path block", "\n"+blk)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(variant.Shared.HomeVar, "")
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	rel := func(s string) string { r, _ := filepath.Rel(home, s); return filepath.ToSlash(r) }
	p("config dir", rel(paths.ConfigDir))
	p("state root", rel(paths.StateRoot))
	p("consent store", rel(filepath.Join(paths.State(fleetapi.Env(variant.Current.DefaultEnv)), "consent.json")))
	t.Setenv(variant.Shared.HomeVar, filepath.Join(home, "kh"))
	paths, _ = config.DefaultPaths()
	p(variant.Shared.HomeVar+" config dir", rel(paths.ConfigDir))
	p(variant.Shared.HomeVar+" state root", rel(paths.StateRoot))

	for _, c := range []struct{ flag, envVar, val, file string }{
		{}, {"dev", "", "", ""}, {"prod", "", "", ""}, {"", variant.Shared.EnvVar, "stage", ""}, {"", variant.Shared.EnvVar, "prod", ""},
		{"", "CLAUDE_PLUGIN_OPTION_FLEET_ENV", "dev", ""}, {"", "FLEET_PLUGIN_ENV", "dev", ""},
		{"", "", "", "stage"}, {"", "", "", "prod"}, {"bogus", "", "", ""},
	} {
		f := config.File{Values: map[string]string{}}
		if c.file != "" {
			f.Values["env"] = c.file
		}
		e, src, err := config.ResolveEnv(c.flag, func(k string) string {
			if k == c.envVar {
				return c.val
			}
			return ""
		}, f)
		p(fmt.Sprintf("resolve flag=%q %s=%q file=%q", c.flag, c.envVar, c.val, c.file), fmt.Sprintf("%s %s %v", e, src, err))
	}
	golden(t, "identity", b.String())
}
