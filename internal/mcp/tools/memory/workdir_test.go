package memorytools

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

func TestWorkdirPerHost(t *testing.T) {
	wd := func() (string, error) { return "/", nil }
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"claude code project", map[string]string{"CLAUDE_PROJECT_DIR": "/src/app"}, "/src/app"},
		{"codex working directory", nil, "/"},
		{"claude desktop: home, not the app's directory", map[string]string{mcp.HostEnvVar: mcp.HostClaudeDesktop}, "/home/u"},
		{"project dir still wins", map[string]string{mcp.HostEnvVar: mcp.HostClaudeDesktop, "CLAUDE_PROJECT_DIR": "/src/app"}, "/src/app"},
	}
	for _, c := range cases {
		if got := workdir(env(c.env), wd, home); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
