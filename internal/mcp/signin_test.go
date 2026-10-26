package mcp

import (
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

func TestSignInRemedy(t *testing.T) {
	desktop := func(env string) func(string) string {
		return func(k string) string {
			return map[string]string{HostEnvVar: HostClaudeDesktop, variant.Shared.EnvVar: env}[k]
		}
	}
	none := func(string) string { return "" }
	// --env is passed only for an env other than the build's default.
	devFlag := " --env dev"
	if variant.Current.DefaultEnv == "dev" {
		devFlag = ""
	}
	cases := []struct {
		name, goos, exe, home string
		getenv                func(string) string
		want                  string
	}{
		{"claude code", "darwin", "/opt/kenaz/bin/kenaz", "/Users/u", none, "Run /" + variant.Current.Plugin + ":login."},
		{"desktop macOS, home abbreviated", "darwin",
			"/Users/u/Library/Application Support/Claude/Claude Extensions/local.mcpb.kameas-ai-inc.kenaz/server/kenaz", "/Users/u",
			desktop("prod"), "Open a terminal and run `~/'Library/Application Support/Claude/Claude Extensions/local.mcpb.kameas-ai-inc.kenaz/server/kenaz' login`, then try again."},
		{"desktop dev env", "darwin", "/opt/kenaz/kenaz", "/Users/u", desktop("dev"),
			"Open a terminal and run `/opt/kenaz/kenaz login" + devFlag + "`, then try again."},
		{"desktop unknown env ignored", "darwin", "/opt/k's/kenaz", "/Users/u", desktop("${user_config.fleet_env}"),
			"Open a terminal and run `'/opt/k'\\''s/kenaz' login`, then try again."},
		{"desktop Windows", "windows", `C:\Users\U\AppData\Roaming\Claude\Claude Extensions\x\server\kenaz.exe`, `C:\Users\u`,
			desktop("stage"), "Open PowerShell and run `& \"$env:USERPROFILE\\AppData\\Roaming\\Claude\\Claude Extensions\\x\\server\\kenaz.exe\" login --env stage`, then try again."},
		{"desktop no executable", "linux", "", "/home/u", desktop(""), "Open a terminal and run `" + variant.Current.Binary + " login`, then try again."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := signInRemedy(c.getenv, c.goos, c.exe, c.home)
			if got != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
			if c.home != "" && strings.Contains(got, c.home+"/") {
				t.Fatalf("home directory (user name) in %q", got)
			}
		})
	}
}
