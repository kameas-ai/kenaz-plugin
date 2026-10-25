package variant_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/pathenv"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// TestGoldenIdentity pins the identity values packages expose, computed by
// those packages for every platform rather than read from one table.
func TestGoldenIdentity(t *testing.T) {
	var b strings.Builder
	p := func(k string, v any) { fmt.Fprintf(&b, "%s: %v\n", k, v) }
	p("keychain service", spool.KeychainService)
	p("relay ports", daemon.DefaultPorts)
	p("env vars", config.EnvVars)
	p("mcp server", mcp.ServerName)
	p("mcp host env var", mcp.HostEnvVar)
	p("official repo", setup.OfficialRepo)

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
	for _, kv := range [][2]string{{"", ""}, {"KENAZ_ENV", "dev"}, {"KENAZ_ENV", "stage"}, {"KENAZ_ENV", "bogus"}} {
		r := daemonstate.Resolver{Home: "/h", GOOS: "linux", Getenv: func(k string) string {
			if k == kv[0] {
				return kv[1]
			}
			return ""
		}}
		p("relay env "+kv[0]+"="+kv[1], r.Env())
	}
	blk, _ := pathenv.Block(pathenv.Zsh, "/d")
	p("path block", "\n"+blk)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("KENAZ_HOME", "")
	t.Setenv("KENAZ_STATE_DIR", "")
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	rel := func(s string) string { r, _ := filepath.Rel(home, s); return filepath.ToSlash(r) }
	p("config dir", rel(paths.ConfigDir))
	p("state root", rel(paths.StateRoot))
	cs, err := consent.DefaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	p("consent store", rel(cs))
	t.Setenv("KENAZ_HOME", filepath.Join(home, "kh"))
	paths, _ = config.DefaultPaths()
	p("KENAZ_HOME config dir", rel(paths.ConfigDir))
	p("KENAZ_HOME state root", rel(paths.StateRoot))

	for _, c := range []struct{ flag, envVar, val, file string }{
		{}, {"dev", "", "", ""}, {"", "KENAZ_ENV", "stage", ""}, {"", "CLAUDE_PLUGIN_OPTION_FLEET_ENV", "dev", ""},
		{"", "FLEET_PLUGIN_ENV", "dev", ""}, {"", "", "", "stage"}, {"bogus", "", "", ""},
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
